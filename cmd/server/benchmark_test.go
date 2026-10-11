package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
	"github.com/calmcacil/sonarr-anime-bridge/internal/testutil"
)

func BenchmarkListHit(b *testing.B) {
	benchmarkListHit(b, "SUMMER", false)
}

func BenchmarkListWinterHit(b *testing.B) {
	benchmarkListHit(b, "WINTER", false)
}

func BenchmarkListHitParallel(b *testing.B) {
	benchmarkListHit(b, "SUMMER", true)
}

func benchmarkListHit(b *testing.B, season string, parallel bool) {
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	b.Cleanup(func() { slog.SetDefault(previous) })
	c := newTestCache(b)
	s := newReadyScheduler(b, c)
	raw, err := os.ReadFile("../../testdata/pipeline-year.json")
	if err != nil {
		b.Fatal(err)
	}
	var fixture []anilist.Show
	if err := json.Unmarshal(raw, &fixture); err != nil {
		b.Fatal(err)
	}
	shows := make([]anilist.Show, 600)
	for i := range shows {
		shows[i] = fixture[i%len(fixture)]
		shows[i].ID = i + 1000
		shows[i].IDMal = testutil.Ptr(16498)
		shows[i].Tags = append([]anilist.Tag{{Name: "Action"}, {Name: "School"}, {Name: "Magic"}, {Name: "Fantasy"}, {Name: "Adventure"}, {Name: "Shounen"}, {Name: "Comedy"}}, shows[i].Tags...)
		if shows[i].Relations == nil {
			shows[i].Relations = &anilist.RelationBlock{Edges: []anilist.RelationEdge{{RelationType: "SEQUEL"}}}
		}
	}
	raw, err = json.Marshal(shows)
	if err != nil {
		b.Fatal(err)
	}
	if err := c.SetYearContext(context.Background(), time.Now().Year(), raw); err != nil {
		b.Fatal(err)
	}
	if season == "WINTER" {
		if err := c.SetYearContext(context.Background(), time.Now().Year()-1, raw); err != nil {
			b.Fatal(err)
		}
	}
	h := handleList(c, s, listCfg)
	url := "/list?season=" + season
	// Hydrate before timing the steady-state path.
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, url, nil))
	if w.Code != http.StatusOK {
		b.Fatalf("warm response: %d", w.Code)
	}
	b.ReportAllocs()
	b.ResetTimer()
	request := func() {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodGet, url, nil))
		if w.Code != http.StatusOK {
			b.Fatalf("response: %d", w.Code)
		}
	}
	if parallel {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				request()
			}
		})
		return
	}
	for b.Loop() {
		request()
	}
}
