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
| `sources` | object[] | **Multi-source aggregate ("一拖多")** — when set, this instance becomes an aggregate endpoint: clients configure a single baseURL and the proxy tries sources in order, auto-failing over when one is exhausted (429 / `FreeUsageLimitError`) with cooldown until the next UTC midnight. Each source fixes its own egress: no client-side param needed. See below. | see below |

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