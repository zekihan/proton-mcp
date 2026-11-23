package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

type (
	identityKey     struct{}
	renewedTokenKey struct{}
)

type IdentityInfo struct {
	Name    *string
	Email   *string
	Subject string
}

func requestToken(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}

	return cookie.Value
}

func sessionToken(r *http.Request) string {
	if token, ok := r.Context().Value(renewedTokenKey{}).(string); ok {
		return token
	}

	return requestToken(r)
}

func CSRF(r *http.Request) string {
	token := sessionToken(r)
	if token == "" {
		return ""
	}

	return tokenHash("csrf:" + token)
}

func ValidCSRF(r *http.Request) bool {
	// The submitted form belongs to the token that authenticated this request.
	token := requestToken(r)

	return token != "" &&
		subtle.ConstantTimeCompare(
			[]byte(tokenHash("csrf:"+token)),
			[]byte(r.PostForm.Get("csrf")),
		) == 1
}

func (svc *Service) isOwner(issuer, subject string) bool {
	return issuer == svc.cfg.Issuer && subject != "" && subject == svc.cfg.OwnerSubject
}

func (svc *Service) createSession(
	ctx context.Context,
	w http.ResponseWriter,
	me *IdentityInfo,
) bool {
	value := randomToken()
	now := svc.now()
	created := timestamp(now)

	_, err := svc.db.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, issuer, subject, name, email, created_at, renewed_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		tokenHash(value), svc.cfg.Issuer, me.Subject, me.Name, me.Email,
		created, created, timestamp(now.Add(sessionLifetime)),
	)
	if err != nil {
		return false
	}

	svc.setCookie(w, sessionCookie, "/", value, int(sessionLifetime/time.Second))

	return true
}

// LogoutHandler serves POST /auth/logout.
func (svc *Service) LogoutHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}

		parseErr := r.ParseForm()
		if parseErr != nil || !ValidCSRF(r) {
			http.Error(w, "Invalid form.", http.StatusForbidden)

			return
		}

		_, err := svc.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE token_hash = ?",
			tokenHash(sessionToken(r)))
		if err != nil {
			slog.ErrorContext(r.Context(), "Could not delete session on sign-out")
		}

		svc.setCookie(w, sessionCookie, "/", "", -1)
		w.WriteHeader(http.StatusNoContent)
	})
}

// RequireSession rejects requests without a valid session with 401 JSON and
// renews the session when due. The identity is available via Identity.
func (svc *Service) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		cookie, err := r.Cookie(sessionCookie)
		if err != nil || cookie.Value == "" {
			requireSignIn(w, r)

			return
		}

		ctx := r.Context()
		now := svc.now()
		hash := tokenHash(cookie.Value)

		var (
			me              IdentityInfo
			issuer, renewed string
		)

		err = svc.db.QueryRowxContext(ctx, `
			SELECT issuer, subject, name, email, renewed_at FROM sessions
			WHERE token_hash = ? AND expires_at > ?`, hash, timestamp(now),
		).Scan(&issuer, &me.Subject, &me.Name, &me.Email, &renewed)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			sessionError(w)

			return
		}

		if err != nil || !svc.isOwner(issuer, me.Subject) {
			requireSignIn(w, r)

			return
		}

		if renewed < timestamp(now.Add(-renewalInterval)) {
			token, renewErr := svc.renewSession(ctx, now, hash, renewed)
			if renewErr != nil {
				sessionError(w)

				return
			}
			if token == "" {
				requireSignIn(w, r)

				return
			}
			svc.setCookie(w, sessionCookie, "/", token, int(sessionLifetime/time.Second))
			ctx = context.WithValue(ctx, renewedTokenKey{}, token)
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, identityKey{}, me)))
	})
}

func (svc *Service) renewSession(
	ctx context.Context,
	now time.Time,
	hash, renewed string,
) (string, error) {
	token := randomToken()
	result, err := svc.db.ExecContext(
		ctx,
		`
  UPDATE sessions SET token_hash = ?, renewed_at = ?, expires_at = ?
  WHERE token_hash = ? AND renewed_at = ? AND expires_at > ?`,
		tokenHash(
			token,
		),
		timestamp(now),
		timestamp(now.Add(sessionLifetime)),
		hash,
		renewed,
		timestamp(now),
	)
	if err != nil {
		return "", fmt.Errorf("renew session: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("count renewed sessions: %w", err)
	}
	if count != 1 {
		return "", nil
	}

	return token, nil
}

func requireSignIn(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && approvalPath.MatchString(r.URL.Path) {
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)

		return
	}

	writeSessionError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
}

func sessionError(w http.ResponseWriter) {
	writeSessionError(w, http.StatusInternalServerError, "internal", "Could not check the session.")
}

func writeSessionError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errcheck,errchkjson // A write failure cannot change the response after headers are sent.
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}

// Identity returns the signed-in owner stored on the request context by
// RequireSession.
func Identity(ctx context.Context) (IdentityInfo, bool) {
	me, ok := ctx.Value(identityKey{}).(IdentityInfo)

	return me, ok
}
