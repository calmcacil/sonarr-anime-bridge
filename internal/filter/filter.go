// Package filter compacts caller-owned show slices in place, preserving order.
package filter

import (
	"slices"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
)

type Config struct {
	ExcludeTags []string
}

type FilterStats struct {
	SkippedDuration int
	SkippedTags     int
}

type FutureStats struct {
	SkippedFuture int
}

func FilterWithStats(shows []anilist.Show, cfg Config) ([]anilist.Show, FilterStats) {
	filtered := shows[:0]
	var stats FilterStats
	for _, show := range shows {
		if show.SkipByDuration() {
			stats.SkippedDuration++
			continue
		}

		if hasExcludedTag(show, cfg.ExcludeTags) {
			stats.SkippedTags++
			continue
		}

		filtered = append(filtered, show)
	}
	return filtered, stats
}

func hasExcludedTag(show anilist.Show, tags []string) bool {
	for _, exclude := range tags {
		if exclude == "" {
			continue
		}
		if show.HasTag(exclude) {
			return true
		}
	}
	return false
}

func FilterFutureWithStats(shows []anilist.Show, aheadMonths int) ([]anilist.Show, FutureStats) {
	var stats FutureStats
	if aheadMonths <= 0 {
		return shows, stats
	}
	filtered := shows[:0]
	for _, show := range shows {
		if !show.IsWithinMonths(aheadMonths) {
			stats.SkippedFuture++
			continue
		}
		filtered = append(filtered, show)
	}
	return filtered, stats
}

func FilterByFormats(shows []anilist.Show, formats []string) []anilist.Show {
	out := shows[:0]
	for _, sh := range shows {
		if slices.Contains(formats, sh.Format) {
			out = append(out, sh)
		}
	}
	return out
}

func FilterBySeason(shows []anilist.Show, season string) []anilist.Show {
	if season == "ALL" {
		return shows
	}
	out := shows[:0]
	for _, sh := range shows {
		if sh.Season == season {
			out = append(out, sh)
			continue
		}
		if sh.Season != "" {
			continue
		}
		if sh.StartDate.Month == nil {
			continue
		}
		m := *sh.StartDate.Month
		if m < 1 || m > 12 {
			continue
		}
		switch season {
		case "WINTER":
			if m == 12 || m == 1 || m == 2 || m == 3 {
				out = append(out, sh)
			}
		case "SPRING":
			if m >= 4 && m <= 6 {
				out = append(out, sh)
			}
		case "SUMMER":
			if m >= 7 && m <= 9 {
				out = append(out, sh)
			}
		case "FALL":
			if m == 10 || m == 11 {
				out = append(out, sh)
			}
		}
	}
	return out
}

func FilterFirstSeason(shows []anilist.Show) []anilist.Show {
	out := shows[:0]
	for _, sh := range shows {
		if sh.IsNew() {
			out = append(out, sh)
		}
	}
	return out
}
