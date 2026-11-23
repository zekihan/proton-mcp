package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/zekihan/proton-mcp/internal/actions"
)

const healthBudget = 5 * time.Second

type health struct {
	flight *healthFlight
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

type healthFlight struct {
	done   chan struct{}
	result healthResult
}

func (health *health) probe(ctx context.Context) healthResult {
	health.mu.Lock()
	if time.Now().Before(health.until) {
		cached := health.result
		health.mu.Unlock()

		return cached
	}
	flight := health.flight
	if flight == nil {
		flight = &healthFlight{done: make(chan struct{})}
		health.flight = flight
		go health.runProbe(context.WithoutCancel(ctx), flight)
	}
	health.mu.Unlock()

	select {
	case <-flight.done:
		return flight.result
	case <-ctx.Done():
		return healthResult{}
	}
}

func (health *health) runProbe(ctx context.Context, flight *healthFlight) {
	ctx, cancel := context.WithTimeout(ctx, healthBudget)
	defer cancel()

	db := startProbe(ctx, health.databaseOK)
	imap := startProbe(ctx, health.svc.Bridge.ProbeIMAP)
	smtp := startProbe(ctx, health.svc.Bridge.ProbeSMTP)
	result := healthResult{
		DB: probeResult(ctx, db), IMAP: probeResult(ctx, imap), SMTP: probeResult(ctx, smtp),
	}

	health.mu.Lock()
	health.result = result
	health.until = time.Now().Add(health.ttl)
	health.flight = nil
	flight.result = result
	close(flight.done)
	health.mu.Unlock()
}

func startProbe(ctx context.Context, probe func(context.Context) bool) <-chan bool {
	result := make(chan bool, 1)
	go func() { result <- check(ctx, probe) }()

	return result
}

func probeResult(ctx context.Context, result <-chan bool) bool {
	select {
	case ok := <-result:
		return ok
	case <-ctx.Done():
		return false
	}
}
