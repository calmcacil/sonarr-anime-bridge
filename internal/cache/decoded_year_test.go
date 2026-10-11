package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
	"github.com/calmcacil/sonarr-anime-bridge/internal/testutil"
)

func seedDecodedYear(t testing.TB, c *Cache, year, id int) {
	t.Helper()
	if err := c.SetYearContext(context.Background(), year, []byte(fmt.Sprintf(`[{"id":%d,"title":{"english":"Test"},"tags":[{"name":"Action"}]}]`, id))); err != nil {
		t.Fatal(err)
	}
}

func decodedRead(t testing.TB, c *Cache, year int) DecodedYear {
	t.Helper()
	got, err := c.PeekDecodedYearContext(context.Background(), year)
	if err != nil || got.DecodeErr != nil {
		t.Fatalf("decoded read: %+v, %v", got, err)
	}
	return got
}

func countDecodes(c *Cache) *atomic.Int64 {
	count := new(atomic.Int64)
	c.decoded.decode = func(raw []byte) ([]anilist.Show, error) {
		count.Add(1)
		return DecodeYearData(raw)
	}
	return count
}

func waitGateRefs(t *testing.T, c *Cache, year, refs int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		c.decoded.mu.Lock()
		gate := c.decoded.gates[year]
		ready := gate != nil && gate.refs == refs
		c.decoded.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("year %d gate did not reach %d callers", year, refs)
		default:
			runtime.Gosched()
		}
	}
}

func TestDecodedYearReuseAndStorageVisibility(t *testing.T) {
	c := newMemCache(t)
	count := countDecodes(c)
	year := time.Now().Year()
	seedDecodedYear(t, c, year, 1)
	first, err := c.GetDecodedYearContext(context.Background(), year)
	if err != nil || !first.Fresh || !first.Present || first.DecodeErr != nil {
		t.Fatalf("first: %+v, %v", first, err)
	}
	if cap(first.Shows) != len(first.Shows) {
		t.Fatal("return slice has spare capacity")
	}
	first.Shows[0].ID = 99
	second := decodedRead(t, c, year)
	if second.Shows[0].ID != 1 || count.Load() != 1 {
		t.Fatalf("caller mutated resident data or decoded twice: %+v, %d", second, count.Load())
	}
	stats, _ := c.StatsContext(context.Background())
	if stats.Hits != 1 || stats.Misses != 0 {
		t.Fatalf("peek changed access accounting: %+v", stats)
	}

	// Same-second and external writes must be observed through exact BLOB bytes.
	if _, err := c.db.Exec(`UPDATE year_cache SET data='[{"id":2}]' WHERE year=?`, year); err != nil {
		t.Fatal(err)
	}
	if got := decodedRead(t, c, year); got.Shows[0].ID != 2 || count.Load() != 2 {
		t.Fatalf("external replacement hidden: %+v, decodes=%d", got, count.Load())
	}
	if _, err := c.db.Exec(`UPDATE year_cache SET data='null' WHERE year=?`, year); err != nil {
		t.Fatal(err)
	}
	bad, err := c.GetDecodedYearContext(context.Background(), year)
	if err != nil || !bad.Present || bad.DecodeErr == nil || len(c.decoded.entries) != 0 {
		t.Fatalf("corruption hidden: %+v, %v", bad, err)
	}
	if _, err := c.db.Exec(`DELETE FROM year_cache WHERE year=?`, year); err != nil {
		t.Fatal(err)
	}
	missing, err := c.GetDecodedYearContext(context.Background(), year)
	if err != nil || missing.Present || missing.DecodeErr != nil {
		t.Fatalf("deletion hidden: %+v, %v", missing, err)
	}
	seedDecodedYear(t, c, year, 3)
	decodedRead(t, c, year)
	if err := c.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetDecodedYearContext(context.Background(), year); err == nil {
		t.Fatal("memory hid SQLite failure")
	}
}

func TestDecodedYearPayloadStates(t *testing.T) {
	for _, payload := range []string{"", "null", "{}", "[", "[null]", "[]", `[{"id":7,"legacy":true}]`} {
		t.Run(payload, func(t *testing.T) {
			c := newMemCache(t)
			count := countDecodes(c)
			if err := c.SetYearContext(context.Background(), 2026, []byte(payload)); err != nil {
				t.Fatal(err)
			}
			want, wantErr := DecodeYearData([]byte(payload))
			for range 2 {
				got, err := c.GetDecodedYearContext(context.Background(), 2026)
				if err != nil || !got.Present || (got.DecodeErr != nil) != (wantErr != nil) || !reflect.DeepEqual(got.Shows, want) {
					t.Fatalf("got %+v / %v; want %+v / %v", got, err, want, wantErr)
				}
			}
			wantDecodes := int64(1)
			if wantErr != nil {
				wantDecodes = 2
			}
			if count.Load() != wantDecodes {
				t.Fatalf("decodes = %d, want %d", count.Load(), wantDecodes)
			}
		})
	}
}

