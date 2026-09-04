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
	"sync"
	"time"
)

type ProxyConfig struct {
	Name    string
	Raw     string
	XrayCfg json.RawMessage
}

func subscriptionCandidates(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil {
		return []string{raw}
	}
	host := strings.ToLower(u.Hostname())
	isLoopback := host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "0.0.0.0"
	if !isLoopback {
		return []string{raw}
	}
	seen := map[string]bool{raw: true}
	var out []string
	out = append(out, raw)
	add := func(h string) {
		uu := *u
		uu.Host = h
		if p := u.Port(); p != "" {
			uu.Host = h + ":" + p
		}
		s := uu.String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, h := range loopbackFallbackHosts {
		add(h)
	}
	return out
}

func splitURLs(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';' || r == ' ' || r == '\t'
	})
	seen := make(map[string]bool, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func fetchOneURL(client *http.Client, subURL string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < fetchAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(fetchBackoffBase * time.Duration(1<<attempt))
		}
		req, _ := http.NewRequest("GET", subURL, nil)
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "text/plain,*/*")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBody))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("http %d", resp.StatusCode)
			continue
		}
		if len(strings.TrimSpace(string(body))) == 0 {
			lastErr = fmt.Errorf("empty response (http %d)", resp.StatusCode)
			continue
		}
		return string(body), nil
	}
	return "", lastErr
}

func FetchSubscription(subURL string) ([]ProxyConfig, error) {
	client := &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
	var lastErr error
	var content string
	for _, cand := range subscriptionCandidates(subURL) {
		body, err := fetchOneURL(client, cand)
		if err != nil {
			lastErr = err
			continue
		}
		content = strings.TrimSpace(body)
		lastErr = nil
		if cand != subURL {
			subURL = cand
		}
		break
	}
	if lastErr != nil {
		return nil, fmt.Errorf("failed to fetch subscription %s (tried %v): %w — hint: inside Docker 127.0.0.1 is the container itself, use host.docker.internal or host LAN IP", subURL, subscriptionCandidates(subURL), lastErr)
	}

	// Clean up content: strip trailing/leading whitespace and carriage returns
	content = strings.ReplaceAll(content, "\r", "")
	content = strings.TrimSpace(content)

	// Try decoding base64 if needed
	decodedContent := decodeBase64Content(content)

	var proxies []ProxyConfig
	for _, line := range strings.Split(decodedContent, "\n") {
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

func FetchAnySubscription(urls []string) ([]ProxyConfig, string, error) {
	var errs []string
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		cfgs, err := FetchSubscription(u)
		if err == nil && len(cfgs) > 0 {
			return cfgs, u, nil
		}
		if err != nil {
			errs = append(errs, u+": "+err.Error())
			debugLog("subscription %s failed, trying next: %v", u, err)
		} else {
			errs = append(errs, u+": empty (0 proxies)")
			debugLog("subscription %s empty, trying next", u)
		}
	}
	if len(errs) == 0 {
		return nil, "", fmt.Errorf("no subscription URLs configured")
	}
	return nil, "", fmt.Errorf("all %d subscription URLs failed: %s", len(errs), strings.Join(errs, " | "))
}

func FetchMergedSubscriptions(urls []string) []ProxyConfig {
	clean := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u != "" {
			clean = append(clean, u)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	if len(clean) == 1 {
		cfgs, err := FetchSubscription(clean[0])
		if err != nil {
			warnLog("subscription fetch failed: %v", err)
			return nil
		}
		if len(cfgs) == 0 {
			warnLog("subscription %s returned 0 proxies", clean[0])
			return nil
		}
		return cfgs
	}
	var mu sync.Mutex
	var merged []ProxyConfig
	var wg sync.WaitGroup
	for _, u := range clean {
		wg.Add(1)
		go func(subURL string) {
			defer wg.Done()
			cfgs, err := FetchSubscription(subURL)
			if err != nil {
				warnLog("subscription %s failed (%v), using other sources", subURL, err)
				return
			}
			if len(cfgs) == 0 {
				warnLog("subscription %s returned 0 proxies, using other sources", subURL)
				return
			}
			mu.Lock()
			merged = append(merged, cfgs...)
			mu.Unlock()
			debugLog("subscription %s contributed %d configs", subURL, len(cfgs))
		}(u)
	}
	wg.Wait()
	if len(merged) == 0 {
		warnLog("all %d subscription URLs failed, keeping existing configs", len(clean))
		return nil
	}
	return dedupConfigs(merged)
}

func decodeBase64Content(s string) string {
	// If it already looks like a list of URLs (e.g. starts with vless://, vmess://, etc.), return as is
	lines := strings.Split(s, "\n")
	if len(lines) > 0 {
		firstLine := strings.TrimSpace(lines[0])
		if strings.HasPrefix(firstLine, "vless://") ||
			strings.HasPrefix(firstLine, "vmess://") ||
			strings.HasPrefix(firstLine, "trojan://") ||
			strings.HasPrefix(firstLine, "ss://") {
			return s
		}
	}

	// Sanitize base64 string: remove all whitespace and newlines
	cleanB64 := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, s)

	// Add missing padding if needed
	if mod := len(cleanB64) % 4; mod != 0 {
		cleanB64 += strings.Repeat("=", 4-mod)
	}

	// Try multiple base64 encodings
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if decoded, err := enc.DecodeString(cleanB64); err == nil {
			return string(decoded)
		}
	}

	return s
}

