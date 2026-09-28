package config

import (
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/datapath"
	"github.com/calmcacil/sonarr-anime-bridge/internal/mappingurl"
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

func LoadQuiet() *Config {
	return load(false)
}

func Log(cfg *Config) {
	if cfg == nil {
		return
	}
	slog.Info("config loaded", "type", "config",
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
}

func load(log bool) *Config {
	cfg := &Config{
		Port:        getEnvInt("PORT", DefaultPort, log),
		CacheDBPath: getEnvStr("CACHE_DB_PATH", DefaultCacheDBPath),
		LogLevel:    getEnvStr("LOG_LEVEL", "info"),

		AnibridgeMappingPath: getEnvStr("MAPPING_PATH", DefaultAnibridgeMappingPath),
		AnibridgeURL:         getEnvStr("MAPPING_URL", DefaultAnibridgeURL),
	}

	// Validate and clamp Port
	if cfg.Port < 1 || cfg.Port > 65535 {
		if log {
			slog.Warn("PORT invalid, using default", "type", "config", "value", cfg.Port, "default", DefaultPort)
		}
		cfg.Port = DefaultPort
	}
	cfg.CacheDBPath = validateDataPath("CACHE_DB_PATH", cfg.CacheDBPath, DefaultCacheDBPath, log)
	cfg.AnibridgeMappingPath = validateDataPath("MAPPING_PATH", cfg.AnibridgeMappingPath, DefaultAnibridgeMappingPath, log)
	cfg.AnibridgeURL = validateMappingURL(cfg.AnibridgeURL, log)

	cfg.PrewarmYears = parseYearList("PREWARM_YEARS", []int{time.Now().Year()}, log)

	cfg.IncludeTypes = parseStringList("INCLUDE_TYPES", []string{"TV", "ONA"})
	validateIncludeTypes(cfg.IncludeTypes, log)
	cfg.ExcludeTags = parseStringList("EXCLUDE_TAGS", nil)
	cfg.FilterFutureEnabled = getEnvBool("FILTER_FUTURE_ENABLED", true, log)
	cfg.DebugEndpointsEnabled = getEnvBool("DEBUG_ENDPOINTS_ENABLED", false, log)
	cfg.AdminToken = getEnvStr("ADMIN_TOKEN", "")

	if log {
		Log(cfg)
	}

	return cfg
}

func getEnvStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvBool(key string, def bool, log bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		if log {
			slog.Warn("boolean env invalid, using default", "type", "config", "key", key, "value", v, "default", def)
		}
	}
	return def
}

func getEnvInt(key string, def int, log bool) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		if log {
			slog.Warn("integer env invalid, using default", "type", "config", "key", key, "value", v, "default", def)
		}
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

func parseYearList(key string, def []int, log bool) []int {
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
			if log {
				slog.Warn("year env entry invalid, skipping", "type", "config", "key", key, "value", p)
			}
			continue
		}
		if y < minYear || y > maxYear {
			if log {
				slog.Warn("year env entry out of range, skipping", "type", "config", "key", key, "year", y, "min", minYear, "max", maxYear)
			}
			continue
		}
		out = append(out, y)
	}
	if len(out) == 0 {
		if log {
			slog.Warn(key+" contained no valid years, falling back to default", "type", "config",
				"raw_value", v, "default", def)
		}
		return def
	}
	return out
}

func validateDataPath(key, path, def string, log bool) string {
	if path == ":memory:" {
		return path
	}
	cleaned, err := datapath.Validate(path)
	if err != nil {
		if log {
			slog.Warn("path env invalid, using default", "type", "config", "key", key, "value", path, "default", def, "error", err)
		}
		return def
	}
	return cleaned
}

func validateMappingURL(raw string, log bool) string {
	u, err := url.Parse(raw)
	valid := err == nil && u.Hostname() != "" && (u.Scheme == "https" || mappingurl.InsecureLoopbackAllowed(u))
	if !valid {
		if log {
			slog.Warn("MAPPING_URL invalid, using default", "type", "config", "value", raw, "default", DefaultAnibridgeURL)
		}
		return DefaultAnibridgeURL
	}
	if u.Scheme == "https" && !mappingurl.AllowedHost(u.Hostname()) {
		if log {
			slog.Warn("MAPPING_URL host is not allowlisted, using default",
				"type", "config",
				"host", u.Hostname(),
				"default", DefaultAnibridgeURL,
			)
		}
		return DefaultAnibridgeURL
	}
	return raw
}

// knownAniListFormats lists format values the AniList API returns for the
// media type ANIME. Used to warn about likely-mistaken INCLUDE_TYPES values.
var knownAniListFormats = []string{"TV", "ONA", "MOVIE", "OVA", "SPECIAL", "TV_SHORT", "MUSIC"}

// validateIncludeTypes logs a warning for any value in the list that doesn't
// match a known AniList format string.
func validateIncludeTypes(types []string, log bool) {
	for _, t := range types {
		if log && !slices.Contains(knownAniListFormats, t) {
			slog.Warn("INCLUDE_TYPES contains unrecognized format, will match no shows", "type", "config",
				"value", t,
				"known_formats", knownAniListFormats,
			)
		}
	}
}
