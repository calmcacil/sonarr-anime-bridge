package anilist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/testutil"
	"golang.org/x/time/rate"
)

func TestIsWithinMonths(t *testing.T) {
	t.Parallel()
	now, future := time.Now(), time.Now().AddDate(0, 2, 0)
	for _, tt := range []struct {
		date FuzzyDate
		want bool
	}{
		{FuzzyDate{}, true},
		{FuzzyDate{Year: testutil.Ptr(now.Year())}, true},
		{FuzzyDate{Year: testutil.Ptr(now.Year() - 1), Month: testutil.Ptr(1)}, true},
		{FuzzyDate{Year: testutil.Ptr(future.Year()), Month: testutil.Ptr(int(future.Month()))}, true},
		{FuzzyDate{Year: testutil.Ptr(2099), Month: testutil.Ptr(12)}, false},
	} {
		if got := (Show{StartDate: tt.date}).IsWithinMonths(3); got != tt.want {
			t.Errorf("IsWithinMonths(%v) = %v, want %v", tt.date, got, tt.want)
		}
	}
}

func TestDisplayTitle(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		english, romaji *string
		want            string
	}{
		{testutil.Ptr("English"), testutil.Ptr("Romaji"), "English"},
		{testutil.Ptr(""), testutil.Ptr("Romaji"), "Romaji"},
		{nil, testutil.Ptr("Romaji"), "Romaji"},
		{nil, nil, "Anime #42"},
	} {
		if got := (Show{ID: 42, Title: Title{English: tt.english, Romaji: tt.romaji}}).DisplayTitle(); got != tt.want {
			t.Errorf("title = %q, want %q", got, tt.want)
		}
	}
}

func TestClient_ConcurrentThrottle(t *testing.T) {
	t.Parallel()

	c := NewWithTimeout(30 * time.Second)
	ctx := context.Background()
	var wg sync.WaitGroup
	// throttle resets the limiter to 700ms per token, so each extra goroutine
	// adds real wait time; four is enough to exercise concurrent access.
	for range 4 {
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

func fastClient(url string, client *http.Client) *Client {
	c := NewWithHTTPClient(url, client)
	c.limiter = rate.NewLimiter(rate.Inf, maxPages+maxRetry)
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return c
}

func pageJSON(page int, next bool, media string) string {
	return fmt.Sprintf(`{"data":{"Page":{"pageInfo":{"currentPage":%d,"hasNextPage":%t},"media":%s}}}`, page, next, media)
}

func TestFetchYear(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		pages []string
		want  []int
		err   bool
	}{
		{"empty", []string{pageJSON(1, false, `[]`)}, nil, false},
		{"pagination", []string{pageJSON(1, true, `[{"id":1}]`), pageJSON(2, false, `[{"id":2}]`)}, []int{1, 2}, false},
		{"missing root", []string{`{}`}, nil, true},
		{"null page", []string{`{"data":{"Page":null}}`}, nil, true},
		{"missing page info", []string{`{"data":{"Page":{"media":[]}}}`}, nil, true},
		{"missing next flag", []string{`{"data":{"Page":{"pageInfo":{"currentPage":1},"media":[]}}}`}, nil, true},
		{"missing media", []string{`{"data":{"Page":{"pageInfo":{"currentPage":1,"hasNextPage":false}}}}`}, nil, true},
		{"null media", []string{pageJSON(1, false, `null`)}, nil, true},
		{"wrong page", []string{pageJSON(2, false, `[]`)}, nil, true},
		{"wrong media type", []string{pageJSON(1, false, `{}`)}, nil, true},
		{"graphql errors", []string{`{"errors":[{"message":"bad query"}]}`}, nil, true},
		{"malformed", []string{`{`}, nil, true},
		{"partial fetch fails", []string{pageJSON(1, true, `[{"id":1}]`), `{}`}, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Query     string
					Variables struct{ Y, Page, PerPage int }
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if r.Method != http.MethodPost || request.Variables.Y != 2026 || request.Variables.Page != calls+1 || request.Variables.PerPage != maxPerPage {
					t.Error("invalid GraphQL request")
				}
				for _, unused := range []string{"episodes", "genres", "status", "node {"} {
					if strings.Contains(request.Query, unused) {
						t.Errorf("query requests unused %q", unused)
					}
				}
				if calls >= len(tt.pages) {
					t.Error("unexpected extra request")
					w.WriteHeader(500)
					return
				}
				_, _ = w.Write([]byte(tt.pages[calls]))
				calls++
			}))
			defer srv.Close()
			shows, err := fastClient(srv.URL, srv.Client()).FetchYear(context.Background(), 2026)
			if (err != nil) != tt.err {
				t.Fatalf("FetchYear error = %v, want error=%v", err, tt.err)
			}
			var ids []int
			for _, show := range shows {
				ids = append(ids, show.ID)
			}
			if !slices.Equal(ids, tt.want) || calls != len(tt.pages) {
				t.Fatalf("IDs=%v calls=%d, want %v/%d", ids, calls, tt.want, len(tt.pages))
			}
		})
	}
}

func TestRequestRetries(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name          string
		status        int
		retryAfter    string
		fail, cancel  bool
		calls, sleeps int
	}{
		{"server retry", 500, "", false, false, 2, 1},
		{"client error", 400, "", true, false, 1, 0},
		{"exhaustion", 503, "", true, false, maxRetry, maxRetry - 1},
		{"retry after", 429, "3", false, false, 2, 2},
		{"clamped", 429, "999999999999", false, false, 2, 2},
		{"invalid retry after", 429, "invalid", false, false, 2, 1},
		{"negative retry after", 429, "-1", false, false, 2, 1},
		{"canceled retry", 503, "", true, true, 1, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls, sleeps := 0, []time.Duration{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls == 1 || tt.fail {
					w.Header().Set("Retry-After", tt.retryAfter)
					w.WriteHeader(tt.status)
					return
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			c := fastClient(srv.URL, srv.Client())
			c.sleep = func(_ context.Context, d time.Duration) error {
				sleeps = append(sleeps, d)
				if tt.cancel {
					return context.Canceled
				}
				return nil
			}
			var dst any
			err := c.doRequest(context.Background(), []byte(`{}`), &dst)
			if (err != nil) != tt.fail || calls != tt.calls || len(sleeps) != tt.sleeps {
				t.Fatalf("error=%v calls=%d sleeps=%v", err, calls, sleeps)
			}
			if tt.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if tt.status == 429 {
				if c.limiter.Limit() != rate.Every(rateLimitBackoff) {
					t.Fatal("429 did not tighten limiter")
				}
				if tt.retryAfter == "3" && sleeps[0] != 3*time.Second {
					t.Fatal("Retry-After ignored")
				}
				if tt.name == "clamped" && sleeps[0] != maxRetryAfter {
					t.Fatal("Retry-After not clamped")
				}
			}
		})
	}
}

func TestFetchYearPageLimitAndCancellation(t *testing.T) {
	t.Parallel()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(pageJSON(calls, true, `[]`)))
	}))
	defer srv.Close()
	c := fastClient(srv.URL, srv.Client())
	if _, err := c.FetchYear(context.Background(), 2026); err == nil || calls != maxPages {
		t.Fatalf("page limit: calls=%d error=%v", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.FetchYear(ctx, 2026); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if err := sleepContext(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleep cancellation: %v", err)
	}
}
