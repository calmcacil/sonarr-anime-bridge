package config

import (
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultAnibridgeMappingPath = "/data/anibridge_mappings.json.zst"
	DefaultAnibridgeURL         = "https://github.com/anibridge/anibridge-mappings/releases/download/v3/mappings.json.zst"
)

type Config struct {
	Port                  int
	PrewarmYears          []int
	IncludeTypes          []string
	ExcludeTags           []string
	CacheDBPath           string
	LogLevel              string
	FilterFutureEnabled   bool
	DebugEndpointsEnabled bool
	AdminToken            string

	AnibridgeMappingPath string
	AnibridgeURL         string
}

const (
	DefaultPort        = 8080
	DefaultCacheDBPath = "/data/cache.db"
)

func Load() *Config {
	cfg := &Config{
		Port:        getEnvInt("PORT", DefaultPort),
		CacheDBPath: getEnvStr("CACHE_DB_PATH", DefaultCacheDBPath),
		LogLevel:    getEnvStr("LOG_LEVEL", "info"),

		AnibridgeMappingPath: getEnvStr("MAPPING_PATH", DefaultAnibridgeMappingPath),
		AnibridgeURL:         getEnvStr("MAPPING_URL", DefaultAnibridgeURL),
	}

	// Validate and clamp Port
	if cfg.Port < 1 || cfg.Port > 65535 {
		slog.Warn("PORT invalid, using default", "value", cfg.Port, "default", DefaultPort)
		cfg.Port = DefaultPort
	}
	cfg.CacheDBPath = validateDataPath("CACHE_DB_PATH", cfg.CacheDBPath, DefaultCacheDBPath)
	cfg.AnibridgeMappingPath = validateDataPath("MAPPING_PATH", cfg.AnibridgeMappingPath, DefaultAnibridgeMappingPath)
	cfg.AnibridgeURL = validateMappingURL(cfg.AnibridgeURL)

	cfg.PrewarmYears = parseYearList("PREWARM_YEARS", []int{time.Now().Year()})

	// If PREWARM_YEARS was set but parsing fell back to the default, warn.
	if v := os.Getenv("PREWARM_YEARS"); v != "" {
		currentYear := time.Now().Year()
		if len(cfg.PrewarmYears) == 1 && cfg.PrewarmYears[0] == currentYear {
			slog.Warn("PREWARM_YEARS contained no valid years, falling back to default",
				"raw_value", v, "default_year", currentYear)
		}
	}

	cfg.IncludeTypes = parseStringList("INCLUDE_TYPES", []string{"TV", "ONA"})
	validateIncludeTypes(cfg.IncludeTypes)
	cfg.ExcludeTags = parseStringList("EXCLUDE_TAGS", nil)
	cfg.FilterFutureEnabled = getEnvBool("FILTER_FUTURE_ENABLED", true)
	cfg.DebugEndpointsEnabled = getEnvBool("DEBUG_ENDPOINTS_ENABLED", false)
	cfg.AdminToken = getEnvStr("ADMIN_TOKEN", "")

	slog.Info("config loaded",
		"port", cfg.Port,
		"include_types", cfg.IncludeTypes,
		"exclude_tags", cfg.ExcludeTags,
		"filter_future_enabled", cfg.FilterFutureEnabled,
		"debug_endpoints_enabled", cfg.DebugEndpointsEnabled,
		"prewarm_years", cfg.PrewarmYears,
		"cache_db_path", cfg.CacheDBPath,
		"mapping_path", cfg.AnibridgeMappingPath,
		"mapping_url", cfg.AnibridgeURL,
		"log_level", cfg.LogLevel,
	)

	return cfg
}

func getEnvStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		slog.Warn("boolean env invalid, using default", "key", key, "value", v, "default", def)
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		slog.Warn("integer env invalid, using default", "key", key, "value", v, "default", def)
	}
	return def
}

func parseStringList(key string, def []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, strings.ToUpper(p))
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func parseYearList(key string, def []int) []int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	currentYear := time.Now().Year()
	minYear := currentYear - 10
	maxYear := currentYear + 10
	parts := strings.Split(v, ",")
	var out []int
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		y, err := strconv.Atoi(p)
		if err != nil || y <= 0 {
			slog.Warn("year env entry invalid, skipping", "key", key, "value", p)
			continue
		}
		if y < minYear || y > maxYear {
			slog.Warn("year env entry out of range, skipping", "key", key, "year", y, "min", minYear, "max", maxYear)
			continue
		}
		out = append(out, y)
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func validateDataPath(key, path, def string) string {
	if path == ":memory:" {
		return path
	}
	if strings.ContainsAny(path, "?&") || strings.Contains(path, "://") {
		slog.Warn("path env looks like a URI or DSN, using default", "key", key, "value", path, "default", def)
		return def
	}
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		slog.Warn("path env must be absolute, using default", "key", key, "value", path, "default", def)
		return def
	}
	for _, base := range []string{"/data", os.TempDir()} {
		rel, err := filepath.Rel(base, cleaned)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return cleaned
		}
	}
	slog.Warn("path env outside allowed data roots, using default", "key", key, "value", path, "default", def)
	return def
}

func validateMappingURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		slog.Warn("MAPPING_URL invalid, using default", "value", raw, "default", DefaultAnibridgeURL)
		return DefaultAnibridgeURL
	}
	if u.Host != "github.com" {
		slog.Warn("MAPPING_URL host is not default upstream", "host", u.Host)
	}
	return raw
}

// knownAniListFormats lists format values the AniList API returns for the
// media type ANIME. Used to warn about likely-mistaken INCLUDE_TYPES values.
var knownAniListFormats = map[string]bool{
	"TV": true, "ONA": true, "MOVIE": true, "OVA": true, "SPECIAL": true,
	"TV_SHORT": true, "MUSIC": true,
}

// validateIncludeTypes logs a warning for any value in the list that doesn't
// match a known AniList format string.
func validateIncludeTypes(types []string) {
	for _, t := range types {
		if !knownAniListFormats[t] {
			slog.Warn("INCLUDE_TYPES contains unrecognized format, will match no shows",
				"value", t,
				"known_formats", []string{"TV", "ONA", "MOVIE", "OVA", "SPECIAL", "TV_SHORT", "MUSIC"},
			)
		}
	}
}
