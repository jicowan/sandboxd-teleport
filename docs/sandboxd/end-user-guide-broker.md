# End‑user guide — connecting an MCP client to the sandbox broker

This guide shows you how to connect Claude (or any MCP client) to the sandbox
broker and authenticate. When you're done you'll have a set of sandbox tools
(browser automation, file operations, and — if you're authorized — shell/code
execution) available in your client, each call running inside your own private,
teleportable sandbox.

## What you're connecting to

You connect to a single HTTPS MCP endpoint. Behind it:

- **agentgateway** verifies your identity token and filters which tools you may see.
- **the broker** derives a durable session for you and forwards your calls.
- **sandboxd** runs the actual tools inside a gVisor sandbox and preserves your
  session's state across disconnects (it checkpoints to storage when idle and
  restores when you return).

You don't need to know any of that to use it — you need the endpoint URL and a
login. Your session is tied to *your identity*, not to a particular server, so
you get the same working directory and state back when you reconnect.

## Prerequisites

1. **The MCP endpoint URL** from your administrator. For the reference
   deployment this is:

   ```
   https://agentgateway.jicomusic.com/mcp
   ```

2. **A user account in the identity provider (Keycloak)** that is a member of the
   required group. Membership determines what you can do:

   | Group | What you get |
   |-------|--------------|
   | `sandbox-users` | Web browsing (`browser_*`) and non‑exec sandbox tools: file operations, the string‑replace editor, markdown conversion, context/packages introspection, and the skill loader. |
   | `sandbox-power` | Everything above **plus** shell and code execution (`sandbox_execute_bash`, `sandbox_execute_code`). |

   If you're only in `sandbox-users`, the execution tools are not just blocked —
   they are hidden from your tool list entirely, so you won't see them.

3. **An OAuth‑capable MCP client.** Claude Code and the Claude desktop app both
   support MCP servers with OAuth. Authentication uses the OAuth authorization‑code
   flow with PKCE; your browser opens for login and the client stores the token.

You do **not** need a client secret, an API key, or any Kubernetes access.

## Connect with Claude Code (CLI)

Add the server (an OAuth‑protected streamable‑HTTP MCP server):

```sh
claude mcp add --transport http aio-sandbox https://agentgateway.jicomusic.com/mcp
```

Then start Claude Code and trigger authentication:

```sh
claude
```

On first use of the server, Claude Code discovers the OAuth authorization server
(Keycloak) automatically and opens your browser to log in. Approve the login;
the browser redirects back to a localhost callback and the client stores your
token. You should then see the `aio-sandbox` server listed as **connected** with
its tools available.

To check status at any time, use the MCP management view:

```sh
/mcp
```

A healthy server shows as `connected` with a non‑empty tool list.

## Connect with the Claude desktop app

Add an MCP server of type **HTTP / streamable HTTP** (not stdio) pointing at the
endpoint:

- **URL:** `https://agentgateway.jicomusic.com/mcp`
- **Auth:** OAuth (the app will open a browser to Keycloak on first connect)

The exact menu path varies by app version (typically *Settings → Connectors /
MCP servers → Add*), but the two values that matter are the URL and choosing the
HTTP transport with OAuth. When prompted, log in through the browser window.

## Connect with another MCP client

Any client that supports **streamable‑HTTP MCP with OAuth 2.0** works. Configure:

- **Endpoint:** `https://agentgateway.jicomusic.com/mcp`
- **Transport:** streamable HTTP (HTTP POST to the endpoint; server may reply with
  Server‑Sent Events for streaming responses).
- **Authorization:** OAuth 2.0 authorization‑code + PKCE. The server publishes its
  OAuth metadata (RFC 9728 protected‑resource metadata), so a compliant client can
  discover the Keycloak authorization server on its own. If your client needs
  values entered manually, ask your admin for:
  - Authorization server / issuer: `https://keycloak.jicomusic.com/realms/sandbox`
  - Client ID: `aio-sandbox-client` (a public client; no secret)
  - Scopes: `openid sandbox`

Every request must carry `Authorization: Bearer <token>`; the front door rejects
anything unauthenticated with `401`.

## First‑connect note for Claude Code: "Trusted Hosts"

On a **brand‑new machine/user**, the first time Claude Code connects it tries to
register itself with the identity provider (OAuth Dynamic Client Registration).
Claude Code guards this behind a **"Trusted Hosts"** allowlist, so on first connect
you may see:

