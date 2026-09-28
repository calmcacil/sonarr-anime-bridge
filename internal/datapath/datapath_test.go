package datapath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	tmp := os.TempDir()
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr string
	}{
		{name: "data root", path: "/data/cache.db", want: "/data/cache.db"},
		{name: "data root itself", path: "/data", want: "/data"},
		{name: "cleaned", path: "/data/sub/../cache.db", want: "/data/cache.db"},
		{name: "temp dir", path: filepath.Join(tmp, "x", "cache.db"), want: filepath.Join(tmp, "x", "cache.db")},
		{name: "relative", path: "data/cache.db", wantErr: "must be absolute"},
		{name: "query string", path: "/data/cache.db?mode=ro", wantErr: "plain filesystem path"},
		{name: "ampersand", path: "/data/a&b.db", wantErr: "plain filesystem path"},
		{name: "uri", path: "file:///data/cache.db", wantErr: "plain filesystem path"},
		{name: "outside roots", path: "/etc/cache.db", wantErr: "allowed data root"},
		{name: "sibling prefix", path: "/database/cache.db", wantErr: "allowed data root"},
		{name: "escape via dotdot", path: "/data/../etc/passwd", wantErr: "allowed data root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Validate(tt.path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Validate(%q) error = %v, want containing %q", tt.path, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate(%q) error = %v", tt.path, err)
			}
			if got != tt.want {
				t.Fatalf("Validate(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
