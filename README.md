# Proton MCP

Proton MCP gives Claude a Streamable HTTP MCP endpoint for one Proton mailbox.
It connects to an existing Proton Mail Bridge over authenticated IMAP and SMTP
with STARTTLS. The owner reviews sensitive actions on a server-rendered page
and receives Telegram notifications.

## Safety model

- Only the configured OIDC subject can use MCP or sign in to approve actions.
- Sending, forwarding, and permanent deletion require a browser POST with a
  valid session and CSRF token. Approval starts a cancellable delay.
- The approval page displays the request-time snapshot. The scheduler checks
  its SHA-256 digest and claims it in SQLite before accessing the mailbox.
  Overdue work encountered at startup requires fresh approval. An execution
  interrupted after its claim is never replayed.
- A draft with a Bcc header is rejected. Forwarded messages contain text only.
  Permanent deletion is scoped to selected UIDs in Trash or Spam. Replacing a
  draft can expunge only an earlier draft tracked by this service.
- Mail read through MCP uses BODY.PEEK. HTML previews contain visible plain
  text and attachment metadata. Telegram and the audit trail omit mail bodies.
- SMTP acceptance determines the `sent` status. If the result is uncertain,
  check the mailbox before creating another request.

Kubernetes can use `/livez` for SQLite-only checks. Use `/healthz` to monitor
Bridge logins as well.

SQLite stores the complete snapshots, sessions and audit trail. Give the
database and its backups the same access controls as the mailbox.

## Configuration

| Environment variable | Default or purpose |
| --- | --- |
| `PUBLIC_URL` | Required HTTPS origin, e.g. `https://proton-mcp.example.com` |
| `LISTEN_ADDR` | `:8080` |
| `METRICS_ADDR` | `:9090`; set empty to disable |
| `DB_PATH` | `/data/proton-mcp.db` |
| `LOG_LEVEL` | `INFO`; audit events always go to stdout as JSON |
| `OIDC_ISSUER` | Required issuer, e.g. `https://auth.example.com` |
| `OIDC_CLIENT_ID` | Required approval-page OAuth client ID |
| `OIDC_CLIENT_SECRET` | Required approval-page OAuth client secret |
| `OWNER_SUBJECT` | Required `sub` shared by the owner's browser and MCP tokens |
| `MCP_CLIENT_ID` | MCP OAuth client ID; empty disables MCP |
| `MCP_AUDIENCE` | `PUBLIC_URL` plus `/mcp` |
| `COOKIE_INSECURE` | `false`; set true only for local HTTP testing |
| `IMAP_ADDR` | Required Bridge IMAP address, e.g. `bridge.example.com:1143` |
| `SMTP_ADDR` | Required Bridge SMTP address, e.g. `bridge.example.com:1025` |
| `BRIDGE_USERNAME` | Required Bridge mailbox address |
| `BRIDGE_PASSWORD` | Required Bridge-generated password |
| `BRIDGE_CA_FILE` | Optional PEM bundle used as the trust roots for Bridge STARTTLS |
| `TELEGRAM_BOT_TOKEN` | Required Telegram bot token |
| `TELEGRAM_CHAT_ID` | Required destination chat |
| `TELEGRAM_API_URL` | `https://api.telegram.org` |
| `SEND_DELAY` | `3m` |
| `APPROVAL_TTL` | `1h` |
| `MAX_BODY_BYTES` | `65536` bytes of decoded text returned by `read_message` before escaping |
| `SAVE_SENT_COPY` | `false`; enable only when Bridge does not file a Sent copy |

## Build and deploy

```sh
make lint
make test
go build ./...
make build/docker
docker build -t ghcr.io/zekihan/proton-mcp:local .
```

See [deployment](docs/DEPLOYMENT.md) for Authelia, Bridge and Claude setup.
See [the design](docs/DESIGN.md) for the tool and lifecycle contract.
