package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- session 标志 ---------------------------------------------------------

func TestResolveSessionMarkerStableAcrossRequests(t *testing.T) {
	// 核心回归：同一客户端连续多次请求必须拿到同一个 session。早先这里每
	// 请求调 mintSessionID()，把上游按 session 计的免费配额逐请求切碎了。
	item := ProxyItem{Listen: "127.0.0.1:3000", SessionFallback: "client"}
	first := resolveSessionMarker(item, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	for i := 0; i < 5; i++ {
		got := resolveSessionMarker(item, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
		if got != first {
			t.Fatalf("session changed between requests: %q -> %q", first, got)
		}
	}
	if !sessionIDRe.MatchString(first) {
		t.Fatalf("session %q does not match the gateway format", first)
	}
}

func TestResolveSessionMarkerDiffersPerClient(t *testing.T) {
	item := ProxyItem{Listen: "127.0.0.1:3000", SessionFallback: "client"}
	a := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	a.RemoteAddr = "10.0.0.1:5555"
	b := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	b.RemoteAddr = "10.0.0.2:5555"
	if resolveSessionMarker(item, a) == resolveSessionMarker(item, b) {
		t.Fatal("two different clients shared one session marker")
	}
}

func TestResolveSessionMarkerUsesClientHeader(t *testing.T) {
	// 客户端自带会话标识时必须原样沿用——这是最准的来源。
	item := ProxyItem{Listen: "127.0.0.1:3000", SessionFallback: "client"}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-Id", "ses_000000000000abcdefghijklmn")
	if got := resolveSessionMarker(item, req); got != "ses_000000000000abcdefghijklmn" {
		t.Fatalf("client session not preserved: %q", got)
	}
}

func TestResolveSessionMarkerCustomHeader(t *testing.T) {
	item := ProxyItem{Listen: "127.0.0.1:3000", SessionHeader: "X-Conv-Id"}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Conv-Id", "my-conversation-7")
	got := resolveSessionMarker(item, req)
	if !sessionIDRe.MatchString(got) {
		t.Fatalf("derived session %q is not gateway-shaped", got)
	}
	// 同一个自定义头值必须稳定映射到同一个 session。
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req2.Header.Set("X-Conv-Id", "my-conversation-7")
	if got2 := resolveSessionMarker(item, req2); got2 != got {
		t.Fatalf("unstable mapping: %q vs %q", got, got2)
	}
}

func TestResolveSessionMarkerRejectsUnsafeHeaderValue(t *testing.T) {
	// 客户端可能塞带换行/非 ASCII 的值，直接透传会让 header 非法。
	item := ProxyItem{Listen: "127.0.0.1:3000"}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-Id", "bad value\r\nInjected: yes")
	got := resolveSessionMarker(item, req)
	if !sessionIDRe.MatchString(got) {
		t.Fatalf("unsafe value was not sanitized: %q", got)
	}
	if strings.ContainsAny(got, " \r\n\t") {
		t.Fatalf("sanitized session still contains whitespace: %q", got)
	}
}

func TestResolveSessionMarkerInstanceFallback(t *testing.T) {
	item := ProxyItem{Listen: "127.0.0.1:3000", SessionFallback: "instance"}
	a := resolveSessionMarker(item, httptest.NewRequest(http.MethodPost, "/v1/x", nil))
	b := resolveSessionMarker(item, httptest.NewRequest(http.MethodPost, "/v1/y", nil))
	if a != b {
		t.Fatalf("instance fallback not stable: %q vs %q", a, b)
	}
}

func TestResolveSessionMarkerRequestFallback(t *testing.T) {
	item := ProxyItem{Listen: "127.0.0.1:3000", SessionFallback: "request"}
	a := resolveSessionMarker(item, httptest.NewRequest(http.MethodPost, "/v1/x", nil))
	b := resolveSessionMarker(item, httptest.NewRequest(http.MethodPost, "/v1/x", nil))
	if a == b {
		t.Fatal("request fallback should mint a new session each time")
	}
}

func TestSessionFallbackModeNormalization(t *testing.T) {
	cases := map[string]string{
		"":         "client",
		"client":   "client",
		"instance": "instance",
		"fixed":    "instance",
		"request":  "request",
		"weird":    "client",
	}
	for in, want := range cases {
		pi := ProxyItem{SessionFallback: in}
		if got := pi.SessionFallbackMode(); got != want {
			t.Errorf("SessionFallbackMode(%q) = %q, want %q", in, got, want)
		}
	}
}
func TestTimeoutUnderCapUnchanged(t *testing.T) {
	p := ProxyItem{Provider: "opencode", TimeoutSecs: 45, FirstByteSecs: 20}
	if got := p.TimeoutDuration(); got != 45*time.Second {
		t.Errorf("TimeoutDuration = %v, want 45s", got)
	}
	if got := p.FirstByteDuration(); got != 20*time.Second {
		t.Errorf("FirstByteDuration = %v, want 20s", got)
	}
}

func TestHardCapLeavesRoomForCloudflare(t *testing.T) {
	// 上限必须严格小于 Cloudflare 的 100s，否则 504 永远送不出去。
	if maxWaitBudgetCF >= 100*time.Second {
		t.Fatalf("maxWaitBudgetCF = %v, must stay below Cloudflare's 100s", maxWaitBudgetCF)
	}
}

// --- 请求元数据与日志 -----------------------------------------------------

func TestNewRIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := newRID()
		if len(id) != 8 {
			t.Fatalf("rid %q has length %d, want 8", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate rid after %d draws: %q", i, id)
		}
		seen[id] = true
	}
}

func TestMetaLogfOmitsEmptyFields(t *testing.T) {
	var buf strings.Builder
	restore := captureLog(&buf)
	m := &reqMeta{rid: "abc12345", listen: "127.0.0.1:3000", model: "big-pickle"}
	m.logf("done", "POST /v1/chat/completions -> 200 took=1.2s")
	restore()

	line := buf.String()
	for _, want := range []string{"rid=abc12345", "listen=127.0.0.1:3000", "model=big-pickle", "event=done", "took=1.2s"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q: %s", want, line)
		}
	}
	// provider/client 没设置就不该出现空占位。
	if strings.Contains(line, "provider= ") || strings.Contains(line, "client= ") {
		t.Errorf("empty fields leaked into log line: %s", line)
	}
}

