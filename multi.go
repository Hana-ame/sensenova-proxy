// multi.go — opencode lane 的多源聚合（"一拖多"）模式。
//
// 照 wintools cmd/multi-proxy 的 fallback 语义：客户端只配一个 baseURL，
// 代理按序尝试多个配置好的上游源（每个源固定 endpoint + v4/v6 出口），
// 遇 exceed（429 / FreeUsageLimitError / daily free usage limit / Banned）
// 把该源冷却到当日 UTC 午夜，后续请求自动落到下一个源；全部不可用回 429。
// 线的选择在 config 层完成（每源固定 net），客户端不感知、不传 param。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	multiConnectTimeout  = 15 * time.Second
	multiHeaderTimeout   = 30 * time.Second
	multiIdleConnTimeout = 90 * time.Second
	multiCooldownShort   = 60 * time.Second
	multiPreReadTimeout  = 10 * time.Second
	multiMaxBody         = 64 << 20
)

// openSource 一个 opencode 上游源：endpoint + 固定出口地址族。
// net: "v4" | "v6" | "auto"（空 = auto）
type openSource struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Net      string `json:"net"`
	Engress  string `json:"engress"`
}

// multiUpstream 运行时上游状态（failover 统计 + 冷却）。
type multiUpstream struct {
	src openSource
	tr  *http.Transport
	// 目标 URL（解析好的）
	target *url.URL
	base   string

	mu            sync.Mutex
	cooldownUntil time.Time
	lastErr       string
	reqs          int64
	exceeded      bool
	// 最近一次失败的上游响应（状态/错误体），供全源失败时聚合出
	// 最贴近上游的错误响应（wintools multi-proxy 同款）。
	lastStatus  int
	lastErrBody any
}

func nextUTCMidnight() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}

func isExceed(status int, body []byte) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	for _, needle := range []string{"FreeUsageLimitError", "daily free usage limit", "exceeded", "Banned"} {
		if bytes.Contains(body, []byte(needle)) {
			return true
		}
	}
	return false
}

func (u *multiUpstream) inCooldown(now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cooldownUntil.After(now)
}

func (u *multiUpstream) setCooldown(d time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.cooldownUntil = time.Now().Add(d)
}

func (u *multiUpstream) setErr(e string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.lastErr = e
}

func (u *multiUpstream) incrReqs() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.reqs++
}

// newMultiUpstream 构建一个源：解析 endpoint、按 net 建 transport（绑定对应
// 出口地址族，与 opencode 单源 lane 同套逻辑）。
func newMultiUpstream(src openSource) (*multiUpstream, error) {
	endpoint := strings.TrimSpace(src.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("source %q: endpoint required", src.Name)
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	target, err := url.Parse(endpoint)
	if err != nil || target.Host == "" {
		return nil, fmt.Errorf("source %q: invalid endpoint %q: %v", src.Name, endpoint, err)
	}

	tr, err := createTransport(src.Engress)
	if err != nil {
		return nil, fmt.Errorf("source %q: egress: %v", src.Name, err)
	}
	tr.DisableCompression = true
	tr.ForceAttemptHTTP2 = true
	tr.IdleConnTimeout = multiIdleConnTimeout

	// net 出口绑定：按地址族固定源 IP（与单源 lane 的 OPENCODE_NET_V4/V6 一致，
	// 这里是 per-source 解析，互不干扰）。
	ip := resolveNetIP(src.Net)
	if ip != nil {
		network := "tcp"
		if ip.To4() != nil {
			network = "tcp4"
		} else {
			network = "tcp6"
		}
		dialer := &net.Dialer{
			Timeout:   multiConnectTimeout,
			KeepAlive: 30 * time.Second,
			LocalAddr: &net.TCPAddr{IP: ip},
		}
		tr.DialContext = func(ctx context.Context, netw, addr string) (net.Conn, error) {
			if netw == "tcp" {
				netw = network
			}
			return dialer.DialContext(ctx, netw, addr)
		}
	}

	return &multiUpstream{src: src, tr: tr, target: target, base: strings.TrimRight(endpoint, "/")}, nil
}

// resolveNetIP 从 env 取某地址族的源 IP（OPENCODE_NET_V4 / OPENCODE_NET_V6）。
// net 为空/auto 时返回 nil（走默认路由 / 系统选线）。
func resolveNetIP(netMode string) net.IP {
	switch strings.ToLower(strings.TrimSpace(netMode)) {
	case "v4":
		if s := strings.TrimSpace(envOr("OPENCODE_NET_V4", "")); s != "" {
			if ip := net.ParseIP(s); ip != nil {
				return ip
			}
		}
	case "v6":
		if s := strings.TrimSpace(envOr("OPENCODE_NET_V6", "")); s != "" {
			if ip := net.ParseIP(s); ip != nil {
				return ip
			}
		}
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// 多源聚合 handler
// ---------------------------------------------------------------------------

// multiHandler 把 /v1/chat/completions 请求按序 failover 到多个 opencode 源。
type multiHandler struct {
	sources []*multiUpstream
}

func (m *multiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		setCORSHeaders(w, r)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch {
	case r.URL.Path == "/status":
		m.status(w)
		return
	case strings.HasSuffix(r.URL.Path, "/v1/models") && r.Method == http.MethodGet:
		m.models(w, r)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, multiMaxBody+1))
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": "read body failed"})
		return
	}
	if len(body) > multiMaxBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request body too large"})
		return
	}
	isStream := false
	if len(body) > 0 {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			writeJSON(w, 400, map[string]any{"error": "invalid JSON"})
			return
		}
		isStream, _ = payload["stream"].(bool)
	}

	lastStatus := http.StatusBadGateway
	var lastBody any = map[string]any{"error": map[string]any{"message": "all sources unavailable", "type": "UpstreamError"}}
	tried := 0
	for _, u := range m.sources {
		if u.inCooldown(time.Now()) {
			continue
		}
		tried++
		if m.trySource(w, u, r, body, isStream) {
			return
		}
		u.mu.Lock()
		lastStatus, lastBody = u.lastStatus, u.lastErrBody
		u.mu.Unlock()
	}
	if tried == 0 {
		anyExceeded := false
		for _, u := range m.sources {
			u.mu.Lock()
			if u.exceeded {
				anyExceeded = true
			}
			u.mu.Unlock()
			if anyExceeded {
				break
			}
		}
		if anyExceeded {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"message": "all sources exceeded daily free usage limit", "type": "FreeUsageLimitError"}})
			return
		}
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"message": "all sources in cooldown", "type": "UpstreamError"}})
		return
	}
	// 全源试过且都失败: 回显最后一个失败源的状态/错误体（最贴近上游）。
	writeJSON(w, lastStatus, lastBody)
}

