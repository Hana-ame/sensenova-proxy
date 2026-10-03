package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ===========================================================================
// opencode zen free-tier lane
//
// The opencode gateway (https://opencode.ai/zen/v1, free tier) refuses to
// serve requests that do not carry the opencode client fingerprint AND do not
// satisfy its request-body gate:
//
//   - stream must be true
//   - tools must declare the lowercase quartet bash / glob / grep / read
//
// This file turns any OpenAI-compatible client into a valid opencode client
// by stamping the headers/id-shape on the way out, rewriting the body to
// satisfy the gate, and hardening the SSE stream on the way back (keepalive
// injection when the upstream stalls or EOFs without a finish_reason, and
// gate-error classification onto the X-Opencode-Gate response header).
//
// It absorbs the fingerprint / squeeze / keepalive achievements of
// wintools/capture-proxy and the id-minting / routing of dsh-our-free-model.
// ===========================================================================

const (
	opencodeUA         = "opencode/1.18.31 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
	opencodeDefaultKey = "public"
	opencodeClientTag  = "desktop"
	opencodeProject    = "global"

	// firstChunkGap bounds how long we wait for the first SSE event before
	// giving up and terminating the stream with [DONE].
	firstChunkGap = 30 * time.Second
	// idleGap is the silence budget between real content events after which a
	// keepalive tool call is injected so the client's tool loop resumes.
	idleGap = 90 * time.Second

	// inf* are the injected keepalive tool call fields.
	infToolName = "bash"
	infIdleArg  = `{"command":"echo 继续"}`
	infCallID   = "call_idle"

	// maxRequestBody caps the request body we are willing to read and re-encode.
	maxRequestBody = 12 << 20
)

var (
	// fingerprintTools is the lowercase tool quartet the free tier requires.
	fingerprintTools = []string{"bash", "glob", "grep", "read"}

	// toolDecoyDesc marks filled-in tools as self-disabling, so if the model
	// ever does invoke one, it is a no-op instead of a surprise device.
	toolDecoyDesc = "This tool is currently unavailable and must not be used."

	// responsesModels are served by /zen/v1/responses, not /chat/completions.
	responsesModels = map[string]bool{
		"muse-spark-1.2-contributor-free": true,
		"muse-spark-1.3-contributor-free": true,
	}
	// messagesModels are served by /zen/v1/messages (Anthropic shape).
	messagesModels = map[string]bool{
		"union-alpha": true,
	}

	// opencodeBase62 is the alphabet used for the random id tail, matching the
	// gateway's 14-char [0-9A-Za-z] ids.
	opencodeBase62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	idMintMu     sync.Mutex
	idMintLastTS uint64 // last minted millisecond timestamp
	idMintSeq    uint64 // per-millisecond monotonic sequence
)

// IsOpencode reports whether this instance should run the opencode lane.
// An explicit provider wins; otherwise the endpoint host is probed.
func (item *ProxyItem) IsOpencode() bool {
	if item.Provider != "" {
		return item.Provider == "opencode"
	}
	if item.TargetURL == nil {
		return false
	}
	host := strings.ToLower(item.TargetURL.Hostname())
	return host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
}

// opencodeBase returns the trimmed upstream base path (e.g. "/zen/v1").
func (item *ProxyItem) opencodeBase() string {
	if item.TargetURL == nil {
		return ""
	}
	return strings.TrimRight(item.TargetURL.Path, "/")
}

// ---------------------------------------------------------------------------
// id minting — gateway-shaped ses_*/msg_* ids
// ---------------------------------------------------------------------------

// round1 把时长压到 0.1s 精度，日志里不用看 15 位小数。
func round1(d time.Duration) time.Duration {
	return d.Round(100 * time.Millisecond)
}

// base62 renders n in the gateway's base62 alphabet.
func base62(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = opencodeBase62[n%62]
		n /= 62
	}
	return string(buf[i:])
}

// hex48 renders the low 48 bits of v as 12 lowercase hex characters.
func hex48(v uint64) string {
	v &= 0xFFFFFFFFFFFF
	var b [6]byte
	binary.BigEndian.PutUint32(b[2:], uint32(v))
	b[0] = byte(v >> 40)
	b[1] = byte(v >> 32)
	return hex.EncodeToString(b[:])
}

