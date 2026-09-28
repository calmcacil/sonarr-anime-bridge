package mappingurl

import (
	"net/url"
	"os"
	"strings"
)

// AllowedHost reports whether host is approved for anibridge mapping downloads.
func AllowedHost(host string) bool {
	switch strings.ToLower(host) {
	case "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com":
		return true
	default:
		return false
	}
}

// InsecureLoopbackAllowed reports whether u is a plain-HTTP loopback URL and
// ALLOW_INSECURE_MAPPING_URL=1 opts in to it (local testing only).
func InsecureLoopbackAllowed(u *url.URL) bool {
	if u.Scheme != "http" || os.Getenv("ALLOW_INSECURE_MAPPING_URL") != "1" {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}
