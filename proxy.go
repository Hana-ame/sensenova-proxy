package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
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

// responseRecorder captures the status code written to ResponseWriter for logging,
// and ensures immediate unbuffered flushing on every single write.
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (rec *responseRecorder) WriteHeader(code int) {
	rec.statusCode = code
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *responseRecorder) Write(b []byte) (int, error) {
	n, err := rec.ResponseWriter.Write(b)
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

		// Prevent upstream gzip/brotli buffering for streaming responses:
		// Upstream gzip compressors buffer small SSE chunks in 4-32KB blocks, delaying token delivery.
		// Forcing identity encoding ensures unbuffered real-time SSE token delivery.
		req.Header.Del("Accept-Encoding")
		req.Header.Set("Accept-Encoding", "identity")

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

	// ModifyResponse injects CORS headers and ensures anti-buffering headers while passing all upstream headers intact
	proxy.ModifyResponse = func(resp *http.Response) error {
		// Pass all headers from upstream intact (never delete cf-* or any other headers).
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

	// ErrorHandler provides clear JSON errors on network failures
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		log.Printf("[%s -> egress:%s] Upstream failure: %v", item.Listen, item.Engress, err)
		setCORSHeaders(rw, req)
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(rw).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":     "bad_gateway",
				"message":  fmt.Sprintf("proxy failed to reach upstream '%s': %v", item.Endpoint, err),
				"listen":   item.Listen,
				"engress":  item.Engress,
			},
		})
	}

	// Wrap in top-level handler for logging, healthcheck, and OPTIONS preflight
	handler := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		start := time.Now()

		// 1. Diagnostic / health check endpoint
		if req.URL.Path == "/_health" || req.URL.Path == "/healthz" {
			setCORSHeaders(rw, req)
			rw.Header().Set("Content-Type", "application/json; charset=utf-8")
			rw.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(rw).Encode(map[string]interface{}{
				"status":      "ok",
				"listen":      item.Listen,
				"endpoint":    item.Endpoint,
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

		// 3. Proxy passthrough with full-duplex streaming and zero-buffering
		rc := http.NewResponseController(rw)
		_ = rc.EnableFullDuplex()

		rec := &responseRecorder{
			ResponseWriter: rw,
			statusCode:     http.StatusOK,
		}

		proxy.ServeHTTP(rec, req)

		duration := time.Since(start)
		log.Printf("[%s -> egress:%s] %s %s -> %d (%v)",
			item.Listen,
			item.Engress,
			req.Method,
			req.URL.Path,
			rec.statusCode,
			duration,
		)
	})

	return handler, nil
}
