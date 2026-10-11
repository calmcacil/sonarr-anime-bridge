package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
)

func integrationBatch(start int, next bool) []byte {
	data := map[string]any{}
	for i := range 4 {
		data[fmt.Sprintf("page%d", i)] = map[string]any{
			"pageInfo": map[string]any{"currentPage": start + i, "hasNextPage": next}, "media": []anilist.Show{},
		}
	}
	body, _ := json.Marshal(map[string]any{"data": data})
	return body
}

func TestBatchedFetchPersistence(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"empty", "missing failure", "refresh failure", "write failure"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "cache.db")
			c, err := cache.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })
			old := []byte(`[{"id":42}]`)
			if mode == "refresh failure" {
				if err := c.SetYearContext(context.Background(), 2025, old); err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE year_cache SET fetched_at=0`); err != nil {
					t.Fatal(err)
				}
				db.Close()
			}
			var calls atomic.Int32
			client := anilist.NewWithHTTPClient("https://example.invalid", &http.Client{Transport: performanceTransport(func(*http.Request) (*http.Response, error) {
				call := calls.Add(1)
				body := integrationBatch(1, false)
				if mode == "missing failure" || mode == "refresh failure" {
					body = integrationBatch(1, true)
					if call > 1 {
						body = []byte(`{}`)
					}
				}
				if mode == "write failure" {
					c.Close()
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
			})})
			s, _ := performanceScheduler(t, c, client, nil)
			err = s.FetchAndStore(context.Background(), 2025, "test")
			if mode == "empty" {
				data, _, present, _ := c.GetYearContext(context.Background(), 2025)
				if err != nil || !present || string(data) != "[]" {
					t.Fatalf("empty year not persisted: %s %v", data, err)
				}
				return
			}
			if err == nil {
				t.Fatal("failed fetch/write published success")
			}
			if mode == "write failure" {
				if calls.Load() != 1 {
					t.Fatal("write failure did not follow a completed upstream fetch")
				}
				return
			}
			data, _, present, _ := c.GetYearContext(context.Background(), 2025)
			if mode == "refresh failure" {
				if !present || !bytes.Equal(data, old) {
					t.Fatal("refresh replaced valid year")
				}
			} else if present {
				t.Fatal("partial missing year persisted")
			}
		})
	}
}

func TestBatchedWarmHitsDuringSharedCooldown(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	for _, year := range []int{2024, 2025} {
		if err := c.SetYearContext(context.Background(), year, []byte(`[]`)); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan struct{})
	var calls atomic.Int32
	client := anilist.NewWithHTTPClient("https://example.invalid", &http.Client{Transport: performanceTransport(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})})
	s, cfg := performanceScheduler(t, c, client, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.FetchAndStore(ctx, 2023, "test") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("fetch did not start")
	}
	for _, season := range []string{"SUMMER", "WINTER"} {
		start := time.Now()
		w := httptest.NewRecorder()
		handleList(c, s, cfg)(w, httptest.NewRequest(http.MethodGet, "/list?year=2025&season="+season, nil))
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" || time.Since(start) > time.Second || calls.Load() != 1 {
			t.Fatal("warm/Winter hit waited for upstream")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled fetch succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cooldown ignored cancellation")
	}
}

func TestBatchedColdCallersShareYearFetch(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	var calls atomic.Int32
	client := anilist.NewWithHTTPClient("https://example.invalid", &http.Client{Transport: performanceTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(25 * time.Millisecond)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(integrationBatch(1, false)))}, nil
	})})
	s, cfg := performanceScheduler(t, c, client, nil)
	h := handleList(c, s, cfg)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			w := httptest.NewRecorder()
			h(w, httptest.NewRequest(http.MethodGet, "/list?year=2025&season=ALL", nil))
			if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
				t.Errorf("list = %d %s", w.Code, w.Body.String())
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("same-year callers made %d requests", calls.Load())
	}
}
