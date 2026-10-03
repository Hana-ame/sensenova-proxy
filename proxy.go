package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// createTransport constructs an http.Transport configured with the specified egress.
// Supports:
// 1. IP address (IPv4/IPv6): binds LocalAddr and dials with tcp4/tcp6.
// 2. Network interface name (e.g. "eth0"): binds socket via SO_BINDTODEVICE on Linux and binds interface IP.
// 3. Upstream proxy URL (http://, https://, socks5://): routes outbound via proxy.
// 4. Empty / "default": standard default system route.
func createTransport(egress string) (*http.Transport, error) {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true, // Do not auto-buffer/compress to ensure real-time streaming
	}

	egress = strings.TrimSpace(egress)
	if egress == "" || strings.EqualFold(egress, "default") || strings.EqualFold(egress, "none") {
		return tr, nil
	}

	// 1. Upstream proxy URL
	if strings.HasPrefix(egress, "http://") || strings.HasPrefix(egress, "https://") ||
		strings.HasPrefix(egress, "socks5://") || strings.HasPrefix(egress, "socks5h://") {
		proxyURL, err := url.Parse(egress)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL '%s': %w", egress, err)
		}
		tr.Proxy = http.ProxyURL(proxyURL)
		return tr, nil
	}

	// 2. IP address (IPv4 or IPv6)
	if ip := net.ParseIP(egress); ip != nil {
		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			LocalAddr: &net.TCPAddr{IP: ip},
		}
		networkType := "tcp"
		if ip.To4() != nil {
			networkType = "tcp4"
		} else {
			networkType = "tcp6"
		}

		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if network == "tcp" {
				network = networkType
			}
			return dialer.DialContext(ctx, network, addr)
		}
		return tr, nil
	}

	// 3. Network interface name (e.g. eth0, ens3, tun0)
	iface, err := net.InterfaceByName(egress)
	if err == nil {
		var ifaceIP net.IP
		addrs, aErr := iface.Addrs()
		if aErr == nil {
			for _, a := range addrs {
				var curIP net.IP
				switch v := a.(type) {
				case *net.IPNet:
					curIP = v.IP
				case *net.IPAddr:
					curIP = v.IP
				}
				if curIP != nil && !curIP.IsLoopback() {
					ifaceIP = curIP
					break
				}
			}
		}

		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			Control: func(network, address string, c syscall.RawConn) error {
				var operr error
				fn := func(fd uintptr) {
					operr = bindSocketToDevice(fd, egress)
				}
				if cErr := c.Control(fn); cErr != nil {
					return cErr
				}
				return operr
			},
		}

		if ifaceIP != nil {
			dialer.LocalAddr = &net.TCPAddr{IP: ifaceIP}
		}

		tr.DialContext = dialer.DialContext
		return tr, nil
	}

	return nil, fmt.Errorf("unrecognized egress '%s': must be a valid IP, interface name, or proxy URL", egress)
}

// setCORSHeaders applies full CORS headers to the response writer.
// Fixes upstream SenseNova CORS preflight missing issue (DOCS/sensenova-proxy-fix.md).
func setCORSHeaders(rw http.ResponseWriter, req *http.Request) {
	origin := req.Header.Get("Origin")
	if origin != "" {
		rw.Header().Set("Access-Control-Allow-Origin", origin)
	} else {
		rw.Header().Set("Access-Control-Allow-Origin", "*")
	}
	rw.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PATCH, HEAD")
	reqHeaders := req.Header.Get("Access-Control-Request-Headers")
	if reqHeaders != "" {
		rw.Header().Set("Access-Control-Allow-Headers", reqHeaders)
	} else {
		rw.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, X-Requested-With, Origin, User-Agent, X-Api-Key, Sentry-Trace, Baggage")
	}
	rw.Header().Set("Access-Control-Allow-Credentials", "true")
	rw.Header().Set("Access-Control-Max-Age", "86400")
	rw.Header().Set("Access-Control-Expose-Headers", "*")
	rw.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
}

