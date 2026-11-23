package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

var approvalPath = regexp.MustCompile(`^/approve/[a-f0-9-]{36}$`)

// LoginHandler serves GET /auth/login.
func (svc *Service) LoginHandler() http.Handler {
	return http.HandlerFunc(svc.login)
}

func (svc *Service) login(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	ctx := r.Context()
	now := svc.now()
	svc.deleteExpired(ctx, timestamp(now))

	value, state, nonce := randomToken(), randomToken(), randomToken()
	verifier := oauth2.GenerateVerifier()

	returnTo := r.URL.Query().Get("next")
	if !approvalPath.MatchString(returnTo) {
		returnTo = "/"
	}

	_, err := svc.db.ExecContext(ctx, `
		INSERT INTO oidc_transactions (id_hash, state, nonce, pkce_verifier, expires_at, return_to)
		VALUES (?, ?, ?, ?, ?, ?)`,
		tokenHash(value), state, nonce, verifier, timestamp(now.Add(transactionLifetime)), returnTo,
	)
	if err != nil {
		signInError(w, http.StatusInternalServerError, "Could not start sign-in.")

		return
	}

	w.Header().Set("Cache-Control", "no-store")
	svc.setCookie(w, oidcCookie, callbackPath, value, int(transactionLifetime/time.Second))
	authURL := svc.oauth.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oidc.Nonce(nonce),
		oauth2.SetAuthURLParam("response_mode", "query"),
	)
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

func (svc *Service) deleteExpired(ctx context.Context, now string) {
	_, err := svc.db.ExecContext(ctx, "DELETE FROM oidc_transactions WHERE expires_at <= ?", now)
	if err != nil {
		slog.ErrorContext(ctx, "Could not delete expired sign-in transactions")
	}

	_, err = svc.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", now)
	if err != nil {
		slog.ErrorContext(ctx, "Could not delete expired sessions")
	}
}

// CallbackHandler serves GET /auth/callback.
func (svc *Service) CallbackHandler() http.Handler {
	return http.HandlerFunc(svc.callback)
}

func (svc *Service) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	svc.setCookie(w, oidcCookie, callbackPath, "", -1)

	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	cookie, err := r.Cookie(oidcCookie)
	if err != nil || cookie.Value == "" {
		invalidSignIn(w)

		return
	}

	ctx := r.Context()

	var state, nonce, verifier, expires, returnTo string

	err = svc.db.QueryRowxContext(ctx, `
		DELETE FROM oidc_transactions WHERE id_hash = ?
		RETURNING state, nonce, pkce_verifier, expires_at, return_to`, tokenHash(cookie.Value),
	).Scan(&state, &nonce, &verifier, &expires, &returnTo)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		signInError(w, http.StatusInternalServerError, "Could not complete sign-in.")

		return
	}

	query := r.URL.Query()

	code := query.Get("code")
	if err != nil || expires <= timestamp(svc.now()) || query.Has("error") || code == "" ||
		subtle.ConstantTimeCompare([]byte(state), []byte(query.Get("state"))) != 1 {
		invalidSignIn(w)

		return
	}

	token, claims := svc.exchange(ctx, code, verifier, nonce)
	if token == nil {
		invalidSignIn(w)

		return
	}

	if !svc.isOwner(token.Issuer, token.Subject) {
		signInError(w, http.StatusForbidden, "This account cannot access Proton MCP.")

		return
	}

	me := IdentityInfo{Subject: token.Subject, Name: claims.Name, Email: claims.Email}
	if !svc.createSession(ctx, w, &me) {
		signInError(w, http.StatusInternalServerError, "Could not create a session.")

		return
	}

	http.Redirect(w, r, returnTo, http.StatusSeeOther)
}

type idClaims struct {
	Name  *string         `json:"name"`
	Email *string         `json:"email"`
	AZP   json.RawMessage `json:"azp"`
}

func (svc *Service) exchange(
	ctx context.Context,
	code, verifier, nonce string,
) (*oidc.IDToken, *idClaims) {
	ctx = oidc.ClientContext(ctx, svc.client)
	oauthToken, err := svc.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, nil
	}

	raw, ok := oauthToken.Extra("id_token").(string)
	if !ok {
		return nil, nil
	}

	token, err := svc.verifier.Verify(ctx, raw)
	if err != nil || token.IssuedAt.IsZero() || token.IssuedAt.After(svc.now().Add(issuedAtSkew)) ||
		subtle.ConstantTimeCompare([]byte(token.Nonce), []byte(nonce)) != 1 {
		return nil, nil
	}

	var claims idClaims
	if token.Claims(&claims) != nil || !svc.validAuthorizedParty(token.Audience, claims.AZP) {
		return nil, nil
	}

	return token, &claims
}

func (svc *Service) validAuthorizedParty(audience []string, raw json.RawMessage) bool {
	if raw == nil {
		return len(audience) == 1
	}

	var azp *string

	return json.Unmarshal(raw, &azp) == nil && azp != nil && *azp == svc.cfg.ClientID
}

func invalidSignIn(w http.ResponseWriter) {
	signInError(w, http.StatusBadRequest, "Sign-in could not be verified.")
}

func signInError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	//nolint:errcheck // A write failure cannot change the response after headers are sent.
	_, _ = w.Write([]byte(`<!doctype html><title>Proton MCP sign-in</title><p>` +
		message + `</p><p><a href="/auth/login">Sign in again</a></p>`))
}
