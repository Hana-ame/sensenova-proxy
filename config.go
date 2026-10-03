package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ProxyItem defines a single proxy mapping instance.
// In the config JSON list, each item has four core properties:
//   - endpoint: target upstream URL (e.g. "https://token.sensenova.cn")
//   - Authkey: SenseNova API key (e.g. "sk-...")
//   - engress: outbound network binding (IPv4/IPv6 IP, interface name, or upstream proxy URL)
//   - listen: local listening address (e.g. "127.0.0.1:8001" or ":8001")
//   - provider (optional): sensenova | openai | opencode | passthrough.
//     Auto-detected from the endpoint host when omitted (opencode.ai -> opencode).
//   - custom_models (optional, opencode lane): extra model ids injected into
//     the /v1/models response so clients see them as available (even if the
//     upstream does not actually serve them).
//   - models_mode (optional): "append" (default) merges custom_models into the
//     upstream list; "replace" serves only custom_models.
type ProxyItem struct {
	Endpoint     string   `json:"endpoint"`
	AuthKey      string   `json:"Authkey"`
	Engress      string   `json:"engress"`
	Listen       string   `json:"listen"`
	Provider     string   `json:"provider"`
	CustomModels []string `json:"custom_models"`
	ModelsMode   string   `json:"models_mode"`

	// TimeoutSecs 上游总超时（秒）：从请求发出到响应结束的整段时间。
	// 0 = 不主动超时（保留 v1.3.0 及之前的行为，只受 http.Server 的
	// ReadTimeout/WriteTimeout 约束）。
	TimeoutSecs float64 `json:"timeout"`

	// FirstByteSecs 等上游响应头的超时（秒），即"首字节"超时。流式响应一旦
	// 拿到响应头就不再受它约束，所以它只治"上游排队/连不上"，不会截断正常
	// 的长流。0 = 不限制。
	FirstByteSecs float64 `json:"first_byte_timeout"`

	// SessionHeader 指定从客户端请求的哪个 header 取会话标识（opencode lane）。
	// 取到的值会作为 X-Session-Id / X-Session-Affinity / X-Opencode-Session
	// 一起发给上游，使同一会话的多次请求命中同一个上游 session。
	// 留空 = 走 SessionFallback 策略。
	SessionHeader string `json:"session_header"`

	// SessionFallback 决定客户端没带会话标识时怎么办：
	//   "client"（默认）——按客户端 IP 派生，同一客户端会话内稳定
	//   "instance"      ——整个实例共用一个固定值
	//   "request"       ——每个请求一个新 session（旧行为，会把上游按 session
	//                     计的免费配额切碎，一般别用）
	SessionFallback string `json:"session_fallback"`

	// Sources 多源聚合（"一拖多"）：非空时该实例为聚合模式——
	// 客户端只配一个 baseURL，请求按序 failover 到这些 opencode 源
	// （每源固定 endpoint + net 出口，遇 exceed 冷却到 UTC 午夜自动换源）。
	SourceItems []openSource `json:"sources"`

	// Parsed upstream target URL
	TargetURL *url.URL `json:"-"`
}