// timeoutBudget 返回这次超时实际生效的预算，用于日志与错误提示。
// 总超时优先；只配了首字节超时时报告首字节预算，避免日志里打出无意义的 0s。
func timeoutBudget(p ProxyItem) time.Duration {
	if to := p.TimeoutDuration(); to > 0 {
		return to
	}
	return p.FirstByteDuration()
}

// isTimeoutErr 统一判定"是不是超时"。三种来源都要认：
//  1. 自己用 context.WithTimeout 包的总超时 -> context.DeadlineExceeded
//  2. Transport.ResponseHeaderTimeout（首字节超时）-> net/http 内部错误，
//     既不是 DeadlineExceeded 也常被包成 *url.Error，只能看 Timeout()/文本
//  3. 底层拨号/TLS 超时 -> net.Error.Timeout()
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "timeout awaiting response headers")
}

// isStreamingResponse 判断响应是否是流式的（SSE 或 chunked）。这类响应的
// 特点是响应头一旦发出就定死了状态码，之后出问题没法改，只能靠日志解释。
func isStreamingResponse(resp *http.Response) bool {
	if resp == nil || resp.Body == nil {
		return false
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		return true
	}
	// 没有 Content-Length 的 2xx 响应就是边收边发。
	if resp.ContentLength < 0 && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true
	}
	return false
}

// wrapStreamWithContextWatcher 给响应体套一层，把"流为什么断了"记到 meta 上。
// 响应头此时已经发出，状态码改不了，这是唯一能解释"客户端看到 200 然后流突然
// 断了"的线索。透传所有读写行为，不改变字节与时序。
//
// 判定依据是**上游请求的 ctx 状态**，不是 Read 返回的错误值：实测预算到期时
// Read 拿到的是 context.Canceled（httputil 把它当"客户端走了"直接静默
// return，连它自己的 ErrorLog 都不打），而不是 DeadlineExceeded。所以光看
// 错误类型会漏判，必须查 ctx.Err()。
func wrapStreamWithContextWatcher(resp *http.Response, meta *reqMeta, reqCtx context.Context) {
	if meta == nil || resp == nil || resp.Body == nil {
		return
	}
	resp.Body = &ctxWatchBody{ReadCloser: resp.Body, meta: meta, reqCtx: reqCtx}
}

// ctxWatchBody 是透明的 ReadCloser，只在读出错时留痕。
type ctxWatchBody struct {
	io.ReadCloser
	meta   *reqMeta
	reqCtx context.Context
}

func (b *ctxWatchBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == nil || errors.Is(err, io.EOF) {
		return n, err
	}
	// 错误类型不可靠（Canceled / DeadlineExceeded 都可能），用 ctx 判原因。
	if cerr := b.reqCtx.Err(); cerr != nil {
		b.meta.setTruncReason(cerr)
	} else {
		b.meta.setTruncReason(err)
	}
	return n, err
}

// isCloudflareHeader checks if an HTTP header is injected by Cloudflare for tracking, CDN telemetry, or identification.
func isCloudflareHeader(name, val string) bool {
	lowerName := strings.ToLower(name)
	lowerVal := strings.ToLower(val)

	// 1. Any header starting with cf- or x-cf- (e.g. cf-ray, cf-cache-status, cf-request-id, cf-visitor, cf-connecting-ip)
	if strings.HasPrefix(lowerName, "cf-") || strings.HasPrefix(lowerName, "x-cf-") {
		return true
	}

	// 2. Cloudflare telemetry and error reporting headers
	if lowerName == "nel" || lowerName == "report-to" {
		return true
	}

	// 3. CDN Loop and upstream tracking
	if lowerName == "cdn-loop" {
		return true
	}

	// 4. Server header advertising Cloudflare
	if lowerName == "server" && strings.Contains(lowerVal, "cloudflare") {
		return true
	}

	// 5. Deprecated Certificate Transparency header
	if lowerName == "expect-ct" {
		return true
	}

	// 6. Upstream Alt-Svc advertising Cloudflare HTTP/3 endpoints
	if lowerName == "alt-svc" {
		return true
	}

	return false
}

