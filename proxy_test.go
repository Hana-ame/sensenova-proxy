package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	// 1. Test array JSON format
	arrayJSON := `[
		{
			"endpoint": "https://token.sensenova.cn",
			"Authkey": "sk-test-key-1",
			"engress": "127.0.0.1",
			"listen": "127.0.0.1:8001"
		},
		{
			"endpoint": "token.sensenova.cn/",
			"authkey": "sk-test-key-2",
			"egress": "",
			"listen": "8002"
		}
	]`
	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.WriteString(arrayJSON); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	tmpFile.Close()

	items, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}

	// Item 1
	if items[0].Endpoint != "https://token.sensenova.cn" {
		t.Errorf("item 0 endpoint = %s, want https://token.sensenova.cn", items[0].Endpoint)
	}
	if items[0].AuthKey != "sk-test-key-1" {
		t.Errorf("item 0 AuthKey = %s, want sk-test-key-1", items[0].AuthKey)
	}
	if items[0].Engress != "127.0.0.1" {
		t.Errorf("item 0 Engress = %s, want 127.0.0.1", items[0].Engress)
	}
	if items[0].Listen != "127.0.0.1:8001" {
		t.Errorf("item 0 Listen = %s, want 127.0.0.1:8001", items[0].Listen)
	}

	// Item 2 normalizations
	if items[1].Endpoint != "https://token.sensenova.cn" {
		t.Errorf("item 1 endpoint = %s, want normalized https://token.sensenova.cn", items[1].Endpoint)
	}
	if items[1].AuthKey != "sk-test-key-2" {
		t.Errorf("item 1 AuthKey = %s, want sk-test-key-2", items[1].AuthKey)
	}
	if items[1].Listen != "127.0.0.1:8002" {
		t.Errorf("item 1 Listen = %s, want normalized 127.0.0.1:8002", items[1].Listen)
	}
}

func TestCORSPreflight(t *testing.T) {
	u, _ := url.Parse("https://token.sensenova.cn")
	item := ProxyItem{
		Endpoint:  "https://token.sensenova.cn",
		AuthKey:   "sk-test",
		Engress:   "",
		Listen:    "127.0.0.1:8001",
		TargetURL: u,
	}

	tr, _ := createTransport("")
	handler, err := BuildProxyHandler(item, tr)
	if err != nil {
		t.Fatalf("BuildProxyHandler failed: %v", err)
	}

	req := httptest.NewRequest("OPTIONS", "/v1/images/generations", nil)
	req.Header.Set("Origin", "https://sensenova.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content for OPTIONS preflight, got %d", resp.StatusCode)
	}
	if origin := resp.Header.Get("Access-Control-Allow-Origin"); origin != "https://sensenova.example.com" {
		t.Errorf("expected Access-Control-Allow-Origin to match Origin header, got %s", origin)
	}
	if methods := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(methods, "POST") {
		t.Errorf("expected POST in Access-Control-Allow-Methods, got %s", methods)
	}
	if headers := resp.Header.Get("Access-Control-Allow-Headers"); headers != "authorization,content-type" {
		t.Errorf("expected Access-Control-Allow-Headers to echo request headers, got %s", headers)
	}
}

