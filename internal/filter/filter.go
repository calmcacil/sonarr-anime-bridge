package filter

import "github.com/calmcacil/sonarr-anime-bridge/internal/anilist"

type Config struct {
	ExcludeTags []string
}

type FilterStats struct {
	Input           int
	Output          int
	SkippedDuration int
	SkippedTags     int
}

type FutureStats struct {
	Input         int
	Output        int
	SkippedFuture int
}

func Filter(shows []anilist.Show, cfg Config) []anilist.Show {
	filtered, _ := FilterWithStats(shows, cfg)
	return filtered
}

func FilterWithStats(shows []anilist.Show, cfg Config) ([]anilist.Show, FilterStats) {
	filtered := make([]anilist.Show, 0, len(shows))
	stats := FilterStats{Input: len(shows)}
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

	stats.Output = len(filtered)
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

func FilterFuture(shows []anilist.Show, aheadMonths int) []anilist.Show {
	filtered, _ := FilterFutureWithStats(shows, aheadMonths)
	return filtered
}

func FilterFutureWithStats(shows []anilist.Show, aheadMonths int) ([]anilist.Show, FutureStats) {
	stats := FutureStats{Input: len(shows)}
	if aheadMonths <= 0 {
		stats.Output = len(shows)
		return shows, stats
	}
	filtered := make([]anilist.Show, 0, len(shows))
	for _, show := range shows {
		if !show.IsWithinMonths(aheadMonths) {
			stats.SkippedFuture++
			continue
		}
		filtered = append(filtered, show)
	}
	stats.Output = len(filtered)
	return filtered, stats
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
