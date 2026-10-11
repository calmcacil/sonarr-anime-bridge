package mapping

// Frozen parser from deb5dbe, retained only for compatibility measurements.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

func referenceParseAnibridgeJSON(ctx context.Context, r io.Reader, src string) (*AnibridgeMapping, error) {
	limited := &limitReader{Reader: r, limit: maxDecodedMappingBytes}
	dec := json.NewDecoder(limited)

	t, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("parse anibridge JSON: expected opening brace: %w", err)
	}
	if t != json.Delim('{') {
		return nil, fmt.Errorf("parse anibridge JSON: expected '{', got %T(%v)", t, t)
	}

	start := time.Now()
	byMAL := map[int]int{}
	byAniList := map[int]int{}

	for dec.More() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("parse anibridge JSON: key token: %w", err)
		}
		key, ok := t.(string)
		if !ok {
			return nil, fmt.Errorf("parse anibridge JSON: expected string key, got %T", t)
		}

		var target map[int]int
		var rawID string
		if rest, ok := strings.CutPrefix(key, "mal:"); ok {
			target, rawID = byMAL, rest
		} else if rest, ok := strings.CutPrefix(key, "anilist:"); ok {
			target, rawID = byAniList, rest
		}
		if target != nil {
			if id, convErr := strconv.Atoi(rawID); convErr == nil && id > 0 {
				tvdbID, ok, err := referenceExtractTVDB(dec)
				if err != nil {
					return nil, fmt.Errorf("parse anibridge JSON: %s: %w", key, err)
				}
				if ok {
					target[id] = tvdbID
				}
			} else if err := referenceSkipValue(dec); err != nil {
				return nil, err
			}
			continue
		}

		switch key {
		case "$meta":
			var meta anibridgeMeta
			if err := dec.Decode(&meta); err != nil {
				slog.Warn("anibridge dataset metadata unreadable; continuing with mapping entries", "type", "resolver",
					"task", "mapping_parse", "outcome", "degraded", "error", err,
					"action", "continue", "consequence", "dataset_metadata_unavailable")
			} else {
				slog.Debug("anibridge dataset metadata", "type", "resolver", "task", "mapping_parse",
					"schema_version", meta.SchemaVersion, "generated_on", meta.GeneratedOn)
			}

		default:
			if err := referenceSkipValue(dec); err != nil {
				return nil, err
			}
		}
	}

	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("parse anibridge JSON: expected closing brace: %w", err)
	}

	slog.Debug("parsed anibridge mapping", "type", "resolver", "task", "mapping_parse", "outcome", "succeeded",
		"mal_entries", len(byMAL), "anilist_entries", len(byAniList),
		"duration_ms", time.Since(start).Milliseconds(), "source", src)

	return &AnibridgeMapping{byMAL: byMAL, byAniList: byAniList}, nil
}

func referenceSkipValue(dec *json.Decoder) error {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("skip value: %w", err)
	}
	return nil
}

// referenceExtractTVDB chooses the best TVDB ID for a single anibridge entry. The
// anibridge data often lists the same show multiple times under different
// season scopes (e.g. `tvdb_show:123:s1` for the regular season and
// `tvdb_show:123:s0` for specials). We prefer s1 entries, and otherwise fall
// back to the scope with the highest source-episode count.
func referenceExtractTVDB(dec *json.Decoder) (int, bool, error) {
	var targets map[string]json.RawMessage
	if err := dec.Decode(&targets); err != nil {
		return 0, false, fmt.Errorf("decode targets: %w", err)
	}

	bestTVDB := 0
	bestEpCount := -1
	bestIsS1 := false

	for descriptor, rawValue := range targets {
		if !strings.HasPrefix(descriptor, "tvdb_show:") {
			continue
		}

		parts := strings.SplitN(descriptor, ":", 3)
		if len(parts) < 3 {
			continue
		}
		tvdbID, convErr := strconv.Atoi(parts[1])
		if convErr != nil || tvdbID <= 0 {
			continue
		}
		scope := parts[2]

		epCount, err := referenceCountSourceEpisodes(rawValue)
		if err != nil {
			return 0, false, fmt.Errorf("count episodes for %s: %w", descriptor, err)
		}

		isS1 := scope == "s1"
		if isS1 != bestIsS1 {
			if isS1 {
				bestTVDB = tvdbID
				bestEpCount = epCount
				bestIsS1 = true
			}
			continue
		}
		if epCount > bestEpCount || (epCount == bestEpCount && tvdbID < bestTVDB) {
			bestTVDB = tvdbID
			bestEpCount = epCount
		}
	}

	if bestTVDB > 0 {
		return bestTVDB, true, nil
	}
	return 0, false, nil
}

func referenceCountSourceEpisodes(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}

	var ranges map[string]string
	if err := json.Unmarshal(raw, &ranges); err != nil {
		return 0, err
	}

	var total int
	for srcRange := range ranges {
		if srcRange == "" {
			return 0, errors.New("empty episode range")
		}
		parts := strings.SplitN(srcRange, "-", 2)
		if len(parts) == 1 {
			ep, err := strconv.Atoi(parts[0])
			if err != nil || ep <= 0 {
				return 0, fmt.Errorf("invalid episode range %q", srcRange)
			}
			total++
			continue
		}
		start, err := strconv.Atoi(parts[0])
		if err != nil || start <= 0 {
			return 0, fmt.Errorf("invalid episode range %q", srcRange)
		}
		if parts[1] == "" {
			total++
			continue
		}
		end, err := strconv.Atoi(parts[1])
		if err != nil || end < start {
			return 0, fmt.Errorf("invalid episode range %q", srcRange)
		}
		total += end - start + 1
	}
	return total, nil
}
