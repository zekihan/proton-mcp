// Package config loads the service configuration from the environment.
package config

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/sethvargo/go-envconfig/v2"
)

type Config struct {
	MCPAudience      string        `env:"MCP_AUDIENCE"`
	OwnerSubject     string        `env:"OWNER_SUBJECT, required"`
	MetricsAddr      string        `env:"METRICS_ADDR, default=:9090"`
	DBPath           string        `env:"DB_PATH, default=/data/proton-mcp.db"`
	TelegramAPIURL   string        `env:"TELEGRAM_API_URL, default=https://api.telegram.org"`
	OIDCIssuer       string        `env:"OIDC_ISSUER, required"`
	OIDCClientID     string        `env:"OIDC_CLIENT_ID, required"`
	IMAPAddr         string        `env:"IMAP_ADDR, required"`
	TelegramChatID   string        `env:"TELEGRAM_CHAT_ID, required"`
	MCPClientID      string        `env:"MCP_CLIENT_ID"`
	ListenAddr       string        `env:"LISTEN_ADDR, default=:8080"`
	PublicURL        string        `env:"PUBLIC_URL, required"`
	OIDCClientSecret string        `env:"OIDC_CLIENT_SECRET, required"`
	SMTPAddr         string        `env:"SMTP_ADDR, required"`
	BridgeCAFile     string        `env:"BRIDGE_CA_FILE"`
	BridgeUsername   string        `env:"BRIDGE_USERNAME, required"`
	BridgePassword   string        `env:"BRIDGE_PASSWORD, required"`
	TelegramBotToken string        `env:"TELEGRAM_BOT_TOKEN, required"`
	LogLevel         slog.Level    `env:"LOG_LEVEL, default=INFO"`
	SendDelay        time.Duration `env:"SEND_DELAY, default=3m"`
	ApprovalTTL      time.Duration `env:"APPROVAL_TTL, default=1h"`
	MaxBodyBytes     int           `env:"MAX_BODY_BYTES, default=65536"`
	CookieInsecure   bool          `env:"COOKIE_INSECURE, default=false"`
	SaveSentCopy     bool          `env:"SAVE_SENT_COPY, default=false"`
}

var errConfig = errors.New("invalid service environment configuration")

func Load(ctx context.Context) (*Config, error) {
	cfg := new(Config)
	if envconfig.Process(ctx, cfg) != nil {
		return nil, errConfig
	}

	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	if !validURL(cfg.PublicURL, cfg.CookieInsecure, true) ||
		!validURL(cfg.OIDCIssuer, cfg.CookieInsecure, false) ||
		!validURL(cfg.TelegramAPIURL, true, false) ||
		cfg.SendDelay <= 0 || cfg.ApprovalTTL <= 0 || cfg.MaxBodyBytes <= 0 {
		return nil, errConfig
	}

	if cfg.MCPAudience == "" {
		cfg.MCPAudience = cfg.PublicURL + "/mcp"
	}

	if cfg.MCPClientID != "" && !validURL(cfg.MCPAudience, cfg.CookieInsecure, false) {
		return nil, errConfig
	}

	return cfg, nil
}

func validURL(raw string, allowHTTP, originOnly bool) bool {
	parsed, err := url.Parse(raw)

	return err == nil && parsed.Hostname() != "" && parsed.Fragment == "" && parsed.User == nil &&
		parsed.RawQuery == "" && (!originOnly || parsed.Path == "") &&
		(parsed.Scheme == "https" || (allowHTTP && parsed.Scheme == "http"))
}
