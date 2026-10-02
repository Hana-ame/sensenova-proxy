package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// --- id minting ------------------------------------------------------------

var sessionIDRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
var requestIDRe = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

func TestMintSessionID(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := mintSessionID()
		if !sessionIDRe.MatchString(id) {
			t.Fatalf("session id %q does not match gateway regex", id)
		}
	}
}

func TestMintRequestID(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := mintRequestID()
		if !requestIDRe.MatchString(id) {
			t.Fatalf("request id %q does not match gateway regex", id)
		}
	}
}

func TestMintSessionIDMonotonicWithinMillisecond(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		id := mintSessionID()
		if seen[id] {
			t.Fatalf("duplicate session id %q within same millisecond", id)
		}
		seen[id] = true
	}
}

func TestSessionForSeedStable(t *testing.T) {
	a := sessionForSeed("chat-abc")
	b := sessionForSeed("chat-abc")
	if a != b {
		t.Fatalf("sessionForSeed is not deterministic: %q vs %q", a, b)
	}
	if !sessionIDRe.MatchString(a) {
		t.Fatalf("sessionForSeed %q does not match regex", a)
	}
	c := sessionForSeed("chat-def")
	if c == a {
		t.Fatalf("different seeds produced the same session id %q", a)
	}
}

// --- header fingerprint ----------------------------------------------------

func TestApplyFingerprintHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	applyFingerprintHeaders(req)

	ua := req.Header.Get("User-Agent")
	if !strings.HasPrefix(ua, "opencode/") {
		t.Fatalf("User-Agent not set to opencode: %q", ua)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := req.Header.Get("X-Opencode-Client"); got != opencodeClientTag {
		t.Fatalf("X-Opencode-Client = %q", got)
	}
	if got := req.Header.Get("X-Opencode-Session"); !sessionIDRe.MatchString(got) {
		t.Fatalf("X-Opencode-Session = %q", got)
	}
	if got := req.Header.Get("X-Opencode-Request"); !requestIDRe.MatchString(got) {
		t.Fatalf("X-Opencode-Request = %q", got)
	}
}

func TestApplyFingerprintHeadersPreservesClientUA(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("User-Agent", "opencode/1.18.30 desktop")
	req.Header.Set("X-Opencode-Session", "ses_000000000000A1B2C3D4E5F6")
	applyFingerprintHeaders(req)
	if got := req.Header.Get("User-Agent"); got != "opencode/1.18.30 desktop" {
		t.Fatalf("overwrote a genuine opencode UA: %q", got)
	}
	if got := req.Header.Get("X-Opencode-Session"); got != "ses_000000000000A1B2C3D4E5F6" {
		t.Fatalf("overwrote a genuine session id: %q", got)
	}
}

// --- body rewrite ----------------------------------------------------------

func TestRewriteOpencodeBodyForcesStreamAndQuartet(t *testing.T) {
	body := []byte(`{"model":"nemotron-3.5-lightning-free","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	out, model, hasTools, err := rewriteOpencodeBody(req)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if model != "nemotron-3.5-lightning-free" {
		t.Fatalf("model = %q", model)
	}
	if hasTools {
		t.Fatalf("client declared no tools, hasTools should be false")
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if payload["stream"] != true {
		t.Fatalf("stream not forced true: %v", payload["stream"])
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 4 {
		t.Fatalf("expected 4 tools, got %v", payload["tools"])
	}
	for _, want := range fingerprintTools {
		found := false
		for _, t := range tools {
			if toolNameOf(t.(map[string]any)) == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("tool %q missing from %v", want, tools)
		}
	}
	if payload["tool_choice"] != "none" {
		t.Fatalf("tool_choice = %v, want none", payload["tool_choice"])
	}
}

func TestRewriteOpencodeBodyPreservesClientTools(t *testing.T) {
	body := []byte(`{
		"model":"mimo-v2.6-flash-free",
		"messages":[{"role":"user","content":"ls"}],
		"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	out, _, hasTools, err := rewriteOpencodeBody(req)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !hasTools {
		t.Fatalf("client declared bash, hasTools should be true")
	}
	var payload map[string]any
	_ = json.Unmarshal(out, &payload)
	tools := payload["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("expected 4 tools, got %d: %v", len(tools), tools)
	}
	if toolNameOf(tools[0].(map[string]any)) != "bash" {
		t.Fatalf("client tool not preserved first: %v", tools[0])
	}
	desc := tools[0].(map[string]any)["function"].(map[string]any)["description"]
	if desc != nil {
		t.Fatalf("client tool description was overwritten: %v", desc)
	}
	if payload["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto", payload["tool_choice"])
	}
}

