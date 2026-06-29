package filter

import (
	"context"
	"log/slog"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
)

type Config struct {
	ExcludeTags []string
}

func Filter(shows []anilist.Show, cfg Config) []anilist.Show {
	filtered := make([]anilist.Show, 0, len(shows))
	debug := slog.Default().Enabled(context.Background(), slog.LevelDebug)
	for _, show := range shows {
		if show.SkipByDuration() {
			if debug {
				slog.Debug("skipped show (duration <= 10 min)", "type", "filter",
					"title", show.DisplayTitle(),
					"duration", show.Duration)
			}
			continue
		}

		if hasExcludedTag(show, cfg.ExcludeTags) {
			if debug {
				slog.Debug("skipped show (excluded tag)", "type", "filter",
					"title", show.DisplayTitle(),
					"tags", show.Tags)
			}
			continue
		}

		filtered = append(filtered, show)
	}

	skipped := len(shows) - len(filtered)
	if skipped > 0 {
		slog.Debug("filtered shows", "type", "filter",
			"total", len(shows),
			"skipped", skipped,
			"remaining", len(filtered))
	}

	return filtered
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

func FilterFuture(shows []anilist.Show, aheadMonths int) []anilist.Show {
	if aheadMonths <= 0 {
		return shows
	}
	filtered := make([]anilist.Show, 0, len(shows))
	debug := slog.Default().Enabled(context.Background(), slog.LevelDebug)
	for _, show := range shows {
		if !show.IsWithinMonths(aheadMonths) {
			if debug {
				slog.Debug("skipped show (too far in the future)", "type", "filter",
					"title", show.DisplayTitle(),
					"ahead_months", aheadMonths)
			}
			continue
		}
		filtered = append(filtered, show)
	}
	return filtered
}

func FilterByFormats(shows []anilist.Show, formats []string) []anilist.Show {
	valid := make(map[string]bool, len(formats))
	for _, f := range formats {
		valid[f] = true
	}
	out := make([]anilist.Show, 0, len(shows))
	for _, sh := range shows {
		if valid[sh.Format] {
			out = append(out, sh)
		}
	}
	return out
}

func FilterBySeason(shows []anilist.Show, season string) []anilist.Show {
	if season == "ALL" {
		return shows
	}
	out := make([]anilist.Show, 0, len(shows))
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
	out := make([]anilist.Show, 0, len(shows))
	for _, sh := range shows {
		if sh.IsNew() {
			out = append(out, sh)
		}
	}
	return out
}
