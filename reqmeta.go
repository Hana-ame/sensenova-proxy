package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
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
