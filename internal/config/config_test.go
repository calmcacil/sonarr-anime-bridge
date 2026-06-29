package config

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func captureConfigSlogOutput(t *testing.T, fn func()) string {
	t.Helper()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	old := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() {
		slog.SetDefault(old)
	})

	fn()
	return buf.String()
}

func TestLoad_Defaults(t *testing.T) {
	for _, key := range []string{
		"PORT", "CACHE_DB_PATH", "LOG_LEVEL",
		"PREWARM_YEARS", "INCLUDE_TYPES", "EXCLUDE_TAGS",
		"MAPPING_PATH", "MAPPING_URL", "FILTER_FUTURE_ENABLED",
	} {
		os.Unsetenv(key)
	}

	var cfg *Config
	logs := captureConfigSlogOutput(t, func() {
		cfg = Load()
	})

	if cfg == nil {
		t.Fatal("Load returned nil config")
	}

	if cfg.Port != DefaultPort {
		t.Errorf("Port = %d, want %d", cfg.Port, DefaultPort)
	}
	if cfg.CacheDBPath != DefaultCacheDBPath {
		t.Errorf("CacheDBPath = %q, want %q", cfg.CacheDBPath, DefaultCacheDBPath)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if len(cfg.PrewarmYears) != 1 || cfg.PrewarmYears[0] != time.Now().Year() {
		t.Errorf("PrewarmYears = %v, want [%d]", cfg.PrewarmYears, time.Now().Year())
	}
	if len(cfg.IncludeTypes) != 2 || cfg.IncludeTypes[0] != "TV" || cfg.IncludeTypes[1] != "ONA" {
		t.Errorf("IncludeTypes = %v, want [TV ONA]", cfg.IncludeTypes)
	}
	if cfg.ExcludeTags != nil {
		t.Errorf("ExcludeTags = %v, want nil", cfg.ExcludeTags)
	}
	if !cfg.FilterFutureEnabled {
		t.Error("FilterFutureEnabled default should be true")
	}
	if cfg.AnibridgeMappingPath != DefaultAnibridgeMappingPath {
		t.Errorf("AnibridgeMappingPath = %q, want %q", cfg.AnibridgeMappingPath, DefaultAnibridgeMappingPath)
	}
	if cfg.AnibridgeURL != DefaultAnibridgeURL {
		t.Errorf("AnibridgeURL = %q, want %q", cfg.AnibridgeURL, DefaultAnibridgeURL)
	}
	if logs == "" {
		t.Fatal("expected config load logs when using Load")
	}
	if !strings.Contains(logs, "type=config") {
		t.Fatalf("expected config log metadata type=config, got: %q", logs)
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	keys := []string{
		"PORT", "LOG_LEVEL", "PREWARM_YEARS",
		"INCLUDE_TYPES", "EXCLUDE_TAGS", "MAPPING_PATH", "MAPPING_URL", "FILTER_FUTURE_ENABLED",
	}
	for _, key := range keys {
		os.Unsetenv(key)
	}

	os.Setenv("PORT", "9090")
	os.Setenv("LOG_LEVEL", "debug")
	os.Setenv("PREWARM_YEARS", "2025,2026")
	os.Setenv("INCLUDE_TYPES", "TV")
	os.Setenv("EXCLUDE_TAGS", "hentai,guro")
	os.Setenv("MAPPING_PATH", "/custom/mapping.json.zst")
	os.Setenv("MAPPING_URL", "https://example.com/mappings.json.zst")
	os.Setenv("FILTER_FUTURE_ENABLED", "false")
	t.Cleanup(func() {
		for _, key := range keys {
			os.Unsetenv(key)
		}
	})

	var cfg *Config
	logs := captureConfigSlogOutput(t, func() {
		cfg = Load()
	})
	if logs == "" {
		t.Fatal("expected config load logs when using Load")
	}
	if !strings.Contains(logs, "type=config") {
		t.Fatalf("expected config log metadata type=config, got: %q", logs)
	}

	if cfg == nil {
		t.Fatal("Load returned nil config")
	}

	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if len(cfg.PrewarmYears) != 2 || cfg.PrewarmYears[0] != 2025 || cfg.PrewarmYears[1] != 2026 {
		t.Errorf("PrewarmYears = %v, want [2025 2026]", cfg.PrewarmYears)
	}
	if len(cfg.IncludeTypes) != 1 || cfg.IncludeTypes[0] != "TV" {
		t.Errorf("IncludeTypes = %v, want [TV]", cfg.IncludeTypes)
	}
	if len(cfg.ExcludeTags) != 2 || cfg.ExcludeTags[0] != "HENTAI" || cfg.ExcludeTags[1] != "GURO" {
		t.Errorf("ExcludeTags = %v, want [HENTAI GURO]", cfg.ExcludeTags)
	}
	if cfg.FilterFutureEnabled {
		t.Error("FilterFutureEnabled should be false")
	}
	if cfg.AnibridgeMappingPath != DefaultAnibridgeMappingPath {
		t.Errorf("AnibridgeMappingPath = %q, want %q", cfg.AnibridgeMappingPath, DefaultAnibridgeMappingPath)
	}
	if cfg.AnibridgeURL != "https://example.com/mappings.json.zst" {
		t.Errorf("AnibridgeURL = %q, want https://example.com/mappings.json.zst", cfg.AnibridgeURL)
	}
}

func TestLoad_IncludeTypesDefault(t *testing.T) {
	os.Unsetenv("INCLUDE_TYPES")
	cfg := LoadQuiet()
	if len(cfg.IncludeTypes) != 2 || cfg.IncludeTypes[0] != "TV" || cfg.IncludeTypes[1] != "ONA" {
		t.Errorf("IncludeTypes default = %v, want [TV ONA]", cfg.IncludeTypes)
	}
}

func TestLoad_IncludeTypesCustom(t *testing.T) {
	os.Setenv("INCLUDE_TYPES", "tv,ona,special")
	t.Cleanup(func() { os.Unsetenv("INCLUDE_TYPES") })

	cfg := LoadQuiet()
	if len(cfg.IncludeTypes) != 3 {
		t.Fatalf("IncludeTypes = %v, want 3 entries", cfg.IncludeTypes)
	}
	for _, want := range []string{"TV", "ONA", "SPECIAL"} {
		found := false
		for _, got := range cfg.IncludeTypes {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("IncludeTypes missing %q", want)
		}
	}
}