```
SDK auth failed: Policy 'Trusted Hosts' rejected request to
client-registration service. Details: Host not trusted.
```

This is a **client‑side safety check in Claude Code**, not a problem with your
account or password — it happens *before* you're even asked to log in. It means
Claude Code doesn't yet trust the identity host (`keycloak.jicomusic.com`) for
client registration.

**Fix:** tell Claude Code to trust the identity host, then reconnect. Set this
environment variable **before launching Claude Code** — put it in your shell
profile (`~/.zshrc` or `~/.bashrc`) so it persists:

```sh
export CLAUDE_CODE_OAUTH_TRUSTED_HOSTS="keycloak.jicomusic.com"
```

Then restart Claude Code and authenticate again. Existing users who connected
earlier won't see this — their client is already registered/cached; it only bites
a genuinely first‑time connection.

> If your organization manages Claude Code centrally, your admin can set this in
> managed settings so new users never hit it — ask them rather than setting it
> yourself.

## Using the tools

Once connected, your client lists the tools you're authorized for. Common ones:

- `browser_navigate`, `browser_click`, `browser_get_markdown`, … — drive a real
  browser inside the sandbox.
- `sandbox_file_operations`, `sandbox_str_replace_editor` — read/write files in
  the sandbox workspace.
- `sandbox_execute_bash`, `sandbox_execute_code` — run commands/code (**power
  users only**).

Just ask Claude to use them naturally ("browse to X and summarize", "write this
file", "run this script"). State you create — files, a browser session, a
running process — persists in **your** session.

### Your session is durable

- Your session id is derived from your identity, so it's the **same every time
  you connect**. Reconnecting resumes your existing sandbox, files intact.
- If you go idle, the platform checkpoints your sandbox to storage and frees the
  worker; your next call transparently restores it (possibly on a different node).
- You don't manage any of this. There's no "session id" to copy around — logging
  in as yourself is enough.

## First call may be slow

If your session has been idle for a while (or is brand new), the **first tool
call can take up to ~45 seconds** while the sandbox cold‑starts or restores from
storage. Subsequent calls are fast. This is normal for the heavier image (which
includes a browser). If your client has a very short per‑request timeout, one
retry usually succeeds.

## Troubleshooting

| Symptom | Likely cause | What to do |
|---------|--------------|------------|
| Client shows `connected · tools fetch failed` | The very first request hit a cold start / no‑capacity blip and the client cached an empty tool list. | Reconnect or re‑authenticate the server (in Claude Code, remove and re‑add, or re‑auth from `/mcp`). |
| `401 Unauthorized` | No token, expired token, or wrong issuer/audience. | Re‑authenticate. Confirm your client is pointed at the right endpoint. |
| Connected, but you don't see `sandbox_execute_bash` / `sandbox_execute_code` | You're in `sandbox-users`, not `sandbox-power`. Exec tools are hidden for standard users. | Ask an admin to add you to `sandbox-power` if you need execution. |
| `403 Forbidden` right after login | Your account isn't in the required group (`sandbox-users`), or the token didn't come through the gateway. | Ask an admin to add you to `sandbox-users`. |
| First call times out | Cold start / restore (~45s). | Retry once; later calls are fast. |
| Login browser window never redirects back | OAuth callback blocked (corporate proxy, non‑loopback redirect). | Ensure your client can open a localhost callback; try Claude Code which uses a loopback redirect. |
| `Policy 'Trusted Hosts' rejected request to client-registration service. Host not trusted` (Claude Code, first connect) | Claude Code's client‑side guard blocks OAuth client registration against an untrusted identity host — happens before login, unrelated to your account. | Set `CLAUDE_CODE_OAUTH_TRUSTED_HOSTS="keycloak.jicomusic.com"` before launching Claude Code and reconnect. See "First‑connect note" above. |

If problems persist, capture the exact error your client shows and share it with
your administrator — they can correlate it against the gateway/broker logs.

## What the platform can see

Your tool calls run inside a sandbox on shared infrastructure. Treat sandbox
output as operational data: administrators can, for debugging, surface a
sandbox's console to cluster logs on some pools. Don't rely on the sandbox for
secret material you wouldn't put in a shared CI environment.
