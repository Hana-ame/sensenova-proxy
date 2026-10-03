# SenseNova Multi-Egress Reverse Proxy

A high-performance, standalone reverse proxy referencing [`gh/Hana-ame/api-pack`](https://github.com/Hana-ame/api-pack), tailored for SenseNova (`https://token.sensenova.cn`) **and** the OpenCode zen free-tier lane (`https://opencode.ai/zen/v1`).

It provides transparent request passthrough, multi-instance concurrency, automated authentication injection, CORS repair, and dedicated outbound network egress per listening port. Pointing an OpenAI-compatible client at an `opencode.ai` endpoint transparently applies the gateway's client fingerprint, the free-tier request-body gate, and SSE stream hardening — no client-side changes required.

---

## 🌟 Key Features

1. **Specific Egress Binding (`engress`)**:
   - **IP Address**: Binds outbound TCP traffic to a specific local IPv4 or IPv6 address (`LocalAddr`).
   - **Network Interface**: On Linux, binds sockets directly to a specific physical or virtual network interface (`eth0`, `ens3`, `wg0`, etc.) using `SO_BINDTODEVICE`.
   - **Proxy URL**: Outbound routing via HTTP, HTTPS, or SOCKS5 proxies (`http://...`, `socks5://...`).
   - **Default Route**: Leave empty or omit for standard routing.

2. **Zero-Buffering Real-Time Streaming**:
   - Immediate per-write flushing (`FlushInterval: -1` and immediate `flusher.Flush()` on every chunk) ensures Server-Sent Events (SSE) tokens stream to clients with zero lag.
   - Forcibly overrides `Accept-Encoding: identity` to prevent upstream SenseNova / Nginx / Cloudflare from gzip-buffering SSE tokens in 4KB-32KB blocks.
   - Automatically sets `X-Accel-Buffering: no`, `Cache-Control: no-cache, no-transform`, and enables full-duplex HTTP streaming.

3. **Full Header Passthrough & Auth Key Exclusion**:
   - All incoming headers from the client and all outgoing headers from upstream (including CDN/Cloudflare headers like `cf-ray`) are passed through completely intact.
   - **Auth Key Exclusion**: The original client request's credentials (`Authorization`, `X-Api-Key`, `api-key`) are safely excluded so client keys are never leaked upstream, and the proxy's configured `Authkey` is injected instead.

4. **CORS Preflight Repair**:
   - Resolves the known upstream SenseNova issue where `OPTIONS` preflight returns 404 or lacks CORS headers (as documented in `Hana-ame/api-pack`).
   - Intercepts `OPTIONS` requests and responds with `204 No Content` and full CORS headers.
   - Injects cross-origin headers (`Access-Control-Allow-Origin`, `Access-Control-Allow-Credentials: true`, `Access-Control-Expose-Headers: *`) into all proxied responses.

5. **Multi-Instance Concurrency**:
   - Runs multiple isolated proxy instances simultaneously from a single JSON config list.
   - Each instance has its own `listen` port, `endpoint`, `Authkey`, and outbound `engress`.

6. **Pure Standalone Binary**:
   - Built exclusively with the Go standard library (`net/http`, `net/http/httputil`, `syscall`).
   - No external third-party dependencies.
   - Compact binary (~7MB) with instant startup.

7. **OpenCode zen Free-Tier Lane** (auto-detected on `opencode.ai` endpoints):
   - **Header fingerprint**: stamps `User-Agent: opencode/1.18.31 …`, `X-Opencode-Client/Session/Request/Project`, and gateway-shaped `ses_…` / `msg_…` ids (bit-inverted, millisecond-monotonic). Client-supplied ids are preserved.
   - **Body gate**: the free tier only answers requests that both stream *and* declare the lowercase tool quartet `bash`/`glob`/`grep`/`read`. This proxy forces `stream: true`, fills any missing quartet slot with a self-disabling decoy, and never lets the model call a decoy (`tool_choice: none` when the client declared no tools, `auto` otherwise).
   - **Body hygiene**: `developer → system` role downgrade (the upstream rejects the developer role), numeric clamps (`max_tokens ≤ 131072`, `0 < top_p ≤ 1`, `0 ≤ temperature ≤ 2`), `reasoning_effort: max`.
   - **Endpoint routing**: models served by `/responses` (muse-spark-*) or `/messages` (union-alpha) are re-routed automatically when the client calls `/chat/completions`.
   - **SSE stream hardening**: an upstream that EOFs or stalls without a `finish_reason` gets a synthetic `bash echo 继续` tool call injected so the client's tool loop resumes instead of dying on a truncated stream.
   - **Gate-error classification**: upstream `FreeTierError` / `FreeUsageLimitError` / `RegionError` / `ModelUnavailable` / `EndpointUnavailable` / `AuthError` responses are logged with an actionable hint and tagged on the `X-Opencode-Gate` response header.

---

## 📋 Configuration (`config.json`)

The configuration file is a JSON array. Each object contains the **four required properties** (plus an optional `provider`):

```json
[
  {
    "endpoint": "https://token.sensenova.cn",
    "Authkey": "sk-your-sensenova-api-key-1",
    "engress": "192.168.1.101",
    "listen": "127.0.0.1:8001"
  },
  {
    "endpoint": "https://opencode.ai/zen/v1",
    "Authkey": "public",
    "engress": "192.168.1.102",
    "listen": "127.0.0.1:8011"
  }
]
```

### Property Description

| Field | Type | Description | Example |
|---|---|---|---|
| `endpoint` | string | Target upstream URL | `"https://token.sensenova.cn"`, `"https://opencode.ai/zen/v1"` |
| `Authkey` | string | API key injected as `Authorization: Bearer <Authkey>`. Defaults to `"public"` for the opencode lane. | `"sk-abc123..."`, `"public"` |
| `engress` | string | Outbound egress IP, interface name, proxy URL, or empty | `"192.168.1.10"`, `"eth0"`, `"socks5://127.0.0.1:1080"` |
| `listen` | string | Local listening IP and port | `"127.0.0.1:8001"`, `":8001"` |
| `provider` | string | Optional. `sensenova` (default), `openai`, `opencode`, or `passthrough`. Auto-detected from the endpoint host when omitted — any `opencode.ai` endpoint runs the free-tier lane. | `"opencode"` |
| `custom_models` | string[] | Optional (opencode lane). Extra model ids injected into the `/v1/models` response so clients see them as available — useful for names the upstream does not actually serve (e.g. `"dsv41f"`). | `["dsv41f"]` |
| `models_mode` | string | Optional. `append` (default) merges `custom_models` into the upstream list; `replace` serves only `custom_models`. | `"append"`, `"replace"` |
| `timeout` | float | Optional (v1.3.1+). Total upstream budget in seconds — from request send to response end. Omitted = 90s default; negative = no proxy-side limit; positive values are clamped to 90s. | `90` |
| `first_byte_timeout` | float | Optional (v1.3.1+). Seconds to wait for the upstream **response headers** ("first byte"). Only bounds queueing/connecting: once headers arrive the stream is no longer limited, so it never truncates a healthy long SSE stream. Omitted = 90s default; negative = unlimited. | `90` |
| `session_header` | string | Optional (v1.3.2+). Name of the **client** request header to take the session id from. Its value is normalised to a valid `ses_*` id and forwarded as `X-Session-Id` / `X-Session-Affinity` / `X-Opencode-Session`, so every request of one conversation lands on the same upstream session (upstream free quota is per-session). | `"X-Session-Id"` |
| `session_fallback` | string | Optional (v1.3.2+). What to do when the client sends no session header: `client` (default, stable per client IP), `instance` (one fixed value for the whole instance), `request` (new session per request — splits the upstream quota, avoid). | `"client"` |
| `sources` | object[] | **Multi-source aggregate ("一拖多")** — when set, this instance becomes an aggregate endpoint: clients configure a single baseURL and the proxy tries sources in order, auto-failing over when one is exhausted (429 / `FreeUsageLimitError`) with cooldown until the next UTC midnight. Each source fixes its own egress: no client-side param needed. See below. | see below |

### Timeouts (v1.3.1+)

By default the proxy waits on the upstream as long as the client does, so a slow
upstream always ends with the **client** giving up first — which the proxy can only
record as `502 ... context canceled`, with nothing to say who hung up. Two optional
per-instance fields let the proxy set its own budget instead:

| Field | Bounds | Use when |
|---|---|---|
| `timeout` | whole request, including body streaming | you want a hard ceiling per request |
| `first_byte_timeout` | only the wait for response headers | upstream is queueing; generation itself is fine |

```json
{ "endpoint": "https://token.sensenova.cn", "Authkey": "sk-...", "listen": "127.0.0.1:8001",
  "timeout": 120, "first_byte_timeout": 30 }
```

**Both default to 90s when omitted.** Omitting them is *not* "no limit" — that
was v1.3.1–v1.3.3 behaviour and it is exactly what produced the `client_gone ...
took=2m3s` log lines: the proxy sat waiting until the client gave up first, then
could only record `502`. Defaults must be safe values. Use a **negative** value
(`-1`) to genuinely disable one of them; any positive value is clamped to 90s.

**Hard cap: 90s.** Cloudflare drops a connection that has not produced response
headers within 100s and returns a bare `524` — no error detail reaches the client
and nothing shows up in the proxy log. The proxy therefore never waits longer
than 90s for the upstream, whatever you configure, so its own `504` + JSON error
arrives first. A configured value above the cap is clamped, and the startup log
says so. If your workload genuinely needs longer, fix the client timeout or use a
faster upstream — do not raise this ceiling.

The failure mode is now explicit instead of ambiguous:

| Situation | Status | `error.code` | Log line |
|---|---|---|---|
| Upstream exceeded `timeout` / `first_byte_timeout` | `504` | `upstream_timeout` | `upstream deadline exceeded (budget=...)` |
| Client hung up while proxy was still waiting | `502` | `client_disconnected` | `client hung up before upstream answered (... not a proxy fault)` |
| Proxy could not reach upstream at all | `502` | `bad_gateway` | `Upstream failure: ...` |

That `client_disconnected` row is the one that used to be misread: a cluster of
`502`s all landing on the same duration (e.g. `2m03s`) is the client's own timeout
firing, not the proxy failing. Look at the client timeout — or set
`first_byte_timeout` below it so the proxy answers first with an actionable `504`.

### Session identity (v1.3.2+)

The opencode gateway meters its free tier **per session**. v1.3.1 minted a fresh
`ses_*` id on every request, which scattered one conversation across many
sessions and split the quota. Now one conversation keeps one id:

| Source of the id | Behaviour |
|---|---|
| Client sends `X-Session-Id` / `X-Opencode-Session` / `X-Session-Affinity` / `X-Conversation-Id` | used as-is (normalised to valid `ses_*` if it is not already) |
| The header named by `session_header` | used as-is (same normalisation) |
| Nothing sent, `session_fallback: client` (default) | derived from the client IP — stable for that client |
| Nothing sent, `session_fallback: instance` | one fixed id for the whole instance |
| Nothing sent, `session_fallback: request` | new id per request (old behaviour, splits quota) |

Unsafe client values (spaces, newlines, non-ASCII) are hashed rather than
forwarded, so a malformed header can never produce an invalid upstream request.

### Log format (v1.3.2+)

Every request carries a short `rid` that is repeated on all of its log lines,
including the asynchronous SSE ones, so a single request can be reconstructed
with `grep rid=3f9a2b1c`. The `listen` field is on every line, which matters when
several instances run at once — previously the opencode lines carried no port at
all, so you could not tell which lane produced them.

```
rid=3f9a2b1c listen=127.0.0.1:3000 model=big-pickle provider=opencode client=127.0.0.1 event=first_sse: waited=3.7s
rid=3f9a2b1c listen=127.0.0.1:3000 model=big-pickle provider=opencode client=127.0.0.1 event=stream_done: took=7.4s saw_tool=false injected=false
rid=3f9a2b1c listen=127.0.0.1:3000 model=big-pickle provider=opencode client=127.0.0.1 event=done: POST /v1/chat/completions -> 200 took=7.4s
```

| `event` | Meaning |
|---|---|
| `first_sse` | upstream sent its first SSE event (value is the wait) |
| `stream_done` | stream finished; `saw_tool`/`injected` explain keepalive handling |
| `keepalive_injected` | a synthetic tool call was appended to keep the stream alive |
| `done` | final line: status code + total duration |
| `upstream_timeout` | the proxy's own budget expired → `504` |
| `client_gone` | client hung up first — explicitly *not* a proxy fault → `502` |
| `gate_error` | upstream refused the free-tier request; `class` is in the `X-Opencode-Gate` response header |
| `body_rewrite_failed` / `models_parse_failed` | request or `/v1/models` body was passed through untouched |
| `stream_truncated` | the stream was cut **after** the 200 header went out, so the status code could not be changed. `cause` is `proxy_timeout_budget_expired` (retry) or `client_gone` (not your fault). This line is the only explanation the client ever gets. |

Both `X-Proxy-RID` and `X-Proxy-Session` come back on the response (success and
error alike), so a client can report the rid it saw.

### Multi-source aggregate (`sources`)

Instead of pointing a client at several opencode instances and switching manually,
configure one aggregate listen that fans a request across multiple upstream sources
in order (wintools `multi-proxy` semantics). When the first source hits the free-tier
daily limit, it is put in cooldown until the next UTC midnight and later requests
automatically land on the next source. If every source is exceeded, the proxy returns
429 `FreeUsageLimitError`.

```json
{
  "listen": "127.0.0.1:8020",
  "sources": [
    { "name": "vps-v4",    "endpoint": "https://vps.moonchan.xyz",        "net": "v4" },
    { "name": "vps-v6",    "endpoint": "https://vps.moonchan.xyz",        "net": "v6" },
    { "name": "cloudcone", "endpoint": "https://cloudcone.moonchan.xyz",  "net": "v4" },
    { "name": "bwh",       "endpoint": "https://bwh.moonchan.xyz",        "net": "v4" }
  ]
}
```

Source fields: `name` (display, used in `/status`), `endpoint` (upstream opencode
base URL; when pointing at another api-pack/sensenova-proxy instance, that instance
handles its own fingerprint/gate), `net` (`v4` / `v6` / `auto`, fixes the outbound
address family per source via `OPENCODE_NET_V4` / `OPENCODE_NET_V6`), `engress`
(optional, reuses the same egress binding as single-instance mode).

Endpoints on the aggregate listen: `POST /v1/chat/completions` (streaming and
non-streaming, with first-chunk pre-read failover), `GET /v1/models` (first healthy
source), `GET /status` (per-source cooldown/error/exceeded stats). Field names are
case-insensitive (e.g. `sources` / `Sources`).

*Note: Field names are case-insensitive and support aliases (e.g. `Authkey` / `authkey`, `engress` / `egress`, `provider` / `mode` / `upstream_type`).*

---

## 🚀 Quick Start

### 1. Build from Source

```bash
cd /home/gekkasayu/sensenova-proxy
./build.sh
```

Or with `go build`:
```bash
go build -ldflags="-s -w" -o sensenova-proxy .
```

### 2. Run the Proxy

```bash
# Using default config.json
./sensenova-proxy

# Or specifying a custom configuration path
./sensenova-proxy -c /path/to/my_config.json
```

---

## 🧪 Testing & Verification

### Health Check
Each proxy instance provides a built-in health endpoint:
```bash
curl http://127.0.0.1:8001/_health
```
Output:
```json
{
  "endpoint": "https://token.sensenova.cn",
  "engress": "192.168.1.101",
  "has_authkey": true,
  "listen": "127.0.0.1:8001",
  "provider": "sensenova",
  "status": "ok",
  "time": "2026-09-20T06:04:52Z"
}
```

### Chat Completions (SenseNova)
```bash
curl -X POST http://127.0.0.1:8001/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "SenseChat-5",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

### Chat Completions (OpenCode free tier)

Point any OpenAI-compatible client at the opencode instance. The proxy handles
the fingerprint and the body gate transparently — `stream` and `tools` do not
need to be set by the client.

```bash
curl -N -X POST http://127.0.0.1:8011/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "nemotron-3.5-lightning-free",
    "messages": [{"role": "user", "content": "Say hi"}]
  }'
```

### Image Generation (SenseNova)
```bash
curl -X POST http://127.0.0.1:8001/v1/images/generations \
  -H "Content-Type: application/json" \
  -d '{
    "model": "sensenova-u1.5-lite",
    "prompt": "cyberpunk city skyline",
    "size": "1024x1024"
  }'
```

---

## 🛠 Systemd Service Setup (Optional)

Create `/etc/systemd/system/sensenova-proxy.service`:

```ini
[Unit]
Description=SenseNova Reverse Proxy Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/home/gekkasayu/sensenova-proxy
ExecStart=/home/gekkasayu/sensenova-proxy/sensenova-proxy -c /home/gekkasayu/sensenova-proxy/config.json
Restart=always
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

Enable and start:
```bash
systemctl daemon-reload
systemctl enable --now sensenova-proxy
```