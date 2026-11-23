package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/zekihan/proton-mcp/internal/actions"
)

type health struct {
	until  time.Time
	svc    *actions.Service
	ttl    time.Duration
	mu     sync.Mutex
	result healthResult
}

type healthResult struct {
	DB   bool `json:"db"`
	IMAP bool `json:"imap"`
	SMTP bool `json:"smtp"`
}

func (health *health) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	result := health.probe(r.Context())

	w.Header().Set("Content-Type", "application/json")

	if !result.DB || !result.IMAP || !result.SMTP {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	//nolint:errcheck,errchkjson // A write error cannot change the response.
	_ = json.NewEncoder(w).Encode(result)
}

type liveResult struct {
	DB bool `json:"db"`
}

func (health *health) live(w http.ResponseWriter, r *http.Request) {
	result := liveResult{DB: check(r.Context(), health.databaseOK)}
	w.Header().Set("Content-Type", "application/json")
	if !result.DB {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	//nolint:errcheck,errchkjson // A write error cannot change the response.
	_ = json.NewEncoder(w).Encode(result)
}

func (health *health) databaseOK(ctx context.Context) bool {
	var available int

	return health.svc.DB.GetContext(
		ctx,
		&available,
		"SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='actions' AND type='table')",
	) == nil &&
		available == 1
}

func (health *health) probe(ctx context.Context) healthResult {
	health.mu.Lock()
	defer health.mu.Unlock()

	if time.Now().Before(health.until) {
		return health.result
	}
	// A disconnected probe caller should not poison the shared result.
	ctx = context.WithoutCancel(ctx)
	health.result = healthResult{
		DB:   check(ctx, health.databaseOK),
		IMAP: check(ctx, health.svc.Bridge.ProbeIMAP),
		SMTP: check(ctx, health.svc.Bridge.ProbeSMTP),
	}
	health.until = time.Now().Add(health.ttl)

	return health.result
}
