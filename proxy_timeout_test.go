package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- 配置解析 ---

func TestTimeoutConfigParsing(t *testing.T) {
	cases := []struct {
		name   string
		json   string
		wantTO float64
		wantFB float64
	}{
		{"numbers", `{"endpoint":"https://example.com","listen":"127.0.0.1:1","timeout":30,"first_byte_timeout":5}`, 30, 5},
		{"strings", `{"endpoint":"https://example.com","listen":"127.0.0.1:1","timeout":"45","first_byte_timeout":"2.5"}`, 45, 2.5},
		{"aliases", `{"endpoint":"https://example.com","listen":"127.0.0.1:1","upstream_timeout":60,"header_timeout":8}`, 60, 8},
		{"absent", `{"endpoint":"https://example.com","listen":"127.0.0.1:1"}`, 0, 0},
		{"negative clamped", `{"endpoint":"https://example.com","listen":"127.0.0.1:1","timeout":-5,"first_byte_timeout":-1}`, 0, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var it ProxyItem
			if err := json.Unmarshal([]byte(c.json), &it); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if err := it.Validate(0); err != nil {
				t.Fatalf("validate: %v", err)
			}
			if it.TimeoutSecs != c.wantTO {
				t.Errorf("TimeoutSecs = %v, want %v", it.TimeoutSecs, c.wantTO)
			}
			if it.FirstByteSecs != c.wantFB {
				t.Errorf("FirstByteSecs = %v, want %v", it.FirstByteSecs, c.wantFB)
			}
		})
	}
}

func TestTimeoutDurationHelpers(t *testing.T) {
	unset := ProxyItem{}
	if got := unset.TimeoutDuration(); got != 0 {
		t.Errorf("unset TimeoutDuration = %v, want 0", got)
	}
	if got := unset.FirstByteDuration(); got != 0 {
		t.Errorf("unset FirstByteDuration = %v, want 0", got)
	}
	withTO := ProxyItem{TimeoutSecs: 1.5}
	if got := withTO.TimeoutDuration(); got != 1500*time.Millisecond {
		t.Errorf("TimeoutDuration = %v, want 1.5s", got)
	}
	withFB := ProxyItem{FirstByteSecs: 0.25}
	if got := withFB.FirstByteDuration(); got != 250*time.Millisecond {
		t.Errorf("FirstByteDuration = %v, want 250ms", got)
	}
}

// --- 端到端行为 ---

// slowUpstream 起一个"慢"上游：delay 之后才写响应头。
func slowUpstream(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func buildHandlerFor(t *testing.T, item ProxyItem) http.Handler {
	t.Helper()
	if err := item.Validate(0); err != nil {
		t.Fatalf("validate: %v", err)
	}
	tr, err := createTransport("")
	if err != nil {
		t.Fatalf("createTransport: %v", err)
	}
	h, err := BuildProxyHandler(item, tr)
	if err != nil {
		t.Fatalf("BuildProxyHandler: %v", err)
	}
	return h
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode error body %q: %v", string(body), err)
	}
	return out.Error.Code
}

// 总超时：代理主动放弃，返回 504 而不是陪着客户端干等。
func TestUpstreamTimeoutReturns504(t *testing.T) {
	up := slowUpstream(t, 600*time.Millisecond)
	item := ProxyItem{Endpoint: up.URL, Listen: "127.0.0.1:0", TimeoutSecs: 0.1}
	h := buildHandlerFor(t, item)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "upstream_timeout" {
		t.Errorf("error.code = %q, want upstream_timeout", got)
	}
}

// 首字节超时：上游排队迟迟不给响应头时快速失败。
func TestFirstByteTimeoutReturns504(t *testing.T) {
	up := slowUpstream(t, 600*time.Millisecond)
	item := ProxyItem{Endpoint: up.URL, Listen: "127.0.0.1:0", FirstByteSecs: 0.1}
	h := buildHandlerFor(t, item)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "upstream_timeout" {
		t.Errorf("error.code = %q, want upstream_timeout", got)
	}
}

// 首字节超时不该截断"已经开始返回"的正常流：拿到响应头之后继续输出是允许的。
func TestFirstByteTimeoutDoesNotCutOngoingStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 响应头已经发出，之后慢一点也应当照常送达
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte("data: done\n\n"))
	}))
	defer srv.Close()

	item := ProxyItem{Endpoint: srv.URL, Listen: "127.0.0.1:0", FirstByteSecs: 0.2}
	h := buildHandlerFor(t, item)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "done") {
		t.Errorf("stream body truncated: %q", rec.Body.String())
	}
}

// 客户端自己断开：必须标成 client_disconnected，别再让人误判成代理故障。
func TestClientDisconnectReturnsClientDisconnected(t *testing.T) {
	up := slowUpstream(t, 600*time.Millisecond)
	item := ProxyItem{Endpoint: up.URL, Listen: "127.0.0.1:0"}
	h := buildHandlerFor(t, item)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`)).WithContext(ctx)
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "client_disconnected" {
		t.Errorf("error.code = %q, want client_disconnected", got)
	}
}

// 向后兼容：不配超时时行为与 v1.3.0 完全一致，慢上游照样等到成功。
func TestNoTimeoutConfigWaitsForSlowUpstream(t *testing.T) {
	up := slowUpstream(t, 300*time.Millisecond)
	item := ProxyItem{Endpoint: up.URL, Listen: "127.0.0.1:0"}
	h := buildHandlerFor(t, item)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want ok", rec.Body.String())
	}
}
