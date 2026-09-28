// Package datapath validates filesystem paths used for persisted service data.
package datapath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Validate returns the cleaned path when it is a plain absolute path under
// /data or the OS temp directory.
func Validate(path string) (string, error) {
	if strings.ContainsAny(path, "?&") || strings.Contains(path, "://") {
		return "", fmt.Errorf("path must be a plain filesystem path: %s", path)
	}
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("path must be absolute: %s", path)
	}
	for _, base := range []string{"/data", os.TempDir()} {
		rel, err := filepath.Rel(base, cleaned)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return cleaned, nil
		}
	}
	return "", fmt.Errorf("path must be under an allowed data root (/data or %s): %s", os.TempDir(), path)
}
