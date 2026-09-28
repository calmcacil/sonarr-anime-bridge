package config

import (
	"bytes"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/testutil"
)

var configEnvKeys = []string{
	"PORT", "CACHE_DB_PATH", "LOG_LEVEL", "PREWARM_YEARS", "INCLUDE_TYPES", "EXCLUDE_TAGS",
	"MAPPING_PATH", "MAPPING_URL", "FILTER_FUTURE_ENABLED", "ALLOW_INSECURE_MAPPING_URL",
	"DEBUG_ENDPOINTS_ENABLED", "ADMIN_TOKEN",
}

// setEnv clears every config key (empty values fall back to defaults), then
// applies overrides. t.Setenv restores the previous values after the test.
func setEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	for _, key := range configEnvKeys {
		t.Setenv(key, overrides[key])
	}
}

// loadWithLogs runs load(true) and returns the config plus text log output.
func loadWithLogs(t *testing.T) (*Config, string) {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return load(true), buf.String()
}

func TestLoad_Defaults(t *testing.T) {
	setEnv(t, nil)
	cfg, logs := loadWithLogs(t)

	want := &Config{
		Port:                 DefaultPort,
		CacheDBPath:          DefaultCacheDBPath,
		LogLevel:             "info",
		PrewarmYears:         []int{time.Now().Year()},
		IncludeTypes:         []string{"TV", "ONA"},
		FilterFutureEnabled:  true,
		AnibridgeMappingPath: DefaultAnibridgeMappingPath,
		AnibridgeURL:         DefaultAnibridgeURL,
	}
	assertConfig(t, cfg, want)
	if !strings.Contains(logs, "config loaded") || !strings.Contains(logs, "type=config") {
		t.Fatalf("expected config load log with type=config, got: %q", logs)
	}
	if strings.Contains(logs, "level=WARN") {
		t.Fatalf("defaults produced warnings: %q", logs)
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	year := time.Now().Year()
	setEnv(t, map[string]string{
		"PORT":                    "9090",
		"LOG_LEVEL":               "debug",
		"PREWARM_YEARS":           strings.Join([]string{strconv.Itoa(year - 1), strconv.Itoa(year)}, ","),
		"INCLUDE_TYPES":           "tv, special",
		"EXCLUDE_TAGS":            "hentai,guro",
		"MAPPING_PATH":            "/data/custom/mapping.json.zst",
		"CACHE_DB_PATH":           "/data/../data/other.db",
		"MAPPING_URL":             "https://release-assets.githubusercontent.com/mappings.json.zst",
		"FILTER_FUTURE_ENABLED":   "false",
		"DEBUG_ENDPOINTS_ENABLED": "true",
		"ADMIN_TOKEN":             "secret",
	})
	cfg, _ := loadWithLogs(t)

	assertConfig(t, cfg, &Config{
		Port:                  9090,
		CacheDBPath:           "/data/other.db",
		LogLevel:              "debug",
		PrewarmYears:          []int{year - 1, year},
		IncludeTypes:          []string{"TV", "SPECIAL"},
		ExcludeTags:           []string{"HENTAI", "GURO"},
		FilterFutureEnabled:   false,
		DebugEndpointsEnabled: true,
		AdminToken:            "secret",
		AnibridgeMappingPath:  "/data/custom/mapping.json.zst",
		AnibridgeURL:          "https://release-assets.githubusercontent.com/mappings.json.zst",
	})
}

func TestLoad_InvalidValuesFallBack(t *testing.T) {
	year := time.Now().Year()
	tests := []struct {
		name    string
		env     map[string]string
		check   func(*Config) bool
		wantLog string
	}{
		{"non-numeric port", map[string]string{"PORT": "abc"}, func(c *Config) bool { return c.Port == DefaultPort }, "integer env invalid"},
		{"port out of range", map[string]string{"PORT": "70000"}, func(c *Config) bool { return c.Port == DefaultPort }, "PORT invalid"},
		{"invalid bool", map[string]string{"FILTER_FUTURE_ENABLED": "maybe"}, func(c *Config) bool { return c.FilterFutureEnabled }, "boolean env invalid"},
		{"relative cache path", map[string]string{"CACHE_DB_PATH": "cache.db"}, func(c *Config) bool { return c.CacheDBPath == DefaultCacheDBPath }, "path env invalid"},
		{"DSN cache path", map[string]string{"CACHE_DB_PATH": "file:/data/cache.db?mode=ro"}, func(c *Config) bool { return c.CacheDBPath == DefaultCacheDBPath }, "path env invalid"},
		{"mapping path outside roots", map[string]string{"MAPPING_PATH": "/etc/mapping.json.zst"}, func(c *Config) bool { return c.AnibridgeMappingPath == DefaultAnibridgeMappingPath }, "path env invalid"},
		{"memory cache kept", map[string]string{"CACHE_DB_PATH": ":memory:"}, func(c *Config) bool { return c.CacheDBPath == ":memory:" }, ""},
		{"years skip invalid entries", map[string]string{"PREWARM_YEARS": "abc," + strconv.Itoa(year-50) + "," + strconv.Itoa(year)}, func(c *Config) bool { return slices.Equal(c.PrewarmYears, []int{year}) }, "year env entry"},
		{"no valid years", map[string]string{"PREWARM_YEARS": "abc"}, func(c *Config) bool { return slices.Equal(c.PrewarmYears, []int{year}) }, "contained no valid years"},
		{"empty type list", map[string]string{"INCLUDE_TYPES": " , "}, func(c *Config) bool { return slices.Equal(c.IncludeTypes, []string{"TV", "ONA"}) }, ""},
		{"unknown type warns", map[string]string{"INCLUDE_TYPES": "TV,SERIES"}, func(c *Config) bool { return slices.Equal(c.IncludeTypes, []string{"TV", "SERIES"}) }, "unrecognized format"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, tt.env)
			cfg, logs := loadWithLogs(t)
			if !tt.check(cfg) {
				t.Fatalf("unexpected config: %+v", cfg)
			}
			if tt.wantLog != "" && !strings.Contains(logs, tt.wantLog) {
				t.Fatalf("expected log %q, got: %q", tt.wantLog, logs)
			}
		})
	}
}

