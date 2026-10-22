package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestProtection(t *testing.T) {
	handler := requestProtection(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }), []string{"pi.example.ts.net"})
	for _, tc := range []struct {
		name, method, host, origin, site, custom string
		want                                     int
	}{
		{name: "local read", method: "GET", host: "127.0.0.1:7337", want: 204},
		{name: "ipv6", method: "GET", host: "[::1]:7337", want: 204},
		{name: "proxy read", method: "GET", host: "pi.example.ts.net", want: 204},
		{name: "dns rebinding", method: "POST", host: "evil.example", origin: "http://evil.example", site: "same-origin", want: 403},
		{name: "unconfigured dns", method: "GET", host: "unknown.example", want: 403},
		{name: "native form", method: "POST", host: "localhost:7337", origin: "http://localhost:7337", want: 204},
		{name: "multipart form", method: "POST", host: "localhost:7337", origin: "http://localhost:7337", site: "same-origin", want: 204},
		{name: "proxy form", method: "POST", host: "pi.example.ts.net", origin: "https://pi.example.ts.net", want: 204},
		{name: "cross site form", method: "POST", host: "localhost:7337", origin: "https://evil.example", site: "cross-site", want: 403},
		{name: "legacy cross origin", method: "POST", host: "localhost:7337", origin: "https://evil.example", want: 403},
		{name: "different port", method: "POST", host: "localhost:7337", origin: "http://localhost:9999", want: 403},
		{name: "same site not same origin", method: "POST", host: "pi.example.ts.net", origin: "https://other.example.ts.net", site: "same-site", want: 403},
		{name: "missing origin", method: "POST", host: "localhost:7337", want: 403},
		{name: "opaque origin", method: "POST", host: "localhost:7337", origin: "null", want: 403},
		{name: "nonbrowser custom header", method: "POST", host: "localhost:7337", custom: "1", want: 204},
		{name: "custom header not bypass", method: "POST", host: "localhost:7337", origin: "https://evil.example", custom: "1", want: 403},
		{name: "fetch metadata", method: "POST", host: "localhost:7337", site: "same-origin", want: 204},
		{name: "delete blocked", method: "DELETE", host: "localhost:7337", origin: "http://evil.example", want: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://"+tc.host+"/", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if tc.custom != "" {
				req.Header.Set("X-Pircd", tc.custom)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