// UnmarshalJSON supports case-insensitive and alias key names in the JSON input.
func (p *ProxyItem) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	getStr := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := raw[k]; ok && v != nil {
				switch val := v.(type) {
				case string:
					return strings.TrimSpace(val)
				case float64:
					return strconv.FormatInt(int64(val), 10)
				}
			}
		}
		return ""
	}

	getStrSlice := func(keys ...string) []string {
		for _, k := range keys {
			if v, ok := raw[k]; ok && v != nil {
				switch val := v.(type) {
				case []interface{}:
					out := make([]string, 0, len(val))
					for _, s := range val {
						if str, ok := s.(string); ok && strings.TrimSpace(str) != "" {
							out = append(out, strings.TrimSpace(str))
						}
					}
					if len(out) > 0 {
						return out
					}
				case string:
					// tolerate a comma-separated inline list
					var out []string
					for _, part := range strings.Split(val, ",") {
						if part = strings.TrimSpace(part); part != "" {
							out = append(out, part)
						}
					}
					if len(out) > 0 {
						return out
					}
				}
			}
		}
		return nil
	}

	getNum := func(keys ...string) float64 {
		for _, k := range keys {
			if v, ok := raw[k]; ok && v != nil {
				switch val := v.(type) {
				case float64:
					return val
				case string:
					if f, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
						return f
					}
				}
			}
		}
		return 0
	}

	p.Endpoint = getStr("endpoint", "Endpoint", "target", "upstream", "url")
	p.AuthKey = getStr("Authkey", "authkey", "AuthKey", "auth_key", "apiKey", "api_key", "key", "token")
	p.Engress = getStr("engress", "egress", "Engress", "Egress", "outbound", "source_ip", "interface")
	p.Listen = getStr("listen", "Listen", "addr", "address", "bind", "port")
	p.Provider = getStr("provider", "Provider", "mode", "upstream_type", "upstreamType", "protocol")
	p.CustomModels = getStrSlice("custom_models", "customModels", "CustomModels", "fake_models", "extra_models")
	p.ModelsMode = strings.ToLower(getStr("models_mode", "modelsMode", "mode_models", "model_list_mode"))
	p.TimeoutSecs = getNum("timeout", "Timeout", "timeout_sec", "timeout_secs", "upstream_timeout")
	p.FirstByteSecs = getNum("first_byte_timeout", "firstByteTimeout", "first_byte", "header_timeout", "response_header_timeout")
	p.SessionHeader = getStr("session_header", "sessionHeader", "SessionHeader", "session_id_header", "session_from")
	p.SessionFallback = strings.ToLower(getStr("session_fallback", "sessionFallback", "session_fallback_mode"))

	// sources: 多源聚合的源列表（每个源可带 name/endpoint/net/engress）
	if v, ok := raw["sources"]; ok && v != nil {
		switch val := v.(type) {
		case []interface{}:
			for _, s := range val {
				if sm, ok := s.(map[string]interface{}); ok {
					src := openSource{}
					src.Name = strAny(sm["name"])
					src.Endpoint = strAny(sm["endpoint"])
					src.Net = strAny(sm["net"])
					src.Engress = strAny(sm["engress"])
					if src.Endpoint != "" {
						p.SourceItems = append(p.SourceItems, src)
					}
				}
			}
		}
	}

	return nil
}

