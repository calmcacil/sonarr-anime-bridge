package anilist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func capturedShows(t testing.TB, n int) []Show {
	t.Helper()
	raw, err := os.ReadFile("testdata/year-2025.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture []Show
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	shows := make([]Show, n)
	for i := range shows {
		shows[i] = fixture[i%len(fixture)]
	}
	return shows
}

// Snapshot pages are generated before timing and served in reverse alias order.
func snapshotBatches(t testing.TB, shows []Show) []string {
	t.Helper()
	last := max(1, (len(shows)+maxPerPage-1)/maxPerPage)
	var batches []string
	for start := 1; start <= last; start += defaultBatchSize {
		var b strings.Builder
		b.WriteString(`{"data":{`)
		for i := defaultBatchSize - 1; i >= 0; i-- {
			if i != defaultBatchSize-1 {
				b.WriteByte(',')
			}
			page := start + i
			lo, hi := min((page-1)*maxPerPage, len(shows)), min(page*maxPerPage, len(shows))
			raw, err := json.Marshal(shows[lo:hi])
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, `"page%d":{"pageInfo":{"currentPage":%d,"hasNextPage":%t},"media":%s}`, i, page, page < last, raw)
		}
		b.WriteString(`}}`)
		batches = append(batches, b.String())
	}
	return batches
}

func bodyResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
}

func TestBatchSnapshotEquivalence(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 50, 51, 100, 150, 200, 201, 400, 482, 5000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			want := capturedShows(t, n)
			batches := snapshotBatches(t, want)
			calls := 0
			client := fastClient("https://example.invalid", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var req struct {
					Query     string
					Variables map[string]int
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatal(err)
				}
				for i := range defaultBatchSize {
					if req.Variables[fmt.Sprintf("p%d", i)] != calls*4+i+1 {
						t.Fatal("wrong page variables")
					}
				}
				if calls >= len(batches) {
					t.Fatal("extra batch")
				}
				body := batches[calls]
				calls++
				return bodyResponse(body), nil
			})})
			got, err := client.FetchYear(context.Background(), 2025)
			if err != nil || calls != len(batches) {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if n == 0 {
				if len(got) != 0 {
					t.Fatal(got)
				}
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("full ordered snapshot differs")
			}
		})
	}
}

func TestBatchRejectsIncompleteResponses(t *testing.T) {
	t.Parallel()
	valid := snapshotBatches(t, capturedShows(t, 51))[0]
	for _, change := range []struct{ name, old, replacement string }{
		{"missing alias", `"page3":`, `"unknown":`},
		{"null alias", `"page3":{"pageInfo":{"currentPage":4,"hasNextPage":false},"media":[]}`, `"page3":null`},
		{"null info", `{"currentPage":4,"hasNextPage":false}`, `null`},
		{"missing next", `"hasNextPage":false`, `"unused":false`},
		{"wrong page", `"currentPage":4`, `"currentPage":3`},
		{"missing media", `"media":[]`, `"unused":[]`},
		{"null media", `"media":[]`, `"media":null`},
		{"post terminal continuation", `"currentPage":4,"hasNextPage":false`, `"currentPage":4,"hasNextPage":true`},
		{"post terminal records", `"media":[]`, `"media":[{"id":9}]`},
		{"partial graphql error", `{"data":`, `{"errors":[{"message":"partial failure"}],"data":`},
	} {
		t.Run(change.name, func(t *testing.T) {
			body := strings.Replace(valid, change.old, change.replacement, 1)
			client := fastClient("https://example.invalid", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return bodyResponse(body), nil })})
			got, err := client.FetchYear(context.Background(), 2025)
			if got != nil || err == nil {
				t.Fatalf("partial success: %v, %v", got, err)
			}
		})
	}
	for _, suffix := range []string{`{}`, strings.Repeat(" ", maxResponseBodySize)} {
		client := fastClient("https://example.invalid", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return bodyResponse(valid + suffix), nil })})
		if got, err := client.FetchYear(context.Background(), 2025); got != nil || err == nil {
			t.Fatal("accepted trailing/oversized response")
		}
	}
}

func TestBatchRetryAndLaterFailure(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		want := capturedShows(t, 482)
		batches := snapshotBatches(t, want)
		calls := 0
		client := fastClient("https://example.invalid", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 2 {
				resp := bodyResponse("temporary")
				resp.StatusCode = 503
				return resp, nil
			}
			if calls == 3 && fail {
				return bodyResponse(`{}`), nil
			}
			index := calls - 1
			if calls > 2 {
				index--
			}
			return bodyResponse(batches[index]), nil
		})})
		got, err := client.FetchYear(context.Background(), 2025)
		if fail {
			if err == nil || got != nil || calls != 3 {
				t.Fatalf("got partial year: %v, calls=%d", err, calls)
			}
			continue
		}
		if err != nil || calls != 4 || !reflect.DeepEqual(got, want) {
			t.Fatalf("retry changed dataset: %v calls=%d", err, calls)
		}
	}
}