func TestRewriteOpencodeBodyDowngradesDeveloper(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"developer","content":"be brief"},{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	out, _, _, err := rewriteOpencodeBody(req)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var payload map[string]any
	_ = json.Unmarshal(out, &payload)
	msgs := payload["messages"].([]any)
	if got := msgs[0].(map[string]any)["role"]; got != "system" {
		t.Fatalf("developer not downgraded to system: %v", got)
	}
}

func TestRewriteOpencodeBodyClampsMaxTokens(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":999999,"top_p":5,"temperature":9}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	out, _, _, err := rewriteOpencodeBody(req)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var payload map[string]any
	_ = json.Unmarshal(out, &payload)
	if n, ok := toInt(payload["max_tokens"]); !ok || n != 131072 {
		t.Fatalf("max_tokens not clamped to 131072: %v", payload["max_tokens"])
	}
	if got := payload["top_p"]; got != float64(1) {
		t.Fatalf("top_p not clamped: %v", got)
	}
	if got := payload["temperature"]; got != float64(2) {
		t.Fatalf("temperature not clamped to 2: %v", got)
	}
}

func TestRewriteOpencodeBodyRejectsNonJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("<html>"))
	if _, _, _, err := rewriteOpencodeBody(req); err == nil {
		t.Fatalf("expected error for non-JSON body")
	}
}

func TestRewriteOpencodeBodyEmpty(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	out, model, hasTools, err := rewriteOpencodeBody(req)
	if err != nil {
		t.Fatalf("empty body should not error: %v", err)
	}
	if model != "" || hasTools {
		t.Fatalf("empty body should yield zero values: model=%q hasTools=%v", model, hasTools)
	}
	if len(out) != 0 {
		t.Fatalf("empty body should pass through unchanged: %q", out)
	}
}

// --- endpoint routing ------------------------------------------------------

