// Package server owns HTTP routing, approval pages and health checks.
package server

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"github.com/zekihan/proton-mcp/internal/actions"
	"github.com/zekihan/proton-mcp/internal/auth"
	"github.com/zekihan/proton-mcp/internal/mail"
)

const (
	healthTTL    = 30 * time.Second
	formLimit    = 4096
	probeTimeout = 3 * time.Second
)

type Server struct {
	actions *actions.Service
	health  *health
}

func New(svc *actions.Service, identity *auth.Service, mcp http.Handler) http.Handler {
	server := &Server{actions: svc, health: &health{svc: svc, ttl: healthTTL}}
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", server.health)
	mux.Handle("/mcp", identity.RequireBearer(mcp))
	mux.Handle("/.well-known/oauth-protected-resource", identity.ProtectedResourceHandler())
	mux.Handle("/auth/login", identity.LoginHandler())
	mux.Handle("/auth/callback", identity.CallbackHandler())
	mux.Handle("/auth/logout", identity.RequireSession(identity.LogoutHandler()))
	mux.Handle("GET /approve/{id}", identity.RequireSession(http.HandlerFunc(server.review)))
	mux.Handle("POST /approve/{id}", identity.RequireSession(http.HandlerFunc(server.decide)))
	mux.Handle("POST /queued/{id}/cancel", identity.RequireSession(http.HandlerFunc(server.cancel)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		//nolint:errcheck // The response cannot be recovered after a write error.
		_, _ = w.Write([]byte("Open an approval link from Telegram to review a mail action."))
	})

	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; "+
				"base-uri 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

type view struct {
	Action      actions.Action
	CSRF        string
	Digest      string
	Message     *mail.Message
	Folder      string
	Messages    []mail.Summary
	Decidable   bool
	Cancellable bool
}

func (server *Server) review(w http.ResponseWriter, r *http.Request) {
	// The session middleware redirects anonymous GET requests back to this path after sign-in.
	action, snapshot, digest, err := server.actions.Review(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)

		return
	}

	content := view{
		Action: action, Digest: digest, CSRF: auth.CSRF(r),
		Folder: snapshot.Location.Folder, Messages: snapshot.Messages,
		Decidable:   action.Status == actions.Pending || action.Status == actions.Stalled,
		Cancellable: action.RunAt != nil,
	}
	if len(snapshot.Raw) > 0 {
		message, err := mail.Inspect(snapshot.Raw, 0)
		if err != nil {
			writeError(w, err)

			return
		}

		content.Message = &message
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	//nolint:errcheck // No response can be recovered if the client disconnects.
	_ = approval.Execute(w, content)
}

func (server *Server) decide(w http.ResponseWriter, r *http.Request) {
	server.apply(w, r, false)
}

func (server *Server) cancel(w http.ResponseWriter, r *http.Request) {
	server.apply(w, r, true)
}

func (server *Server) apply(w http.ResponseWriter, r *http.Request, cancel bool) {
	r.Body = http.MaxBytesReader(w, r.Body, formLimit)
	parseErr := r.ParseForm()
	if parseErr != nil || !auth.ValidCSRF(r) {
		http.Error(w, "Invalid approval form.", http.StatusForbidden)

		return
	}

	decision := r.PostForm.Get("decision")
	if cancel {
		decision = "cancel"
	} else if decision != "approve" && decision != "reject" {
		http.Error(w, "Invalid decision.", http.StatusBadRequest)

		return
	}

	id := r.PathValue("id")

	err := server.actions.Decide(r.Context(), id, decision, r.PostForm.Get("snapshot"))
	if err != nil {
		writeError(w, err)

		return
	}

	http.Redirect(w, r, "/approve/"+url.PathEscape(id), http.StatusSeeOther)
}

func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mail.ErrNotFound):
		http.Error(w, "Action not found.", http.StatusNotFound)
	case errors.Is(err, actions.ErrConflict):
		http.Error(w, "This action has changed. Reload the approval page.", http.StatusConflict)
	case errors.Is(err, actions.ErrIntegrity):
		http.Error(w, "The snapshot failed its integrity check.", http.StatusConflict)
	default:
		http.Error(w, "Could not complete the request.", http.StatusInternalServerError)
	}
}

var approval = template.Must(template.New("approval").Parse(`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mail approval</title>
<style>
body { font: 1rem/1.5 system-ui, sans-serif; color-scheme: light dark;
  margin: 2rem auto; padding: 0 1rem; max-width: 55rem }
pre { white-space: pre-wrap; overflow-wrap: anywhere; border: 1px solid gray; padding: 1rem; unicode-bidi: plaintext }
dd, li { overflow-wrap: anywhere; unicode-bidi: isolate }
button { font: inherit; padding: .4rem .8rem; margin: 0 .4rem .75rem 0 }
</style>
<main>
<h1>Mail approval</h1>
<p><strong>{{.Action.Kind}}</strong> · {{.Action.Status}}</p>
{{with .Action.RunAt}}<p>Scheduled for {{.Format "2006-01-02 15:04:05 MST"}}</p>{{end}}
{{with .Action.Detail}}<p>{{.}}</p>{{end}}
{{with .Message}}
<dl>
<dt>From</dt><dd>{{range .From}}<div>{{.}}</div>{{end}}</dd>
<dt>To</dt><dd>{{range .To}}<div>{{.}}</div>{{end}}</dd>
<dt>Cc</dt><dd>{{range .Cc}}<div>{{.}}</div>{{end}}</dd>
<dt>Bcc</dt><dd>None</dd>
<dt>Subject</dt><dd>{{.Subject}}</dd>
</dl>
<pre>{{.Body}}</pre>
{{with .HTMLBody}}<h2>HTML version as text</h2><pre>{{.}}</pre>{{end}}
{{if .Attachments}}<h2>Attachments</h2><ul>
{{range .Attachments}}<li>{{.Name}} ({{.Type}}, {{.Size}} bytes)</li>{{end}}</ul>{{end}}
{{end}}
{{if .Messages}}
<p>Folder <strong>{{.Folder}}</strong></p>
<ul>{{range .Messages}}<li>UID {{.UID}} · {{.Subject}} · {{range .From}}{{.}} {{end}}</li>{{end}}</ul>
{{end}}
{{if .Decidable}}
<form method="post" action="/approve/{{.Action.ID}}">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<input type="hidden" name="snapshot" value="{{.Digest}}">
<button name="decision" value="approve">Approve</button>
<button name="decision" value="reject">Reject</button>
</form>
{{end}}
{{if .Cancellable}}
<form method="post" action="/queued/{{.Action.ID}}/cancel">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<button>Cancel action</button>
</form>
{{end}}
<form method="post" action="/auth/logout">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<button>Sign out</button>
</form>
</main>
</html>`))

func check(ctx context.Context, probe func(context.Context) bool) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	return probe(ctx)
}
