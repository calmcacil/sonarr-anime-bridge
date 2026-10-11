package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
	"github.com/calmcacil/sonarr-anime-bridge/internal/mapping"
	"github.com/calmcacil/sonarr-anime-bridge/internal/testutil"
)

type heldYearFetcher struct {
	started chan int
	release chan struct{}
	calls   atomic.Int64
	result  func(int) ([]anilist.Show, error)
}

func (f *heldYearFetcher) FetchYear(ctx context.Context, year int) ([]anilist.Show, error) {
	f.calls.Add(1)
	f.started <- year
	select {
	case <-f.release:
		if f.result != nil {
			return f.result(year)
		}
		return []anilist.Show{{ID: year}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func waitYearTasks(t *testing.T, s *Scheduler, want int) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		s.bgMu.Lock()
		count := len(s.yearTasks)
		s.bgMu.Unlock()
		if count == want {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("active year tasks = %d, want %d", count, want)
		default:
			runtime.Gosched()
		}
	}
}

func TestScheduleYearFetchCoalescesAndJoins(t *testing.T) {
	f := &heldYearFetcher{started: make(chan int, 2), release: make(chan struct{})}
	s := newTestScheduler(t, nil, nil, f, nil)
	if got := s.ScheduleYearFetch(2026, time.Minute, "stale_refresh"); got != YearFetchScheduled {
		t.Fatal(got)
	}
	<-f.started
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			if got := s.ScheduleYearFetch(2026, 2*time.Minute, "winter_overflow"); got != YearFetchAlreadyScheduled {
				t.Errorf("duplicate schedule = %v", got)
			}
		})
	}
	wg.Wait()
	waitYearTasks(t, s, 1)
	if f.calls.Load() != 1 {
		t.Fatal("duplicate upstream fetch")
	}
	if got := s.ScheduleYearFetch(2025, time.Minute, "winter_overflow"); got != YearFetchScheduled {
		t.Fatal(got)
	}
	if year := <-f.started; year != 2025 {
		t.Fatal(year)
	}
	waitYearTasks(t, s, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.FetchAndStore(ctx, 2026, "cache_miss"); !errors.Is(err, context.Canceled) {
		t.Fatalf("synchronous waiter: %v", err)
	}
	waitYearTasks(t, s, 2) // Request cancellation does not cancel accepted background work.
	if err := s.cache.ClearContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitYearTasks(t, s, 2) // Clear does not cancel upstream work; it may repopulate.
	joined := make(chan error, 1)
	go func() { joined <- s.FetchAndStore(context.Background(), 2026, "cache_miss") }()
	close(f.release)
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	waitYearTasks(t, s, 0)
	if f.calls.Load() != 2 {
		t.Fatalf("calls=%d, want one per year", f.calls.Load())
	}
	for _, year := range []int{2025, 2026} {
		got, err := s.cache.PeekDecodedYearContext(context.Background(), year)
		if err != nil || !got.Present || got.Shows[0].ID != year {
			t.Fatalf("year %d: %+v, %v", year, got, err)
		}
	}
}

func TestScheduleYearFetchReleasesOnEveryExit(t *testing.T) {
	for _, mode := range []string{"success", "failure", "panic", "canceled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			f := &heldYearFetcher{started: make(chan int, 2), release: make(chan struct{}), result: func(int) ([]anilist.Show, error) {
				switch mode {
				case "failure":
					return nil, errors.New("unavailable")
				case "panic":
					panic("fetch panic")
				default:
					return []anilist.Show{}, nil
				}
			}}
			s := newTestScheduler(t, nil, nil, f, nil)
			appCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.appCtx = appCtx
			timeout := time.Minute
			if mode == "deadline" {
				timeout = 50 * time.Millisecond
			}
			if s.ScheduleYearFetch(2026, timeout, "test") != YearFetchScheduled {
				t.Fatal("not scheduled")
			}
			<-f.started
			if mode == "canceled" {
				cancel()
			} else if mode != "deadline" {
				close(f.release)
			}
			waitYearTasks(t, s, 0)
			if mode == "canceled" {
				if got := s.ScheduleYearFetch(2026, timeout, "test"); got != YearFetchRejected {
					t.Fatalf("canceled app admitted task: %v", got)
				}
				return
			}
			// Cooldowns still belong to FetchAndStore, not task admission.
			if s.ScheduleYearFetch(2026, time.Minute, "retry") != YearFetchScheduled {
				t.Fatal("completed task retained its reservation")
			}
			waitYearTasks(t, s, 0)
		})
	}
}