func TestMetaCarriesThroughContext(t *testing.T) {
	m := &reqMeta{rid: "deadbeef", listen: "127.0.0.1:3000", sess: "ses_x"}
	req := withReqMeta(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), m)
	got := metaFrom(req.Context())
	if got.rid != "deadbeef" || got.sess != "ses_x" {
		t.Fatalf("meta lost in context: %+v", got)
	}
}

func TestMetaFromEmptyContextDoesNotPanic(t *testing.T) {
	// SSE pump 等路径可能拿不到 meta，必须兜住而不是 panic。
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	m := metaFrom(req.Context())
	if m == nil || m.rid == "" {
		t.Fatalf("expected a usable placeholder meta, got %+v", m)
	}
	m.logf("noop", "should not panic")
}

func TestRound1TrimsPrecision(t *testing.T) {
	if got := round1(1234567 * time.Microsecond); got != 1200*time.Millisecond {
		t.Errorf("round1 = %v, want 1.2s", got)
	}
}

// captureLog 把 log 输出重定向到 buf，返回恢复函数。
func captureLog(buf *strings.Builder) func() {
	var out io.Writer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	oldPrefix := log.Prefix()
	out = buf
	log.SetOutput(out)
	log.SetFlags(0)
	log.SetPrefix("")
	return func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	}
}

