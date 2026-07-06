package mappingurl

import "strings"

// AllowedHost reports whether host is approved for anibridge mapping downloads.
func AllowedHost(host string) bool {
	switch strings.ToLower(host) {
	case "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com":
		return true
	default:
		return false
	}
}
