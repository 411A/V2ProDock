package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type ProxyConfig struct {
	Name    string
	Raw     string
	XrayCfg json.RawMessage
}

func FetchSubscription(subURL string) ([]ProxyConfig, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(subURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch subscription: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	content := strings.TrimSpace(string(body))

	// Try multiple base64 decodings
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if decoded, err := enc.DecodeString(content); err == nil {
			content = string(decoded)
			break
		}
	}

	var proxies []ProxyConfig
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		proxy, err := parseToXrayConfig(line)
		if err != nil {
			continue
		}
		proxies = append(proxies, *proxy)
	}
	return proxies, nil
}

func parseToXrayConfig(raw string) (*ProxyConfig, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	scheme := strings.ToLower(u.Scheme)
	name, _ := url.PathUnescape(strings.TrimPrefix(u.Fragment, "#"))
	if name == "" {
		name = u.Host
	}

	switch scheme {
	case "vless":
		return parseVless(u, raw, name)
	case "vmess":
		return parseVmess(raw, name)
	case "trojan":
		return parseTrojan(u, raw, name)
	case "ss":
		return parseSS(u, raw, name)
	case "hysteria2", "hy2":
		return parseHy2(u, raw, name)
	default:
		return nil, fmt.Errorf("unsupported: %s", scheme)
	}
}

func parseVless(u *url.URL, raw, name string) (*ProxyConfig, error) {
	q := u.Query()
	server := u.Hostname()
	port := toInt(u.Port(), 443)
	uuid := u.User.Username()
	security := q.Get("security")
	flow := q.Get("flow")
	transport := q.Get("type")
	if transport == "" {
		transport = "tcp"
	}

	stream := map[string]interface{}{
		"network":  transport,
		"security": security,
	}

	switch transport {
	case "ws":
		host := q.Get("host")
		path := q.Get("path")
		if path == "" {
			path = "/"
		}
		stream["wsSettings"] = map[string]interface{}{
			"path": path,
			"host": host,
		}
	case "grpc":
		stream["grpcSettings"] = map[string]interface{}{
			"serviceName": q.Get("serviceName"),
		}
	}

	// Security settings are transport-independent (REALITY/TLS work with any transport)
	switch security {
	case "reality":
		stream["realitySettings"] = map[string]interface{}{
			"serverName":  q.Get("sni"),
			"fingerprint": q.Get("fp"),
			"publicKey":   q.Get("pbk"),
			"shortId":     q.Get("sid"),
			"spiderX":     q.Get("spx"),
		}
		if flow != "" {
			stream["flow"] = flow
		}
	case "tls":
		stream["tlsSettings"] = buildTLS(q, transport)
	}

	outbound := map[string]interface{}{
		"protocol": "vless",
		"settings": map[string]interface{}{
			"vnext": []map[string]interface{}{
				{
					"address": server,
					"port":    port,
					"users": []map[string]interface{}{
						{
							"id":         uuid,
							"encryption": "none",
							"flow":       flow,
							"password":   "",
						},
					},
				},
			},
		},
		"streamSettings": stream,
		"tag":            "proxy",
	}

	cfgBytes, _ := json.Marshal(outbound)
	return &ProxyConfig{Name: name, Raw: raw, XrayCfg: cfgBytes}, nil
}

func parseVmess(raw, name string) (*ProxyConfig, error) {
	b64 := strings.TrimPrefix(raw, "vmess://")

	// Try multiple base64 decodings
	var jsonData []byte
	var err error
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		jsonData, err = enc.DecodeString(b64)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to decode vmess base64: %w", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(jsonData, &m); err != nil {
		return nil, err
	}

	server, _ := m["add"].(string)
	port := toInt(fmt.Sprint(m["port"]), 443)
	id, _ := m["id"].(string)
	aid := toInt(fmt.Sprint(m["aid"]), 0)
	net, _ := m["net"].(string)
	if net == "" {
		net = "tcp"
	}
	tls, _ := m["tls"].(string)

	stream := map[string]interface{}{
		"network":  net,
		"security": tls,
	}
	if net == "ws" {
		stream["wsSettings"] = map[string]interface{}{
			"path": m["path"],
			"host": m["host"],
		}
	}
	if tls == "tls" {
		stream["tlsSettings"] = map[string]interface{}{
			"serverName":  m["sni"],
			"fingerprint": m["fp"],
		}
	}

	outbound := map[string]interface{}{
		"protocol": "vmess",
		"settings": map[string]interface{}{
			"vnext": []map[string]interface{}{
				{
					"address": server,
					"port":    port,
					"users": []map[string]interface{}{
						{
							"id":       id,
							"alterId":  aid,
							"security": "auto",
						},
					},
				},
			},
		},
		"streamSettings": stream,
		"tag":            "proxy",
	}

	cfgBytes, _ := json.Marshal(outbound)
	if name == "" {
		name = server
	}
	return &ProxyConfig{Name: name, Raw: raw, XrayCfg: cfgBytes}, nil
}