func TestSessionMarkerLengthIsEnforced(t *testing.T) {
	// 实测教训：ses_ + 12 hex + 15 个字母数字（多一个字符）会被上游回
	// 403 FreeTierError，而只查字符集的旧实现会把它原样透传。所以长度也
	// 必须校验，长度不对就派生一个新的合法值。
	tooLong := "ses_000000000000mytestsession01" // 27 字符尾段
	if isValidSessionMarker(tooLong) {
		t.Error("over-length session marker accepted as valid")
	}
	got := sanitizeSessionMarker(tooLong)
	if got == tooLong {
		t.Error("over-length value was forwarded unchanged")
	}
	if !isValidSessionMarker(got) {
		t.Errorf("derived marker is still invalid: %q", got)
	}
}

func TestSessionMarkerHexPrefixIsEnforced(t *testing.T) {
	// 前 12 位必须是小写 hex，大写或非 hex 都要重派生。
	bad := "ses_00000000000ZZZZZZZZZZZZZZ"
	if isValidSessionMarker(bad) {
		t.Error("non-hex prefix accepted as valid")
	}
	if got := sanitizeSessionMarker(bad); !isValidSessionMarker(got) {
		t.Errorf("derived marker invalid: %q", got)
	}
}

func TestSessionMarkerAcceptsWellFormedValue(t *testing.T) {
	good := "ses_000000000000mytestsession0"
	if !isValidSessionMarker(good) {
		t.Fatalf("well-formed marker rejected: %q", good)
	}
	if got := sanitizeSessionMarker(good); got != good {
		t.Errorf("well-formed marker was rewritten: %q", got)
	}
}

func TestTruncCauseRecordedOnce(t *testing.T) {
	m := &reqMeta{rid: "x"}
	if _, ok := m.truncCause(); ok {
		t.Error("fresh meta reported a truncation cause")
	}
	m.setTruncReason(nil)
	if _, ok := m.truncCause(); ok {
		t.Error("nil error was recorded as a cause")
	}
	m.setTruncReason(context.DeadlineExceeded)
	m.setTruncReason(io.ErrUnexpectedEOF) // 第二个应被忽略
	cause, ok := m.truncCause()
	if !ok || !errors.Is(cause, context.DeadlineExceeded) {
		t.Errorf("truncCause = (%v, %v), want the first error", cause, ok)
	}
}

func TestCtxWatchBodyRecordsReadError(t *testing.T) {
	m := &reqMeta{rid: "x"}
	src := io.NopCloser(&errReader{data: []byte("partial"), err: context.DeadlineExceeded})
	resp := &http.Response{Body: src, StatusCode: 200}
	wrapStreamWithContextWatcher(resp, m, context.Background())

	buf := make([]byte, 64)
	// 第一次读拿到数据（err 为 nil），第二次读才拿到源错误。
	resp.Body.Read(buf)
	_, err := resp.Body.Read(buf)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read err = %v, want DeadlineExceeded surfaced", err)
	}
	if cause, ok := m.truncCause(); !ok || !errors.Is(cause, context.DeadlineExceeded) {
		t.Errorf("truncCause = (%v, %v), want DeadlineExceeded", cause, ok)
	}
}

func TestCtxWatchBodyUsesCtxNotErrorType(t *testing.T) {
	// 实测：预算到期时 Read 拿到的是 context.Canceled，而不是 DeadlineExceeded
	//（httputil 把它当"客户端走了"静默 return）。所以原因必须从 ctx 读，
	// 否则会把"代理预算到期"误标成"客户端先撤"。
	m := &reqMeta{rid: "x"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond) // 确保 ctx 已到期

	src := io.NopCloser(&errReader{data: []byte("x"), err: context.Canceled})
	resp := &http.Response{Body: src, StatusCode: 200}
	wrapStreamWithContextWatcher(resp, m, ctx)
	resp.Body.Read(make([]byte, 8))
	resp.Body.Read(make([]byte, 8))

	cause, ok := m.truncCause()
	if !ok || !errors.Is(cause, context.DeadlineExceeded) {
		t.Errorf("truncCause = (%v, %v), want DeadlineExceeded taken from ctx", cause, ok)
	}
}

