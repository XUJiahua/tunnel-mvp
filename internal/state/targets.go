package state

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ApplicationURL accepts an origin or a plain path prefix. Escaped/dot/backslash
// prefixes are intentionally rejected: the public and upstream mount are equal.
func ApplicationURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || len(raw) > 2048 || strings.Contains(u.Path, "//") {
		return nil, fmt.Errorf("HTTP/HTTPS target without credentials, query or fragment required")
	}
	if strings.HasPrefix(u.Path, "/_tunnel/") || u.Path == "/_tunnel" {
		return nil, fmt.Errorf("target path overlaps reserved gateway namespace")
	}
	if u.Path != "" && u.Path != "/" {
		for _, segment := range strings.Split(strings.Trim(u.Path, "/"), "/") {
			if segment == "" || segment == "." || segment == ".." {
				return nil, fmt.Errorf("target path must contain plain, non-dot segments")
			}
			for _, c := range segment {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", c)) {
					return nil, fmt.Errorf("target path supports ASCII letters, digits and -._~ only")
				}
			}
		}
		u.Path = strings.TrimSuffix(u.Path, "/") + "/"
	} else {
		u.Path = ""
	}
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80" {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	return u, nil
}
func ValidCustomDomain(host string) bool {
	if len(host) > 253 || !strings.Contains(host, ".") || host != strings.ToLower(host) || net.ParseIP(host) != nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !resourceID.MatchString(label) {
			return false
		}
	}
	return true
}
