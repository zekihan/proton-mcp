# AGENTS.md

Guidance for AI agents working in this repository.

## Documents

- `docs/DESIGN.md` — source of truth for behavior and safety rules.
- `docs/DEPLOYMENT.md` — Bridge, Authelia and deployment configuration.

## Layout

| Path | Contents |
| --- | --- |
| `main.go` | Entry point, scheduler and listeners |
| `internal/config` | Environment configuration |
| `internal/store` | SQLite schema and audit records |
| `internal/auth` | OIDC sign-in, sessions and MCP bearer authentication |
| `internal/mail` | Bridge IMAP/SMTP and message parsing |
| `internal/actions` | Snapshots, approvals, scheduler and Telegram outbox |
| `internal/mcp` | Agent tools and tool audit |
| `internal/server` | HTTP routing, health probe and approval UI |

## Commands

```sh
make lint
make test
make build/local
make build/docker
```

## Rules

- Preserve the authenticated approval and cancellation window for gated actions.
- Use UID-scoped IMAP operations and check UIDVALIDITY before expunging.
- Never log mail bodies, raw snapshots or secrets. Use example addresses in fixtures.
