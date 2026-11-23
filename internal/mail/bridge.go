// Package mail implements the Bridge's IMAP and SMTP operations.
package mail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

const commandTimeout = 20 * time.Second

var (
	ErrBridge   = errors.New("Bridge operation failed")
	ErrNotFound = errors.New("message or folder not found")
	ErrUnsafe   = errors.New("operation cannot be performed safely")
	ErrUIDLimit = errors.New("uids must contain at most 100 entries")
	ErrInput    = errors.New("invalid mail arguments")
	ErrTooLarge = errors.New("message exceeds the supported size")
)

var errBridgeCA = errors.New("BRIDGE_CA_FILE must contain a readable PEM CA bundle")

// LoadBridgeTLS loads trust roots for Bridge STARTTLS only.
func LoadBridgeTLS(caFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile == "" {
		return cfg, nil
	}
	pemData, err := os.ReadFile(caFile)
	if err != nil {
		return nil, errBridgeCA
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemData) {
		return nil, errBridgeCA
	}
	cfg.RootCAs = roots

	return cfg, nil
}

type Bridge struct {
	TLSConfig *tls.Config
	IMAPAddr  string
	SMTPAddr  string
	Username  string
	Password  string
}

func (bridge *Bridge) ProbeIMAP(ctx context.Context) bool {
	_, closeBridge, err := bridge.imap(ctx)
	if err == nil {
		closeBridge()
	}

	return err == nil
}

func (bridge *Bridge) ProbeSMTP(ctx context.Context) bool {
	_, closeBridge, err := bridge.smtp(ctx)
	if err == nil {
		closeBridge()
	}

	return err == nil
}

func (bridge *Bridge) dial(
	ctx context.Context,
	addr string,
) (net.Conn, *tls.Config, func(), error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, nil, nil, ErrBridge
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if bridge.TLSConfig != nil {
		cfg = bridge.TLSConfig.Clone()
	}

	cfg.ServerName = host
	dialer := net.Dialer{Timeout: commandTimeout}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, nil, ErrBridge
	}

	stop := context.AfterFunc(ctx, func() { closeConn(conn) })

	return conn, cfg, func() {
		stop()
		closeConn(conn)
	}, nil
}

func closeConn(conn net.Conn) {
	_ = conn.Close()
}

func (bridge *Bridge) imap(ctx context.Context) (*imapclient.Client, func(), error) {
	conn, cfg, closeBridge, err := bridge.dial(ctx, bridge.IMAPAddr)
	if err != nil {
		return nil, nil, err
	}

	client, err := imapclient.NewStartTLS(conn, &imapclient.Options{TLSConfig: cfg})
	if err != nil {
		closeBridge()

		return nil, nil, ErrBridge
	}

	if client.Login(bridge.Username, bridge.Password).Wait() != nil {
		closeBridge()

		return nil, nil, ErrBridge
	}

	return client, closeBridge, nil
}

func (bridge *Bridge) smtp(ctx context.Context) (*smtp.Client, func(), error) {
	conn, cfg, closeBridge, err := bridge.dial(ctx, bridge.SMTPAddr)
	if err != nil {
		return nil, nil, err
	}

	client, err := smtp.NewClientStartTLS(conn, cfg)
	if err != nil {
		closeBridge()

		return nil, nil, ErrBridge
	}

	client.CommandTimeout = commandTimeout

	client.SubmissionTimeout = commandTimeout
	if client.Auth(sasl.NewPlainClient("", bridge.Username, bridge.Password)) != nil {
		closeBridge()

		return nil, nil, ErrBridge
	}

	return client, closeBridge, nil
}