func parseToXrayConfig(raw string) (*ProxyConfig, error) {
	if strings.HasPrefix(raw, "vmess://") {
		return parseVmess(raw, "")
	}
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
		transport = q.Get("headerType")
	}
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
		wsSettings := map[string]interface{}{
			"path": path,
		}
		if host != "" {
			wsSettings["headers"] = map[string]string{"Host": host}
		}
		stream["wsSettings"] = wsSettings
	case "grpc":
		serviceName := q.Get("serviceName")
		if serviceName == "" {
			serviceName = q.Get("path")
		}
		grpcSettings := map[string]interface{}{
			"serviceName": serviceName,
		}
		if q.Get("mode") == "multi" {
			grpcSettings["multiMode"] = true
		}
		stream["grpcSettings"] = grpcSettings
	case "http", "h2":
		stream["network"] = "http"
		path := q.Get("path")
		if path == "" {
			path = "/"
		}
		host := q.Get("host")
		httpSettings := map[string]interface{}{
			"path": path,
		}
		if host != "" {
			httpSettings["host"] = strings.Split(host, ",")
		}
		stream["httpSettings"] = httpSettings
	case "splithttp", "httpupgrade":
		path := q.Get("path")
		if path == "" {
			path = "/"
		}
		host := q.Get("host")
		settingsKey := transport + "Settings"
		settings := map[string]interface{}{
			"path": path,
		}
		if host != "" {
			settings["host"] = host
		}
		stream[settingsKey] = settings
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

	// Sanitize base64 string
	b64 = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, b64)

	if mod := len(b64) % 4; mod != 0 {
		b64 += strings.Repeat("=", 4-mod)
	}

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
	switch net {
	case "ws":
		wsSettings := map[string]interface{}{}
		if path, ok := m["path"].(string); ok && path != "" {
			wsSettings["path"] = path
		}
		if host, ok := m["host"].(string); ok && host != "" {
			wsSettings["headers"] = map[string]string{"Host": host}
		}
		stream["wsSettings"] = wsSettings
	case "grpc":
		grpcSettings := map[string]interface{}{}
		if path, ok := m["path"].(string); ok && path != "" {
			grpcSettings["serviceName"] = path
		}
		stream["grpcSettings"] = grpcSettings
	case "http", "h2":
		stream["network"] = "http"
		httpSettings := map[string]interface{}{}
		if path, ok := m["path"].(string); ok && path != "" {
			httpSettings["path"] = path
		}
		if host, ok := m["host"].(string); ok && host != "" {
			httpSettings["host"] = strings.Split(host, ",")
		}
		stream["httpSettings"] = httpSettings
	}

	if tls == "tls" {
		tlsSettings := map[string]interface{}{}
		if sni, ok := m["sni"].(string); ok && sni != "" {
			tlsSettings["serverName"] = sni
		}
		if fp, ok := m["fp"].(string); ok && fp != "" {
			tlsSettings["fingerprint"] = fp
		}
		stream["tlsSettings"] = tlsSettings
	}

	scy, _ := m["scy"].(string)
	if scy == "" {
		scy = "auto"
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
							"security": scy,
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
		if ps, ok := m["ps"].(string); ok && ps != "" {
			name = ps
		} else {
			name = server
		}
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

	// Xray-supported Shadowsocks ciphers (AEAD only — stream ciphers removed in Xray 26+)
	supportedSS := map[string]bool{
		"aes-128-gcm":                   true,
		"aes-256-gcm":                   true,
		"chacha20-poly1305":             true,
		"chacha20-ietf-poly1305":        true,
		"xchacha20-ietf-poly1305":       true,
		"2022-blake3-aes-128-gcm":       true,
		"2022-blake3-aes-256-gcm":       true,
		"2022-blake3-chacha20-poly1305": true,
	}
	if !supportedSS[method] {
		return nil, fmt.Errorf("unsupported SS cipher: %s", method)
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