func TestEndpointFor(t *testing.T) {
	cases := map[string]string{
		"nemotron-3.5-lightning-free":                "/chat/completions",
		"muse-spark-1.3-contributor-free":            "/responses",
		"union-alpha":                                "/messages",
		"muse-spark-1.3-contributor-free (thinking)": "/responses",
		"anthropic/muse-spark-1.2-contributor-free":  "/responses",
	}
	for model, want := range cases {
		if got := endpointFor(model); got != want {
			t.Fatalf("endpointFor(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestTrimModelID(t *testing.T) {
	if got := trimModelID("anthropic/muse-spark-1.2-contributor-free"); got != "muse-spark-1.2-contributor-free" {
		t.Fatalf("vendor prefix not stripped: %q", got)
	}
	if got := trimModelID("model (thinking)"); got != "model" {
		t.Fatalf("suffix not stripped: %q", got)
	}
}

// --- gate error classification --------------------------------------------

func TestClassifyGateError(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"error":{"message":"x","type":"FreeTierError"}}`, "FreeTierError"},
		{`{"error":{"message":"x","type":"FreeUsageLimitError"}}`, "FreeUsageLimitError"},
		{`{"error":{"message":"x","type":"RegionError"}}`, "RegionError"},
		{`{"error":{"message":"Model is unavailable","type":"server_error"}}`, "ModelUnavailable"},
		{`{"error":{"message":"Endpoint is unavailable","type":"server_error"}}`, "EndpointUnavailable"},
		{`{"error":{"message":"Missing API key","type":"AuthError"}}`, "AuthError"},
		{`{"error":{"message":"boom"}}`, ""},
	}
	for _, c := range cases {
		resp := &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(c.body)),
		}
		class, hint := classifyGateError(resp)
		if class != c.want {
			t.Fatalf("classifyGateError(%q) = %q, want %q", c.body, class, c.want)
		}
		if c.want != "" && hint == "" {
			t.Fatalf("classifyGateError(%q) returned a class but no hint", c.body)
		}
		// The body must be restored so the client still receives it.
		got, _ := io.ReadAll(resp.Body)
		if string(got) != c.body {
			t.Fatalf("classifyGateError corrupted the body: got %q", string(got))
		}
	}
}

func TestGateHintNonEmpty(t *testing.T) {
	classes := []string{
		"FreeTierError", "FreeUsageLimitError", "RegionError",
		"ModelUnavailable", "EndpointUnavailable", "AuthError",
	}
	for _, c := range classes {
		hint := gateHintClassifies([]byte(c))
		if !strings.HasPrefix(hint, "gate:") {
			t.Fatalf("hint for %q = %q, want prefix 'gate:'", c, hint)
		}
	}
}

// --- SSE helpers -----------------------------------------------------------

func TestIsRealSSE(t *testing.T) {
	if !isRealSSE([]byte("data: {}\n\n")) {
		t.Fatalf("data: should be real SSE")
	}
	if !isRealSSE([]byte("event: message\n")) {
		t.Fatalf("event: should be real SSE")
	}
	if isRealSSE([]byte("HTTP/1.1 200 OK\r\n")) {
		t.Fatalf("HTTP headers are not SSE")
	}
}

func TestNextSSEEvent(t *testing.T) {
	buf := []byte("data: {\"a\":1}\n\ndata: {\"b\":2}")
	ev, rest, ok := nextSSEEvent(buf)
	if !ok {
		t.Fatalf("expected an event")
	}
	if !bytes.Contains(ev, []byte("data: {\"a\":1}")) {
		t.Fatalf("wrong event: %q", ev)
	}
	if !strings.HasPrefix(string(rest), "data: {\"b\":2}") {
		t.Fatalf("wrong remainder: %q", rest)
	}
	if _, _, ok := nextSSEEvent([]byte("data: {\"a\":1}")); ok {
		t.Fatalf("incomplete event should not parse")
	}
	ev, _, ok = nextSSEEvent([]byte("data: x\r\n\r\n"))
	if !ok || !bytes.Contains(ev, []byte("data: x")) {
		t.Fatalf("CRLF framing not handled: %q ok=%v", ev, ok)
	}
}

func TestHasToolCall(t *testing.T) {
	ev := []byte(`data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"bash","arguments":"{}"}}]}}]}`)
	if !hasToolCall(ev) {
		t.Fatalf("should detect a tool call")
	}
	if hasToolCall([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`)) {
		t.Fatalf("content is not a tool call")
	}
}

func TestHasContent(t *testing.T) {
	if hasContent([]byte(`data: {"choices":[{"delta":{}}]}`)) {
		t.Fatalf("empty delta is not content")
	}
	if !hasContent([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`)) {
		t.Fatalf("content should count")
	}
	if !hasContent([]byte(`data: [DONE]`)) {
		t.Fatalf("[DONE] should count")
	}
	if !hasContent([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)) {
		t.Fatalf("finish_reason should count")
	}
}

func TestHasError(t *testing.T) {
	if !hasError([]byte(`data: {"error":{"type":"FreeTierError"}}`)) {
		t.Fatalf("should detect an error")
	}
	if hasError([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`)) {
		t.Fatalf("content is not an error")
	}
}

func TestFinishReason(t *testing.T) {
	ev := []byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	if fr := finishReason(ev); fr == nil || *fr != "stop" {
		t.Fatalf("finish_reason = %v", fr)
	}
	if fr := finishReason([]byte(`data: [DONE]`)); fr != nil {
		t.Fatalf("[DONE] should not carry a finish_reason: %v", fr)
	}
}

// --- provider detection ----------------------------------------------------