func TestScheduleYearFetchShutdownAdmission(t *testing.T) {
	s := newTestScheduler(t, nil, nil, nil, nil)
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() { s.ScheduleYearFetch(i+2000, time.Minute, "test") })
	}
	if err := s.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	waitYearTasks(t, s, 0)
	if s.ScheduleYearFetch(2026, time.Minute, "test") != YearFetchRejected {
		t.Fatal("shutdown admitted a new task")
	}
}

func TestDecodedProcessingPreservesResidentGraph(t *testing.T) {
	show := tvShow(1, 101, "Winter", "WINTER")
	show.Tags = []anilist.Tag{{Name: "Action"}, {Name: "School"}}
	show.Duration = testutil.Ptr(24)
	show.StartDate = anilist.FuzzyDate{Year: testutil.Ptr(2020), Month: testutil.Ptr(1), Day: testutil.Ptr(1)}
	show.Relations = &anilist.RelationBlock{Edges: []anilist.RelationEdge{{RelationType: "SEQUEL"}}}
	current := []anilist.Show{show, tvShow(2, 102, "Summer", "SUMMER")}
	previous := []anilist.Show{tvShow(3, 103, "December", "WINTER")}
	previous[0].StartDate.Month = testutil.Ptr(12)
	s := newTestScheduler(t, nil, &config.Config{IncludeTypes: []string{"TV"}, FilterFutureEnabled: true}, nil, map[int]int{101: 1001, 102: 1002, 103: 1003})
	for year, shows := range map[int][]anilist.Show{2026: current, 2025: previous} {
		if err := s.cache.SetYearContext(context.Background(), year, mustMarshal(t, shows)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.cache.PeekDecodedYearContext(context.Background(), 2026)
	if err != nil {
		t.Fatal(err)
	}
	before := mustMarshal(t, first.Shows)
	var wg sync.WaitGroup
	for _, season := range []string{"ALL", "WINTER", "SPRING", "SUMMER", "FALL"} {
		for _, category := range []string{"series", "series-new"} {
			wg.Go(func() {
				data, err := s.cache.PeekDecodedYearContext(context.Background(), 2026)
				if err != nil {
					t.Error(err)
					return
				}
				prior, err := s.cache.PeekDecodedYearContext(context.Background(), 2025)
				if err != nil {
					t.Error(err)
					return
				}
				s.Process(data.Shows, prior.Shows, season, 2026, category)
			})
		}
	}
	wg.Wait()
	after, err := s.cache.PeekDecodedYearContext(context.Background(), 2026)
	if err != nil || !slices.Equal(before, mustMarshal(t, after.Shows)) || !slices.Equal(before, mustMarshal(t, first.Shows)) {
		t.Fatal("processing changed the resident object graph")
	}
	prior, err := s.cache.PeekDecodedYearContext(context.Background(), 2025)
	if err != nil || !reflect.DeepEqual(prior.Shows, previous) {
		t.Fatal("winter processing changed prior-year objects")
	}
	// Each new response must use current mappings rather than cached resolution.
	s.resolver.SetMapping(mapping.NewAnibridgeMapping(map[int]int{101: 2001, 102: 2002}, nil))
	got := s.Process(after.Shows, nil, "ALL", 2026, "series")
	if !slices.Equal(got, []Show{{TVDBID: 2001, Title: "Winter"}, {TVDBID: 2002, Title: "Summer"}}) {
		t.Fatalf("mapping swap hidden by memory: %+v", got)
	}
}

func TestPrewarmHydratesWithoutHits(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			f := &sequenceFetcher{responses: func(int) ([]anilist.Show, error) { return []anilist.Show{{ID: 1}}, nil }}
			s := newTestScheduler(t, nil, &config.Config{PrewarmYears: []int{2026}}, f, nil)
			if existing {
				if err := s.cache.SetYearContext(context.Background(), 2026, []byte(`[{"id":1}]`)); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Prewarm(context.Background()); err != nil {
				t.Fatal(err)
			}
			stats, err := s.cache.StatsContext(context.Background())
			if err != nil || stats.Hits != 0 || stats.Misses != 0 {
				t.Fatalf("prewarm recorded user access: %+v %v", stats, err)
			}
			wantCalls := 1
			if existing {
				wantCalls = 0
			}
			if f.CallCount() != wantCalls {
				t.Fatalf("upstream calls = %d, want %d", f.CallCount(), wantCalls)
			}
			// Corrupting the row after prewarm must still be visible.
			if err := s.cache.SetYearContext(context.Background(), 2026, []byte(`null`)); err != nil {
				t.Fatal(err)
			}
			got, err := s.cache.PeekDecodedYearContext(context.Background(), 2026)
			if err != nil || got.DecodeErr == nil {
				t.Fatalf("corruption masked: %+v %v", got, err)
			}
		})
	}
}