func TestAuthKeyAndPassthrough(t *testing.T) {
	// Spin up a mock upstream server
	var receivedAuth string
	var receivedPath string
	var receivedQuery string
	var receivedBody string

	var receivedXApiKey string
	var receivedCustomHeader string

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		receivedAuth = req.Header.Get("Authorization")
		receivedXApiKey = req.Header.Get("X-Api-Key")
		receivedCustomHeader = req.Header.Get("X-Request-Trace-ID")
		receivedPath = req.URL.Path
		receivedQuery = req.URL.RawQuery
		b, _ := io.ReadAll(req.Body)
		receivedBody = string(b)

		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.Header().Set("cf-ray", "123456789-mock") // must be preserved and passed through
		rw.Header().Set("X-Custom-Upstream", "preserved-value")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{"status":"success","data":[{"url":"https://example.com/image.png"}]}`))
	}))
	defer mockUpstream.Close()

	u, _ := url.Parse(mockUpstream.URL)
	item := ProxyItem{
		Endpoint:  mockUpstream.URL,
		AuthKey:   "sk-secret-key-12345",
		Engress:   "",
		Listen:    "127.0.0.1:8001",
		TargetURL: u,
	}

	tr, _ := createTransport("")
	handler, err := BuildProxyHandler(item, tr)
	if err != nil {
		t.Fatalf("BuildProxyHandler failed: %v", err)
	}

	// Send POST with client original auth key and custom header
	requestPayload := `{"model":"sensenova-u1.5-lite","prompt":"a friendly robot"}`
	req := httptest.NewRequest("POST", "/v1/images/generations?stream=true", strings.NewReader(requestPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Authorization", "Bearer client-original-secret")
	req.Header.Set("X-Api-Key", "client-original-secret")
	req.Header.Set("X-Request-Trace-ID", "trace-abc-123")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	// Verify upstream received configured auth key, original client auth keys were excluded
	if receivedAuth != "Bearer sk-secret-key-12345" {
		t.Errorf("expected Authorization 'Bearer sk-secret-key-12345', got '%s'", receivedAuth)
	}
	if receivedXApiKey != "" {
		t.Errorf("expected original X-Api-Key to be excluded, got '%s'", receivedXApiKey)
	}
	if receivedCustomHeader != "trace-abc-123" {
		t.Errorf("expected X-Request-Trace-ID 'trace-abc-123', got '%s'", receivedCustomHeader)
	}
	if receivedPath != "/v1/images/generations" {
		t.Errorf("expected path '/v1/images/generations', got '%s'", receivedPath)
	}
	if receivedQuery != "stream=true" {
		t.Errorf("expected query 'stream=true', got '%s'", receivedQuery)
	}
	if receivedBody != requestPayload {
		t.Errorf("expected body '%s', got '%s'", requestPayload, receivedBody)
	}

	// Verify all upstream headers (including cf-ray, X-Custom-Upstream) are preserved and passed through
	if cfRay := resp.Header.Get("cf-ray"); cfRay != "123456789-mock" {
		t.Errorf("expected cf-ray header to be passed through, got '%s'", cfRay)
	}
	if custom := resp.Header.Get("X-Custom-Upstream"); custom != "preserved-value" {
		t.Errorf("expected X-Custom-Upstream 'preserved-value', got '%s'", custom)
	}
	if accel := resp.Header.Get("X-Accel-Buffering"); accel != "no" {
		t.Errorf("expected X-Accel-Buffering 'no', got '%s'", accel)
	}
	if cors := resp.Header.Get("Access-Control-Allow-Origin"); cors != "http://localhost:3000" {
		t.Errorf("expected Access-Control-Allow-Origin 'http://localhost:3000', got '%s'", cors)
	}
}

func TestOriginalRequestAuthKeyExcluded(t *testing.T) {
	var receivedAuth string
	var receivedXApiKey string
	var receivedApiKey string
	var receivedCustom string

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		receivedAuth = req.Header.Get("Authorization")
		receivedXApiKey = req.Header.Get("X-Api-Key")
		receivedApiKey = req.Header.Get("api-key")
		receivedCustom = req.Header.Get("X-Custom-Client")
		rw.WriteHeader(http.StatusOK)
	}))
	defer mockUpstream.Close()

	u, _ := url.Parse(mockUpstream.URL)
	// Proxy item configured with NO auth key
	item := ProxyItem{
		Endpoint:  mockUpstream.URL,
		AuthKey:   "",
		Listen:    "127.0.0.1:8001",
		TargetURL: u,
	}

	tr, _ := createTransport("")
	handler, err := BuildProxyHandler(item, tr)
	if err != nil {
		t.Fatalf("BuildProxyHandler failed: %v", err)
	}

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer client-secret-token")
	req.Header.Set("X-Api-Key", "client-raw-api-key")
	req.Header.Set("api-key", "client-azure-key")
	req.Header.Set("X-Custom-Client", "keep-this-header")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if receivedAuth != "" {
		t.Errorf("expected Authorization to be excluded from original request, got '%s'", receivedAuth)
	}
	if receivedXApiKey != "" {
		t.Errorf("expected X-Api-Key to be excluded from original request, got '%s'", receivedXApiKey)
	}
	if receivedApiKey != "" {
		t.Errorf("expected api-key to be excluded from original request, got '%s'", receivedApiKey)
	}
	if receivedCustom != "keep-this-header" {
		t.Errorf("expected X-Custom-Client 'keep-this-header', got '%s'", receivedCustom)
	}
}

func TestHealthCheck(t *testing.T) {
	u, _ := url.Parse("https://token.sensenova.cn")
	item := ProxyItem{
		Endpoint:  "https://token.sensenova.cn",
		AuthKey:   "sk-test-key",
		Engress:   "10.88.0.3",
		Listen:    "127.0.0.1:8001",
		TargetURL: u,
	}

	tr, _ := createTransport("")
	handler, err := BuildProxyHandler(item, tr)
	if err != nil {
		t.Fatalf("BuildProxyHandler failed: %v", err)
	}

	req := httptest.NewRequest("GET", "/_health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode health JSON: %v", err)
	}
	if data["status"] != "ok" {
		t.Errorf("expected status 'ok', got %v", data["status"])
	}
	if data["listen"] != "127.0.0.1:8001" {
		t.Errorf("expected listen '127.0.0.1:8001', got %v", data["listen"])
	}
	if data["engress"] != "10.88.0.3" {
		t.Errorf("expected engress '10.88.0.3', got %v", data["engress"])
	}
}

func TestCreateTransportVariations(t *testing.T) {
	// 1. Empty / default
	tr1, err := createTransport("")
	if err != nil || tr1 == nil {
		t.Errorf("createTransport('') error: %v", err)
	}

	// 2. IP address
	tr2, err := createTransport("127.0.0.1")
	if err != nil || tr2 == nil {
		t.Errorf("createTransport('127.0.0.1') error: %v", err)
	}

	// 3. Network interface (lo exists on all machines)
	tr3, err := createTransport("lo")
	if err != nil || tr3 == nil {
		t.Errorf("createTransport('lo') error: %v", err)
	}

	// 4. Proxy URL
	tr4, err := createTransport("http://127.0.0.1:8080")
	if err != nil || tr4 == nil {
		t.Errorf("createTransport('http://127.0.0.1:8080') error: %v", err)
	}

	// 5. Invalid
	_, err = createTransport("invalid_random_string_not_found")
	if err == nil {
		t.Errorf("expected error for invalid egress, got nil")
	}
}

func TestSSEStreamingRealtimeDelivery(t *testing.T) {
	// Mock upstream streaming server
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("expected flusher on test server")
		}
		flusher.Flush()

		for i := 1; i <= 3; i++ {
			time.Sleep(100 * time.Millisecond)
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			flusher.Flush()
		}
	}))
	defer mockUpstream.Close()

	u, _ := url.Parse(mockUpstream.URL)
	item := ProxyItem{
		Endpoint:  mockUpstream.URL,
		TargetURL: u,
		Listen:    "127.0.0.1:0",
	}

	tr, _ := createTransport("")
	handler, err := BuildProxyHandler(item, tr)
	if err != nil {
		t.Fatalf("BuildProxyHandler failed: %v", err)
	}

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Content-Type", "application/json")

	t0 := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 256)
	var arrivalTimes []time.Duration
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			arrivalTimes = append(arrivalTimes, time.Since(t0))
		}
		if err != nil {
			break
		}
	}

	t.Logf("Chunk arrival times: %v", arrivalTimes)
	if len(arrivalTimes) < 3 {
		t.Fatalf("expected at least 3 chunk arrivals, got %d", len(arrivalTimes))
	}
	// Check that the first chunk arrived well before the last chunk (streamed, not buffered until completion)
	span := arrivalTimes[len(arrivalTimes)-1] - arrivalTimes[0]
	if span < 150*time.Millisecond {
		t.Errorf("chunks arrived all at once (span=%v), expected real-time streaming", span)
	}
}