func TestIsOpencodeByHost(t *testing.T) {
	items := []struct {
		endpoint string
		want     bool
	}{
		{"https://opencode.ai/zen/v1", true},
		{"https://opencode.ai", true},
		{"https://token.sensenova.cn", false},
		{"https://api.openai.com/v1", false},
		{"https://sub.opencode.ai/v1", true},
	}
	for _, c := range items {
		item := ProxyItem{Endpoint: c.endpoint, Listen: "127.0.0.1:8001"}
		if err := item.Validate(0); err != nil {
			t.Fatalf("validate(%q): %v", c.endpoint, err)
		}
		if got := item.IsOpencode(); got != c.want {
			t.Fatalf("IsOpencode(%q) = %v, want %v", c.endpoint, got, c.want)
		}
	}
}

func TestProviderOverride(t *testing.T) {
	item := ProxyItem{Endpoint: "https://token.sensenova.cn", Provider: "opencode", Listen: "127.0.0.1:8001"}
	if err := item.Validate(0); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !item.IsOpencode() {
		t.Fatalf("explicit provider=opencode should win over the host")
	}
	if item.AuthKey != "public" {
		t.Fatalf("AuthKey not defaulted: %q", item.AuthKey)
	}
}

func TestProviderValidation(t *testing.T) {
	bad := ProxyItem{Endpoint: "https://x", Provider: "bogus"}
	if err := bad.Validate(0); err == nil {
		t.Fatalf("unknown provider should be rejected")
	}
}

// --- end-to-end handler ----------------------------------------------------

func TestOpencodeHandlerEndToEnd(t *testing.T) {
	var gotUA, gotSession, gotRequest string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		gotUA = req.Header.Get("User-Agent")
		gotSession = req.Header.Get("X-Opencode-Session")
		gotRequest = req.Header.Get("X-Opencode-Request")
		gotBody, _ = io.ReadAll(req.Body)
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	item := ProxyItem{Endpoint: upstream.URL + "/v1", Provider: "opencode", Listen: "127.0.0.1:0"}
	if err := item.Validate(0); err != nil {
		t.Fatalf("validate: %v", err)
	}
	tr, err := createTransport("")
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	handler, err := BuildProxyHandler(item, tr)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	reqBody := `{"model":"nemotron-3.5-lightning-free","messages":[{"role":"developer","content":"be brief"},{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if gotUA == "" || !strings.HasPrefix(gotUA, "opencode/") {
		t.Fatalf("User-Agent not forwarded: %q", gotUA)
	}
	if !sessionIDRe.MatchString(gotSession) {
		t.Fatalf("X-Opencode-Session = %q", gotSession)
	}
	if !requestIDRe.MatchString(gotRequest) {
		t.Fatalf("X-Opencode-Request = %q", gotRequest)
	}
	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v: %s", err, string(gotBody))
	}
	if sent["stream"] != true {
		t.Fatalf("stream not forced true: %v", sent["stream"])
	}
	tools, _ := sent["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("expected 4 tools on the wire, got %d", len(tools))
	}
	msgs := sent["messages"].([]any)
	if got := msgs[0].(map[string]any)["role"]; got != "system" {
		t.Fatalf("developer not downgraded: %v", got)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("SSE body not forwarded: %q", rec.Body.String())
	}
}

func TestOpencodeHandlerKeepaliveInjection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	}))
	defer upstream.Close()

	item := ProxyItem{Endpoint: upstream.URL + "/v1", Provider: "opencode", Listen: "127.0.0.1:0"}
	_ = item.Validate(0)
	tr, _ := createTransport("")
	handler, _ := BuildProxyHandler(item, tr)

	reqBody := `{"model":"m","stream":true,"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "partial") {
		t.Fatalf("real content lost: %q", body)
	}
	if !strings.Contains(body, `"name":"bash"`) {
		t.Fatalf("keepalive tool call not injected: %q", body)
	}
	if !strings.Contains(body, "echo 继续") {
		t.Fatalf("keepalive echo argument missing: %q", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason=tool_calls missing: %q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("no [DONE] terminator: %q", body)
	}
}

func TestOpencodeHandlerNonSSEPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer upstream.Close()

	item := ProxyItem{Endpoint: upstream.URL + "/v1", Provider: "opencode", Listen: "127.0.0.1:0"}
	_ = item.Validate(0)
	tr, _ := createTransport("")
	handler, _ := BuildProxyHandler(item, tr)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); got != `{"data":[{"id":"model-a"}]}` {
		t.Fatalf("non-SSE body mangled: %q", got)
	}
}