func TestLoad_MappingURL(t *testing.T) {
	tests := []struct {
		name          string
		url           string
		allowInsecure bool
		wantDefault   bool
		wantLog       string
	}{
		{name: "github", url: "https://github.com/anibridge/anibridge-mappings/releases/download/v3/mappings.json.zst"},
		{name: "objects host", url: "https://objects.githubusercontent.com/mappings.json.zst"},
		{name: "release assets host", url: "https://release-assets.githubusercontent.com/mappings.json.zst"},
		{name: "non-allowlisted host", url: "https://example.com/mappings.json.zst", wantDefault: true, wantLog: "MAPPING_URL host is not allowlisted"},
		{name: "plain http", url: "http://github.com/mappings.json.zst", wantDefault: true, wantLog: "MAPPING_URL invalid"},
		{name: "no host", url: "https:///mappings.json.zst", wantDefault: true, wantLog: "MAPPING_URL invalid"},
		{name: "loopback ipv4 without opt-in", url: "http://127.0.0.1:18080/mappings.json.zst", wantDefault: true},
		{name: "localhost without opt-in", url: "http://localhost/mappings.json.zst", wantDefault: true},
		{name: "loopback ipv6 without opt-in", url: "http://[::1]:18080/mappings.json.zst", wantDefault: true},
		{name: "loopback ipv4 with opt-in", url: "http://127.0.0.1:18080/mappings.json.zst", allowInsecure: true},
		{name: "localhost with opt-in", url: "http://localhost/mappings.json.zst", allowInsecure: true},
		{name: "loopback ipv6 with opt-in", url: "http://[::1]:18080/mappings.json.zst", allowInsecure: true},
		{name: "opt-in stays loopback only", url: "http://example.com/mappings.json.zst", allowInsecure: true, wantDefault: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"MAPPING_URL": tt.url}
			if tt.allowInsecure {
				env["ALLOW_INSECURE_MAPPING_URL"] = "1"
			}
			setEnv(t, env)
			cfg, logs := loadWithLogs(t)
			want := tt.url
			if tt.wantDefault {
				want = DefaultAnibridgeURL
			}
			if cfg.AnibridgeURL != want {
				t.Fatalf("AnibridgeURL = %q, want %q", cfg.AnibridgeURL, want)
			}
			if tt.wantLog != "" && !strings.Contains(logs, tt.wantLog) {
				t.Fatalf("expected log %q, got: %q", tt.wantLog, logs)
			}
		})
	}
}

func TestLoadQuietDoesNotLog(t *testing.T) {
	setEnv(t, map[string]string{"PORT": "abc"})
	logs := testutil.CaptureLogs(t, slog.LevelDebug)
	LoadQuiet()
	if n := len(logs.Records()); n != 0 {
		t.Fatalf("LoadQuiet logged %d records", n)
	}
}

func assertConfig(t *testing.T, got, want *Config) {
	t.Helper()
	if got.Port != want.Port || got.CacheDBPath != want.CacheDBPath || got.LogLevel != want.LogLevel ||
		got.FilterFutureEnabled != want.FilterFutureEnabled || got.DebugEndpointsEnabled != want.DebugEndpointsEnabled ||
		got.AdminToken != want.AdminToken || got.AnibridgeMappingPath != want.AnibridgeMappingPath ||
		got.AnibridgeURL != want.AnibridgeURL ||
		!slices.Equal(got.PrewarmYears, want.PrewarmYears) ||
		!slices.Equal(got.IncludeTypes, want.IncludeTypes) ||
		!slices.Equal(got.ExcludeTags, want.ExcludeTags) {
		t.Fatalf("config mismatch:\n got  %+v\n want %+v", got, want)
	}
}