// base62FromBytes maps each byte to one base62 character (byte % 62), matching
// the gateway's random tail construction.
func base62FromBytes(b []byte) string {
	out := make([]byte, len(b))
	for i, x := range b {
		out[i] = opencodeBase62[x%62]
	}
	return string(out)
}

// mintIDPart produces a 26-char identifier body: 12 hex (from the inverted /
// plain millisecond counter) + 14 random base62 chars. Session ids are
// bit-inverted, request ids are not — matching the gateway's observed format.
func mintIDPart(invert bool) string {
	idMintMu.Lock()
	now := uint64(time.Now().UnixMilli())
	if now == idMintLastTS {
		idMintSeq++
	} else {
		idMintLastTS = now
		idMintSeq = 0
	}
	v := now*0x1000 + idMintSeq
	idMintMu.Unlock()

	if invert {
		v = ^v
	}
	h := hex48(v)

	raw := make([]byte, 14)
	if _, err := rand.Read(raw); err != nil {
		// Extremely unlikely; fall back to a deterministic tail derived from v.
		for i := range raw {
			raw[i] = byte(v>>uint(8*(i%6))) ^ byte(0x5a+i)
		}
	}
	return h + base62FromBytes(raw)
}

func mintSessionID() string { return "ses_" + mintIDPart(true) }
func mintRequestID() string { return "msg_" + mintIDPart(false) }

func uint64FromBytes(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

// sessionForSeed derives a stable session id for a conversation seed, so a
// whole conversation (and its free quota — which is per-session) sticks to one
// id: sha256("our-free-model\0"+seed) -> ses_ + hex48(first 8 bytes) +
// base62(bytes 8..22).
func sessionForSeed(seed string) string {
	sum := sha256.Sum256([]byte("our-free-model\x00" + seed))
	return "ses_" + hex48(uint64FromBytes(sum[0:8])) + base62FromBytes(sum[8:22])
}

func sha256digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// ---------------------------------------------------------------------------
// header fingerprint
// ---------------------------------------------------------------------------

// resolveSessionMarker 决定这次请求用哪个会话标识。
//
// 上游的免费配额是按 session 计的，所以这个值必须在同一会话内稳定——早先
// 这里每请求调一次 mintSessionID()，等于把配额逐请求切碎，而且日志里
// 每次都是不同的 ses_* 值，看着像随机噪声。优先级：
//
//  1. 客户端自带（X-Session-Id / X-Opencode-Session / X-Session-Affinity，
//     或配置 session_header 指定的任意头）——原样沿用，这是最准的。
//  2. session_fallback = client（默认）——按客户端 IP 派生，同一客户端稳定。
//  3. session_fallback = instance——整个实例共用一个固定值。
//  4. session_fallback = request——每请求新值（保留旧行为，一般不用）。
func resolveSessionMarker(item ProxyItem, req *http.Request) string {
	// 1) 客户端显式提供的
	if item.SessionHeader != "" {
		if v := strings.TrimSpace(req.Header.Get(item.SessionHeader)); v != "" {
			return sanitizeSessionMarker(v)
		}
	}
	for _, k := range []string{"X-Session-Id", "X-Opencode-Session", "X-Session-Affinity", "X-Conversation-Id"} {
		if v := strings.TrimSpace(req.Header.Get(k)); v != "" {
			return sanitizeSessionMarker(v)
		}
	}

	// 2~4) 回退策略
	switch item.SessionFallbackMode() {
	case "instance":
		return sessionForSeed("instance\x00" + item.Listen)
	case "request":
		return mintSessionID()
	default: // "client"
		ip, _, err := net.SplitHostPort(req.RemoteAddr)
		if err != nil {
			ip = req.RemoteAddr
		}
		return sessionForSeed(ip)
	}
}

// sanitizeSessionMarker 把客户端给的值规整成上游认的 ses_* 形式，同时保留
// 可读性：如果客户端已经给了合法的 ses_ 值就原样返回，否则哈希派生一个。
// 直接透传任意字符可能被上游拒掉（换行、非 ASCII 都会让 header 非法），
// 所以这里统一走一遍。
func sanitizeSessionMarker(v string) string {
	if strings.HasPrefix(v, "ses_") && len(v) > 4 && isSessionSafe(v[4:]) {
		return v
	}
	return sessionForSeed("client\x00" + v)
}

// isSessionSafe 检查 ses_ 后面的部分是否只含上游允许的字符（base62 + 十六进制）。
func isSessionSafe(s string) bool {
	for _, c := range s {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			continue
		}
		return false
	}
	return true
}