func TestOpencodeHandlerGateHeader(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusForbidden)
		_, _ = rw.Write([]byte(`{"error":{"type":"FreeTierError","message":"shape"}`))
	}))
	defer upstream.Close()

	item := ProxyItem{Endpoint: upstream.URL + "/v1", Provider: "opencode", Listen: "127.0.0.1:0"}
	_ = item.Validate(0)
	tr, _ := createTransport("")
	handler, _ := BuildProxyHandler(item, tr)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("X-Opencode-Gate"); got != "FreeTierError" {
		t.Fatalf("X-Opencode-Gate = %q, want FreeTierError", got)
	}
	if !strings.Contains(rec.Body.String(), "FreeTierError") {
		t.Fatalf("error body corrupted: %q", rec.Body.String())
	}
}

// --- faked /v1/models ------------------------------------------------------

func TestRewriteModelsBodyAppend(t *testing.T) {
	orig := []byte(`{"object":"list","data":[{"id":"model-a"}]}`)
	out := rewriteModelsBody(orig, []string{"dsv41f", "model-a"}, "append")
	var resp modelsResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	ids := make(map[string]bool)
	for _, m := range resp.Data {
		ids[m.ID] = true
	}
	if len(ids) != 2 || !ids["model-a"] || !ids["dsv41f"] {
		t.Fatalf("append failed: got %v", ids)
	}
}

func TestRewriteModelsBodyReplace(t *testing.T) {
	orig := []byte(`{"object":"list","data":[{"id":"model-a"}]}`)
	out := rewriteModelsBody(orig, []string{"dsv41f"}, "replace")
	var resp modelsResponse
	_ = json.Unmarshal(out, &resp)
	if len(resp.Data) != 1 || resp.Data[0].ID != "dsv41f" {
		t.Fatalf("replace failed: %+v", resp)
	}
}

func TestRewriteModelsBodyPassthrough(t *testing.T) {
	// No custom models -> untouched.
	orig := []byte(`{"object":"list","data":[{"id":"model-a"}]}`)
	if out := rewriteModelsBody(orig, nil, "append"); !bytes.Equal(out, orig) {
		t.Fatalf("nil custom should pass through: %s", out)
	}
	// Unparseable body -> untouched.
	bad := []byte(`<html>not json</html>`)
	if out := rewriteModelsBody(bad, []string{"dsv41f"}, "append"); !bytes.Equal(out, bad) {
		t.Fatalf("unparseable body should pass through: %s", out)
	}
}

func TestOpencodeHandlerModelsFake(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"object":"list","data":[{"id":"nemotron-3.5-lightning-free"}]}`))
	}))
	defer upstream.Close()

	item := ProxyItem{
		Endpoint:     upstream.URL + "/v1",
		Provider:     "opencode",
		Listen:       "127.0.0.1:0",
		CustomModels: []string{"dsv41f", "my-model"},
		ModelsMode:   "append",
	}
	_ = item.Validate(0)
	tr, _ := createTransport("")
	handler, _ := BuildProxyHandler(item, tr)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp modelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v: %s", err, rec.Body.String())
	}
	ids := make(map[string]bool)
	for _, m := range resp.Data {
		ids[m.ID] = true
	}
	if !ids["dsv41f"] || !ids["my-model"] || !ids["nemotron-3.5-lightning-free"] {
		t.Fatalf("custom models not injected: %v", ids)
	}
}

func TestOpencodeHandlerModelsReplace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"object":"list","data":[{"id":"nemotron-3.5-lightning-free"}]}`))
	}))
	defer upstream.Close()

	item := ProxyItem{
		Endpoint:     upstream.URL + "/v1",
		Provider:     "opencode",
		Listen:       "127.0.0.1:0",
		CustomModels: []string{"dsv41f"},
		ModelsMode:   "replace",
	}
	_ = item.Validate(0)
	tr, _ := createTransport("")
	handler, _ := BuildProxyHandler(item, tr)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var resp modelsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Data) != 1 || resp.Data[0].ID != "dsv41f" {
		t.Fatalf("replace mode failed: %+v", resp)
	}
}