// responseRecorder captures the status code written to ResponseWriter for logging,
// and ensures immediate unbuffered flushing on every single write.
//
// 它还负责"响应头已发出、之后 body 被掐断"这种流式场景的可观测性：客户端
// 看到的是 200，然后流毫无预兆地结束，状态码改不了（HTTP 头早就发出去了）。
// 没有日志就只能猜。这里记下首次写 body 的错误与是否正常收尾，handler 收尾
// 时统一打一行 stream_truncated。
type responseRecorder struct {
	http.ResponseWriter
	statusCode int

	started  bool     // 是否已经写过 body（即响应头已发出）
	writeErr error    // 首次非 nil 的写错误
	finished bool     // 正常写完（无错误）
	meta     *reqMeta // 用于结构化日志，可为 nil
}

func (rec *responseRecorder) WriteHeader(code int) {
	rec.statusCode = code
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *responseRecorder) Write(b []byte) (int, error) {
	n, err := rec.ResponseWriter.Write(b)
	if n > 0 || len(b) == 0 {
		rec.started = true
	}
	if err != nil && rec.writeErr == nil {
		rec.writeErr = err
	}
	if err == nil && n == len(b) {
		rec.finished = true
	}
	if flusher, ok := rec.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

func (rec *responseRecorder) Flush() {
	if flusher, ok := rec.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (rec *responseRecorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
}

// BuildProxyHandler constructs the HTTP handler for a specific ProxyItem.
func BuildProxyHandler(item ProxyItem, tr *http.Transport) (http.Handler, error) {
	// 首字节超时只约束"等上游响应头"这一段。流式响应一旦拿到响应头就不再受
	// 它约束，所以它治的是上游排队/连不上（日志里那种"生成 1.4s 却总共
	// 30.9s"），不会截断正常进行中的长流。
	if fb := item.FirstByteDuration(); fb > 0 {
		tr.ResponseHeaderTimeout = fb
	}

	proxy := &httputil.ReverseProxy{
		Transport:     tr,
		FlushInterval: -1, // Immediate flushing for SSE / streaming responses
	}

	// Director rewrites target URL, headers, and injects Authkey
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = item.TargetURL.Scheme
		req.URL.Host = item.TargetURL.Host
		req.Host = item.TargetURL.Host

		// Normalize paths cleanly without double /v1
		basePath := strings.TrimRight(item.TargetURL.Path, "/")
		reqPath := req.URL.Path
		if !strings.HasPrefix(reqPath, "/") {
			reqPath = "/" + reqPath
		}
		if strings.HasSuffix(basePath, "/v1") && strings.HasPrefix(reqPath, "/v1/") {
			basePath = strings.TrimSuffix(basePath, "/v1")
		}
		req.URL.Path = basePath + reqPath

		// Exclude auth key from the original request
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
		req.Header.Del("api-key")

		// Inject or override Authkey if configured
		if item.AuthKey != "" {
			authHeader := item.AuthKey
			if !strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
				authHeader = "Bearer " + authHeader
			}
			req.Header.Set("Authorization", authHeader)
		}

		// opencode lane: stamp the gateway fingerprint and enforce the body
		// gate (stream:true + the bash/glob/grep/read quartet). Everything the
		// upstream rejects would otherwise surface as a 403 FreeTierError with
		// no hint as to why.
		if item.IsOpencode() {
			meta := metaFrom(req.Context())
			sess := resolveSessionMarker(item, req)
			meta.sess = sess
			applyFingerprintHeaders(req, sess)
			newBody, model, _, err := rewriteOpencodeBody(req)
			if err != nil {
				meta.logf("body_rewrite_failed", "err=%v (forwarding as-is)", err)
			} else {
				meta.model = model
				req.Body = io.NopCloser(bytes.NewReader(newBody))
				req.ContentLength = int64(len(newBody))
				req.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
				req.GetBody = func() (io.ReadCloser, error) {
					return io.NopCloser(bytes.NewReader(newBody)), nil
				}
				// Some free models are only served by /responses or /messages.
				if np := reRouteModelEndpoint(req, model); np != "" {
					req.URL.Path = strings.TrimSuffix(req.URL.Path, "/chat/completions") + np
				}
			}
		}

		// Prevent upstream gzip/brotli buffering for streaming responses:
		// Upstream gzip compressors buffer small SSE chunks in 4-32KB blocks, delaying token delivery.
		// Forcing identity encoding ensures unbuffered real-time SSE token delivery.
		req.Header.Del("Accept-Encoding")
		req.Header.Set("Accept-Encoding", "identity")

		// Strip any incoming Cloudflare tracking headers
		for k := range req.Header {
			if isCloudflareHeader(k, req.Header.Get(k)) {
				req.Header.Del(k)
			}
		}

		// Append client IP to X-Forwarded-For
		if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
				req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
			} else {
				req.Header.Set("X-Forwarded-For", clientIP)
			}
			if req.Header.Get("X-Real-IP") == "" {
				req.Header.Set("X-Real-IP", clientIP)
			}
		}
	}

	// ModifyResponse strips all Cloudflare tracking headers, telemetry, and cookies,
	// ensures anti-buffering headers, and injects CORS headers while passing all other upstream headers
	proxy.ModifyResponse = func(resp *http.Response) error {
		// opencode lane: harden SSE streams and classify gate errors.
		if item.IsOpencode() {
			switch {
			case resp.StatusCode == http.StatusOK && strings.HasSuffix(resp.Request.URL.Path, "/models"):
				// Faked /v1/models: merge (or replace with) custom model ids so
				// clients see them as available even when the upstream does not
				// serve them.
				if len(item.CustomModels) > 0 {
					if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "application/json") {
						if resp.Body != nil {
							body, err := io.ReadAll(resp.Body)
							resp.Body.Close()
							if err == nil {
								nb := rewriteModelsBody(body, item.CustomModels, item.ModelsMode, metaFrom(resp.Request.Context()))
								resp.Body = io.NopCloser(bytes.NewReader(nb))
								resp.ContentLength = int64(len(nb))
								// The stale Content-Length no longer matches the
								// rewritten body; let the transport re-chunk.
								resp.Header.Del("Content-Length")
							}
						}
					}
				}
			case resp.StatusCode == http.StatusOK:
				if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
					if resp.Body != nil {
						// Recover the model name from the (rewritten) request body
						// so a synthetic keepalive chunk carries the right tag.
						model := ""
						if gb := resp.Request.GetBody; gb != nil {
							if bc, err := gb(); err == nil {
								body, _ := io.ReadAll(bc)
								var p map[string]any
								if json.Unmarshal(body, &p) == nil {
									model, _ = p["model"].(string)
								}
							}
						}
						resp.Body = newSSEPumpBody(resp.Body, model, metaFrom(resp.Request.Context()))
					}
				}
			case resp.StatusCode >= 400:
				// Classify the gate error and surface it to both the logs and the
				// client, so a blocked egress IP or an exhausted session quota is
				// diagnosable instead of showing up as an opaque 4xx.
				if class, hint := classifyGateError(resp); class != "" {
					resp.Header.Set("X-Opencode-Gate", class)
					metaFrom(resp.Request.Context()).logf("gate_error",
						"upstream=%d class=%s hint=%q", resp.StatusCode, class, hint)
				}
			}
		}

		// Strip all Cloudflare-injected tracking headers from upstream response
		for k, v := range resp.Header {
			val := ""
			if len(v) > 0 {
				val = v[0]
			}
			if isCloudflareHeader(k, val) {
				resp.Header.Del(k)
			}
		}

		// Filter out Cloudflare tracking cookies (__cf_bm, cf_clearance, __cfduid, etc.)
		if cookies := resp.Header.Values("Set-Cookie"); len(cookies) > 0 {
			resp.Header.Del("Set-Cookie")
			for _, cookie := range cookies {
				cookieLower := strings.ToLower(strings.TrimSpace(cookie))
				if strings.HasPrefix(cookieLower, "__cf") || strings.HasPrefix(cookieLower, "cf_") {
					continue // Strip Cloudflare tracking cookie
				}
				resp.Header.Add("Set-Cookie", cookie)
			}
		}

		// 流式响应（任意 content-type，不只是 opencode）：挂一个监视上游
		// context 的 goroutine。httputil.ReverseProxy 在 body copy 读到非 EOF
		// 错误时只往它自己的 ErrorLog 打一行就返回，错误**不经过**
		// ResponseWriter.Write，所以包装响应体抓不到（实测 90s 预算到期时
		// Write 一次错误都没收到）。这里直接盯 ctx：预算到期或客户端断开都会
		// 让这里拿到确切原因，handler 收尾时就能打出一行可解释的日志——否则
		// 客户端只看到 200 然后流突然断掉，无从判断发生了什么。
		if isStreamingResponse(resp) {
			wrapStreamWithContextWatcher(resp, metaFrom(resp.Request.Context()), resp.Request.Context())
		}

		// 回显本次请求的 rid / session，客户端出问题时报一个 rid 就能在日志里
		// 捞出全部相关行；session 也可见，便于确认同一会话确实命中了同一个
		// 上游 session。
		meta := metaFrom(resp.Request.Context())
		resp.Header.Set("X-Proxy-RID", meta.rid)
		if meta.sess != "" {
			resp.Header.Set("X-Proxy-Session", meta.sess)
		}

		// Set anti-buffering hints for downstream clients / proxies
		resp.Header.Set("X-Accel-Buffering", "no")

		contentType := resp.Header.Get("Content-Type")
		if strings.HasPrefix(contentType, "text/event-stream") {
			resp.Header.Set("Cache-Control", "no-cache, no-transform")
			resp.Header.Set("Connection", "keep-alive")
		}

		// Inject CORS headers on response
		origin := resp.Request.Header.Get("Origin")
		if origin != "" {
			resp.Header.Set("Access-Control-Allow-Origin", origin)
		} else if resp.Header.Get("Access-Control-Allow-Origin") == "" {
			resp.Header.Set("Access-Control-Allow-Origin", "*")
		}
		resp.Header.Set("Access-Control-Allow-Credentials", "true")
		resp.Header.Set("Access-Control-Expose-Headers", "*")
		resp.Header.Set("Cross-Origin-Resource-Policy", "cross-origin")

		return nil
	}

	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		// 区分"上游超时"与"客户端自己断开"。两者以前都记成
		// `Upstream failure: context canceled` + 502，看着像代理故障，实际常常
		// 是客户端等不及先撤了、代理被动收尾。分开记，排错才不跑偏。
		timedOut := isTimeoutErr(err)
		clientGone := errors.Is(err, context.Canceled)
		meta := metaFrom(req.Context())

		switch {
		case timedOut:
			meta.logf("upstream_timeout", "budget=%s err=%v (proxy gave up first; raise the budget or lower first_byte_timeout if this is too aggressive)",
				timeoutBudget(item), err)
		case clientGone:
			meta.logf("client_gone", "err=%v (client hung up while proxy was still waiting upstream - not a proxy fault)", err)
		default:
			meta.logf("upstream_failure", "err=%v", err)
		}

		status, code := http.StatusBadGateway, "bad_gateway"
		hint := fmt.Sprintf("proxy failed to reach upstream '%s': %v", item.Endpoint, err)
		switch {
		case timedOut:
			status, code = http.StatusGatewayTimeout, "upstream_timeout"
			hint = fmt.Sprintf("upstream '%s' did not finish within %v (timeout budget)", item.Endpoint, timeoutBudget(item))
		case clientGone:
			code = "client_disconnected"
			hint = fmt.Sprintf("client disconnected before upstream '%s' replied; raise the client timeout or lower first_byte_timeout", item.Endpoint)
		}

		setCORSHeaders(rw, req)
		m := metaFrom(req.Context())
		rw.Header().Set("X-Proxy-RID", m.rid)
		if m.sess != "" {
			rw.Header().Set("X-Proxy-Session", m.sess)
		}
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.WriteHeader(status)
		_ = json.NewEncoder(rw).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    code,
				"message": hint,
				"listen":  item.Listen,
				"engress": item.Engress,
			},
		})
	}

	// Wrap in top-level handler for logging, healthcheck, and OPTIONS preflight
	handler := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		start := time.Now()

		// 每个请求一个 rid，贯穿本次请求产生的所有日志行（含 SSE pump 那些
		// 异步打的）。多实例并行时，listen + rid 就能唯一定位一次请求。
		clientIP, _, _ := net.SplitHostPort(req.RemoteAddr)
		req = withReqMeta(req, &reqMeta{
			rid:      newRID(),
			listen:   item.Listen,
			engress:  item.Engress,
			provider: item.Provider,
			clientIP: clientIP,
		})
		meta := metaFrom(req.Context())

		// 1. Diagnostic / health check endpoint
		if req.URL.Path == "/_health" || req.URL.Path == "/healthz" {
			setCORSHeaders(rw, req)
			rw.Header().Set("Content-Type", "application/json; charset=utf-8")
			rw.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(rw).Encode(map[string]interface{}{
				"status":      "ok",
				"listen":      item.Listen,
				"endpoint":    item.Endpoint,
				"provider":    item.Provider,
				"engress":     item.Engress,
				"has_authkey": item.AuthKey != "",
				"time":        time.Now().Format(time.RFC3339),
			})
			return
		}

		// 2. CORS OPTIONS preflight: return 204 No Content immediately
		if req.Method == http.MethodOptions {
			setCORSHeaders(rw, req)
			rw.WriteHeader(http.StatusNoContent)
			return
		}

		// 上游总超时：到点由代理主动放弃并返回 504，而不是陪着客户端一起干等、
		// 等它自己超时断开（那样只会在日志里留下一串 2m03s 的 502，看不出谁先撤）。
		if to := item.TimeoutDuration(); to > 0 {
			ctx, cancel := context.WithTimeout(req.Context(), to)
			defer cancel()
			req = req.WithContext(ctx)
		}

		// 3. Proxy passthrough with full-duplex streaming and zero-buffering
		rc := http.NewResponseController(rw)
		_ = rc.EnableFullDuplex()

		// 上游 ctx 一到期（预算耗尽 / 客户端断开）就立刻记一笔。不挂在收尾
		// 逻辑上：实测流被掐断时 ServeHTTP 之后的代码可能根本不会执行到
		// （httputil 走 panic(ErrAbortHandler) 直接终结连接），所以日志必须
		// 在事件发生的那一刻打出来，不能等 defer。
		watchDone := make(chan struct{})
		defer close(watchDone)
		go func() {
			select {
			case <-req.Context().Done():
				meta.setTruncReason(req.Context().Err())
				meta.logTruncated(req.Method, req.URL.Path, round1(time.Since(start)))
			case <-watchDone:
			}
		}()

		rec := &responseRecorder{
			ResponseWriter: rw,
			statusCode:     http.StatusOK,
			meta:           meta,
		}

		proxy.ServeHTTP(rec, req)

		duration := time.Since(start)
		meta.logf("done", "%s %s -> %d took=%s", req.Method, req.URL.Path, rec.statusCode, round1(duration))

		// 流式响应一旦开了头，状态码就定死了——客户端看到 200 然后流突然断掉，
		// 不会有任何错误信息。这一行是唯一的解释，必须打。SSE pump 自己也会
		// 打一条 stream_truncated（它能区分得更细），这里只做兜底：非 2xx 或
		// 没写过 body 的路径不算截断，交给 ErrorHandler 的日志。
		// ctx 被取消时上面的 goroutine 已经打过 stream_truncated；这里只在
		// ctx 活着但 body 写失败时兜底（例如上游提前断）。
		if _, ok := meta.truncCause(); !ok && rec.started && rec.writeErr != nil {
			meta.logf("stream_truncated",
				"status=%d took=%s cause=write_error err=%v note=headers_already_sent",
				rec.statusCode, round1(duration), rec.writeErr)
		}
	})

	return handler, nil
}
