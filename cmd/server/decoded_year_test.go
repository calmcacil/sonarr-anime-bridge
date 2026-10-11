package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
)

func TestHandleListDecodedIsolationAndClear(t *testing.T) {
	c := newTestCache(t)
	s := newReadyScheduler(t, c, fakeFetcher{shows: []anilist.Show{}})
	year := time.Now().Year()
	for _, input := range []struct {
		name string
		year int
	}{{"pipeline-year.json", year}, {"pipeline-prior.json", year - 1}} {
		raw, err := os.ReadFile("../../testdata/" + input.name)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.SetYearContext(context.Background(), input.year, raw); err != nil {
			t.Fatal(err)
		}
	}
	h := handleList(c, s, listCfg)
	inputs := make(map[int][]byte)
	for _, y := range []int{year, year - 1} {
		data, err := c.PeekDecodedYearContext(context.Background(), y)
		if err != nil {
			t.Fatal(err)
		}
		inputs[y], err = json.Marshal(data.Shows)
		if err != nil {
			t.Fatal(err)
		}
	}
	expected := make(map[string]string)
	for _, season := range []string{"ALL", "WINTER", "SPRING", "SUMMER", "FALL"} {
		for _, category := range []string{"series", "series-new"} {
			url := fmt.Sprintf("/list?season=%s&year=%d&category=%s", season, year, category)
			expected[url] = serve(t, h, "GET", url).Body.String()
		}
	}
	var wg sync.WaitGroup
	for range 10 {
		for url, want := range expected {
			wg.Go(func() {
				w := serve(t, h, "GET", url)
				if w.Code != http.StatusOK || w.Body.String() != want {
					t.Errorf("%s: %d %s, want %s", url, w.Code, w.Body.String(), want)
				}
			})
		}
	}
	wg.Wait()
	for y, before := range inputs {
		data, err := c.PeekDecodedYearContext(context.Background(), y)
		if err != nil {
			t.Fatal(err)
		}
		after, err := json.Marshal(data.Shows)
		if err != nil || !slices.Equal(before, after) {
			t.Fatalf("year %d resident object changed", y)
		}
	}
	clear := serve(t, handleCacheClear(c, &config.Config{DebugEndpointsEnabled: true}), "POST", "/cache/clear")
	if clear.Code != http.StatusOK {
		t.Fatal(clear.Code)
	}
	w := serve(t, h, "GET", fmt.Sprintf("/list?season=SUMMER&year=%d", year))
	if w.Code != http.StatusOK || w.Body.String() != "[]" {
		t.Fatalf("clear reused old decoding: %d %s", w.Code, w.Body.String())
	}
}

func TestHandleListWarmStorageFailure(t *testing.T) {
	c := newTestCache(t)
	s := newReadyScheduler(t, c)
	year := time.Now().Year()
	seedYear(t, c, year, `[{"id":1,"idMal":16498,"format":"TV","season":"SUMMER"}]`)
	h := handleList(c, s, listCfg)
	if w := serve(t, h, "GET", "/list?season=SUMMER"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if w := serve(t, h, "GET", "/list?season=SUMMER"); w.Code != 500 {
		t.Fatalf("warm memory hid storage failure: %d", w.Code)
	}
}

// Schedule admission must be reflected in the request metadata during shutdown.
func TestHandleListRejectedStaleRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	c, err := cache.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	s := newReadyScheduler(t, c)
	year := time.Now().Year()
	seedYear(t, c, year, `[]`)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE year_cache SET fetched_at=?`, time.Now().Add(-48*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	metadata := new(requestLogMetadata)
	r := httptest.NewRequest("GET", "/list?season=SUMMER", nil)
	r = r.WithContext(context.WithValue(r.Context(), requestMetadataKey{}, metadata))
	w := httptest.NewRecorder()
	handleList(c, s, listCfg)(w, r)
	if w.Code != 200 || metadata.cacheState != "stale" || metadata.refreshScheduled {
		t.Fatalf("rejected refresh reported accepted: %d %+v", w.Code, metadata)
	}
}
