# Homelab deployment

Run one replica of `ghcr.io/zekihan/proton-mcp` with the Recreate deployment
strategy. Mount a writable PVC at `/data` and an emptyDir at `/tmp`. Run as
UID and GID `25120` with `readOnlyRootFilesystem: true`. Keep port 9090 on
the internal network for Prometheus. Route the public TLS ingress to port 8080.
Use `/livez` for Kubernetes probes. It checks SQLite independently of Bridge.
Use `/healthz` for external monitoring. It adds an IMAP login and SMTP AUTH
check, with one probe in flight and a 30-second cache.

Mount a PEM bundle containing the Bridge CA and set `BRIDGE_CA_FILE` to its
path. The bundle supplies the trust roots for Bridge STARTTLS only. OIDC and
Telegram continue to use the system roots.
The host in each Bridge address must match the certificate's subject
alternative name. Configure IMAP and SMTP for STARTTLS and pass the
Bridge-issued credentials through Kubernetes Secrets. Keep the Bridge ports
private and restrict the service's network access to the Bridge, Authelia,
Telegram and DNS.

## Authelia

Register separate confidential authorization-code clients with PKCE `S256`.
Configure the owner policy for both. Set `OWNER_SUBJECT` to the subject the
provider actually returns for the owner on both clients.

| Client | Redirect URIs | Configuration |
| --- | --- | --- |
| `proton-mcp` | `https://proton-mcp.example.com/auth/callback` | `openid`, `profile`, `email` |
| `proton-mcp-mcp` | `https://claude.ai/api/mcp/auth_callback`, `http://localhost:33418/callback` | `openid`, `offline_access`; allow refresh tokens and sign JWT access tokens with RS256 |

Set the MCP client's Authelia audience to `https://proton-mcp.example.com/mcp`
and set `access_token_signed_response_alg: RS256`. Allow that resource in
access tokens minted for the MCP client. The access token must include its
client identifier in `client_id` or `azp`. Configure `MCP_CLIENT_ID` as
`proton-mcp-mcp` and supply the browser client's ID and secret through the
`OIDC_CLIENT_ID` and `OIDC_CLIENT_SECRET` environment variables.
[Authelia client configuration](https://www.authelia.com/configuration/identity-providers/openid-connect/clients/)

In Claude, add the custom connector URL
`https://proton-mcp.example.com/mcp` and use the MCP client credentials. For
Claude Code, run the following and enter the MCP client's secret when prompted.

```sh
claude mcp add --transport http --client-id proton-mcp-mcp --client-secret --callback-port 33418 proton-mail https://proton-mcp.example.com/mcp
```

## Bridge checks

Inspect folder discovery and verify `UIDPLUS` support before relying on draft
replacement or permanent deletion. The service prefers the `\\Trash` and
`\\Junk` attributes. It falls back to the folder names Trash, Spam, and Junk.
After one approved test send, verify the delivered message, draft removal, and
that exactly one copy appears in Sent. Leave `SAVE_SENT_COPY` at its default
unless this check confirms the Bridge does not file a copy.

Telegram approval and cancellation links open the same owner-only approval
page. The cancel button on that page sends the required authenticated POST.
Keep Telegram link previews disabled and pass the bot token and chat ID
through Kubernetes Secrets.
