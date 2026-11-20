package main

// Unified sources: subscription share-links and operator-declared static
// upstreams become the same ProxyConfig type. A static upstream is not a
// separate mode; it is one candidate with its own source label.
//
// UPSTREAM_PROXY is sugar for one static entry named "upstream". It is parsed
// by the same code path as STATIC_UPSTREAMS, so there is only one renderer and
// one set of credential rules.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type staticUpstreamCfg struct {
	Name   string
	Scheme string
	Host   string
	Port   int
	User   string
	Pass   string
	TLS    bool
}

// staticUpstreamsConfigured reports whether at least one static source is
// declared. A non-empty but invalid value still counts: startup then fails
// closed instead of silently pretending no static source exists.
func staticUpstreamsConfigured() bool {
	return strings.TrimSpace(os.Getenv("STATIC_UPSTREAMS")) != "" ||
		strings.TrimSpace(os.Getenv("UPSTREAM_PROXY")) != ""
}

// staticUpstreamConfigs parses STATIC_UPSTREAMS and the legacy UPSTREAM_PROXY
// sugar into pool candidates. Entry syntax is name=socks5://...,
// name=http://..., or name=https://... separated by comma or newline. A
// missing name gets a stable static-N label.
func staticUpstreamConfigs() ([]ProxyConfig, error) {
	var out []ProxyConfig
	for i, entry := range splitStaticEntries(os.Getenv("STATIC_UPSTREAMS")) {
		name, raw := splitStaticEntry(entry, i+1)
		cfg, err := staticUpstreamConfig(name, raw)
		if err != nil {
			return nil, fmt.Errorf("STATIC_UPSTREAMS entry %d: %w", i+1, err)
		}
		out = append(out, *cfg)
	}
	if raw := strings.TrimSpace(os.Getenv("UPSTREAM_PROXY")); raw != "" {
		cfg, err := staticUpstreamConfig("upstream", raw)
		if err != nil {
			return nil, fmt.Errorf("UPSTREAM_PROXY: %w", err)
		}
		out = append(out, *cfg)
	}
	return out, nil
}

func splitStaticEntries(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r", "\n")
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		for _, entry := range strings.Split(line, ",") {
			entry = strings.TrimSpace(entry)
			if entry != "" {
				out = append(out, entry)
			}
		}
	}
	return out
}

func splitStaticEntry(entry string, n int) (name, raw string) {
	if i := strings.Index(entry, "="); i > 0 {
		candidate := strings.TrimSpace(entry[:i])
		// A URL's userinfo may contain '=', so only treat the left side as a
		// name when it is not itself a scheme-bearing URL.
		if candidate != "" && !strings.Contains(candidate, "://") {
			name = candidate
			raw = strings.TrimSpace(entry[i+1:])
		}
	}
	if name == "" {
		name = fmt.Sprintf("static-%d", n)
	}
	if raw == "" {
		raw = entry
	}
	return name, raw
}

// staticUpstreamConfig renders one operator-declared proxy as a normal pool
// config. Endpoint and Raw are credential-free; userinfo lives only in XrayCfg.
func staticUpstreamConfig(name, raw string) (*ProxyConfig, error) {
	up, err := parseStaticUpstream(name, raw)
	if err != nil {
		return nil, err
	}
	server := map[string]any{
		"address": up.Host,
		"port":    up.Port,
	}
	if up.User != "" || up.Pass != "" {
		server["users"] = []any{map[string]any{"user": up.User, "pass": up.Pass}}
	}
	protocol := "socks"
	if up.Scheme == "http" || up.Scheme == "https" {
		protocol = "http"
	}
	outbound := map[string]any{
		"protocol": protocol,
		"tag":      upstreamOutTag,
		"settings": map[string]any{
			"servers": []any{server},
		},
	}
	if up.TLS {
		outbound["streamSettings"] = map[string]any{
			"network":  "tcp",
			"security": "tls",
			"tlsSettings": map[string]any{
				"serverName": up.Host,
			},
		}
	}
	cfgBytes, err := json.Marshal(outbound)
	if err != nil {
		// Marshal of this fixed map cannot fail in practice; keep the error
		// credential-free if it ever does.
		return nil, fmt.Errorf("static upstream config could not be rendered")
	}

	endpoint := strings.ToLower(net.JoinHostPort(up.Host, strconv.Itoa(up.Port)))
	return &ProxyConfig{
		Name:     up.Name,
		Raw:      up.Scheme + "://" + endpoint,
		Endpoint: endpoint,
		XrayCfg:  cfgBytes,
		Static:   true,
		Source:   "static:" + up.Name,
	}, nil
}

// parseStaticUpstream accepts socks5://, socks5h://, socks://, http:// and
// https:// with optional user:pass. Error strings are generic: the raw value
// may carry credentials and must never reach a log or API response.
func parseStaticUpstream(name, raw string) (staticUpstreamCfg, error) {
	var out staticUpstreamCfg
	out.Name = strings.TrimSpace(name)
	if out.Name == "" {
		return out, fmt.Errorf("static upstream is missing a name")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, fmt.Errorf("static upstream is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return out, fmt.Errorf("static upstream is not a valid URL")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "socks", "socks5", "socks5h", "http", "https":
	default:
		return out, fmt.Errorf("static upstream scheme must be socks5, http or https")
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return out, fmt.Errorf("static upstream is missing a host")
	}
	if u.Path != "" && u.Path != "/" {
		return out, fmt.Errorf("static upstream must not include a path")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return out, fmt.Errorf("static upstream must not include a query or fragment")
	}
	portRaw := u.Port()
	if portRaw == "" {
		return out, fmt.Errorf("static upstream is missing a port")
	}
	port, err := strconv.Atoi(portRaw)
	if err != nil || port < 1 || port > 65535 {
		return out, fmt.Errorf("static upstream port is invalid")
	}
	if u.User != nil {
		out.User = u.User.Username()
		out.Pass, _ = u.User.Password()
	}
	out.Scheme = scheme
	out.Host = host
	out.Port = port
	out.TLS = scheme == "https"
	return out, nil
}

// upstreamProxyConfig is the legacy single-entry view of UPSTREAM_PROXY,
// retained for callers/tests that only need that one config. The manager uses
// staticUpstreamConfigs so it can merge both source declarations.
func upstreamProxyConfig() (*ProxyConfig, error) {
	raw := strings.TrimSpace(os.Getenv("UPSTREAM_PROXY"))
	if raw == "" {
		return nil, nil
	}
	return staticUpstreamConfig("upstream", raw)
}
