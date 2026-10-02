package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
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

	p.Endpoint = getStr("endpoint", "Endpoint", "target", "upstream", "url")
	p.AuthKey = getStr("Authkey", "authkey", "AuthKey", "auth_key", "apiKey", "api_key", "key", "token")
	p.Engress = getStr("engress", "egress", "Engress", "Egress", "outbound", "source_ip", "interface")
	p.Listen = getStr("listen", "Listen", "addr", "address", "bind", "port")
	p.Provider = getStr("provider", "Provider", "mode", "upstream_type", "upstreamType", "protocol")
	p.CustomModels = getStrSlice("custom_models", "customModels", "CustomModels", "fake_models", "extra_models")
	p.ModelsMode = strings.ToLower(getStr("models_mode", "modelsMode", "mode_models", "model_list_mode"))

	return nil
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