// applyFingerprintHeaders stamps the opencode client fingerprint onto an
// outbound request. Values the client already supplied are preserved.
//
// sess 是本次请求的稳定会话标识（由 resolveSessionMarker 决定），三个
// session 头用同一个值，保证上游按 session 归并。
func applyFingerprintHeaders(req *http.Request, sess string) {
	if ua := req.Header.Get("User-Agent"); !strings.HasPrefix(ua, "opencode") {
		req.Header.Set("User-Agent", opencodeUA)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	setIfEmpty := func(k, v string) {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
	if sess == "" {
		sess = mintSessionID()
	}
	setIfEmpty("X-Opencode-Client", opencodeClientTag)
	setIfEmpty("X-Opencode-Project", opencodeProject)
	// session 三个头必须**强制覆盖**，不能用 setIfEmpty：客户端可能带了非法值
	//（含空格/换行/非 ASCII），透传上去会让上游拒绝，而这里恰恰是统一规整
	// 成合法 ses_* 的地方。resolveSessionMarker 已经把客户端值清洗过了，
	// 所以覆盖不会丢信息。
	req.Header.Set("X-Session-Id", sess)
	req.Header.Set("X-Session-Affinity", sess)
	req.Header.Set("X-Opencode-Session", sess)
	setIfEmpty("X-Opencode-Request", mintRequestID())
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "text/event-stream")
	}
}

// ---------------------------------------------------------------------------
// request-body gate & hygiene
// ---------------------------------------------------------------------------

// rewriteOpencodeBody reads the client body, sanitizes roles and clamps
// parameters, forces the free-tier gate (stream:true + tool quartet), and
// returns the re-encoded body plus the resolved model and whether the client
// declared any tools. An empty body passes through untouched.
func rewriteOpencodeBody(r *http.Request) ([]byte, string, bool, error) {
	body, err := readRequestBody(r)
	if err != nil {
		return nil, "", false, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, "", false, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", false, fmt.Errorf("invalid JSON body: %w", err)
	}

	model, _ := payload["model"].(string)

	sanitizeRoles(payload)

	// Numeric clamps (the gateway rejects out-of-range values).
	if v, ok := payload["max_tokens"]; ok {
		if n, isInt := toInt(v); isInt {
			if n < 1 {
				n = 1
			}
			if n > 131072 {
				n = 131072
			}
			payload["max_tokens"] = n
		}
	}
	if v, ok := payload["top_p"]; ok {
		if f, isF := toFloat(v); isF {
			if f <= 0 {
				f = 0.1
			}
			if f > 1 {
				f = 1
			}
			payload["top_p"] = f
		}
	}
	if v, ok := payload["temperature"]; ok {
		if f, isF := toFloat(v); isF {
			if f < 0 {
				f = 0
			}
			if f > 2 {
				f = 2
			}
			payload["temperature"] = f
		}
	}

	payload["reasoning_effort"] = "max"
	payload["stream"] = true

	// Tool quartet: preserve client tools, fill missing slots with decoys.
	rawTools, _ := payload["tools"].([]any)
	_, _ = ensureToolQuartet(payload, rawTools, false)

	out, err := json.Marshal(payload)
	if err != nil {
		return nil, "", false, fmt.Errorf("re-encode body: %w", err)
	}
	return out, model, len(rawTools) > 0, nil
}

// ensureToolQuartet makes sure tools contains bash/glob/grep/read, filling
// missing slots with self-disabling decoys, and sets tool_choice so the model
// can never be forced to invoke a decoy: "auto" when the client brought tools
// (or when the shape is the flat responses-style one), "none" otherwise.
// Returns the final tools array and whether the client declared any tools.
func ensureToolQuartet(payload map[string]any, tools []any, flat bool) ([]any, bool) {
	seen := make(map[string]bool, len(tools))
	hadClientTools := false
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if n := toolNameOf(m); n != "" {
			seen[n] = true
			hadClientTools = true
		}
	}
	for _, want := range fingerprintTools {
		if seen[want] {
			continue
		}
		tools = append(tools, decoyTool(want, flat))
	}
	payload["tools"] = tools
	if hadClientTools || flat {
		payload["tool_choice"] = "auto"
	} else {
		payload["tool_choice"] = "none"
	}
	return tools, hadClientTools
}

