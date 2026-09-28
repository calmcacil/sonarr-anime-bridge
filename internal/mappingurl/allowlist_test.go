package mappingurl

import (
	"net/url"
	"testing"
)

func TestInsecureLoopbackAllowed(t *testing.T) {
	tests := []struct {
		raw   string
		optIn string
		want  bool
	}{
		{"http://127.0.0.1:8080/m.zst", "1", true},
		{"http://LOCALHOST/m.zst", "1", true},
		{"http://[::1]:8080/m.zst", "1", true},
		{"http://127.0.0.1/m.zst", "", false},
		{"http://127.0.0.1/m.zst", "true", false},
		{"https://127.0.0.1/m.zst", "1", false},
		{"http://example.com/m.zst", "1", false},
		{"http://127.0.0.2/m.zst", "1", false},
	}
	for _, tt := range tests {
		t.Setenv("ALLOW_INSECURE_MAPPING_URL", tt.optIn)
		u, err := url.Parse(tt.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := InsecureLoopbackAllowed(u); got != tt.want {
			t.Errorf("InsecureLoopbackAllowed(%q, opt-in=%q) = %v, want %v", tt.raw, tt.optIn, got, tt.want)
		}
	}
}

func TestAllowedHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"GITHUB.com", true},
		{"objects.githubusercontent.com", true},
		{"release-assets.githubusercontent.com", true},
		{"example.com", false},
		{"github.com.example.com", false},
	}

	for _, tt := range tests {
		if got := AllowedHost(tt.host); got != tt.want {
			t.Fatalf("AllowedHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}
