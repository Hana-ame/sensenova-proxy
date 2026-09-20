# SenseNova Multi-Egress Reverse Proxy

A high-performance, standalone reverse proxy referencing [`gh/Hana-ame/api-pack`](https://github.com/Hana-ame/api-pack) tailored for SenseNova (`https://token.sensenova.cn`).

It provides transparent request passthrough, multi-instance concurrency, automated authentication injection, CORS repair, and dedicated outbound network egress per listening port.

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

---

## 📋 Configuration (`config.json`)

The configuration file is a JSON array. Each object contains the **four required properties**:

```json
[
  {
    "endpoint": "https://token.sensenova.cn",
    "Authkey": "sk-your-sensenova-api-key-1",
    "engress": "192.168.1.101",
    "listen": "127.0.0.1:8001"
  },
  {
    "endpoint": "https://token.sensenova.cn",
    "Authkey": "sk-your-sensenova-api-key-2",
    "engress": "eth0",
    "listen": "127.0.0.1:8002"
  },
  {
    "endpoint": "https://token.sensenova.cn",
    "Authkey": "sk-your-sensenova-api-key-3",
    "engress": "socks5://127.0.0.1:1080",
    "listen": "127.0.0.1:8003"
  },
  {
    "endpoint": "https://token.sensenova.cn",
    "Authkey": "sk-your-sensenova-api-key-4",
    "engress": "",
    "listen": "127.0.0.1:8004"
  }
]
```

### Property Description

| Field | Type | Description | Example |
|---|---|---|---|
| `endpoint` | string | Target upstream URL | `"https://token.sensenova.cn"` |
| `Authkey` | string | SenseNova API Key (injected as `Authorization: Bearer <Authkey>`) | `"sk-abc123..."` |
| `engress` | string | Outbound egress IP, interface name, proxy URL, or empty | `"192.168.1.10"`, `"eth0"`, `"socks5://127.0.0.1:1080"` |
| `listen` | string | Local listening IP and port | `"127.0.0.1:8001"`, `":8001"` |

*Note: Field names are case-insensitive and support aliases (e.g. `Authkey` / `authkey`, `engress` / `egress`).*

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
  "status": "ok",
  "time": "2026-09-20T06:04:52Z"
}
```

### Chat Completions (OpenAI Compatible)
```bash
curl -X POST http://127.0.0.1:8001/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "SenseChat-5",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
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
