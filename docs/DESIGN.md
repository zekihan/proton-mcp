# proton-mcp contract (shared by all pieces)

## Purpose
Go MCP server that lets Claude (claude.ai connector and Claude Code) work on a Proton
mailbox through an existing in-cluster Proton Bridge (IMAP 1143 STARTTLS, SMTP 1025
STARTTLS). Public repo github.com/zekihan/proton-mcp, AGPL-3.0-only like family-tab.
Nothing personal or secret in the repo: no real addresses, chat IDs, hostnames other than
examples. Tests use fake IMAP/SMTP servers.

## Image
ghcr.io/zekihan/proton-mcp, built like family-tab (goreleaser + zekihan/release-workflows
decide/publish, distroless base ghcr.io/zekihan/distroless, USER 25120:25120).
Ports: 8080 http, 9090 metrics (internal listener, go-commons metrics like family-tab).
Volume /data (SQLite at /data/proton-mcp.db). /tmp emptyDir. readOnlyRootFilesystem.

## Environment (all names fixed)
PUBLIC_URL            https://proton-mcp.example.com
LISTEN_ADDR           :8080
METRICS_ADDR          :9090
DB_PATH               /data/proton-mcp.db
LOG_LEVEL             INFO
OIDC_ISSUER           https://auth.example.com
OIDC_CLIENT_ID        browser client for the approval page (homelab: proton-mcp)
OIDC_CLIENT_SECRET    secret
OWNER_SUBJECT         the only Authelia subject accepted (both browser and MCP tokens)
MCP_CLIENT_ID         MCP connector client (homelab: proton-mcp-mcp); empty disables /mcp
MCP_AUDIENCE          defaults to PUBLIC_URL + /mcp
COOKIE_INSECURE       "false"
IMAP_ADDR             bridge.example.com:1143
SMTP_ADDR             bridge.example.com:1025
BRIDGE_USERNAME       secret
BRIDGE_PASSWORD       secret
BRIDGE_CA_FILE        optional PEM CA bundle used exclusively for Bridge STARTTLS; other HTTPS uses system roots
TELEGRAM_BOT_TOKEN    secret
TELEGRAM_CHAT_ID      secret
SEND_DELAY            3m   (Go duration)
APPROVAL_TTL          1h
MAX_BODY_BYTES        65536
TELEGRAM_API_URL      https://api.telegram.org (HTTPS required except on loopback)
SAVE_SENT_COPY        false

## HTTP surface
/livez                        200 when SQLite is OK; 503 otherwise, JSON detail; no Bridge checks
/healthz                      200 when DB ok AND IMAP login AND SMTP auth to Bridge succeed (cached ~30s, one shared check with a 5s total budget and 3s per probe); 503 otherwise, JSON detail
/mcp                          MCP streamable HTTP, bearer JWT (RS256, issuer JWKS, aud=MCP_AUDIENCE, sub=OWNER_SUBJECT)
/.well-known/oauth-protected-resource  resource metadata naming OIDC_ISSUER
/auth/login, /auth/callback, /auth/logout   browser OIDC (PKCE) session for approval pages; renewal rotates the session token
/approve/{id}                 GET shows the exact pending action; POST approve / reject (CSRF-protected, owner session required)
/queued/{id}/cancel           POST cancel a queued action (owner session, CSRF)
Security headers, no external assets, no remote fetches.

## Tools (MCP)
Ungated:
- list_folders
- list_messages(folder, limit<=100, cursor, unread_only)
- search_messages(folder, query fields: from,to,subject,text,since,before, limit)
- read_message(folder, uid)  -> BODY.PEEK (never sets \Seen); HTML->plain text, strip hidden
  elements/comments/styles/scripts; wrap body in <untrusted_email from="..." subject="...">...</untrusted_email>;
  cap MAX_BODY_BYTES with truncation note; attachments listed as name/type/size only.
- mark_messages(folder, uids[], read bool), up to 100 UIDs
- move_messages(folder, uids[], to_folder)  (incl. Trash; discard_draft == move to Trash), up to 100 UIDs
- save_draft(to[], cc[], subject, body_text, in_reply_to_folder?, in_reply_to_uid?, replace_draft_uid?)
  -> APPEND to Drafts with \Draft; sets In-Reply-To/References when replying.
  When replace_draft_uid is given AND that UID is recorded in the DB as created by this server,
  the old copy is expunged (UID EXPUNGE) without approval; otherwise refuse to expunge.
  No attachments in v1.
- cancel_send(action_id)  cancel a queued or pending action
- get_action(action_id)   status: pending|approved_queued|cancelled|rejected|expired|stalled|sent|failed|done
Gated (create a pending action, Telegram ping with link PUBLIC_URL/approve/{id}, return action id + status, NEVER "sent"):
- send_draft(draft_uid)  snapshot the draft (full RFC822 bytes + sha256) at request time;
  approval page renders From/To/Cc/Bcc(reject any Bcc)/Subject/body from the snapshot.
- forward_message(folder, uid, to[], note_text)  body-only forward, no attachments; snapshot built at request time.
- delete_permanently(folder, uids[])  up to 100 UIDs in Trash or Spam; snapshot lists the messages.

## Lifecycle
pending --approve--> approved_queued (run_at = now+SEND_DELAY) --timer--> sent/done | failed
pending --reject--> rejected ; pending --TTL--> expired ; queued --cancel--> cancelled (draft untouched)
Cancellation stays available while the scheduler prepares the Bridge connection. A
transaction checks cancellation and records the start of execution immediately
before the first SMTP or IMAP write. Later cancellation attempts return a conflict.
On startup, overdue queued actions that have not started execution are marked
stalled. Telegram sends a link to /approve/{id} for reconfirmation. The action
expires after APPROVAL_TTL from the stall. Re-approval queues it with a fresh
SEND_DELAY. Interrupted execution is marked failed and never replayed.
Sending: SMTP via Bridge with the snapshot bytes exactly. After SMTP accepts, expunge the
draft (send_draft only). Do NOT append to Sent (Proton files it); make this behavior a flag
SAVE_SENT_COPY default false.
Telegram messages: approval request, queued ("sending at HH:MM", cancel link), sent, failed,
stalled. Never include message bodies in Telegram; subject + recipients only.
No rate limits. Any recipient allowed (approval is the check).

## Audit
Every tool call and state transition logged as structured JSON on stdout (action, ids,
folder, uids, recipients, status). Never log bodies or secrets. Also stored in SQLite audit table.

## Concurrency
Single replica (Recreate). One scheduler goroutine; DB is the source of truth; execution
transitions use a conditional UPDATE so an action can run at most once.