func TestDecodedYearConcurrentAndCancelable(t *testing.T) {
	t.Run("one decode for concurrent readers", func(t *testing.T) {
		c := newMemCache(t)
		seedDecodedYear(t, c, 2026, 1)
		seedDecodedYear(t, c, 2025, 2)
		started, release := make(chan struct{}), make(chan struct{})
		var count atomic.Int64
		c.decoded.decode = func(raw []byte) ([]anilist.Show, error) {
			if count.Add(1) == 1 {
				close(started)
				<-release
			}
			return DecodeYearData(raw)
		}
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				got, err := c.GetDecodedYearContext(context.Background(), 2026)
				if err != nil || len(got.Shows) != 1 {
					t.Errorf("reader: %+v, %v", got, err)
				}
			})
		}
		<-started
		waitGateRefs(t, c, 2026, 16)
		// Another year's decode is independent of the blocked year.
		decodedRead(t, c, 2025)
		close(release)
		wg.Wait()
		if count.Load() != 2 || len(c.decoded.gates) != 0 {
			t.Fatalf("decodes=%d, gates=%d", count.Load(), len(c.decoded.gates))
		}
		stats, _ := c.StatsContext(context.Background())
		if stats.Hits != 16 {
			t.Fatalf("hits = %d, want 16", stats.Hits)
		}
	})
	t.Run("cancel waiter and leader without breaking another reader", func(t *testing.T) {
		c := newMemCache(t)
		seedDecodedYear(t, c, 2026, 1)
		started, release := make(chan struct{}), make(chan struct{})
		var count atomic.Int64
		c.decoded.decode = func(raw []byte) ([]anilist.Show, error) {
			if count.Add(1) == 1 {
				close(started)
				<-release
			}
			return DecodeYearData(raw)
		}
		leaderCtx, cancelLeader := context.WithCancel(context.Background())
		defer cancelLeader()
		leader := make(chan error, 1)
		go func() { _, err := c.GetDecodedYearContext(leaderCtx, 2026); leader <- err }()
		<-started
		waiterCtx, cancelWaiter := context.WithCancel(context.Background())
		waiter := make(chan error, 1)
		go func() { _, err := c.GetDecodedYearContext(waiterCtx, 2026); waiter <- err }()
		waitGateRefs(t, c, 2026, 2)
		cancelWaiter()
		if err := <-waiter; !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter: %v", err)
		}
		other := make(chan error, 1)
		go func() { _, err := c.GetDecodedYearContext(context.Background(), 2026); other <- err }()
		waitGateRefs(t, c, 2026, 2)
		cancelLeader()
		close(release)
		if err := <-leader; !errors.Is(err, context.Canceled) {
			t.Fatalf("leader: %v", err)
		}
		if err := <-other; err != nil {
			t.Fatal(err)
		}
		if count.Load() != 2 || len(c.decoded.gates) != 0 || len(c.decoded.entries) != 1 {
			t.Fatalf("decodes=%d, gates=%d, entries=%d", count.Load(), len(c.decoded.gates), len(c.decoded.entries))
		}
	})
}

