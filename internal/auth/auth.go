// Package auth implements SSO sign-in against one OIDC provider and the
// server-side sessions (DESIGN.md section 7, Sign-in).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jmoiron/sqlx"
	"golang.org/x/oauth2"
)

const (
	oidcCookie    = "pm_oidc_tx"
	sessionCookie = "pm_session"
	callbackPath  = "/auth/callback"

	oidcHTTPTimeout     = 12 * time.Second
	transactionLifetime = 10 * time.Minute
	sessionLifetime     = 30 * 24 * time.Hour
	renewalInterval     = time.Hour
	issuedAtSkew        = 5 * time.Minute

	// Fixed-width UTC timestamps sort chronologically in SQLite.
	timestampLayout = "2006-01-02T15:04:05.000000000Z"
)

var errNoJWKS = errors.New("OIDC discovery document has no jwks_uri")

// Config holds what the auth service needs from the server configuration.
type Config struct {
	PublicURL    string
	Issuer       string
	ClientID     string
	ClientSecret string
	OwnerSubject string
	// MCPClientID is the OAuth client whose access tokens may call /mcp; empty
	// disables MCP.
	MCPClientID string
	// MCPAudience is the resource the access tokens must name in aud.
	MCPAudience    string
	CookieInsecure bool
}

// Service owns the sign-in flow and sessions.
type Service struct {
	db       *sqlx.DB
	client   *http.Client
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
	bearer   *oidc.IDTokenVerifier
	now      func() time.Time
	cfg      Config
}

// New discovers the provider and returns the service. Db must already be
// migrated.
func New(ctx context.Context, cfg *Config, db *sqlx.DB) (*Service, error) {
	client := &http.Client{Timeout: oidcHTTPTimeout}
	ctx = oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discover OIDC provider: %w", err)
	}

	var discovery struct {
		JWKSURL string `json:"jwks_uri"`
	}

	err = provider.Claims(&discovery)
	if err != nil || discovery.JWKSURL == "" {
		return nil, errNoJWKS
	}

	// One key set serves both verifiers, so keys are fetched and cached once.
	keySet := oidc.NewRemoteKeySet(ctx, discovery.JWKSURL)

	service := &Service{
		cfg:      *cfg,
		db:       db,
		client:   client,
		now:      time.Now,
		verifier: nil,
		bearer:   nil,
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  strings.TrimRight(cfg.PublicURL, "/") + callbackPath,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
	}
	service.verifier = oidc.NewVerifier(cfg.Issuer, keySet, &oidc.Config{
		ClientID: cfg.ClientID,
		SupportedSigningAlgs: []string{
			oidc.RS256, oidc.RS384, oidc.RS512,
			oidc.ES256, oidc.ES384, oidc.ES512,
			oidc.PS256, oidc.PS384, oidc.PS512,
			oidc.EdDSA,
		},
		Now:                        func() time.Time { return service.now() },
		SkipClientIDCheck:          false,
		SkipExpiryCheck:            false,
		SkipIssuerCheck:            false,
		InsecureSkipSignatureCheck: false,
	})
	service.bearer = newBearerVerifier(
		cfg.Issuer,
		keySet,
		func() time.Time { return service.now() },
	)

	return service, nil
}

func randomToken() string {
	var random [32]byte
	rand.Read(random[:])

	return base64.RawURLEncoding.EncodeToString(random[:])
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

func timestamp(instant time.Time) string {
	return instant.UTC().Format(timestampLayout)
}

func (svc *Service) setCookie(w http.ResponseWriter, name, path, value string, maxAge int) {
	//nolint:gosec // Secure is configurable for local HTTP; cookies use Max-Age and host-only scope.
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Path:     path,
		Value:    value,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   !svc.cfg.CookieInsecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}

	w.Header().Set("Allow", method)
	http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)

	return false
}
