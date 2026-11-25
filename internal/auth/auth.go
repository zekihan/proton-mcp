// Package auth implements SSO sign-in against one OIDC provider and the
// server-side sessions (DESIGN.md section 7, Sign-in).
//
// Bearer protection for /mcp comes from go-commons/oidcauth (see New). The
// browser flow stays local because it has proton-specific behavior the
// shared package does not expose as options: sign-in returns to the
// /approve/{id} page that required it (oidc_transactions.return_to),
// anonymous approval visits redirect to sign-in and back, session renewal
// rotates the session token (DESIGN.md), and sign-out requires CSRF.
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
	"github.com/zekihan/go-commons/oidcauth"
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

// Service owns the sign-in flow and sessions. The browser flow stays local
// (see oidc.go and session.go); bearer protection for /mcp is delegated to
// the shared oidcauth service (methods below).
type Service struct {
	shared   *oidcauth.Service[oidcauth.IdentityInfo]
	db       *sqlx.DB
	client   *http.Client
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
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

	keySet := oidc.NewRemoteKeySet(ctx, discovery.JWKSURL)

	shared, err := oidcauth.New(ctx, &oidcauth.Config[oidcauth.IdentityInfo]{
		Authorize:         oidcauth.IdentityInfoAuthorize,
		HTTPClient:        client,
		PublicURL:         cfg.PublicURL,
		Issuer:            cfg.Issuer,
		ClientID:          cfg.ClientID,
		ClientSecret:      cfg.ClientSecret,
		OwnerSubject:      cfg.OwnerSubject,
		MCPClientID:       cfg.MCPClientID,
		MCPAudience:       cfg.MCPAudience,
		AppName:           "Proton MCP",
		SessionCookie:     sessionCookie,
		TransactionCookie: oidcCookie,
		SuccessPath:       "/",
		CookieInsecure:    cfg.CookieInsecure,
	}, db)
	if err != nil {
		return nil, fmt.Errorf("create shared auth service: %w", err)
	}

	service := &Service{
		cfg:      *cfg,
		db:       db,
		client:   client,
		shared:   shared,
		now:      time.Now,
		verifier: nil,
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

	return service, nil
}

// MCPEnabled reports whether an MCP client is configured.
func (svc *Service) MCPEnabled() bool {
	return svc.shared.MCPEnabled()
}

// ProtectedResourceHandler serves the protected resource metadata for /mcp.
func (svc *Service) ProtectedResourceHandler() http.Handler {
	return svc.shared.ProtectedResourceHandler()
}

// RequireBearer accepts only access tokens issued to the MCP client for
// the owner, via the shared oidcauth service.
func (svc *Service) RequireBearer(next http.Handler) http.Handler {
	return svc.shared.RequireBearer(next)
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
