package mapping

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
)

// ResolvedShow is the per-show resolution result handed back to the
// scheduler. Fields are kept stable so the scheduler's loop can stay the
// same when the underlying data source changes.
type ResolvedShow struct {
	TVDBID   int
	Title    string
	Resolved bool
}

// Resolver turns AniList show records into TVDB IDs using an in-memory
// anibridge mapping. The mapping can be swapped at runtime (e.g. after a
// scheduled refresh) without blocking lookups.
type Resolver struct {
	mapping atomic.Pointer[AnibridgeMapping]
}

// NewResolver creates a Resolver with no mapping set. Callers must invoke
// SetMapping before any Resolve/ResolveBatch call, or those calls will
// return zero results.
func NewResolver() *Resolver {
	return &Resolver{}
}

// SetMapping atomically replaces the underlying mapping. Lookups already
// in flight may continue using the old mapping; that's fine because the
// shape of the API (lookups by ID) is read-only.
func (r *Resolver) SetMapping(m *AnibridgeMapping) {
	if m == nil {
		return
	}
	r.mapping.Store(m)
}

// Mapping returns the currently-active mapping, or nil if none is set.
func (r *Resolver) Mapping() *AnibridgeMapping {
	return r.mapping.Load()
}

// Resolve looks up a single show. MAL is tried first; if no MAL ID is
// present or the MAL lookup misses, AniList is used as a fallback.
func (r *Resolver) Resolve(s anilist.Show) (int, bool) {
	m := r.mapping.Load()
	if m == nil {
		return 0, false
	}

	debug := slog.Default().Enabled(context.Background(), slog.LevelDebug)
	if s.IDMal != nil && *s.IDMal > 0 {
		if tvdbID, ok := m.LookupByMAL(*s.IDMal); ok {
			if debug {
				slog.Debug("resolved via anibridge (MAL)", "type", "resolver",
					"title", s.DisplayTitle(),
					"anilist", s.ID, "mal", *s.IDMal, "tvdb", tvdbID)
			}
			return tvdbID, true
		}
	}

	if s.ID > 0 {
		if tvdbID, ok := m.LookupByAniList(s.ID); ok {
			if debug {
				slog.Debug("resolved via anibridge (AniList fallback)", "type", "resolver",
					"title", s.DisplayTitle(),
					"anilist", s.ID, "tvdb", tvdbID)
			}
			return tvdbID, true
		}
	}

	return 0, false
}

// ResolveBatch resolves every show in the slice and returns a map keyed by
// the show's AniList ID. The returned ResolvedShow.Resolved flag is true
// for shows that successfully mapped to a TVDB ID.
func (r *Resolver) ResolveBatch(shows []anilist.Show) map[int]ResolvedShow {
	result := make(map[int]ResolvedShow, len(shows))
	for _, show := range shows {
		rs := ResolvedShow{
			Title: show.DisplayTitle(),
		}
		if tvdbID, ok := r.Resolve(show); ok {
			rs.TVDBID = tvdbID
			rs.Resolved = true
		}
		result[show.ID] = rs
	}
	return result
}
