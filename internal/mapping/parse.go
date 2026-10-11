package mapping

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

// Decode formerly scanned each root property's value separately. Token does
// not enforce its 10,000-container limit, so preserve that same value boundary.
const maxMappingValueDepth = 10000

func parseAnibridgeJSON(ctx context.Context, r io.Reader, src string) (*AnibridgeMapping, error) {
	limited := &limitReader{Reader: r, limit: maxDecodedMappingBytes}
	dec := json.NewDecoder(limited)

	t, err := parserToken(ctx, dec)
	if err != nil {
		return nil, fmt.Errorf("parse anibridge JSON: expected opening brace: %w", err)
	}
	if t != json.Delim('{') {
		return nil, fmt.Errorf("parse anibridge JSON: expected '{', got %T(%v)", t, t)
	}
	// Ignored numbers need only be syntactically valid, even outside float64 range.
	dec.UseNumber()

	start := time.Now()
	byMAL := map[int]int{}
	byAniList := map[int]int{}

	for dec.More() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t, err := parserToken(ctx, dec)
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
				tvdbID, ok, err := extractTVDB(ctx, dec)
				if err != nil {
					return nil, fmt.Errorf("parse anibridge JSON: %s: %w", key, err)
				}
				if ok {
					target[id] = tvdbID
				}
			} else if err := skipValue(ctx, dec, 0); err != nil {
				return nil, err
			}
			continue
		}

		switch key {
		case "$meta":
			meta, semanticErr, err := parseDatasetMetadata(ctx, dec)
			if err != nil {
				return nil, fmt.Errorf("parse anibridge JSON: dataset metadata: %w", err)
			}
			if semanticErr != nil {
				slog.Warn("anibridge dataset metadata unreadable; continuing with mapping entries", "type", "resolver",
					"task", "mapping_parse", "outcome", "degraded", "error", semanticErr,
					"action", "continue", "consequence", "dataset_metadata_unavailable")
			} else {
				slog.Debug("anibridge dataset metadata", "type", "resolver", "task", "mapping_parse",
					"schema_version", meta.SchemaVersion, "generated_on", meta.GeneratedOn)
			}

		default:
			if err := skipValue(ctx, dec, 0); err != nil {
				return nil, err
			}
		}
	}

	if _, err := parserToken(ctx, dec); err != nil {
		return nil, fmt.Errorf("parse anibridge JSON: expected closing brace: %w", err)
	}

	slog.Debug("parsed anibridge mapping", "type", "resolver", "task", "mapping_parse", "outcome", "succeeded",
		"mal_entries", len(byMAL), "anilist_entries", len(byAniList),
		"duration_ms", time.Since(start).Milliseconds(), "source", src)

	return &AnibridgeMapping{byMAL: byMAL, byAniList: byAniList}, nil
}

func parserToken(ctx context.Context, dec *json.Decoder) (json.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t, err := dec.Token()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return t, err
}

// skipToken consumes the rest of a value whose first token has already been read.
// Decoder checks delimiter pairing and syntax; the explicit stack avoids recursion.
func skipToken(ctx context.Context, dec *json.Decoder, t json.Token, enclosingDepth int) error {
	d, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	stack := []json.Delim{d}
	if len(stack)+enclosingDepth > maxMappingValueDepth {
		return errors.New("exceeded max depth")
	}
	for len(stack) > 0 {
		t, err := parserToken(ctx, dec)
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{', '[':
				stack = append(stack, d)
				if len(stack)+enclosingDepth > maxMappingValueDepth {
					return errors.New("exceeded max depth")
				}
			case '}', ']':
				stack = stack[:len(stack)-1]
			}
		}
	}
	return nil
}

func skipValue(ctx context.Context, dec *json.Decoder, enclosingDepth int) error {
	t, err := parserToken(ctx, dec)
	if err == nil {
		err = skipToken(ctx, dec, t, enclosingDepth)
	}
	if err != nil {
		return fmt.Errorf("skip value: %w", err)
	}
	return nil
}

func parseDatasetMetadata(ctx context.Context, dec *json.Decoder) (anibridgeMeta, error, error) {
	var meta anibridgeMeta
	t, err := parserToken(ctx, dec)
	if err != nil || t == nil {
		return meta, nil, err
	}
	if t != json.Delim('{') {
		if err := skipToken(ctx, dec, t, 0); err != nil {
			return meta, nil, err
		}
		return meta, errors.New("invalid dataset metadata type"), nil
	}
	var semanticErr error
	for dec.More() {
		t, err := parserToken(ctx, dec)
		if err != nil {
			return meta, nil, err
		}
		key := t.(string)
		var field *string
		switch {
		case strings.EqualFold(key, "schema_version"):
			field = &meta.SchemaVersion
		case strings.EqualFold(key, "generated_on"):
			field = &meta.GeneratedOn
		}
		t, err = parserToken(ctx, dec)
		if err != nil {
			return meta, nil, err
		}
		if field != nil && t != nil {
			if value, ok := t.(string); ok {
				*field = value
			} else {
				semanticErr = errors.New("invalid dataset metadata field type")
			}
		}
		if err := skipToken(ctx, dec, t, 1); err != nil {
			return meta, nil, err
		}
	}
	_, err = parserToken(ctx, dec)
	if err != nil {
		return meta, nil, err
	}
	return meta, semanticErr, nil
}