func TestDuplicateResolutionAndDiscoveryUseBatch(t *testing.T) {
	s := newTestScheduler(t, nil, &config.Config{IncludeTypes: []string{"TV"}}, nil, map[int]int{101: 1001, 102: 1002})
	input := []anilist.Show{tvShow(1, 101, "First", "SUMMER"), tvShow(2, 101, "Other", "SUMMER"), tvShow(1, 102, "Last", "SUMMER"), tvShow(3, 999, "Unknown", "SUMMER")}
	batch := s.resolveBatch(input)
	want := []Show{{TVDBID: 1002, Title: "Last"}, {TVDBID: 1001, Title: "Other"}, {TVDBID: 1002, Title: "Last"}}
	if got := responseShows(input, batch); !slices.Equal(got, want) {
		t.Fatalf("duplicate resolution = %+v", got)
	}
	s.resolver.SetMapping(mapping.NewAnibridgeMapping(map[int]int{101: 2001, 102: 2002}, nil))
	s.trackNewMappings(context.Background(), input, batch, "SUMMER", 2026)
	count, err := s.cache.CountSeenMappings(context.Background())
	if err != nil || count != 2 {
		t.Fatalf("discovery = %d, %v", count, err)
	}
	added, err := s.cache.MarkSeenMappings(context.Background(), []cache.SeenMapping{
		{TVDBID: 1001, Season: "SUMMER", Year: 2026}, {TVDBID: 1002, Season: "SUMMER", Year: 2026},
	})
	if err != nil || len(added) != 0 {
		t.Fatalf("discovery did not use response snapshot: %+v, %v", added, err)
	}
	// The next batch sees the replacement while the completed batch stays intact.
	if got := responseShows(input, s.resolveBatch(input)); got[0].TVDBID != 2002 || batch[1].TVDBID != 1002 {
		t.Fatal("batch snapshot not preserved")
	}
}

// Ensure the prewarm result remains JSON-compatible for persistent empty years.
func TestDecodedEmptyYearProcess(t *testing.T) {
	s := newTestScheduler(t, nil, &config.Config{IncludeTypes: []string{"TV"}}, nil, nil)
	if err := s.cache.SetYearContext(context.Background(), 2026, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	data, err := s.cache.PeekDecodedYearContext(context.Background(), 2026)
	if err != nil || !data.Present || data.DecodeErr != nil {
		t.Fatal(data, err)
	}
	raw, err := json.Marshal(s.Process(data.Shows, nil, "ALL", 2026, "series"))
	if err != nil || string(raw) != "[]" {
		t.Fatalf("empty response: %s, %v", raw, err)
	}
}