func TestAdmissionRollingMinuteAndLowerAllowance(t *testing.T) {
	t.Parallel()
	for _, limit := range []string{"", "100", "10"} {
		client := fastClient("", nil)
		client.interval = rateLimitDelay
		resp := bodyResponse("")
		resp.Header = http.Header{"X-Ratelimit-Limit": []string{limit}}
		if _, err := client.observeAllowance(resp); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		var dispatches []time.Time
		for range 70 {
			if err := client.throttle(context.Background(), time.Time{}); err != nil {
				t.Fatal(err)
			}
			dispatches = append(dispatches, client.now())
		}
		ceiling := 27
		if limit == "10" {
			ceiling = 10
		}
		for i, start := range dispatches {
			count := 0
			for _, stamp := range dispatches[i:] {
				if stamp.Sub(start) < time.Minute {
					count++
				}
			}
			if count > ceiling {
				t.Fatalf("%s allowance: %d dispatches/min", limit, count)
			}
		}
		if limit == "" && dispatches[2].Sub(dispatches[0]) != 4600*time.Millisecond {
			t.Fatal("ten-page batching pacing bound")
		}
	}
}

func TestSharedCooldownHeaders(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, retry, reset, remaining string
		status                        int
		delay                         time.Duration
		fail                          bool
	}{
		{"seconds", "3", "", "", 429, 3 * time.Second, false},
		{"reset later", "3", "10", "", 429, 10 * time.Second, false},
		{"missing", "", "", "", 429, time.Minute, false},
		{"invalid", "invalid", "past", "", 429, time.Minute, false},
		{"negative", "-1", "-1", "", 429, time.Minute, false},
		{"overflow", "999999999999999999999999", "", "", 429, time.Minute, false},
		{"long cooldown", "121", "", "", 429, 121 * time.Second, true},
		{"success exhausted", "", "12", "0", 200, 12 * time.Second, false},
		{"success no reset", "", "", "0", 200, time.Minute, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := fastClient("", nil)
			now := client.now()
			reset := tt.reset
			if seconds, err := time.ParseDuration(tt.reset + "s"); err == nil && seconds > 0 {
				reset = fmt.Sprint(now.Add(seconds).Unix())
			}
			resp := bodyResponse("")
			defer resp.Body.Close()
			resp.StatusCode = tt.status
			resp.Header = http.Header{"Retry-After": []string{tt.retry}, "X-Ratelimit-Reset": []string{reset}, "X-Ratelimit-Remaining": []string{tt.remaining}}
			_, err := client.observeAllowance(resp)
			if (err != nil) != tt.fail || client.cooldownUntil.Sub(now) != tt.delay {
				t.Fatalf("deadline=%v error=%v", client.cooldownUntil.Sub(now), err)
			}
			// A canceled caller and missing headers cannot clear shared state.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if !errors.Is(client.throttle(ctx, time.Time{}), context.Canceled) {
				t.Fatal("cancellation lost")
			}
			resp.StatusCode = 200
			resp.Header = nil
			_, _ = client.observeAllowance(resp)
			if client.cooldownUntil.Sub(now) != tt.delay {
				t.Fatal("cooldown erased")
			}
			if tt.fail {
				if client.throttle(context.Background(), time.Time{}) == nil {
					t.Fatal("retried before long deadline")
				}
				return
			}
			if err := client.throttle(context.Background(), now.Add(2*time.Second)); err != nil || client.now().Sub(now) != tt.delay {
				t.Fatalf("wait did not overlap: %v %v", client.now().Sub(now), err)
			}
		})
	}
}