func TestDecodedYearMutationFences(t *testing.T) {
	for _, mutation := range []string{"replace", "clear", "prune", "close"} {
		t.Run(mutation, func(t *testing.T) {
			c := newMemCache(t)
			seedDecodedYear(t, c, 2020, 1)
			if _, err := c.db.Exec(`UPDATE year_cache SET last_hit=?`, time.Now().Add(-20*24*time.Hour).Unix()); err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			c.decoded.decode = func(raw []byte) ([]anilist.Show, error) {
				close(started)
				<-release
				return DecodeYearData(raw)
			}
			done := make(chan error, 1)
			go func() { _, err := c.PeekDecodedYearContext(context.Background(), 2020); done <- err }()
			<-started
			switch mutation {
			case "replace":
				seedDecodedYear(t, c, 2020, 2)
			case "clear":
				if err := c.ClearContext(context.Background()); err != nil {
					t.Fatal(err)
				}
			case "prune":
				if n, err := c.PruneStaleYearsContext(context.Background(), 14); err != nil || n != 1 {
					t.Fatalf("prune: %d, %v", n, err)
				}
			case "close":
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if len(c.decoded.entries) != 0 || len(c.decoded.gates) != 0 {
				t.Fatal("old decode readmitted data after mutation")
			}
		})
	}
	t.Run("read admission between SQL commit and invalidation", func(t *testing.T) {
		c := newMemCache(t)
		seedDecodedYear(t, c, 2026, 1)
		committed, invalidate := make(chan struct{}), make(chan struct{})
		c.decoded.beforeInvalidate = func() { close(committed); <-invalidate }
		done := make(chan error, 1)
		go func() { done <- c.SetYearContext(context.Background(), 2026, []byte(`[{"id":2}]`)) }()
		<-committed
		if got := decodedRead(t, c, 2026); got.Shows[0].ID != 2 || len(c.decoded.entries) != 1 {
			t.Fatalf("committed read: %+v", got)
		}
		close(invalidate)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		c.decoded.beforeInvalidate = nil
		if len(c.decoded.entries) != 0 {
			t.Fatal("mutation returned before invalidating an overlapping admission")
		}
	})
}

func TestDecodedYearFailedMutationsRetainState(t *testing.T) {
	c := newMemCache(t)
	seedDecodedYear(t, c, 2026, 1)
	decodedRead(t, c, 2026)
	generation := c.decoded.generation
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.SetYearContext(ctx, 2026, []byte(`[]`)); err == nil {
		t.Fatal("canceled write succeeded")
	}
	if err := c.ClearContext(ctx); err == nil {
		t.Fatal("canceled clear succeeded")
	}
	if _, err := c.PruneStaleYearsContext(ctx, 14); err == nil {
		t.Fatal("canceled prune succeeded")
	}
	if c.decoded.generation != generation || len(c.decoded.entries) != 1 || decodedRead(t, c, 2026).Shows[0].ID != 1 {
		t.Fatal("failed mutation invalidated valid decoded data")
	}
}

func TestDecodedYearCapacityAndRestart(t *testing.T) {
	c, path := newFileCache(t)
	count := countDecodes(c)
	c.decoded.maxYears = 2
	for year := 2000; year <= 2002; year++ {
		seedDecodedYear(t, c, year, year)
	}
	decodedRead(t, c, 2000)
	decodedRead(t, c, 2001)
	decodedRead(t, c, 2000) // Touch the first year, making 2001 least recently used.
	decodedRead(t, c, 2002)
	if c.decoded.entries[2001] != nil || len(c.decoded.entries) != 2 {
		t.Fatal("LRU capacity was not respected")
	}
	decodedRead(t, c, 2001)
	if count.Load() != 4 {
		t.Fatalf("evicted data did not decode again: %d", count.Load())
	}
	if err := c.ClearContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.decoded.maxBytes = 1
	seedDecodedYear(t, c, 2026, 7)
	for range 2 {
		if got := decodedRead(t, c, 2026); got.Shows[0].ID != 7 {
			t.Fatal("oversized data must remain usable")
		}
	}
	if len(c.decoded.entries) != 0 || c.decoded.bytes != 0 || len(c.decoded.gates) != 0 {
		t.Fatal("oversized decoding was retained")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.decoded.entries) != 0 || decodedRead(t, reopened, 2026).Shows[0].ID != 7 {
		t.Fatal("restart must discard decoded state and retain persisted data")
	}
}

func TestDecodedYearAccounting(t *testing.T) {
	show := anilist.Show{
		IDMal: testutil.Ptr(10), Duration: testutil.Ptr(24), Format: "TV", Season: "SUMMER",
		Title:     anilist.Title{English: testutil.Ptr("English"), Romaji: testutil.Ptr("Romaji")},
		StartDate: anilist.FuzzyDate{Year: testutil.Ptr(2026), Month: testutil.Ptr(7), Day: testutil.Ptr(1)},
		Tags:      make([]anilist.Tag, 1, 3),
		Relations: &anilist.RelationBlock{Edges: make([]anilist.RelationEdge, 1, 2)},
	}
	show.Tags[0].Name = "Action"
	show.Relations.Edges[0].RelationType = "SEQUEL"
	shows := make([]anilist.Show, 1, 4)
	shows[0] = show
	raw := make([]byte, 10, 100)
	empty := decodedYearSize(nil, nil)
	want := int64(100 + 4*unsafe.Sizeof(show) + 5*unsafe.Sizeof(int(0)) + 2*unsafe.Sizeof("") + 13 + 8 + 3*unsafe.Sizeof(anilist.Tag{}) + 6 + unsafe.Sizeof(anilist.RelationBlock{}) + 2*unsafe.Sizeof(anilist.RelationEdge{}) + 6)
	if got := decodedYearSize(raw, shows) - empty; got != want {
		t.Fatalf("accounted nested payload = %d, want %d", got, want)
	}
}

func TestDecodedYearByteBudgetAndGateCleanup(t *testing.T) {
	c := newMemCache(t)
	for year := 2000; year < 2003; year++ {
		seedDecodedYear(t, c, year, year)
	}
	decodedRead(t, c, 2000)
	c.decoded.maxBytes = c.decoded.bytes * 2
	decodedRead(t, c, 2001)
	decodedRead(t, c, 2002)
	if c.decoded.entries[2000] != nil || len(c.decoded.entries) != 2 || c.decoded.bytes > c.decoded.maxBytes {
		t.Fatal("payload budget did not evict the least recently used year")
	}
	c.decoded.maxBytes = 1
	for year := 2010; year < 2040; year++ {
		seedDecodedYear(t, c, year, year)
		decodedRead(t, c, year)
	}
	if len(c.decoded.gates) != 0 {
		t.Fatal("arbitrary oversized years left a permanent decode gate registry")
	}
}

func TestDecodedYearFreshnessAndRetention(t *testing.T) {
	c := newMemCache(t)
	year := time.Now().Year()
	seedDecodedYear(t, c, year, 1)
	decodedRead(t, c, year)
	old := time.Now().Add(-48 * time.Hour).Unix()
	if _, err := c.db.Exec(`UPDATE year_cache SET fetched_at=?, last_hit=?`, old, old); err != nil {
		t.Fatal(err)
	}
	stale, err := c.GetDecodedYearContext(context.Background(), year)
	if err != nil || stale.Fresh {
		t.Fatalf("resident hit renewed freshness: %+v, %v", stale, err)
	}
	if n, err := c.PruneStaleYearsContext(context.Background(), 1); err != nil || n != 0 {
		t.Fatalf("memory hit did not protect user retention: %d, %v", n, err)
	}
	for _, at := range []time.Time{time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if fresh := c.yearFreshAt(2026, at.Add(-48*time.Hour).Unix(), at); fresh != (at.Year() == 2027) {
			t.Fatalf("calendar rollover freshness at %s: %v", at, fresh)
		}
	}
	for _, current := range []bool{false, true} {
		at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		year, ttl := 2025, 7*24*time.Hour
		if current {
			year, ttl = 2026, 24*time.Hour
		}
		if c.yearFreshAt(year, at.Add(-ttl).Unix(), at) || !c.yearFreshAt(year, at.Add(-ttl+time.Second).Unix(), at) {
			t.Fatal("freshness boundary changed")
		}
	}
}

func BenchmarkDecodedYear(b *testing.B) {
	for _, mode := range []string{"FirstLoad", "Warm", "Parallel", "Oversized"} {
		b.Run(mode, func(b *testing.B) {
			c, err := Open(":memory:")
			if err != nil {
				b.Fatal(err)
			}
			defer c.Close()
			shows := make([]anilist.Show, 600)
			for i := range shows {
				shows[i] = anilist.Show{ID: i + 1, Title: anilist.Title{English: testutil.Ptr("Benchmark")}, Tags: []anilist.Tag{{Name: "Action"}}}
			}
			raw, err := json.Marshal(shows)
			if err != nil {
				b.Fatal(err)
			}
			if err := c.SetYearContext(context.Background(), 2026, raw); err != nil {
				b.Fatal(err)
			}
			if mode == "Oversized" {
				c.decoded.maxBytes = 1
			}
			read := func() {
				got, err := c.PeekDecodedYearContext(context.Background(), 2026)
				if err != nil || got.DecodeErr != nil || len(got.Shows) != len(shows) {
					b.Fatalf("decoded read: %+v, %v", got, err)
				}
			}
			read()
			b.ReportAllocs()
			b.ResetTimer()
			if mode == "Parallel" {
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						read()
					}
				})
				return
			}
			for b.Loop() {
				if mode == "FirstLoad" {
					b.StopTimer()
					c.decoded.invalidate(0, true, false)
					b.StartTimer()
				}
				read()
			}
		})
	}
}