func TestCtxWatchBodyFallsBackToReadError(t *testing.T) {
	// ctx 还活着但源坏了：原因只能取 Read 的错误。
	m := &reqMeta{rid: "x"}
	src := io.NopCloser(&errReader{data: []byte("x"), err: io.ErrUnexpectedEOF})
	resp := &http.Response{Body: src, StatusCode: 200}
	wrapStreamWithContextWatcher(resp, m, context.Background())
	resp.Body.Read(make([]byte, 8))
	resp.Body.Read(make([]byte, 8))
	if cause, ok := m.truncCause(); !ok || !errors.Is(cause, io.ErrUnexpectedEOF) {
		t.Errorf("truncCause = (%v, %v), want the read error", cause, ok)
	}
}

func TestCtxWatchBodyPassesThroughCleanReads(t *testing.T) {
	m := &reqMeta{rid: "x"}
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("hello")), StatusCode: 200}
	wrapStreamWithContextWatcher(resp, m, context.Background())
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "hello" {
		t.Errorf("body = %q, want %q", got, "hello")
	}
	if _, ok := m.truncCause(); ok {
		t.Error("clean body recorded a truncation")
	}
}

func TestIsStreamingResponse(t *testing.T) {
	sse := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader("")), StatusCode: 200, ContentLength: -1}
	if !isStreamingResponse(sse) {
		t.Error("SSE response not detected as streaming")
	}
	chunked := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")),
		StatusCode: 200, ContentLength: -1}
	if !isStreamingResponse(chunked) {
		t.Error("chunked 2xx not detected as streaming")
	}
	fixed := &http.Response{Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader("{}")), StatusCode: 200, ContentLength: 2}
	if isStreamingResponse(fixed) {
		t.Error("fixed-length JSON wrongly detected as streaming")
	}
	if isStreamingResponse(nil) {
		t.Error("nil response reported as streaming")
	}
}

// errReader 先吐出 data 再返回 err，用来模拟"流到一半被掐断"。
type errReader struct {
	data []byte
	err  error
	done bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	n := copy(p, r.data)
	return n, nil
}

func TestIsTruncatedBodyErr(t *testing.T) {
	// 客户端中途断开：body 读不完整，转发上游没有意义。
	for _, e := range []error{
		io.ErrUnexpectedEOF,
		fmt.Errorf("read tcp 1.2.3.4:1->5.6.7.8:2: unexpected EOF"),
		fmt.Errorf("read tcp: connection reset by peer"),
		fmt.Errorf("write: broken pipe"),
	} {
		if !isTruncatedBodyErr(e) {
			t.Errorf("isTruncatedBodyErr(%v) = false, want true", e)
		}
	}
	// 解析类错误：body 本身是完整的，可以照原样转发。
	for _, e := range []error{
		nil,
		fmt.Errorf("invalid JSON body: unexpected token"),
		fmt.Errorf("json: cannot unmarshal string into Go value of type int"),
	} {
		if isTruncatedBodyErr(e) {
			t.Errorf("isTruncatedBodyErr(%v) = true, want false", e)
		}
	}
}

func TestBodyTruncatedRecordedOnce(t *testing.T) {
	m := &reqMeta{rid: "x"}
	if m.truncatedBody() != nil {
		t.Error("fresh meta reported a truncated body")
	}
	m.setBodyTruncated(io.ErrUnexpectedEOF)
	m.setBodyTruncated(fmt.Errorf("other"))
	if got := m.truncatedBody(); !errors.Is(got, io.ErrUnexpectedEOF) {
		t.Errorf("truncatedBody = %v, want the first error", got)
	}
}
