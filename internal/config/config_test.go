package config

import "testing"

func TestCanonicalBrowserOrigins(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://Console.Example.com:443/", "https://console.example.com"},
		{"http://LOCALHOST:80/", "http://localhost"},
		{"http://[::1]:80/", "http://[::1]"},
		{"http://localhost:8080/", "http://localhost:8080"},
	} {
		u, err := Origin(tc.raw)
		if err != nil || u.String() != tc.want {
			t.Fatalf("%s: %v %v", tc.raw, u, err)
		}
	}
	for _, raw := range []string{"ftp://internal", "https://user:pass@internal", "https://internal/path", "https://internal?q=1", "https://internal?", "https://internal/#fragment"} {
		if _, err := Origin(raw); err == nil {
			t.Fatalf("invalid origin accepted: %s", raw)
		}
	}
}
func TestConsoleOutsideApplicationNamespace(t *testing.T) {
	for _, console := range []string{"https://access.example.net", "https://console.access.example.net"} {
		s := Server{PublicURL: console, AccessURL: "https://access.example.net"}
		if s.Validate() == nil {
			t.Fatalf("overlapping console namespace accepted: %s", console)
		}
	}
}
