package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 第一源返回 FreeUsageLimitError（exceed）→ 冷却到 UTC 午夜 →
// 请求自动落到第二源拿到 200。
func TestMultiAggregateFailover(t *testing.T) {
	var src2Hits int
	src1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"FreeUsageLimitError","message":"daily free usage limit"}}`))
	}))
	defer src1.Close()
	src2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src2Hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer src2.Close()

	h, err := buildMultiHandler([]openSource{
		{Name: "src1", Endpoint: src1.URL},
		{Name: "src2", Endpoint: src2.URL},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if src2Hits != 1 {
		t.Fatalf("src2 should have been hit exactly once, got %d", src2Hits)
	}
	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}

	// 第一源已 exceed 冷却，再发一个请求仍应落在 src2。
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK || src2Hits != 2 {
		t.Fatalf("after exceed, src2 should keep serving (hits=%d)", src2Hits)
	}
}

// 全部源 exceed → 回 429 FreeUsageLimitError。
func TestMultiAggregateAllExceeded(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"FreeUsageLimitError"}}`))
	}))
	defer up.Close()

	h, _ := buildMultiHandler([]openSource{
		{Name: "a", Endpoint: up.URL},
		{Name: "b", Endpoint: up.URL},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	// 429 时才可能检测到流式请求头未提交，这里只要错误类型正确。
	var obj map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &obj)
	if typ, _ := obj["error"].(map[string]any)["type"].(string); typ != "FreeUsageLimitError" {
		t.Fatalf("error type = %q, want FreeUsageLimitError", typ)
	}
}

// /status 聚合所有源的状态。
func TestMultiAggregateStatus(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	h, _ := buildMultiHandler([]openSource{
		{Name: "one", Endpoint: up.URL, Net: "v4"},
	})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var obj map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &obj)
	srcs, ok := obj["sources"].(map[string]any)
	if !ok {
		t.Fatalf("no sources in /status: %s", rec.Body.String())
	}
	if _, ok := srcs["one"].(map[string]any); !ok {
		t.Fatalf("source 'one' missing: %s", rec.Body.String())
	}
}

// 流式：第一源首块超时（只发头不发数据）→ 换第二源正常出流。
func TestMultiAggregateStreamStuckSource(t *testing.T) {
	// 第一源：200 + SSE 头，但永远不发数据（模拟卡死）；客户端断开即退出，
	// 避免 httptest.Server.Close() 无限等 handler。
	src1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
	}))
	defer src1.Close()
	src2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer src2.Close()

	h, _ := buildMultiHandler([]openSource{
		{Name: "stuck", Endpoint: src1.URL},
		{Name: "ok", Endpoint: src2.URL},
	})

	body := `{"model":"m","stream":true,"messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("streamed content missing: %s", rec.Body.String())
	}
}

// 非流式模型列表转发第一个健康源。
func TestMultiAggregateModels(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer up.Close()

	h, _ := buildMultiHandler([]openSource{{Name: "one", Endpoint: up.URL}})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "m1") {
		t.Fatalf("models: %d %s", rec.Code, rec.Body.String())
	}
}

var _ = io.Discard
