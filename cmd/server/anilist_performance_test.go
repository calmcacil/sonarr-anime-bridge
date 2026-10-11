package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
	"github.com/calmcacil/sonarr-anime-bridge/internal/scheduler"
)

type performanceTransport func(*http.Request) (*http.Response, error)

func (f performanceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func performanceScheduler(t testing.TB, c *cache.Cache, client *anilist.Client, years []int) (*scheduler.Scheduler, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	writeTestMappingFile(t, dir)
	cfg := &config.Config{IncludeTypes: []string{"TV", "ONA"}, PrewarmYears: years,
		AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"), AnibridgeURL: "http://127.0.0.1:1/nonexistent"}
	s := scheduler.NewWithFetcher(c, cfg, client)
	s.LoadResolverContext(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Wait(ctx); err != nil {
			t.Error(err)
		}
	})
	return s, cfg
}

type fetchPerformanceSample struct {
	ElapsedMS      float64   `json:"elapsed_ms"`
	CompletedMS    []float64 `json:"completed_ms"`
	DispatchMS     []float64 `json:"dispatch_ms"`
	Requests       int       `json:"requests"`
	ResponseHashes []string  `json:"response_hashes"`
}

// Opt-in measurement through real scheduler writes, list responses, and prewarm.
// Independent scenario clients represent isolated processes/upstream budgets.
func TestAniListPerformance(t *testing.T) {
	output := os.Getenv("ANILIST_PERFORMANCE_OUTPUT")
	if output == "" {
		t.Skip("set ANILIST_PERFORMANCE_OUTPUT to retain paced performance samples")
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../internal/anilist/testdata/year-2025.json")
	if err != nil {
		t.Fatal(err)
	}
	var captured []anilist.Show
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	for _, latency := range []time.Duration{50 * time.Millisecond, 500 * time.Millisecond} {
		for _, pages := range []int{1, 4, 5, 10, 12} {
			for _, flow := range []string{"cold", "prewarm", "concurrent"} {
				year := time.Now().Year() - 1
				name := fmt.Sprintf("%dms-%dpages-%s", latency.Milliseconds(), pages, flow)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					shows := make([]anilist.Show, pages*50)
					for i := range shows {
						shows[i] = captured[i%len(captured)]
					}
					pageBodies := make([]json.RawMessage, pages+4)
					for i := range pageBodies {
						media := shows[min(i*50, len(shows)):min((i+1)*50, len(shows))]
						body, err := json.Marshal(map[string]any{"pageInfo": map[string]any{"currentPage": i + 1, "hasNextPage": i+1 < pages}, "media": media})
						if err != nil {
							t.Fatal(err)
						}
						pageBodies[i] = body
					}
					var samples []fetchPerformanceSample
					for range 6 {
						var mu sync.Mutex
						sample := fetchPerformanceSample{}
						var start time.Time
						client := anilist.NewWithHTTPClient("https://example.invalid", &http.Client{Timeout: 30 * time.Second, Transport: performanceTransport(func(req *http.Request) (*http.Response, error) {
							var payload struct{ Variables map[string]int }
							if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
								return nil, err
							}
							mu.Lock()
							sample.DispatchMS = append(sample.DispatchMS, float64(time.Since(start))/float64(time.Millisecond))
							sample.Requests++
							mu.Unlock()
							timer := time.NewTimer(latency)
							defer timer.Stop()
							select {
							case <-req.Context().Done():
								return nil, req.Context().Err()
							case <-timer.C:
							}
							data := map[string]json.RawMessage{}
							if page := payload.Variables["page"]; page != 0 {
								data["Page"] = pageBodies[page-1]
							} else {
								for i := range 4 {
									page := payload.Variables[fmt.Sprintf("p%d", i)]
									data[fmt.Sprintf("page%d", i)] = pageBodies[page-1]
								}
							}
							body, err := json.Marshal(map[string]any{"data": data})
							if err != nil {
								return nil, err
							}
							return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
						})})
						c := newTestCache(t)
						dir := t.TempDir()
						writeTestMappingFile(t, dir)
						years := []int{year}
						if flow != "cold" {
							years = append(years, year+1)
						}
						cfg := &config.Config{IncludeTypes: []string{"TV", "ONA"}, PrewarmYears: years, AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"), AnibridgeURL: "http://127.0.0.1:1/nonexistent"}
						s := scheduler.NewWithFetcher(c, cfg, client)
						s.LoadResolverContext(context.Background())
						h := handleList(c, s, cfg)
						start = time.Now()
						ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
						request := func(y int) {
							w := httptest.NewRecorder()
							h(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/list?season=ALL&year=%d", y), nil).WithContext(ctx))
							if w.Code != 200 {
								t.Errorf("list status %d", w.Code)
							}
							data, err := c.PeekDecodedYearContext(ctx, y)
							if err != nil || !data.Present || len(data.Shows) != len(shows) {
								t.Errorf("incomplete persisted year: %d, %v", len(data.Shows), err)
							}
							mu.Lock()
							sample.CompletedMS = append(sample.CompletedMS, float64(time.Since(start))/float64(time.Millisecond))
							sample.ResponseHashes = append(sample.ResponseHashes, fmt.Sprintf("%x", sha256.Sum256(w.Body.Bytes())))
							mu.Unlock()
						}
						switch flow {
						case "prewarm":
							if err := s.Prewarm(ctx); err != nil {
								t.Error(err)
							}
							for _, y := range years {
								request(y)
							}
						case "concurrent":
							var wg sync.WaitGroup
							for _, y := range years {
								wg.Go(func() { request(y) })
							}
							wg.Wait()
						default:
							request(year)
						}
						sample.ElapsedMS = float64(time.Since(start)) / float64(time.Millisecond)
						cancel()
						waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
						if err := s.Wait(waitCtx); err != nil {
							t.Error(err)
						}
						waitCancel()
						samples = append(samples, sample)
					}
					encoded, err := json.MarshalIndent(samples, "", "  ")
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(output, name+".json"), encoded, 0o644); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
