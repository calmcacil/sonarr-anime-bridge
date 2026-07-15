package anilist

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/testutil"
)

func TestIsSeries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		format string
		want   bool
	}{
		{"TV", true},
		{"ONA", true},
		{"MOVIE", false},
		{"OVA", false},
		{"SPECIAL", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.format, func(t *testing.T) {
			s := Show{Format: tc.format}
			if got := s.IsSeries(); got != tc.want {
				t.Errorf("IsSeries() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsNew(t *testing.T) {
	t.Parallel()

	t.Run("no relations", func(t *testing.T) {
		s := Show{Relations: nil}
		if !s.IsNew() {
			t.Error("expected IsNew() = true with nil relations")
		}
	})

	t.Run("empty relations", func(t *testing.T) {
		s := Show{Relations: &RelationBlock{Edges: nil}}
		if !s.IsNew() {
			t.Error("expected IsNew() = true with empty edges")
		}
	})

	t.Run("has prequel", func(t *testing.T) {
		s := Show{Relations: &RelationBlock{
			Edges: []RelationEdge{{RelationType: "PREQUEL"}},
		}}
		if s.IsNew() {
			t.Error("expected IsNew() = false with PREQUEL")
		}
	})

	t.Run("has parent", func(t *testing.T) {
		s := Show{Relations: &RelationBlock{
			Edges: []RelationEdge{{RelationType: "PARENT"}},
		}}
		if s.IsNew() {
			t.Error("expected IsNew() = false with PARENT")
		}
	})

	t.Run("has unrelated relation", func(t *testing.T) {
		s := Show{Relations: &RelationBlock{
			Edges: []RelationEdge{{RelationType: "SEQUEL"}},
		}}
		if !s.IsNew() {
			t.Error("expected IsNew() = true with SEQUEL edge")
		}
	})
}

func TestSkipByDuration(t *testing.T) {
	t.Parallel()

	t.Run("nil duration", func(t *testing.T) {
		s := Show{Duration: nil}
		if s.SkipByDuration() {
			t.Error("expected false for nil duration")
		}
	})

	t.Run("short duration", func(t *testing.T) {
		s := Show{Duration: testutil.Ptr(6)}
		if !s.SkipByDuration() {
			t.Error("expected true for duration <= 10")
		}
	})

	t.Run("exact boundary", func(t *testing.T) {
		s := Show{Duration: testutil.Ptr(10)}
		if !s.SkipByDuration() {
			t.Error("expected true for duration == 10")
		}
	})

	t.Run("long duration", func(t *testing.T) {
		s := Show{Duration: testutil.Ptr(24)}
		if s.SkipByDuration() {
			t.Error("expected false for duration > 10")
		}
	})
}

func TestHasTag(t *testing.T) {
	t.Parallel()

	s := Show{Tags: []Tag{
		{Name: "Action"},
		{Name: "Hentai"},
		{Name: "Sci-Fi"},
	}}

	if !s.HasTag("Action") {
		t.Error("expected Action tag to match")
	}
	if !s.HasTag("action") {
		t.Error("expected case-insensitive match")
	}
	if !s.HasTag("HENTAI") {
		t.Error("expected case-insensitive match for HENTAI")
	}
	if s.HasTag("Comedy") {
		t.Error("expected Comedy tag not to match")
	}
}

func TestIsWithinMonths(t *testing.T) {
	t.Parallel()

	now := time.Now()

	t.Run("nil date", func(t *testing.T) {
		s := Show{StartDate: FuzzyDate{Year: nil, Month: nil}}
		if !s.IsWithinMonths(3) {
			t.Error("expected true for unknown date")
		}
	})

	t.Run("nil month", func(t *testing.T) {
		s := Show{StartDate: FuzzyDate{Year: testutil.Ptr(2026), Month: nil}}
		if !s.IsWithinMonths(3) {
			t.Error("expected true when month is nil")
		}
	})

	t.Run("past date", func(t *testing.T) {
		year := now.Year() - 1
		s := Show{StartDate: FuzzyDate{Year: &year, Month: testutil.Ptr(1)}}
		if !s.IsWithinMonths(3) {
			t.Error("expected true for past date")
		}
	})

	t.Run("future date within range", func(t *testing.T) {
		futureMonth := int(now.AddDate(0, 2, 0).Month())
		futureYear := now.Year()
		if futureMonth == 1 && now.Month() == 12 {
			futureYear++
		}
		s := Show{StartDate: FuzzyDate{Year: &futureYear, Month: &futureMonth}}
		if !s.IsWithinMonths(3) {
			t.Error("expected true for date within range")
		}
	})

	t.Run("far future date", func(t *testing.T) {
		year := 2099
		s := Show{StartDate: FuzzyDate{Year: &year, Month: testutil.Ptr(12)}}
		if s.IsWithinMonths(12) {
			t.Error("expected false for far future date")
		}
	})
}

func TestDisplayTitle(t *testing.T) {
	t.Parallel()

	t.Run("english title preferred", func(t *testing.T) {
		s := Show{Title: Title{
			English: testutil.Ptr("Attack on Titan"),
			Romaji:  testutil.Ptr("Shingeki no Kyojin"),
		}}
		if got := s.DisplayTitle(); got != "Attack on Titan" {
			t.Errorf("got %q, want %q", got, "Attack on Titan")
		}
	})

	t.Run("empty english falls back to romaji", func(t *testing.T) {
		s := Show{Title: Title{
			English: testutil.Ptr(""),
			Romaji:  testutil.Ptr("Shingeki no Kyojin"),
		}}
		if got := s.DisplayTitle(); got != "Shingeki no Kyojin" {
			t.Errorf("got %q, want %q", got, "Shingeki no Kyojin")
		}
	})

	t.Run("no english uses romaji", func(t *testing.T) {
		s := Show{Title: Title{
			English: nil,
			Romaji:  testutil.Ptr("Shingeki no Kyojin"),
		}}
		if got := s.DisplayTitle(); got != "Shingeki no Kyojin" {
			t.Errorf("got %q, want %q", got, "Shingeki no Kyojin")
		}
	})

	t.Run("no titles uses ID", func(t *testing.T) {
		s := Show{ID: 42, Title: Title{English: nil, Romaji: nil}}
		if got := s.DisplayTitle(); got != "Anime #42" {
			t.Errorf("got %q, want %q", got, "Anime #42")
		}
	})
}

func TestIsWinterStart(t *testing.T) {
	t.Parallel()

	t.Run("nil month", func(t *testing.T) {
		s := Show{StartDate: FuzzyDate{Month: nil}}
		if !s.IsWinterStart() {
			t.Error("expected true when month is nil")
		}
	})

	t.Run("december", func(t *testing.T) {
		s := Show{StartDate: FuzzyDate{Month: testutil.Ptr(12)}}
		if !s.IsWinterStart() {
			t.Error("expected true for December")
		}
	})

	t.Run("january", func(t *testing.T) {
		s := Show{StartDate: FuzzyDate{Month: testutil.Ptr(1)}}
		if !s.IsWinterStart() {
			t.Error("expected true for January")
		}
	})

	t.Run("february", func(t *testing.T) {
		s := Show{StartDate: FuzzyDate{Month: testutil.Ptr(2)}}
		if !s.IsWinterStart() {
			t.Error("expected true for February")
		}
	})

	t.Run("march", func(t *testing.T) {
		s := Show{StartDate: FuzzyDate{Month: testutil.Ptr(3)}}
		if !s.IsWinterStart() {
			t.Error("expected true for March")
		}
	})

	t.Run("april", func(t *testing.T) {
		s := Show{StartDate: FuzzyDate{Month: testutil.Ptr(4)}}
		if s.IsWinterStart() {
			t.Error("expected false for April")
		}
	})

	t.Run("july", func(t *testing.T) {
		s := Show{StartDate: FuzzyDate{Month: testutil.Ptr(7)}}
		if s.IsWinterStart() {
			t.Error("expected false for July")
		}
	})

	t.Run("november", func(t *testing.T) {
		s := Show{StartDate: FuzzyDate{Month: testutil.Ptr(11)}}
		if s.IsWinterStart() {
			t.Error("expected false for November")
		}
	})
}

func TestClient_ConcurrentThrottle(t *testing.T) {
	t.Parallel()

	c := New()
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.throttle(ctx); err != nil {
				t.Errorf("throttle: %v", err)
			}
		}()
	}
	wg.Wait()
}