func TestQueuedAdmissionRechecksCooldownAndAllowance(t *testing.T) {
	t.Parallel()
	client := fastClient("", nil)
	client.interval = rateLimitDelay
	if err := client.throttle(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	advance := client.sleep
	waits := 0
	start := client.now()
	client.sleep = func(ctx context.Context, delay time.Duration) error {
		waits++
		if waits == 1 {
			resp := bodyResponse("")
			defer resp.Body.Close()
			resp.StatusCode = 429
			resp.Header = http.Header{"Retry-After": []string{"10"}, "X-Ratelimit-Limit": []string{"2"}}
			if _, err := client.observeAllowance(resp); err != nil {
				return err
			}
		}
		return advance(ctx, delay)
	}
	if err := client.throttle(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if waits != 2 || client.now().Sub(start) < 33*time.Second {
		t.Fatalf("queued admission ignored updated limits: %d %s", waits, client.now().Sub(start))
	}
}

func TestConcurrentAdmissionsDoNotBurst(t *testing.T) {
	t.Parallel()
	client := NewWithHTTPClient("", nil)
	client.interval = 15 * time.Millisecond
	client.cooldownUntil = time.Now().Add(30 * time.Millisecond)
	var mu sync.Mutex
	var stamps []time.Time
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if err := client.throttle(context.Background(), time.Time{}); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			stamps = append(stamps, time.Now())
			mu.Unlock()
		})
	}
	wg.Wait()
	for i := 1; i < len(stamps); i++ {
		if stamps[i].Sub(stamps[i-1]) < 10*time.Millisecond {
			t.Fatal("cooldown released a burst")
		}
	}
}

func TestVirtualConcurrentCooldownRecovery(t *testing.T) {
	t.Parallel()
	client := fastClient("", nil)
	client.interval = rateLimitDelay
	start := client.now()
	if err := client.throttle(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	type ticket struct {
		delay   time.Duration
		release chan struct{}
	}
	waiting := make(chan ticket, 3)
	advance := client.sleep
	client.sleep = func(ctx context.Context, delay time.Duration) error {
		release := make(chan struct{})
		select {
		case waiting <- ticket{delay, release}:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-release:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := make(chan error, 3)
	for range 3 {
		go func() { completed <- client.throttle(ctx, time.Time{}) }()
	}
	receive := func(n int) []ticket {
		t.Helper()
		var tickets []ticket
		for range n {
			select {
			case ticket := <-waiting:
				tickets = append(tickets, ticket)
			case <-time.After(time.Second):
				t.Fatal("missing virtual waiter")
			}
		}
		return tickets
	}
	queued := receive(3)
	resp := bodyResponse("")
	defer resp.Body.Close()
	resp.StatusCode = 429
	resp.Header = http.Header{"Retry-After": []string{"10"}}
	if _, err := client.observeAllowance(resp); err != nil {
		t.Fatal(err)
	}
	if err := advance(ctx, rateLimitDelay); err != nil {
		t.Fatal(err)
	}
	for _, ticket := range queued {
		close(ticket.release)
	}
	queued = receive(3)
	if err := advance(ctx, 10*time.Second-rateLimitDelay); err != nil {
		t.Fatal(err)
	}
	for _, ticket := range queued {
		close(ticket.release)
	}
	for remaining := 2; remaining >= 0; remaining-- {
		select {
		case err := <-completed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("no admitted caller")
		}
		if remaining == 0 {
			break
		}
		queued = receive(remaining)
		if err := advance(ctx, rateLimitDelay); err != nil {
			t.Fatal(err)
		}
		for _, ticket := range queued {
			close(ticket.release)
		}
	}
	if got := client.now().Sub(start); got != 10*time.Second+2*rateLimitDelay {
		t.Fatalf("burst or lost cooldown: %s", got)
	}
}

func TestCanceledQueuedWaiterPreservesBudget(t *testing.T) {
	t.Parallel()
	client := fastClient("", nil)
	client.interval = rateLimitDelay
	start := client.now()
	client.lastAdmission = start
	client.cooldownUntil = start.Add(10 * time.Second)
	advance := client.sleep
	waiting := make(chan struct{})
	client.sleep = func(ctx context.Context, _ time.Duration) error {
		close(waiting)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.throttle(ctx, time.Time{}) }()
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("waiter did not queue")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if client.lastAdmission != start || client.cooldownUntil != start.Add(10*time.Second) {
		t.Fatal("cancellation changed shared constraints")
	}
	client.sleep = advance
	if err := client.throttle(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if client.now() != start.Add(10*time.Second) {
		t.Fatal("another caller ignored shared cooldown")
	}
}

func TestRecoveredAllowanceKeepsDefaultCeiling(t *testing.T) {
	t.Parallel()
	client := fastClient("", nil)
	resp := bodyResponse("")
	defer resp.Body.Close()
	for _, value := range []string{"10", "100"} {
		resp.Header = http.Header{"X-Ratelimit-Limit": []string{value}}
		if _, err := client.observeAllowance(resp); err != nil {
			t.Fatal(err)
		}
	}
	if client.interval != rateLimitDelay {
		t.Fatal("recovery exceeded default ceiling or kept obsolete allowance")
	}
}
