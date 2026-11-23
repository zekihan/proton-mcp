package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zekihan/go-commons/metrics"
	"github.com/zekihan/go-commons/utils"

	"github.com/zekihan/proton-mcp/internal/actions"
	"github.com/zekihan/proton-mcp/internal/auth"
	"github.com/zekihan/proton-mcp/internal/config"
	"github.com/zekihan/proton-mcp/internal/mail"
	"github.com/zekihan/proton-mcp/internal/mcp"
	"github.com/zekihan/proton-mcp/internal/server"
	"github.com/zekihan/proton-mcp/internal/store"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 10 * time.Second
	requestTimeout    = 30 * time.Second
	discoveryTimeout  = 15 * time.Second
)

var errDiscovery = errors.New("OIDC discovery failed")

func main() {
	err := run()
	if err != nil {
		slog.Error("Server failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	started := time.Now()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(ctx)
	if err != nil {
		return err
	}

	slog.SetDefault(
		slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})),
	)
	utils.LogVersionInfo(ctx)

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}

	defer func() {
		closeErr := db.Close()
		if closeErr != nil {
			slog.Error("Could not close SQLite")
		}
	}()

	svc := &actions.Service{
		DB: db, Bridge: &mail.Bridge{
			IMAPAddr: cfg.IMAPAddr, SMTPAddr: cfg.SMTPAddr,
			Username: cfg.BridgeUsername, Password: cfg.BridgePassword,
		}, Delay: cfg.SendDelay, TTL: cfg.ApprovalTTL, MaxBody: cfg.MaxBodyBytes,
		SaveSentCopy: cfg.SaveSentCopy, Telegram: &actions.Telegram{
			PublicURL: cfg.PublicURL, APIURL: cfg.TelegramAPIURL,
			Token: cfg.TelegramBotToken, ChatID: cfg.TelegramChatID,
		},
	}
	err = svc.Recover(ctx, started)
	if err != nil {
		return err
	}

	authCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	identity, err := auth.New(authCtx, &auth.Config{
		PublicURL: cfg.PublicURL, Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID,
		ClientSecret: cfg.OIDCClientSecret, OwnerSubject: cfg.OwnerSubject,
		MCPClientID: cfg.MCPClientID, MCPAudience: cfg.MCPAudience,
		CookieInsecure: cfg.CookieInsecure,
	}, db)

	cancel()

	if err != nil {
		return errDiscovery
	}

	return runServers(ctx, cfg, svc, identity)
}

func runServers(
	ctx context.Context,
	cfg *config.Config,
	svc *actions.Service,
	identity *auth.Service,
) error {
	ctx, stop := context.WithCancel(ctx)

	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)

		svc.Run(ctx)
	}()

	defer func() {
		stop()
		<-schedulerDone
	}()

	registry := metrics.New()

	if cfg.MetricsAddr != "" {
		go func() {
			err := registry.ListenAndServe(ctx, cfg.MetricsAddr)
			if err != nil {
				slog.ErrorContext(ctx, "Metrics server failed")
			}
		}()
	}

	handler := server.New(svc, identity, mcp.NewHandler(svc, utils.Version))
	httpServer := &http.Server{
		Addr: cfg.ListenAddr, Handler: registry.Middleware(handler),
		ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: requestTimeout,
		IdleTimeout: idleTimeout,
	}
	slog.InfoContext(ctx, "Listening", slog.String("addr", cfg.ListenAddr))

	return serve(ctx, httpServer)
}

func serve(ctx context.Context, httpServer *http.Server) error {
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	err := httpServer.Shutdown(shutdownCtx)
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	return nil
}