// decoyTool builds a self-disabling tool declaration for the given slot.
func decoyTool(name string, flat bool) map[string]any {
	body := map[string]any{
		"name":        name,
		"description": toolDecoyDesc,
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
	}
	if flat {
		body["type"] = "function"
		return body
	}
	return map[string]any{"type": "function", "function": body}
}

// toolNameOf extracts the tool name from either the chat shape
// ({type:function,function:{name}}) or the flat responses-style shape.
func toolNameOf(m map[string]any) string {
	if n, ok := m["name"].(string); ok && n != "" {
		return n
	}
	if fn, ok := m["function"].(map[string]any); ok {
		if n, ok := fn["name"].(string); ok && n != "" {
			return n
		}
	}
	return ""
}

// pushTool appends a tool declaration to the payload's tools array.
func pushTool(payload map[string]any, tool map[string]any) {
	tools, _ := payload["tools"].([]any)
	payload["tools"] = append(tools, tool)
}

// sanitizeRoles downgrades developer messages to system — the gateway rejects
// the developer role.
func sanitizeRoles(payload map[string]any) {
	msgs, ok := payload["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if mm["role"] == "developer" {
			mm["role"] = "system"
		}
	}
}

func readRequestBody(r *http.Request) ([]byte, error) {
	if r == nil || r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		return nil, err
	}
	return body, nil
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// endpoint routing — some free models live behind dedicated endpoints
// ---------------------------------------------------------------------------

func endpointFor(model string) string {
	model = trimModelID(model)
	if responsesModels[model] {
		return "/responses"
	}
	if messagesModels[model] {
		return "/messages"
	}
	return "/chat/completions"
}

// trimModelID strips a vendor prefix ("anthropic/x") and a trailing
// " (thinking)" style qualifier.
func trimModelID(model string) string {
	if i := strings.Index(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	if i := strings.Index(model, " ("); i >= 0 {
		model = model[:i]
	}
	return model
}

func parseOpencodeModel(model string) string {
	return trimModelID(model)
}

// reRouteModelEndpoint rewrites a client's /chat/completions request to the
// endpoint the model actually lives on. Returns "" when no rewrite is needed.
func reRouteModelEndpoint(req *http.Request, model string) string {
	if !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		return ""
	}
	ep := endpointFor(trimModelID(model))
	if ep == "/chat/completions" {
		return ""
	}
	return ep
}

func ensureURLScheme(s string) string {
	if strings.Contains(s, "://") {
		return s
	}
	return "https://" + s
}

// ---------------------------------------------------------------------------
// SSE stream pump: idle detection + keepalive injection
// ---------------------------------------------------------------------------

// ssePumpBody replaces resp.Body with a reader that parses upstream SSE events,
// detects a stalled or prematurely terminated stream, and injects a synthetic
// bash tool call so the client's tool loop resumes. Non-event bytes pass
// through untouched so the framing the client expects is preserved.
//
// The proxy always injects the bash/glob/grep/read quartet into the request
// body (see rewriteOpencodeBody), so an SSE response from this lane is always
// in a tool-capable conversation and the keepalive injection is always valid.
type ssePumpBody struct {
	upstream io.ReadCloser
	out      chan pumpChunk
	done     chan struct{}
	model    string
	meta     *reqMeta

	pending []byte // leftover payload from an oversized chunk
}

type pumpChunk struct {
	data []byte
	eof  bool
	err  error
}

func newSSEPumpBody(upstream io.ReadCloser, model string, meta *reqMeta) *ssePumpBody {
	if meta == nil {
		meta = &reqMeta{rid: "-"}
	}
	meta.model = model
	b := &ssePumpBody{
		upstream: upstream,
		out:      make(chan pumpChunk, 64),
		done:     make(chan struct{}),
		model:    model,
		meta:     meta,
	}
	go b.pump()
	return b
}

func (b *ssePumpBody) Read(p []byte) (int, error) {
	for {
		if len(b.pending) > 0 {
			n := copy(p, b.pending)
			b.pending = b.pending[n:]
			return n, nil
		}
		select {
		case c, ok := <-b.out:
			if !ok {
				return 0, io.EOF
			}
			if c.err != nil {
				return 0, c.err
			}
			if c.eof {
				return 0, io.EOF
			}
			if len(c.data) <= len(p) {
				return copy(p, c.data), nil
			}
			n := copy(p, c.data)
			b.pending = c.data[n:]
			return n, nil
		case <-b.done:
			return 0, io.EOF
		}
	}
}

