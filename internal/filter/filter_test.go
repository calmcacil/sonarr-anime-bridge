package filter

import (
	"slices"
	"testing"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
	"github.com/calmcacil/sonarr-anime-bridge/internal/testutil"
)

func ids(shows []anilist.Show) []int {
	out := make([]int, 0, len(shows))
	for _, s := range shows {
		out = append(out, s.ID)
	}
	return out
}

func tagged(id int, tag string) anilist.Show {
	return anilist.Show{ID: id, Tags: []anilist.Tag{{Name: tag}}}
}

func related(id int, relation string) anilist.Show {
	return anilist.Show{ID: id, Relations: &anilist.RelationBlock{Edges: []anilist.RelationEdge{{RelationType: relation}}}}
}

func seasonal(id int, season string) anilist.Show {
	return anilist.Show{ID: id, Season: season}
}

func startMonth(id, month int) anilist.Show {
	return anilist.Show{ID: id, StartDate: anilist.FuzzyDate{Month: testutil.Ptr(month)}}
}

func starting(id, year, month int) anilist.Show {
	return anilist.Show{ID: id, StartDate: anilist.FuzzyDate{Year: testutil.Ptr(year), Month: testutil.Ptr(month)}}
}

func TestFilterWithStats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                       string
		shows                      []anilist.Show
		excludeTags                []string
		wantIDs                    []int
		wantSkipDuration, wantTags int
	}{
		{
			name: "skips short duration",
			shows: []anilist.Show{
				{ID: 1, Duration: testutil.Ptr(24)},
				{ID: 2, Duration: testutil.Ptr(6)},
				{ID: 3, Duration: testutil.Ptr(10)},
				{ID: 4},
			},
			wantIDs:          []int{1, 4},
			wantSkipDuration: 2,
		},
		{
			name:        "excludes tags",
			shows:       []anilist.Show{tagged(1, "Action"), tagged(2, "Hentai"), tagged(3, "Comedy")},
			excludeTags: []string{"Hentai"},
			wantIDs:     []int{1, 3},
			wantTags:    1,
		},
		{
			name:        "ignores empty exclusions and scans remaining tags",
			shows:       []anilist.Show{{ID: 1, Tags: []anilist.Tag{{Name: "Action"}, {Name: "Hentai"}}}, tagged(2, "Comedy")},
			excludeTags: []string{"", "Guro", "Hentai"},
			wantIDs:     []int{2},
			wantTags:    1,
		},
		{
			name:        "excludes tags case-insensitively",
			shows:       []anilist.Show{tagged(1, "HENTAI")},
			excludeTags: []string{"hentai"},
			wantIDs:     []int{},
			wantTags:    1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, stats := FilterWithStats(slices.Clone(tc.shows), Config{ExcludeTags: tc.excludeTags})
			if !slices.Equal(ids(got), tc.wantIDs) {
				t.Errorf("IDs = %v, want %v", ids(got), tc.wantIDs)
			}
			if stats.SkippedDuration != tc.wantSkipDuration || stats.SkippedTags != tc.wantTags {
				t.Errorf("stats = %+v, want SkippedDuration=%d SkippedTags=%d", stats, tc.wantSkipDuration, tc.wantTags)
			}
		})
	}
}

func TestFilterFutureWithStats(t *testing.T) {
	t.Parallel()

	shows := []anilist.Show{starting(1, 2099, 12), starting(2, 2020, 1)}
	tests := []struct {
		name        string
		aheadMonths int
		wantIDs     []int
		wantSkipped int
	}{
		{"removes future shows", 3, []int{2}, 1},
		{"no limit when months is zero", 0, []int{1, 2}, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, stats := FilterFutureWithStats(slices.Clone(shows), tc.aheadMonths)
			if !slices.Equal(ids(got), tc.wantIDs) || stats.SkippedFuture != tc.wantSkipped {
				t.Errorf("IDs = %v SkippedFuture = %d, want %v and %d", ids(got), stats.SkippedFuture, tc.wantIDs, tc.wantSkipped)
			}
		})
	}
}

