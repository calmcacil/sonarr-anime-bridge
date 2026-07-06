package mappingurl

import "testing"

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
