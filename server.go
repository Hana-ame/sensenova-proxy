package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maskKey provides a safe masked representation of an API key for logs.
func maskKey(k string) string {
	if k == "" {
		return "(none)"
	}
	if len(k) <= 8 {
		return "***"
	}
	return k[:4] + "..." + k[len(k)-4:]
}

// ProxyManager manages all listening proxy instances.
type ProxyManager struct {
	items   []ProxyItem
	servers []*http.Server
}

// NewProxyManager creates a manager for the configured proxy items.
func NewProxyManager(items []ProxyItem) *ProxyManager {
	return &ProxyManager{
		items: items,
	}
}

// Start launches all proxy listeners concurrently and blocks until an interrupt signal is received.
func (m *ProxyManager) Start() error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(m.items))

	for i, item := range m.items {
		var handler http.Handler
		var err error

		// 多源聚合模式（"一拖多"）：配置了 sources 时，客户端一个 baseURL
		// 按序 failover 到多个 opencode 源，遇到 exceed 冷却换源。
		if len(item.SourceItems) > 0 {
			handler, err = buildMultiHandler(item.SourceItems)
		} else {
			var tr *http.Transport
			tr, err = createTransport(item.Engress)
			if err == nil {
				handler, err = BuildProxyHandler(item, tr)
			}
		}
		if err != nil {
			return fmt.Errorf("proxy #%d (%s): failed to build handler: %w", i+1, item.Listen, err)
		}

		// opencode zen streams can legitimately idle for a while; give them a
		// longer read/write budget than the default 300s.
		readTO, writeTO := 300*time.Second, 300*time.Second
		if item.IsOpencode() || len(item.SourceItems) > 0 {
			readTO, writeTO = 600*time.Second, 600*time.Second
		}

		srv := &http.Server{
			Addr:           item.Listen,
			Handler:        handler,
			ReadTimeout:    readTO,
			WriteTimeout:   writeTO,
			IdleTimeout:    120 * time.Second,
			MaxHeaderBytes: 1 << 20, // 1MB
		}
		m.servers = append(m.servers, srv)

		egressDesc := item.Engress
		if strings.TrimSpace(egressDesc) == "" {
			egressDesc = "(default route)"
		}

		toDesc := "timeout: none"
		if to := item.TimeoutDuration(); to > 0 {
			toDesc = fmt.Sprintf("timeout: %v", to)
		}
		if fb := item.FirstByteDuration(); fb > 0 {
			toDesc += fmt.Sprintf(", first_byte: %v", fb)
		}
		// 配置值被 90s 硬上限钳过就明说，否则用户以为自己设了 10 分钟。
		if item.TimeoutSecs > maxWaitBudget.Seconds() {
			toDesc += fmt.Sprintf(" (configured %.0fs clamped to %v)", item.TimeoutSecs, maxWaitBudget)
		}
		if item.FirstByteSecs > maxWaitBudget.Seconds() {
			toDesc += fmt.Sprintf(" (first_byte configured %.0fs clamped to %v)", item.FirstByteSecs, maxWaitBudget)
		}

		sessDesc := fmt.Sprintf("session: %s", item.SessionFallbackMode())
		if item.SessionHeader != "" {
			sessDesc += fmt.Sprintf(" (client header: %s)", item.SessionHeader)
		}

		log.Printf("🚀 [Instance #%d] Listening on http://%s -> %s [provider: %s, egress: %s, auth: %s, %s, %s]",
			i+1, item.Listen, item.Endpoint, item.Provider, egressDesc, maskKey(item.AuthKey), toDesc, sessDesc)

		wg.Add(1)
		go func(s *http.Server, addr string) {
			defer wg.Done()
			if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("❌ Server on %s stopped with error: %v", addr, err)
				errChan <- err
			}
		}(srv, item.Listen)
	}

	// Listen for termination signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	select {
	case sig := <-sigChan:
		log.Printf("\nReceived signal '%v', initiating graceful shutdown...", sig)
	case err := <-errChan:
		log.Printf("Fatal error on proxy listener: %v", err)
	}

	return m.Shutdown()
}

// Shutdown gracefully stops all running proxy servers.
func (m *ProxyManager) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, srv := range m.servers {
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			_ = s.Shutdown(ctx)
		}(srv)
	}
	wg.Wait()
	log.Println("All proxy instances stopped successfully.")
	return nil
}