func (b *ssePumpBody) Close() error {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
	return b.upstream.Close()
}

type rawChunk struct {
	data []byte
	eof  bool
	err  error
}

func (b *ssePumpBody) pump() {
	raw := make(chan rawChunk, 32)
	go func() {
		buf := make([]byte, 16384)
		for {
			n, err := b.upstream.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				select {
				case raw <- rawChunk{data: cp}:
				case <-b.done:
					return
				}
			}
			if err != nil {
				m := rawChunk{}
				if errors.Is(err, io.EOF) {
					m.eof = true
				} else {
					m.err = err
				}
				select {
				case raw <- m:
				case <-b.done:
					return
				}
				close(raw)
				return
			}
		}
	}()

	var acc []byte
	finished := false
	doneSent := false
	injected := false
	sawTool := false
	lastReal := time.Now()
	started := time.Now()

	send := func(data []byte) bool {
		if len(data) == 0 {
			return true
		}
		select {
		case b.out <- pumpChunk{data: data}:
			return true
		case <-b.done:
			return false
		}
	}
	sendEOF := func() {
		select {
		case b.out <- pumpChunk{eof: true}:
		case <-b.done:
		}
	}

	// Pre-read: wait for the first complete real SSE event, or bail.
	preEv, preRest, err := b.preRead(raw)
	if err != nil {
		b.meta.logf("pre_read_failed", "err=%v", err)
		send([]byte("data: [DONE]\n\n"))
		sendEOF()
		return
	}
	acc = preRest
	if !send(append(append([]byte{}, preEv...), '\n', '\n')) {
		return
	}
	b.meta.logf("first_sse", "waited=%s", round1(time.Since(started)))

	if hasContent(preEv) {
		lastReal = time.Now()
	}
	if finishReason(preEv) != nil {
		finished = true
	}
	if bytes.Contains(preEv, []byte("data: [DONE]")) {
		finished = true
		doneSent = true
	}

	timer := time.NewTimer(idleGap)
	defer timer.Stop()

loop:
	for {
		// Drain complete events accumulated so far.
		for {
			ev, rest, ok := nextSSEEvent(acc)
			if !ok {
				acc = rest
				break
			}
			acc = rest
			if !isRealSSE(ev) {
				continue
			}
			if hasToolCall(ev) {
				sawTool = true
			}
			if hasError(ev) {
				if !finished && !injected {
					if !send(idleInject(b.model)) {
						break loop
					}
					injected = true
					doneSent = true
					break loop
				}
				continue // drop the error event
			}
			if finishReason(ev) != nil {
				finished = true
			}
			if bytes.Contains(ev, []byte("data: [DONE]")) {
				finished = true
				doneSent = true
			}
			if hasContent(ev) {
				lastReal = time.Now()
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idleGap)
			}
			if !send(append(append([]byte{}, ev...), '\n', '\n')) {
				break loop
			}
		}

		select {
		case m, ok := <-raw:
			if !ok || m.eof {
				if m.err != nil && !finished {
					b.meta.logf("midstream_error", "err=%v", m.err)
				}
				if !finished && !doneSent && !injected {
					if !send(idleInject(b.model)) {
						break loop
					}
					injected = true
					doneSent = true
				}
				if !doneSent {
					send([]byte("data: [DONE]\n\n"))
					doneSent = true
				}
				sendEOF()
				break loop
			}
			if m.err != nil {
				if !finished && !injected {
					if !send(idleInject(b.model)) {
						break loop
					}
					injected = true
					doneSent = true
				}
				if !doneSent {
					send([]byte("data: [DONE]\n\n"))
					doneSent = true
				}
				sendEOF()
				break loop
			}
			acc = append(acc, m.data...)
			if time.Since(lastReal) > idleGap {
				if !finished && !injected {
					if !send(idleInject(b.model)) {
						break loop
					}
					injected = true
					doneSent = true
				}
				if !doneSent {
					send([]byte("data: [DONE]\n\n"))
				}
				sendEOF()
				break loop
			}
			timer.Reset(idleGap - time.Since(lastReal))
		case <-timer.C:
			if !finished && !injected {
				if !send(idleInject(b.model)) {
					break loop
				}
				injected = true
				doneSent = true
			}
			if !doneSent {
				send([]byte("data: [DONE]\n\n"))
			}
			sendEOF()
			break loop
		case <-b.done:
			break loop
		}
	}

	if injected {
		b.meta.logf("keepalive_injected", "note=synthetic tool call appended to keep the stream alive")
	}
	b.meta.logf("stream_done", "took=%s saw_tool=%v injected=%v", round1(time.Since(started)), sawTool, injected)
}

