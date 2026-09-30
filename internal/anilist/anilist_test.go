package anilist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
			err := c.doRequest(context.Background(), []byte(`{}`), &dst, 2026, 1)
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

func TestClient_RetryLogsDescribeAppliedBackoff(t *testing.T) {
	for _, tt := range []struct {
		name, retryAfter, reason, header string
		status                           int
	}{
		{"network", "", "network_error", "not_applicable", 0},
		{"server error", "", "http_error", "not_applicable", http.StatusServiceUnavailable},
		{"rate limit missing header", "", "rate_limited", "missing", http.StatusTooManyRequests},
		{"rate limit invalid header", "not-a-delay", "rate_limited", "invalid", http.StatusTooManyRequests},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs := testutil.CaptureLogs(t, slog.LevelDebug)
			attempts := 0
			client := fastClient("https://private.invalid/configured?secret=token", &http.Client{
				Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					attempts++
					if attempts == 1 {
						if tt.status == 0 {
							return nil, errors.New("network unavailable")
						}
						return &http.Response{StatusCode: tt.status,
							Header: http.Header{"Retry-After": []string{tt.retryAfter}},
							Body:   io.NopCloser(strings.NewReader("private upstream body"))}, nil
					}
					return &http.Response{StatusCode: http.StatusOK,
						Body: io.NopCloser(strings.NewReader(pageJSON(1, false, `[{"id":42}]`)))}, nil
				}),
			})
			var sleeps []time.Duration
			client.sleep = func(ctx context.Context, delay time.Duration) error {
				sleeps = append(sleeps, delay)
				return ctx.Err()
			}
			shows, err := client.FetchYear(context.Background(), 2026)
			if err != nil || len(shows) != 1 || shows[0].ID != 42 || attempts != 2 || len(sleeps) != 1 {
				t.Fatalf("FetchYear = %+v, %v after %d attempts, sleeps=%v", shows, err, attempts, sleeps)
			}
			retries, pages := 0, 0
			for _, rec := range logs.Records() {
				for _, attr := range rec.Attrs {
					if strings.Contains(attr.String(), "private") || strings.Contains(attr.String(), "secret") {
						t.Errorf("request URL/body leaked in log: %+v", rec)
					}
				}
				if rec.Attrs["outcome"].String() == "retrying" {
					retries++
					if rec.Level != slog.LevelWarn || rec.Attrs["reason"].String() != tt.reason || rec.Attrs["status"].Int64() != int64(tt.status) {
						t.Errorf("retry severity/reason/status = %+v", rec)
					}
					if rec.Attrs["year"].Int64() != 2026 || rec.Attrs["page"].Int64() != 1 || rec.Attrs["attempt"].Int64() != 1 || rec.Attrs["max_attempts"].Int64() != maxRetry {
						t.Errorf("retry context = %+v", rec)
					}
					if delay := rec.Attrs["retry_in_ms"].Int64(); delay < 1500 || delay > 2500 || delay != sleeps[0].Milliseconds() {
						t.Errorf("applied jittered backoff = %dms, sleep=%v, want 1500..2500", delay, sleeps[0])
					}
					if rec.Attrs["retry_after"].String() != tt.header {
						t.Errorf("retry header state = %s, want %s", rec.Attrs["retry_after"], tt.header)
					}
				}
				if _, ok := rec.Attrs["shows"]; ok {
					pages++
					if rec.Level != slog.LevelDebug || rec.Attrs["shows"].Int64() != 1 || rec.Attrs["year"].Int64() != 2026 || rec.Attrs["page"].Int64() != 1 {
						t.Errorf("page progress = %+v", rec)
					}
				}
			}
			if retries != 1 || pages != 1 {
				t.Errorf("retry/page records = %d/%d, want 1/1", retries, pages)
			}
		})
	}
}

func TestClient_TerminalFailureReportsActualAttempts(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"client error", http.StatusBadRequest, "invalid query"},
		{"malformed response", http.StatusOK, "not JSON"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs := testutil.CaptureLogs(t, slog.LevelDebug)
			attempts := 0
			client := fastClient("https://example.invalid", &http.Client{
				Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					attempts++
					return &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(tt.body))}, nil
				}),
			})
			_, err := client.FetchYear(context.Background(), 2026)
			if attempts != 1 || err == nil || !strings.Contains(err.Error(), "after 1 attempts") {
				t.Fatalf("terminal error = %v after %d attempts, want actual single attempt", err, attempts)
			}
			for _, rec := range logs.Records() {
				if rec.Attrs["outcome"].String() == "retrying" {
					t.Errorf("nonretryable failure announced a retry: %+v", rec)
				}
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
