package main

// Single-upstream mode: an operator already has a working SOCKS5 proxy
// elsewhere and wants this daemon to front it with stable ports, health,
// memos, throughput and upload evidence. Unlike the normal pool, this mode
// runs exactly ONE xray child and disables subscription fetching: there is no
// second pool to rank, and pretending the subscription configs are still in
// use would make memos, pinning and status endpoints lie.
//
// The upstream is deliberately generic (any SOCKS5 server). No host, port or
// provider is assumed.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// upstreamProxyEnabled reports whether the operator opted into single-upstream
// mode. A non-empty but invalid value still counts as enabled: startup fails
// closed rather than silently falling back to the subscription pool.
func upstreamProxyEnabled() bool {
	return strings.TrimSpace(os.Getenv("UPSTREAM_PROXY")) != ""
}

type upstreamProxyCfg struct {
	Host string
	Port int
	User string
	Pass string
}

// parseUpstreamProxy accepts socks5://[user:pass@]host:port. Error strings are
// deliberately generic: the raw value may carry credentials and must never
// reach a log, API response or test failure message.
func parseUpstreamProxy(raw string) (upstreamProxyCfg, error) {
	var out upstreamProxyCfg
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, fmt.Errorf("UPSTREAM_PROXY is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return out, fmt.Errorf("UPSTREAM_PROXY is not a valid URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h", "socks":
	default:
		return out, fmt.Errorf("UPSTREAM_PROXY scheme must be socks5://")
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return out, fmt.Errorf("UPSTREAM_PROXY is missing a host")
	}
	if u.Path != "" && u.Path != "/" {
		return out, fmt.Errorf("UPSTREAM_PROXY must not include a path")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return out, fmt.Errorf("UPSTREAM_PROXY must not include a query or fragment")
	}
	portRaw := u.Port()
	if portRaw == "" {
		return out, fmt.Errorf("UPSTREAM_PROXY is missing a port")
	}
	port, err := strconv.Atoi(portRaw)
	if err != nil || port < 1 || port > 65535 {
		return out, fmt.Errorf("UPSTREAM_PROXY port is invalid")
	}
	if u.User != nil {
		out.User = u.User.Username()
		out.Pass, _ = u.User.Password()
	}
	out.Host, out.Port = host, port
	return out, nil
}

// upstreamProxyConfig turns UPSTREAM_PROXY into the single ProxyConfig this
// daemon serves. The identity is host:port only; credentials stay inside the
// 0600 xray config and are never part of Name, Raw, Endpoint or logs.
func upstreamProxyConfig() (*ProxyConfig, error) {
	raw := strings.TrimSpace(os.Getenv("UPSTREAM_PROXY"))
	if raw == "" {
		return nil, nil
	}
	up, err := parseUpstreamProxy(raw)
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
	outbound := map[string]any{
		"protocol": "socks",
		"tag":      upstreamOutTag,
		"settings": map[string]any{
			"servers": []any{server},
		},
	}
	cfgBytes, err := json.Marshal(outbound)
	if err != nil {
		// Marshal of this fixed map cannot fail in practice; keep the error
		// credential-free if it ever does.
		return nil, fmt.Errorf("UPSTREAM_PROXY config could not be rendered")
	}

	endpoint := strings.ToLower(net.JoinHostPort(up.Host, strconv.Itoa(up.Port)))
	return &ProxyConfig{
		Name:     "upstream " + endpoint,
		Raw:      "socks5://" + endpoint,
		Endpoint: endpoint,
		XrayCfg:  cfgBytes,
	}, nil
}
