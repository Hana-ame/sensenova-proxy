package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// request metadata & structured logging
// ---------------------------------------------------------------------------
//
// 以前一次请求相关的日志散在各处，格式各不相同，而且 opencode 内部那几行
// 完全不带 listen 端口——多实例并行时根本分不清是哪条 lane 在说话。这里给
// 每个请求发一个短 rid，通过 context 传下去，所有日志行统一成：
//
//	rid=3f9a2b1c listen=127.0.0.1:3000 model=big-pickle event=first_sse: ...
//
// 这样一次请求的所有行可以 grep 'rid=3f9a2b1c' 全部捞出来，多实例也不会串。
//
// 注意 rid 和 session 标志是两回事，不要混用：
//   - rid      每个请求唯一，只为串日志，用完即弃。
//   - session  稳定标识，同一客户端会话的所有请求共用同一个值，绝不每次重掷，
//     因为上游免费配额是按 session 计的。

type reqMetaKey struct{}

// reqMeta carries the per-request facts that log lines need. It is attached to
// the request context so goroutines (notably the SSE pump) can log with the
// same identity as the handler that started them.
type reqMeta struct {
	rid      string // short unique id for this request, used to correlate lines
	listen   string // instance listen address
	engress  string // egress description (may be empty)
	provider string
	clientIP string
	model    string // filled in by the opencode body rewrite
	sess     string // stable session marker

	// truncReason 记录流式响应被掐断的原因。httputil.ReverseProxy 在
	// body copy 中读到非 EOF 错误时，只往自己的 ErrorLog 打一行就直接
	// 返回——错误不会经过 ResponseWriter.Write，所以包装响应体是抓不到的
	// （实测 90s 预算到期时 Write 一次错误都没收到）。而 ErrorLog 是实例
	// 级的，拿不到 per-request 上下文。折中办法：由 ErrorLog 把原因记到
	// 一个按请求路由的槽位，handler 收尾时读出来补日志。
	truncMu     sync.Mutex
	truncReason error
}

// setTruncReason 记录流被掐断的原因（只记第一个）。
func (m *reqMeta) setTruncReason(err error) {
	if err == nil {
		return
	}
	m.truncMu.Lock()
	if m.truncReason == nil {
		m.truncReason = err
	}
	m.truncMu.Unlock()
}

// logTruncated 在流被掐断的当刻打一行日志（不等收尾）。SSE 响应头已发出，
// 状态码改不了，客户端只看到 200 然后流突然断掉——这行是唯一的解释。
func (m *reqMeta) logTruncated(method, path string, took time.Duration) {
	cause, ok := m.truncCause()
	if !ok {
		return
	}
	causeName := "proxy_timeout_budget_expired"
	if errors.Is(cause, context.Canceled) {
		causeName = "client_gone"
	}
	m.logf("stream_truncated",
		"method=%s path=%s took=%s cause=%s err=%v note=headers_already_sent_client_saw_200_then_stream_ended",
		method, path, took, causeName, cause)
}

// truncCause 返回流被掐断的原因与是否记录过。
func (m *reqMeta) truncCause() (error, bool) {
	m.truncMu.Lock()
	defer m.truncMu.Unlock()
	return m.truncReason, m.truncReason != nil
}

func withReqMeta(req *http.Request, m *reqMeta) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), reqMetaKey{}, m))
}

func metaFrom(ctx context.Context) *reqMeta {
	if m, ok := ctx.Value(reqMetaKey{}).(*reqMeta); ok {
		return m
	}
	return &reqMeta{rid: "-"}
}

// logf writes one structured line. Empty fields are omitted rather than printed
// as blank placeholders, so the output stays readable when a lane has no egress.
func (m *reqMeta) logf(event, format string, args ...interface{}) {
	var b strings.Builder
	b.WriteString("rid=")
	b.WriteString(m.rid)
	b.WriteString(" listen=")
	b.WriteString(m.listen)
	if m.model != "" {
		b.WriteString(" model=")
		b.WriteString(m.model)
	}
	if m.provider != "" {
		b.WriteString(" provider=")
		b.WriteString(m.provider)
	}
	if m.clientIP != "" {
		b.WriteString(" client=")
		b.WriteString(m.clientIP)
	}
	b.WriteString(" event=")
	b.WriteString(event)
	b.WriteString(": ")
	if len(args) == 0 {
		b.WriteString(format)
	} else {
		b.WriteString(fmt.Sprintf(format, args...))
	}
	log.Print(b.String())
}

// ridCounter backs the fallback request id, used only when crypto/rand fails.
var ridCounter uint64

// newRID mints a short per-request id: 4 bytes of crypto randomness in
// lowercase hex. Short on purpose — these lines get read by humans, and a
// 32-char id pushes the actual message off the right edge of the terminal.
func newRID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		binary.BigEndian.PutUint32(b[:], uint32(atomic.AddUint64(&ridCounter, 1)))
	}
	const hexdig = "0123456789abcdef"
	out := make([]byte, 8)
	for i, c := range b {
		out[i*2] = hexdig[c>>4]
		out[i*2+1] = hexdig[c&0x0f]
	}
	return string(out)
}