type targetSummary struct {
	id, episodes int
	isS1         bool
	err          error
}

func extractTVDB(ctx context.Context, dec *json.Decoder) (int, bool, error) {
	dec.UseNumber()
	t, err := parserToken(ctx, dec)
	if err != nil {
		return 0, false, fmt.Errorf("decode targets: %w", err)
	}
	if t == nil {
		return 0, false, nil
	}
	if t != json.Delim('{') {
		return 0, false, errors.New("decode targets: expected object or null")
	}
	// Last descriptor wins, including its semantic error. Syntax errors are fatal.
	summaries := make(map[string]targetSummary)
	for dec.More() {
		t, err := parserToken(ctx, dec)
		if err != nil {
			return 0, false, fmt.Errorf("decode targets: %w", err)
		}
		descriptor := t.(string)
		rawID, scope, found := strings.Cut(strings.TrimPrefix(descriptor, "tvdb_show:"), ":")
		id, convErr := strconv.Atoi(rawID)
		if !strings.HasPrefix(descriptor, "tvdb_show:") || !found || convErr != nil || id <= 0 {
			if err := skipValue(ctx, dec, 1); err != nil {
				return 0, false, fmt.Errorf("decode targets: %w", err)
			}
			continue
		}
		episodes, semanticErr, err := scanEpisodeRanges(ctx, dec)
		if err != nil {
			return 0, false, fmt.Errorf("decode targets: %w", err)
		}
		summaries[descriptor] = targetSummary{id: id, episodes: episodes, isS1: scope == "s1", err: semanticErr}
	}
	if _, err := parserToken(ctx, dec); err != nil {
		return 0, false, fmt.Errorf("decode targets: %w", err)
	}
	best := targetSummary{episodes: -1}
	for descriptor, candidate := range summaries {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		if candidate.err != nil {
			return 0, false, fmt.Errorf("count episodes for %s: %w", descriptor, candidate.err)
		}
		if candidate.isS1 != best.isS1 {
			if candidate.isS1 {
				best = candidate
			}
			continue
		}
		if candidate.episodes > best.episodes || (candidate.episodes == best.episodes && candidate.id < best.id) {
			best = candidate
		}
	}
	return best.id, best.id > 0, nil
}

// Semantic errors can be overwritten by a later target descriptor. Reader and
// syntax errors cannot; they are returned separately to stop parsing immediately.
func scanEpisodeRanges(ctx context.Context, dec *json.Decoder) (int, error, error) {
	t, err := parserToken(ctx, dec)
	if err != nil || t == nil {
		return 0, nil, err
	}
	if t != json.Delim('{') {
		return 0, errors.New("expected episode object or null"), skipToken(ctx, dec, t, 1)
	}
	ranges := make(map[string]struct{})
	var semanticErr error
	for dec.More() {
		t, err := parserToken(ctx, dec)
		if err != nil {
			return 0, nil, err
		}
		ranges[t.(string)] = struct{}{}
		t, err = parserToken(ctx, dec)
		if err != nil {
			return 0, nil, err
		}
		if _, ok := t.(string); !ok && t != nil {
			semanticErr = errors.New("expected string or null episode destination")
		}
		if err := skipToken(ctx, dec, t, 2); err != nil {
			return 0, nil, err
		}
	}
	if _, err := parserToken(ctx, dec); err != nil {
		return 0, nil, err
	}
	if semanticErr != nil {
		return 0, semanticErr, nil
	}
	var total int
	for srcRange := range ranges {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		count, err := sourceRangeCount(srcRange)
		if err != nil {
			return 0, err, nil
		}
		total += count
	}
	return total, nil, nil
}

func sourceRangeCount(srcRange string) (int, error) {
	if srcRange == "" {
		return 0, errors.New("empty episode range")
	}
	first, last, hasEnd := strings.Cut(srcRange, "-")
	start, err := strconv.Atoi(first)
	if err != nil || start <= 0 {
		return 0, fmt.Errorf("invalid episode range %q", srcRange)
	}
	if !hasEnd || last == "" {
		return 1, nil
	}
	end, err := strconv.Atoi(last)
	if err != nil || end < start {
		return 0, fmt.Errorf("invalid episode range %q", srcRange)
	}
	return end - start + 1, nil
}