// status 输出各源聚合状态（类似 multi-proxy 的 /status）。
func (m *multiHandler) status(w http.ResponseWriter) {
	now := time.Now()
	srcs := map[string]any{}
	for _, u := range m.sources {
		u.mu.Lock()
		cd := u.cooldownUntil.Sub(now).Seconds()
		lastErr := u.lastErr
		reqs := u.reqs
		exceeded := u.exceeded
		u.mu.Unlock()
		if cd < 0 {
			cd = 0
		}
		srcs[u.src.Name] = map[string]any{
			"base": u.base, "net": u.src.Net,
			"cooldown_sec": int(cd), "reqs": reqs,
			"last_err": lastErr, "exceeded": exceeded,
		}
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "sources": srcs})
}

// models 转发第一个非冷却源的模型列表。
func (m *multiHandler) models(w http.ResponseWriter, r *http.Request) {
	for _, u := range m.sources {
		if u.inCooldown(time.Now()) {
			continue
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u.base+"/v1/models", nil)
		if err != nil {
			continue
		}
		applyFingerprintHeaders(req)
		resp, err := u.tr.RoundTrip(req)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		h := w.Header()
		mergeHeaders(h, resp.Header)
		h.Set("Content-Type", "application/json")
		setCORSHeaders(w, r)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		return
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": []any{}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if h.Get("Access-Control-Allow-Origin") == "" {
		h.Set("Access-Control-Allow-Origin", "*")
	}
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// trySource 把请求转发到单个 opencode 源：按序应用 opencode 指纹/门禁，
// 遇到不可用/超时/超配额返回 false 让上层换源。成功返回 true。
func (m *multiHandler) trySource(w http.ResponseWriter, u *multiUpstream, r *http.Request, body []byte, isStream bool) bool {
	path := r.URL.Path
	if len(body) == 0 && strings.HasSuffix(path, "/chat/completions") {
		path = "/v1/models"
	}
	req, err := m.buildRequest(u, r.Method, path, body, r.Header)
	if err != nil {
		u.setErr(err.Error())
		return false
	}

	log.Printf("%s: connecting... ", u.src.Name)
	resp, err := u.tr.RoundTrip(req)
	if err != nil {
		log.Printf("%s: connect failed: %v", u.src.Name, err)
		u.setCooldown(multiCooldownShort)
		u.setErr(err.Error())
		return false
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		var obj any
		if err := json.Unmarshal(data, &obj); err != nil {
			obj = map[string]any{"error": map[string]any{"message": string(data)}}
		}
		u.setErr(fmt.Sprintf("HTTP %d", resp.StatusCode))
		u.mu.Lock()
		u.lastStatus, u.lastErrBody = resp.StatusCode, obj
		// exceed (429 / FreeUsageLimitError / Banned): 该源配额用尽, 冷却到
		// 当日 UTC 午夜, 后续请求不再碰它, 自动落下一个源。
		if isExceed(resp.StatusCode, data) {
			u.exceeded = true
			u.cooldownUntil = nextUTCMidnight()
			log.Printf("%s: EXCEEDED HTTP %d -> cooldown until %s (UTC), next source", u.src.Name, resp.StatusCode, u.cooldownUntil.UTC().Format("15:04"))
		} else {
			u.exceeded = false
		}
		u.mu.Unlock()
		log.Printf("%s: HTTP %d -> next source", u.src.Name, resp.StatusCode)
		return false
	}

	u.incrReqs()
	u.setErr("")
	u.mu.Lock()
	u.exceeded = false
	u.mu.Unlock()

	if !isStream {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		h := w.Header()
		mergeHeaders(h, resp.Header)
		h.Set("Content-Type", "application/json")
		setCORSHeaders(w, r)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return true
	}

	// 流式：预读首块（最多 multiPreReadTimeout），拿不到就换源。
	type readRes struct {
		buf []byte
		err error
	}
	ch := make(chan readRes, 1)
	go func() {
		buf := make([]byte, 4096)
		n, err := resp.Body.Read(buf)
		ch <- readRes{buf[:n], err}
	}()
	timer := time.NewTimer(multiPreReadTimeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		if len(res.buf) == 0 {
			log.Printf("%s: stream closed before first chunk -> next source", u.src.Name)
			resp.Body.Close()
			u.setCooldown(multiCooldownShort)
			u.setErr("first chunk stall")
			return false
		}
		h := w.Header()
		mergeHeaders(h, resp.Header)
		setCORSHeaders(w, r)
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write(res.buf)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = io.Copy(w, resp.Body)
		resp.Body.Close()
		return true
	case <-timer.C:
		log.Printf("%s: first chunk timeout %.0fs -> next source", u.src.Name, multiPreReadTimeout.Seconds())
		resp.Body.Close()
		u.setCooldown(multiCooldownShort)
		u.setErr("first chunk timeout")
		return false
	}
}

// buildRequest 构造发往 opencode 源的请求，应用指纹 + 请求体门禁
// （与单源 lane 同逻辑：stream:true、四件套、decoy、路径路由）。
func (m *multiHandler) buildRequest(u *multiUpstream, method, path string, body []byte, head http.Header) (*http.Request, error) {
	full := u.base + path
	req, err := http.NewRequestWithContext(context.Background(), method, full, nil)
	if err != nil {
		return nil, err
	}
	// 复制客户端 header
	for k, vv := range head {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	// opencode 门禁：重写 body（指纹在 proxyhandler 的 Director 里做，这里
	// 直接内联——multiUpstream 不是 httputil.ReverseProxy）。
	if len(body) > 0 {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		model, _ := payload["model"].(string)
		sanitizeRolesPublic(payload)
		payload["stream"] = true
		rawTools, _ := payload["tools"].([]any)
		_, _ = ensureToolQuartetPublic(payload, rawTools)
		rewritten, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(rewritten))
		req.ContentLength = int64(len(rewritten))
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(rewritten)), nil
		}
		// 端点路由：muse-spark -> /responses 等
		if np := reRouteModelEndpoint(req, model); np != "" {
			req.URL.Path = strings.TrimSuffix(req.URL.Path, "/chat/completions") + np
		}
	}
	// 指纹
	applyFingerprintHeaders(req)
	req.Host = u.target.Host
	return req, nil
}

// --- 小工具：与 opencode.go 的函数签名对齐（避免改单源代码） ---

func sanitizeRolesPublic(payload map[string]any) { sanitizeRoles(payload) }
func ensureToolQuartetPublic(p map[string]any, tools []any) ([]any, bool) {
	return ensureToolQuartet(p, tools, false)
}

func mergeHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// buildMultiHandler 从配置的 sources 构建多源聚合 handler。
func buildMultiHandler(sources []openSource) (http.Handler, error) {
	if len(sources) == 0 {
		return nil, fmt.Errorf("no sources configured")
	}
	m := &multiHandler{}
	for _, src := range sources {
		name := strings.TrimSpace(src.Name)
		if name == "" {
			name = src.Endpoint
		}
		src.Name = name
		u, err := newMultiUpstream(src)
		if err != nil {
			return nil, err
		}
		m.sources = append(m.sources, u)
	}
	names := make([]string, 0, len(m.sources))
	for _, u := range m.sources {
		names = append(names, u.src.Name+"@"+u.src.Net+"@"+u.base)
	}
	log.Printf("multi-source aggregate: %s", strings.Join(names, ", "))
	return m, nil
}