// preRead waits for the first complete real SSE event from raw, bounded by
// firstChunkGap. Returns the event bytes (without the trailing blank line) and
// any trailing partial bytes, or an error on timeout / upstream failure.
func (b *ssePumpBody) preRead(raw <-chan rawChunk) ([]byte, []byte, error) {
	deadline := time.Now().Add(firstChunkGap)
	var buf []byte
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return nil, nil, fmt.Errorf("upstream sent no real SSE event within %s", firstChunkGap)
		}
		timer := time.NewTimer(remain)
		select {
		case m, ok := <-raw:
			timer.Stop()
			if !ok || m.eof {
				return nil, nil, fmt.Errorf("upstream closed before first SSE event")
			}
			if m.err != nil {
				return nil, nil, m.err
			}
			buf = append(buf, m.data...)
			if ev, rest, complete := nextSSEEvent(buf); complete && isRealSSE(ev) {
				return ev, rest, nil
			}
		case <-timer.C:
			return nil, nil, fmt.Errorf("upstream stalled before first SSE event")
		case <-b.done:
			return nil, nil, fmt.Errorf("client disconnected")
		}
	}
}

// idleInject builds a two-event keepalive stream: a bash tool call followed by
// finish_reason=tool_calls and [DONE].
func idleInject(model string) []byte {
	toolEvt := fmt.Sprintf(
		`{"id":"idle","object":"chat.completion.chunk","created":0,"model":%s,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":null}]}`,
		jsonString(model), infCallID, infToolName, jsonString(infIdleArg))
	finishEvt := fmt.Sprintf(
		`{"id":"idle","object":"chat.completion.chunk","created":0,"model":%s,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		jsonString(model))
	return []byte("data: " + toolEvt + "\n\ndata: " + finishEvt + "\n\ndata: [DONE]\n\n")
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ---------------------------------------------------------------------------
// SSE parsing helpers
// ---------------------------------------------------------------------------

// isRealSSE reports whether buf already contains a real SSE data/event line
// (as opposed to stray HTTP framing or comments).
func isRealSSE(buf []byte) bool {
	for _, line := range bytes.Split(buf, []byte("\n")) {
		t := bytes.TrimSpace(line)
		if len(t) == 0 {
			continue
		}
		if bytes.HasPrefix(t, []byte(":")) {
			continue // SSE comment
		}
		if bytes.HasPrefix(t, []byte("data:")) || bytes.HasPrefix(t, []byte("event:")) {
			return true
		}
	}
	return false
}

// nextSSEEvent extracts the first complete SSE event from buf. CRLF framing is
// preferred, with LF as the fallback. Returns (event, rest, ok); ok is false
// when no complete event is present yet.
func nextSSEEvent(buf []byte) ([]byte, []byte, bool) {
	for _, sep := range [][]byte{[]byte("\r\n\r\n"), []byte("\n\n")} {
		if idx := bytes.Index(buf, sep); idx >= 0 {
			return buf[:idx], buf[idx+len(sep):], true
		}
	}
	return nil, buf, false
}

func hasToolCall(ev []byte) bool {
	return bytes.Contains(ev, []byte(`"tool_calls"`))
}

// hasContent reports whether the event carries anything a client would count
// as progress (content, tool calls, reasoning, a finish_reason, or [DONE]).
func hasContent(ev []byte) bool {
	if bytes.Contains(ev, []byte("data: [DONE]")) {
		return true
	}
	for _, needle := range []string{`"content"`, `"tool_calls"`, `"reasoning"`, `"finish_reason"`} {
		if bytes.Contains(ev, []byte(needle)) {
			return true
		}
	}
	return false
}

// hasError reports whether the event is an SSE error object (has an "error" key).
func hasError(ev []byte) bool {
	line := sseDataLine(ev)
	if len(line) == 0 || line[0] != '{' {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil {
		return false
	}
	_, ok := obj["error"]
	return ok
}

// finishReason returns the first non-empty finish_reason in the event, or nil.
func finishReason(ev []byte) *string {
	var p struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	line := sseDataLine(ev)
	if len(line) == 0 || line[0] != '{' {
		return nil
	}
	if err := json.Unmarshal(line, &p); err != nil {
		return nil
	}
	for _, c := range p.Choices {
		if c.FinishReason != nil && *c.FinishReason != "" {
			return c.FinishReason
		}
	}
	return nil
}

// sseDataLine returns the JSON payload of a "data: {...}" line, or nil.
func sseDataLine(ev []byte) []byte {
	idx := bytes.Index(ev, []byte("data:"))
	if idx < 0 {
		return nil
	}
	line := bytes.TrimSpace(ev[idx+len("data:"):])
	return line
}

// ---------------------------------------------------------------------------
// gate error classification
// ---------------------------------------------------------------------------

// gateClassOf returns the class token of a gateway error body, or "".
func gateClassOf(body []byte) string {
	s := string(body)
	switch {
	case strings.Contains(s, "FreeTierError"):
		return "FreeTierError"
	case strings.Contains(s, "FreeUsageLimitError"):
		return "FreeUsageLimitError"
	case strings.Contains(s, "RegionError"):
		return "RegionError"
	case strings.Contains(s, "Model is unavailable"):
		return "ModelUnavailable"
	case strings.Contains(s, "Endpoint is unavailable"):
		return "EndpointUnavailable"
	case strings.Contains(s, "AuthError") || strings.Contains(s, "Missing API key"):
		return "AuthError"
	}
	return ""
}

// classifyGateError reads the upstream error body, restores it for the client,
// and returns (class, hint). Empty class means "not a gate error".
func classifyGateError(resp *http.Response) (string, string) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", ""
	}
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	class := gateClassOf(body)
	if class == "" {
		return "", ""
	}
	return class, gateHintClassifies([]byte(class))
}

// gateHintClassifies returns an actionable hint for a gate error class.
func gateHintClassifies(class []byte) string {
	switch string(class) {
	case "FreeTierError":
		return "gate: request was rejected by the opencode free-tier shape check. This proxy already forces stream:true and the bash/glob/grep/read tool quartet, so a FreeTierError here means the upstream gateway is blocking this egress IP or the gateway rules changed. Check the egress of this instance (engress field) against the ones that work."
	case "FreeUsageLimitError":
		return "gate: the free-tier daily quota for this session/egress IP is exhausted. Try a different egress, or wait for the UTC quota reset."
	case "RegionError":
		return "gate: this model is region-locked for this egress IP. Try a different egress, or a different model (nemotron-3.5-lightning-free, mimo-v2.6-flash-free were open on 2026-10-02)."
	case "ModelUnavailable":
		return "gate: the upstream model is unavailable right now. Try again later or pick another free model."
	case "EndpointUnavailable":
		return "gate: the upstream endpoint is unavailable right now. Try again later."
	case "AuthError":
		return "gate: the upstream rejected authentication. Check the Authkey of this instance."
	}
	return ""
}

// ---------------------------------------------------------------------------
// faked /v1/models response — custom model ids injected into the model list so
// clients see them as available even when the upstream does not serve them.
// ---------------------------------------------------------------------------

// modelsResponse mirrors the OpenAI-style list body we receive from upstream.
type modelsResponse struct {
	Object string `json:"object"`
	Data   []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// rewriteModelsBody merges custom ids into (or outright replaces) the upstream
// /v1/models list. mode is "append" (default) or "replace". On any parse
// failure the original body is returned unchanged.
func rewriteModelsBody(body []byte, custom []string, mode string, meta *reqMeta) []byte {
	if len(custom) == 0 {
		return body
	}
	var resp modelsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		meta.logf("models_parse_failed", "err=%v (passing through)", err)
		return body
	}

	seen := make(map[string]bool, len(resp.Data)+len(custom))
	if mode == "replace" {
		resp.Data = nil
	} else {
		for _, m := range resp.Data {
			seen[m.ID] = true
		}
	}
	for _, id := range custom {
		if seen[id] {
			continue
		}
		seen[id] = true
		resp.Data = append(resp.Data, struct {
			ID string `json:"id"`
		}{ID: id})
	}
	resp.Object = "list"

	out, err := json.Marshal(resp)
	if err != nil {
		return body
	}
	return out
}