func parseTrojan(u *url.URL, raw, name string) (*ProxyConfig, error) {
	q := u.Query()
	server := u.Hostname()
	port := toInt(u.Port(), 443)
	password := u.User.Username()
	transport := q.Get("type")
	if transport == "" {
		transport = "tcp"
	}

	stream := map[string]interface{}{
		"network":  transport,
		"security": "tls",
		"tlsSettings": map[string]interface{}{
			"serverName":  q.Get("sni"),
			"fingerprint": q.Get("fp"),
		},
	}
	// Only add ALPN for non-WebSocket transports (deprecated in xray v26 for WS)
	if transport != "ws" {
		stream["tlsSettings"].(map[string]interface{})["alpn"] = []string{"h2", "http/1.1"}
	}

	switch transport {
	case "ws":
		stream["wsSettings"] = map[string]interface{}{
			"path": q.Get("path"),
			"host": q.Get("host"),
		}
	case "grpc":
		stream["grpcSettings"] = map[string]interface{}{
			"serviceName": q.Get("serviceName"),
		}
	}

	// Trojan in xray: password is at the SERVER level, NOT inside users
	outbound := map[string]interface{}{
		"protocol": "trojan",
		"settings": map[string]interface{}{
			"servers": []map[string]interface{}{
				{
					"address":  server,
					"port":     port,
					"password": password,
				},
			},
		},
		"streamSettings": stream,
		"tag":            "proxy",
	}

	cfgBytes, _ := json.Marshal(outbound)
	if name == "" {
		name = server
	}
	return &ProxyConfig{Name: name, Raw: raw, XrayCfg: cfgBytes}, nil
}

func parseSS(u *url.URL, raw, name string) (*ProxyConfig, error) {
	server := u.Hostname()
	port := toInt(u.Port(), 443)

	userInfo := u.User.Username()
	decoded, err := base64.StdEncoding.DecodeString(userInfo)
	var method, password string
	if err == nil {
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) == 2 {
			method = parts[0]
			password = parts[1]
		}
	}
	if method == "" {
		method = "chacha20-ietf-poly1305"
		password = userInfo
	}

	outbound := map[string]interface{}{
		"protocol": "shadowsocks",
		"settings": map[string]interface{}{
			"servers": []map[string]interface{}{
				{
					"address":  server,
					"port":     port,
					"method":   method,
					"password": password,
				},
			},
		},
		"streamSettings": map[string]interface{}{
			"network": "tcp",
		},
		"tag": "proxy",
	}

	cfgBytes, _ := json.Marshal(outbound)
	if name == "" {
		name = server
	}
	return &ProxyConfig{Name: name, Raw: raw, XrayCfg: cfgBytes}, nil
}

func parseHy2(_ *url.URL, _, _ string) (*ProxyConfig, error) {
	// Hysteria2 is NOT supported by xray-core - skip it
	return nil, fmt.Errorf("hysteria2 not supported by xray-core")
}

func buildTLS(q url.Values, transport string) map[string]interface{} {
	m := map[string]interface{}{
		"serverName": q.Get("sni"),
	}
	if fp := q.Get("fp"); fp != "" {
		m["fingerprint"] = fp
	}
	// Don't include ALPN for WebSocket transport (deprecated in xray v26)
	if transport != "ws" {
		if alpn := q.Get("alpn"); alpn != "" {
			m["alpn"] = strings.Split(alpn, ",")
		}
	}
	return m
}

func toInt(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" || s == "<nil>" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}
