package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
	"github.com/calmcacil/sonarr-anime-bridge/internal/scheduler"
)

type lifecycleFetcher struct {
	started chan struct{}
	once    sync.Once
}

func (f *lifecycleFetcher) FetchYear(ctx context.Context, _ int) ([]anilist.Show, error) {
	f.once.Do(func() { close(f.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestLifecycleCancelsStartupPrewarmAndRequests(t *testing.T) {
	t.Setenv("ALLOW_INSECURE_MAPPING_URL", "1")
	for _, phase := range []string{"startup", "prewarm", "request"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			_ = listener.Close()
			cfg := &config.Config{
				Port: port, CacheDBPath: filepath.Join(dir, "cache.db"),
				AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"),
				AnibridgeURL:         "http://127.0.0.1:1/nonexistent", IncludeTypes: []string{"TV"},
				PrewarmYears: []int{time.Now().Year()},
			}
			fetcher := &lifecycleFetcher{started: make(chan struct{})}
			started := fetcher.started
			if phase == "startup" {
				started = make(chan struct{})
				var once sync.Once
				upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					once.Do(func() { close(started) })
					<-r.Context().Done()
				}))
				defer upstream.Close()
				cfg.AnibridgeURL = upstream.URL + "/mappings.json.zst"
			} else {
				writeTestMappingFile(t, dir)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- runContext(ctx, cfg, func(c *cache.Cache, config *config.Config) *scheduler.Scheduler {
					if phase == "request" {
						if err := c.SetYearContext(ctx, time.Now().Year(), []byte("[]")); err != nil {
							return scheduler.NewWithFetcher(c, config, fakeFetcher{err: err})
						}
					}
					return scheduler.NewWithFetcher(c, config, fetcher)
				})
			}()
			requestDone := make(chan struct{})
			if phase == "request" {
				url := fmt.Sprintf("http://127.0.0.1:%d", port)
				client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
				deadline := time.Now().Add(5 * time.Second)
				for {
					resp, requestErr := client.Get(url + "/health")
					if requestErr == nil {
						_ = resp.Body.Close()
						if resp.StatusCode == http.StatusOK {
							break
						}
					}
					if time.Now().After(deadline) {
						t.Fatal("listener did not become ready")
					}
					time.Sleep(10 * time.Millisecond)
				}
				go func() {
					defer close(requestDone)
					resp, requestErr := client.Get(fmt.Sprintf("%s/list?year=%d", url, time.Now().Year()-1))
					if requestErr == nil {
						_ = resp.Body.Close()
					}
				}()
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("controlled work did not start")
			}
			cancel()
			select {
			case runErr := <-done:
				if runErr != nil {
					t.Fatalf("cancelled service: %v", runErr)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("service exceeded HTTP/scheduler shutdown budgets before closing storage")
			}
			if phase == "request" {
				select {
				case <-requestDone:
				case <-time.After(time.Second):
					t.Fatal("active HTTP request survived shutdown")
				}
			}
			reopened, err := cache.Open(cfg.CacheDBPath)
			if err != nil {
				t.Fatalf("reopen after stop: %v", err)
			}
			defer reopened.Close()
			if _, err := reopened.StatsContext(context.Background()); err != nil {
				t.Fatalf("reopened cache unusable: %v", err)
			}
		})
	}
}