// strAny 辅助：把 interface{} 安全转 string（空返回 ""）。
func strAny(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// maxWaitBudget 是代理允许等上游的硬上限，同时是**默认值**。
//
// Cloudflare 默认 100s 拿不到源站首字节就断连接并回 524 —— 客户端看到的是
// 一个没有任何错误细节的 HTML 页面，代理侧日志里也什么都看不到（CF 早就
// 把连接掐了）。所以代理必须抢在 CF 之前自己收尾：90s 上限留了 10s 余量，
// 让 504 + JSON 错误能真正送到客户端。
//
// 无论配置写成多少，都会被钳到这个值。**没配也按这个值算**，不是"不配就不
// 限制"：实测日志里那些 `client_gone ... took=2m3s` 就是没配任何超时的产物
// —— 代理陪着等到客户端自己超时断开，只能被动记 502。默认值必须是安全值。
//
// 需要更长等待的场景应该调客户端超时或换更快的上游，而不是放宽这个上限。
// 真要显式关掉，用 timeout: -1 / first_byte_timeout: -1（见下面两个方法）。
const maxWaitBudget = 90 * time.Second

// SessionFallbackMode 返回归一化后的会话回退策略，默认 "client"。
func (p *ProxyItem) SessionFallbackMode() string {
	switch p.SessionFallback {
	case "instance", "fixed", "global":
		return "instance"
	case "request", "per_request", "new":
		return "request"
	default:
		return "client"
	}
}

// TimeoutDuration 返回上游总超时。
//
//	未配置（0）-> maxWaitBudget（90s）：这是默认值，不是"不限制"
//	负数       -> 0，显式关闭总超时（仍受 first_byte_timeout 约束）
//	正数       -> 钳到 maxWaitBudget，防止 Cloudflare 先掐断连接（524）
func (p *ProxyItem) TimeoutDuration() time.Duration {
	if p.TimeoutSecs < 0 {
		return 0
	}
	if p.TimeoutSecs == 0 {
		return maxWaitBudget
	}
	return clampBudget(time.Duration(p.TimeoutSecs * float64(time.Second)))
}

// FirstByteDuration 返回等待上游响应头的超时，语义同 TimeoutDuration：
// 未配置走 maxWaitBudget 默认值，负数显式关闭，正数钳到上限。首字节超时是防
// Cloudflare 524 的主力闸——524 的成因就是"源站迟迟没有响应头"。
func (p *ProxyItem) FirstByteDuration() time.Duration {
	if p.FirstByteSecs < 0 {
		return 0
	}
	if p.FirstByteSecs == 0 {
		return maxWaitBudget
	}
	return clampBudget(time.Duration(p.FirstByteSecs * float64(time.Second)))
}

// clampBudget 把超出上限的等待预算压到 maxWaitBudget。首字节超时尤其需要：
// 它就是防 Cloudflare 524 的那道闸，写大了等于没写。
func clampBudget(d time.Duration) time.Duration {
	if d > maxWaitBudget {
		return maxWaitBudget
	}
	return d
}

// providerIsValid reports whether a normalized provider value is known.
func providerIsValid(p string) bool {
	switch p {
	case "", "sensenova", "openai", "opencode", "passthrough":
		return true
	}
	return false
}

// Validate normalizes and checks each configuration item.
func (p *ProxyItem) Validate(index int) error {
	// Endpoint validation & normalization
	if p.Endpoint == "" {
		p.Endpoint = "https://token.sensenova.cn"
	}
	if !strings.HasPrefix(p.Endpoint, "http://") && !strings.HasPrefix(p.Endpoint, "https://") {
		p.Endpoint = "https://" + p.Endpoint
	}
	p.Endpoint = strings.TrimRight(p.Endpoint, "/")

	u, err := url.Parse(p.Endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("config item[%d]: invalid endpoint '%s': %w", index, p.Endpoint, err)
	}
	p.TargetURL = u

	// Provider normalization & validation
	p.Provider = strings.ToLower(strings.TrimSpace(p.Provider))
	if !providerIsValid(p.Provider) {
		return fmt.Errorf("config item[%d]: unknown provider '%s' (want sensenova|openai|opencode|passthrough)", index, p.Provider)
	}
	if p.Provider == "" {
		host := strings.ToLower(u.Hostname())
		if host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai") {
			p.Provider = "opencode"
		} else {
			p.Provider = "sensenova"
		}
	}
	if p.Provider == "opencode" && p.AuthKey == "" {
		p.AuthKey = opencodeDefaultKey
	}
	if p.ModelsMode == "" {
		p.ModelsMode = "append"
	}
	if p.ModelsMode != "append" && p.ModelsMode != "replace" {
		p.ModelsMode = "append"
	}
	// 负超时没有意义，归零即"不限制"
	if p.TimeoutSecs < 0 {
		p.TimeoutSecs = 0
	}
	if p.FirstByteSecs < 0 {
		p.FirstByteSecs = 0
	}

	// Listen address validation & normalization
	if p.Listen == "" {
		return fmt.Errorf("config item[%d]: listen address is required (e.g. '127.0.0.1:8001')", index)
	}
	if !strings.Contains(p.Listen, ":") {
		// e.g. "8001" -> "127.0.0.1:8001"
		p.Listen = "127.0.0.1:" + p.Listen
	}

	return nil
}

// LoadConfig reads and parses the JSON configuration file.
// Supports both a JSON array of items:
//
//	[ {"endpoint": "...", "Authkey": "...", "engress": "...", "listen": "..."}, ... ]
//
// and a JSON object containing a list:
//
//	{ "proxies": [ ... ] }
func LoadConfig(path string) ([]ProxyItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var items []ProxyItem
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, fmt.Errorf("parse JSON array: %w", err)
		}
	} else if strings.HasPrefix(trimmed, "{") {
		var wrapper struct {
			Proxies []ProxyItem `json:"proxies"`
			Servers []ProxyItem `json:"servers"`
			List    []ProxyItem `json:"list"`
			Items   []ProxyItem `json:"items"`
		}
		if err := json.Unmarshal(data, &wrapper); err != nil {
			return nil, fmt.Errorf("parse JSON object: %w", err)
		}
		if len(wrapper.Proxies) > 0 {
			items = wrapper.Proxies
		} else if len(wrapper.Servers) > 0 {
			items = wrapper.Servers
		} else if len(wrapper.List) > 0 {
			items = wrapper.List
		} else if len(wrapper.Items) > 0 {
			items = wrapper.Items
		} else {
			// Single object format fallback
			var single ProxyItem
			if err := json.Unmarshal(data, &single); err == nil && single.Listen != "" {
				items = []ProxyItem{single}
			} else {
				return nil, fmt.Errorf("JSON object does not contain a valid proxies list")
			}
		}
	} else {
		return nil, fmt.Errorf("config must start with '[' (array) or '{' (object)")
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("no proxy entries found in config")
	}

	for i := range items {
		if err := items[i].Validate(i); err != nil {
			return nil, err
		}
	}

	return items, nil
}
