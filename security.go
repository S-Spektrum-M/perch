package main

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// requestProtection is a browser safety boundary, not authentication. Local
// interface IPs are accepted; DNS names other than localhost require opt-in.
func requestProtection(next http.Handler, extraHosts []string) http.Handler {
	hosts := map[string]bool{"localhost": true}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			if ip, _, err := net.ParseCIDR(addr.String()); err == nil {
				hosts[ip.String()] = true
			}
		}
	}
	for _, host := range extraHosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			hosts[host] = true
		}
	}
	crossOrigin := http.NewCrossOriginProtection()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := url.Parse("http://" + r.Host)
		if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Host == "" {
			http.Error(w, "invalid Host", http.StatusForbidden)
			return
		}
		host := strings.ToLower(u.Hostname())
		ip := net.ParseIP(host)
		if !hosts[host] && !(ip != nil && ip.IsLoopback()) {
			http.Error(w, "untrusted Host; configure -allowed-hosts for this DNS name", http.StatusForbidden)
			return
		}
		if err := crossOrigin.Check(r); err != nil {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			// Older browsers may omit Fetch Metadata. Require Origin in that case,
			// or an explicit custom header for non-browser clients (not form-sendable).
			if r.Header.Get("Sec-Fetch-Site") != "same-origin" && r.Header.Get("Origin") == "" && r.Header.Get("X-Pircd") != "1" {
				http.Error(w, "missing request origin", http.StatusForbidden)
				return
			}
			// Compare the entire origin, including port. CrossOriginProtection's
			// legacy-browser fallback only compares hostnames.
			if origin := r.Header.Get("Origin"); origin != "" {
				o, err := url.Parse(origin)
				if err != nil || (o.Scheme != "http" && o.Scheme != "https") || o.User != nil || o.Path != "" || o.RawQuery != "" || o.Fragment != "" || !strings.EqualFold(o.Host, r.Host) {
					http.Error(w, "cross-origin request rejected", http.StatusForbidden)
					return
				}
				// Scheme may be HTTPS at a TLS-terminating proxy. Do not trust forwarded
				// headers; Host plus the browser's Fetch Metadata remains the boundary.
			}
		}
		next.ServeHTTP(w, r)
	})
}