func TestFilterFirstSeason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		shows   []anilist.Show
		wantIDs []int
	}{
		{"no relations", []anilist.Show{{ID: 1, Title: anilist.Title{English: testutil.Ptr("Original Show")}}}, []int{1}},
		{"empty relations", []anilist.Show{{ID: 2, Relations: &anilist.RelationBlock{}}}, []int{2}},
		{"has prequel", []anilist.Show{related(3, "PREQUEL")}, []int{}},
		{"has parent", []anilist.Show{related(4, "PARENT")}, []int{}},
		{"has sequel only", []anilist.Show{related(5, "SEQUEL")}, []int{5}},
		{"mixed", []anilist.Show{{ID: 1}, related(2, "PREQUEL"), related(3, "PARENT"), related(4, "SEQUEL")}, []int{1, 4}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ids(FilterFirstSeason(slices.Clone(tc.shows))); !slices.Equal(got, tc.wantIDs) {
				t.Errorf("IDs = %v, want %v", got, tc.wantIDs)
			}
		})
	}
}

func TestFilterByFormats(t *testing.T) {
	t.Parallel()

	shows := []anilist.Show{
		{ID: 1, Format: "TV"}, {ID: 2, Format: "ONA"}, {ID: 3, Format: "TV_SHORT"},
		{ID: 4, Format: "MOVIE"}, {ID: 5, Format: "OVA"}, {ID: 6, Format: "SPECIAL"},
	}
	tests := []struct {
		name    string
		formats []string
		wantIDs []int
	}{
		{"keeps listed formats", []string{"TV", "ONA", "TV_SHORT"}, []int{1, 2, 3}},
		{"nil formats", nil, []int{}},
		{"empty formats", []string{}, []int{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ids(FilterByFormats(slices.Clone(shows), tc.formats)); !slices.Equal(got, tc.wantIDs) {
				t.Errorf("IDs = %v, want %v", got, tc.wantIDs)
			}
		})
	}
}

func TestFilterBySeason(t *testing.T) {
	t.Parallel()

	allSeasons := []anilist.Show{seasonal(1, "SPRING"), seasonal(2, "SUMMER"), seasonal(3, "FALL"), seasonal(4, "WINTER")}
	tests := []struct {
		name    string
		season  string
		shows   []anilist.Show
		wantIDs []int
	}{
		{"WINTER excludes other seasons", "WINTER", allSeasons, []int{4}},
		{"SPRING excludes other seasons", "SPRING", allSeasons, []int{1}},
		{"SUMMER excludes other seasons", "SUMMER", allSeasons, []int{2}},
		{"FALL excludes other seasons", "FALL", allSeasons, []int{3}},
		{"fallback WINTER/Dec", "WINTER", []anilist.Show{startMonth(1, 12)}, []int{1}},
		{"fallback WINTER/Jan", "WINTER", []anilist.Show{startMonth(1, 1)}, []int{1}},
		{"fallback WINTER/Mar", "WINTER", []anilist.Show{startMonth(1, 3)}, []int{1}},
		{"fallback WINTER/Apr", "WINTER", []anilist.Show{startMonth(1, 4)}, []int{}},
		{"fallback SPRING/Apr", "SPRING", []anilist.Show{startMonth(1, 4)}, []int{1}},
		{"fallback SPRING/Mar", "SPRING", []anilist.Show{startMonth(1, 3)}, []int{}},
		{"fallback SUMMER/Jul", "SUMMER", []anilist.Show{startMonth(1, 7)}, []int{1}},
		{"fallback SUMMER/Jun", "SUMMER", []anilist.Show{startMonth(1, 6)}, []int{}},
		{"fallback FALL/Oct", "FALL", []anilist.Show{startMonth(1, 10)}, []int{1}},
		{"fallback FALL/Dec", "FALL", []anilist.Show{startMonth(1, 12)}, []int{}},
		{"fallback out-of-range month", "WINTER", []anilist.Show{startMonth(1, 13)}, []int{}},
		{"empty season with unknown month", "WINTER", []anilist.Show{{ID: 1}}, []int{}},
		{"ALL keeps everything", "ALL", []anilist.Show{seasonal(1, "WINTER"), seasonal(2, "SUMMER"), {ID: 3}}, []int{1, 2, 3}},
		{"unknown season", "INVALID", []anilist.Show{seasonal(1, "WINTER")}, []int{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ids(FilterBySeason(slices.Clone(tc.shows), tc.season)); !slices.Equal(got, tc.wantIDs) {
				t.Errorf("FilterBySeason(%q) IDs = %v, want %v", tc.season, got, tc.wantIDs)
			}
		})
	}
}
