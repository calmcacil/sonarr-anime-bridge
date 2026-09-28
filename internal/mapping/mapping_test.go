package mapping

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
	"github.com/calmcacil/sonarr-anime-bridge/internal/testutil"
)

// fixtureMAL1 maps MAL 1 to TVDB 100.
const fixtureMAL1 = `{ "mal:1": { "tvdb_show:100:s1": { "1-12": "1-12" } } }`

// --- AnibridgeMapping lookups ----------------------------------------------

func TestAnibridgeMappingLookup(t *testing.T) {
	t.Parallel()

	am := NewAnibridgeMapping(
		map[int]int{16498: 12345, 99999: 67890},
		map[int]int{100: 54321, 200: 98765},
	)
	tests := []struct {
		name   string
		lookup func(int) (int, bool)
		id     int
		want   int
		wantOK bool
	}{
		{"MAL known", am.LookupByMAL, 16498, 12345, true},
		{"MAL unknown", am.LookupByMAL, 1, 0, false},
		{"MAL zero", am.LookupByMAL, 0, 0, false},
		{"AniList known", am.LookupByAniList, 100, 54321, true},
		{"AniList unknown", am.LookupByAniList, 999, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.lookup(tt.id)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("lookup(%d) = %d, %v; want %d, %v", tt.id, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestNewAnibridgeMapping_EmptyMaps(t *testing.T) {
	t.Parallel()

	am := NewAnibridgeMapping(nil, nil)
	if mal, ani := am.Stats(); mal != 0 || ani != 0 {
		t.Errorf("expected zero entries, got mal=%d ani=%d", mal, ani)
	}
	if _, ok := am.LookupByMAL(1); ok {
		t.Error("expected miss on empty map")
	}
}

func TestAnibridgeMappingKeysSorted(t *testing.T) {
	t.Parallel()

	am := NewAnibridgeMapping(
		map[int]int{30: 3, 10: 1, 20: 2},
		map[int]int{300: 3, 100: 1, 200: 2},
	)
	malKeys, aniListKeys := am.Keys()
	if want := []int{10, 20, 30}; !reflect.DeepEqual(malKeys, want) {
		t.Fatalf("malKeys = %v, want %v", malKeys, want)
	}
	if want := []int{100, 200, 300}; !reflect.DeepEqual(aniListKeys, want) {
		t.Fatalf("aniListKeys = %v, want %v", aniListKeys, want)
	}
}

// --- Resolver behavior ------------------------------------------------------

func showWithMAL(id int, mal int, title string) anilist.Show {
	return anilist.Show{
		ID:    id,
		IDMal: testutil.Ptr(mal),
		Title: anilist.Title{English: testutil.Ptr(title)},
	}
}

func showAnilistOnly(id int, title string) anilist.Show {
	return anilist.Show{
		ID:    id,
		Title: anilist.Title{English: testutil.Ptr(title)},
	}
}

// TestResolver_Resolve is not parallel because it captures debug logs to
// assert which lookup path produced the result.
func TestResolver_Resolve(t *testing.T) {
	am := NewAnibridgeMapping(
		map[int]int{16498: 12345},
		map[int]int{1: 99999, 42: 77777},
	)
	const (
		viaMAL     = "resolved via anibridge (MAL)"
		viaAniList = "resolved via anibridge (AniList fallback)"
	)
	tests := []struct {
		name    string
		mapping *AnibridgeMapping
		show    anilist.Show
		want    int
		wantOK  bool
		wantLog string
	}{
		{"NoMapping", nil, showWithMAL(1, 16498, "Test"), 0, false, ""},
		{"PrefersMAL", am, showWithMAL(1, 16498, "Priority"), 12345, true, viaMAL},
		{"AniListFallback", am, showAnilistOnly(42, "AniList Original"), 77777, true, viaAniList},
		{"MALMissAniListHit", am, showWithMAL(1, 999999, "MAL miss"), 99999, true, viaAniList},
		{"MALMissAniListMiss", am, showWithMAL(123, 999999, "Unknown"), 0, false, ""},
		{"NoMALAniListMiss", am, showAnilistOnly(123456, "Unknown"), 0, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := testutil.CaptureLogs(t, slog.LevelDebug)
			r := NewResolver()
			r.SetMapping(tt.mapping)
			if (r.Mapping() == nil) != (tt.mapping == nil) {
				t.Fatalf("Mapping() = %v, want %v", r.Mapping(), tt.mapping)
			}

			got, ok := r.Resolve(tt.show)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("Resolve = %d, %v; want %d, %v", got, ok, tt.want, tt.wantOK)
			}
			for _, msg := range []string{viaMAL, viaAniList} {
				if _, logged := logs.Find(msg); logged != (msg == tt.wantLog) {
					t.Errorf("log %q present = %v, want %v", msg, logged, msg == tt.wantLog)
				}
			}
		})
	}
}

func TestResolver_ResolveBatch(t *testing.T) {
	t.Parallel()

	r := NewResolver()
	r.SetMapping(NewAnibridgeMapping(
		map[int]int{16498: 12345},
		map[int]int{42: 77777},
	))

	got := r.ResolveBatch([]anilist.Show{
		showWithMAL(1, 16498, "Via MAL"),
		showAnilistOnly(42, "Via AniList"),
		showWithMAL(99, 99999, "Unknown"),
		showAnilistOnly(1000, "Unknown AniList"),
	})
	want := map[int]ResolvedShow{
		1:    {TVDBID: 12345, Title: "Via MAL", Resolved: true},
		42:   {TVDBID: 77777, Title: "Via AniList", Resolved: true},
		99:   {Title: "Unknown"},
		1000: {Title: "Unknown AniList"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResolveBatch =\n%+v\nwant\n%+v", got, want)
	}
}

func TestResolver_SetMapping(t *testing.T) {
	t.Parallel()

	r := NewResolver()
	r.SetMapping(NewAnibridgeMapping(map[int]int{1: 100}, nil))
	r.SetMapping(nil)
	if got, _ := r.Resolve(showWithMAL(1, 1, "Persist")); got != 100 {
		t.Errorf("after SetMapping(nil): got %d, want prior mapping 100", got)
	}

	r.SetMapping(NewAnibridgeMapping(map[int]int{1: 200}, nil))
	if got, _ := r.Resolve(showWithMAL(1, 1, "Swap")); got != 200 {
		t.Errorf("after swap: got %d, want 200", got)
	}
}

// --- Parser tests -----------------------------------------------------------

func TestParseAnibridgeJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fixture     string
		wantMAL     map[int]int
		wantAniList map[int]int
		wantErr     bool
	}{
		{
			name: "small fixture ignores unrelated namespaces",
			fixture: `{
  "$meta": { "schema_version": "3.0.0", "generated_on": "2026-01-01T00:00:00Z" },
  "mal:1": { "tvdb_show:100:s1": { "1-12": "1-12" } },
  "mal:2": {
    "tvdb_show:200:s1": { "1-24": "1-24" },
    "tvdb_show:200:s0": { "1-3": "4-6" }
  },
  "anilist:42": { "tvdb_show:300:s1": { "1-13": "1-13" } },
  "anilist:99": { "tvdb_show:400:s2": { "1-12": "1-12" } },
  "anidb:999:R": { "mal:1": { "1-12": "1-12" } }
}`,
			wantMAL:     map[int]int{1: 100, 2: 200},
			wantAniList: map[int]int{42: 300, 99: 400},
		},
		{
			// MAL 10 prefers s1 despite s2 having more episodes; MAL 11 has no
			// s1 so the scope with the most episodes (s0) wins.
			name: "prefers s1 then episode count",
			fixture: `{
  "mal:10": {
    "tvdb_show:500:s1": { "1-24": "1-24" },
    "tvdb_show:501:s2": { "1-50": "1-50" },
    "tvdb_show:502:s0": { "1-6": "1-6" }
  },
  "mal:11": {
    "tvdb_show:600:s0": { "1-50": "1-50" },
    "tvdb_show:601:s2": { "1-24": "1-24" }
  }
}`,
			wantMAL: map[int]int{10: 500, 11: 600},
		},
		{
			name: "skips invalid keys",
			fixture: `{
  "mal:abc": { "tvdb_show:1:s1": { "1": "1" } },
  "mal:-5": { "tvdb_show:1:s1": { "1": "1" } },
  "anilist:": { "tvdb_show:1:s1": { "1": "1" } },
  "mal:7": { "tvdb_show:0:s1": { "1": "1" } }
}`,
		},
		{name: "rejects malformed entry", fixture: `{ "mal:8": "not an object" }`, wantErr: true},
		{name: "rejects non-object root", fixture: `[]`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			am, err := parseAnibridgeJSON(context.Background(), strings.NewReader(tt.fixture), "test")
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAnibridgeJSON: %v", err)
			}
			if !mapsEqual(am.byMAL, tt.wantMAL) {
				t.Errorf("byMAL = %v, want %v", am.byMAL, tt.wantMAL)
			}
			if !mapsEqual(am.byAniList, tt.wantAniList) {
				t.Errorf("byAniList = %v, want %v", am.byAniList, tt.wantAniList)
			}
		})
	}
}

// mapsEqual treats nil and empty maps as equal.
func mapsEqual(a, b map[int]int) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func TestParseAnibridgeFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	valid := filepath.Join(dir, "test.json.zst")
	writeZstdFile(t, valid, fixtureMAL1)

	am, err := parseAnibridgeFileContext(context.Background(), valid)
	if err != nil {
		t.Fatalf("parseAnibridgeFileContext: %v", err)
	}
	if tvdbID, ok := am.LookupByMAL(1); !ok || tvdbID != 100 {
		t.Errorf("expected MAL 1 -> TVDB 100, got %d, %v", tvdbID, ok)
	}

	if _, err := parseAnibridgeFileContext(context.Background(), filepath.Join(dir, "missing.json.zst")); err == nil {
		t.Error("expected error for missing file")
	}
}

// --- Metadata I/O -----------------------------------------------------------

func TestReadMetadata_MissingFile(t *testing.T) {
	t.Parallel()

	m, err := ReadMetadata(filepath.Join(t.TempDir(), "absent.meta.json"))
	if err != nil {
		t.Errorf("missing sidecar should not be an error, got %v", err)
	}
	if !reflect.DeepEqual(m, Metadata{}) {
		t.Errorf("expected zero metadata, got %+v", m)
	}
}

func TestWriteAndReadMetadata_RoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.meta.json")
	want := Metadata{
		ETag:         `"0x8DEC2CA2CEB7643"`,
		LastModified: "Fri, 05 Jun 2026 06:17:52 GMT",
		MD5:          "ee20b3531f9453369bbcb16c1cda9a5d",
		FetchedAt:    time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
		URL:          config.DefaultAnibridgeURL,
		MALKeys:      []int{1, 2},
		AniListKeys:  []int{3},
	}
	if err := WriteMetadata(path, want); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
	}
}

func TestWriteMetadata_RejectsPathConflict(t *testing.T) {
	t.Parallel()

	parent := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(parent, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteMetadata(filepath.Join(parent, "test.meta.json"), Metadata{ETag: "x"}); err == nil {
		t.Error("expected error when parent is a regular file")
	}
}

// --- HTTP conditional fetch ------------------------------------------------

func TestValidateRemoteURL_RejectsNonAllowlistedHost(t *testing.T) {
	t.Parallel()

	err := validateRemoteURL("https://example.com/mappings.json.zst")
	if err == nil || !strings.Contains(err.Error(), "host is not allowlisted") {
		t.Fatalf("error = %v, want allowlist failure", err)
	}
}

func TestHead_NotFound(t *testing.T) {
	t.Parallel()

	if _, err := Head(context.Background(), newStatusServer(t, http.StatusNotFound).URL); err == nil {
		t.Error("expected error on 404")
	}
}

func TestHead_OKReturnsMetadata(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD, got %s", r.Method)
		}
		w.Header().Set("ETag", `"abc123"`)
		w.Header().Set("Last-Modified", "Fri, 05 Jun 2026 06:17:52 GMT")
		w.Header().Set(md5Header, "3q2+7w==") // base64 of 0xDEADBEEF
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	m, err := Head(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if m.ETag != `"abc123"` {
		t.Errorf("ETag: got %q", m.ETag)
	}
	if m.MD5 != "deadbeef" {
		t.Errorf("MD5: got %q want deadbeef", m.MD5)
	}
	if m.LastModified != "Fri, 05 Jun 2026 06:17:52 GMT" {
		t.Errorf("LastModified: got %q", m.LastModified)
	}
	if m.FetchedAt.IsZero() {
		t.Error("FetchedAt should be populated")
	}
}

func TestFetch_VerifiesMD5(t *testing.T) {
	t.Parallel()

	payload := []byte("hello world")
	up := newUpstream(t, `"v1"`, payload)

	data, m, err := Fetch(context.Background(), up.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("payload mismatch: got %q want %q", data, payload)
	}
	if want := md5Hex(payload); m.MD5 != want {
		t.Errorf("MD5: got %q want %q", m.MD5, want)
	}
}

func TestFetch_MD5Mismatch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(md5Header, "AAAAAAAAAAAAAAAAAAAAAA==")
		w.Write([]byte("actual content"))
	}))
	t.Cleanup(srv.Close)

	if _, _, err := Fetch(context.Background(), srv.URL); err == nil {
		t.Error("expected MD5 mismatch error")
	}
}

// --- LoadOrFetch ------------------------------------------------------------

func TestLoadOrFetch_ETagShortCircuits(t *testing.T) {
	t.Parallel()

	const etag = `"v1"`
	up := newUpstream(t, etag, nil)
	path := filepath.Join(t.TempDir(), "mapping.json.zst")
	seedCache(t, path, zstdBytes(fixtureMAL1), &Metadata{ETag: etag, URL: up.URL, FetchedAt: time.Now().UTC()})

	am, meta, err := LoadOrFetch(context.Background(), path, up.URL)
	if err != nil {
		t.Fatalf("LoadOrFetch: %v", err)
	}
	if tvdbID, ok := am.LookupByMAL(1); !ok || tvdbID != 100 {
		t.Errorf("expected cached MAL 1 -> TVDB 100, got %d, %v", tvdbID, ok)
	}
	if meta.ETag != etag {
		t.Errorf("ETag = %q, want %q", meta.ETag, etag)
	}
	if h, g := up.heads.Load(), up.gets.Load(); h != 1 || g != 0 {
		t.Errorf("HEAD=%d GET=%d, want 1 and 0", h, g)
	}
}

func TestLoadOrFetch_RefreshesFromUpstream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cached   []byte
		sideETag string
	}{
		{
			name:     "ETag changed",
			cached:   zstdBytes(`{ "mal:99": { "tvdb_show:999:s1": { "1": "1" } } }`),
			sideETag: `"v1"`,
		},
		{
			// HEAD matches the sidecar ETag, so the corrupt-cache branch runs.
			name:     "ETag match but cache corrupt",
			cached:   []byte("not valid zstd"),
			sideETag: `"v2"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			up := newUpstream(t, `"v2"`, zstdBytes(fixtureMAL1))
			path := filepath.Join(t.TempDir(), "mapping.json.zst")
			seedCache(t, path, tt.cached, &Metadata{ETag: tt.sideETag, URL: up.URL, FetchedAt: time.Now().Add(-24 * time.Hour)})

			am, meta, err := LoadOrFetch(context.Background(), path, up.URL)
			if err != nil {
				t.Fatalf("LoadOrFetch: %v", err)
			}
			if h, g := up.heads.Load(), up.gets.Load(); h != 1 || g != 1 {
				t.Errorf("HEAD=%d GET=%d, want 1 and 1", h, g)
			}
			if tvdbID, ok := am.LookupByMAL(1); !ok || tvdbID != 100 {
				t.Errorf("expected fresh MAL 1 -> TVDB 100, got %d, %v", tvdbID, ok)
			}
			if meta.ETag != `"v2"` {
				t.Errorf("ETag = %q, want \"v2\"", meta.ETag)
			}
			if _, err := parseAnibridgeFileContext(context.Background(), path); err != nil {
				t.Errorf("cache not rewritten with fresh payload: %v", err)
			}
		})
	}
}

func TestLoadOrFetch_MD5MatchSkipsRewrite(t *testing.T) {
	t.Parallel()

	payload := zstdBytes(fixtureMAL1)
	up := newUpstream(t, `"v2"`, payload)
	path := filepath.Join(t.TempDir(), "mapping.json.zst")
	// The cached file differs from upstream on purpose: a hit on MAL 99
	// proves the MD5 fast path reused the cache instead of the download.
	cached := zstdBytes(`{ "mal:99": { "tvdb_show:999:s1": { "1": "1" } } }`)
	seedCache(t, path, cached, &Metadata{ETag: `"v1"`, MD5: md5Hex(payload), URL: up.URL})

	am, meta, err := LoadOrFetch(context.Background(), path, up.URL)
	if err != nil {
		t.Fatalf("LoadOrFetch: %v", err)
	}
	if tvdbID, ok := am.LookupByMAL(99); !ok || tvdbID != 999 {
		t.Errorf("expected cached MAL 99 -> TVDB 999, got %d, %v", tvdbID, ok)
	}
	if meta.ETag != `"v2"` || !reflect.DeepEqual(meta.MALKeys, []int{99}) {
		t.Errorf("meta = %+v, want ETag \"v2\" and MALKeys [99]", meta)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, cached) {
		t.Error("cache file was rewritten on MD5 match")
	}
}

func TestLoadOrFetch_URLChangeForcesRefresh(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, `"v1"`, zstdBytes(fixtureMAL1))
	path := filepath.Join(t.TempDir(), "mapping.json.zst")
	seedCache(t, path, zstdBytes(`{ "mal:99": { "tvdb_show:999:s1": { "1": "1" } } }`),
		&Metadata{ETag: `"v1"`, URL: "https://example.com/old-url", FetchedAt: time.Now()})

	am, meta, err := LoadOrFetch(context.Background(), path, up.URL)
	if err != nil {
		t.Fatalf("LoadOrFetch: %v", err)
	}
	if meta.URL != up.URL {
		t.Errorf("expected URL updated, got %q", meta.URL)
	}
	if tvdbID, ok := am.LookupByMAL(1); !ok || tvdbID != 100 {
		t.Errorf("expected refreshed mapping, got %d, %v", tvdbID, ok)
	}
	if _, ok := am.LookupByMAL(99); ok {
		t.Error("stale entry from old URL survived refresh")
	}
}

func TestLoadOrFetch_FetchFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cached  []byte
		meta    *Metadata
		wantErr bool
	}{
		{name: "falls back to cache", cached: zstdBytes(fixtureMAL1)},
		{
			name:   "HEAD failure falls back to cache",
			cached: zstdBytes(fixtureMAL1),
			meta:   &Metadata{ETag: `"v1"`},
		},
		{name: "no cache errors", wantErr: true},
		{
			name:    "URL change does not fall back",
			cached:  zstdBytes(fixtureMAL1),
			meta:    &Metadata{ETag: `"v1"`, URL: "https://example.com/old-url"},
			wantErr: true,
		},
		{name: "unreadable cache errors", cached: []byte("not valid zstd"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newStatusServer(t, http.StatusInternalServerError)
			path := filepath.Join(t.TempDir(), "mapping.json.zst")
			if tt.cached != nil {
				seedCache(t, path, tt.cached, tt.meta)
			}

			am, _, err := LoadOrFetch(context.Background(), path, srv.URL)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadOrFetch: %v", err)
			}
			if tvdbID, ok := am.LookupByMAL(1); !ok || tvdbID != 100 {
				t.Errorf("expected cached MAL 1 -> TVDB 100, got %d, %v", tvdbID, ok)
			}
		})
	}
}

func TestLoadOrFetch_WritesKeySnapshotToSidecar(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, `"v1"`, zstdBytes(`{
		"mal:1": { "tvdb_show:100:s1": { "1-12": "1-12" } },
		"anilist:42": { "tvdb_show:200:s1": { "1": "1" } }
	}`))
	path := filepath.Join(t.TempDir(), "mapping.json.zst")

	_, meta, err := LoadOrFetch(context.Background(), path, up.URL)
	if err != nil {
		t.Fatalf("LoadOrFetch: %v", err)
	}
	persisted, err := ReadMetadata(metaPath(path))
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	for name, m := range map[string]Metadata{"returned": meta, "persisted": persisted} {
		if !reflect.DeepEqual(m.MALKeys, []int{1}) || !reflect.DeepEqual(m.AniListKeys, []int{42}) {
			t.Errorf("%s keys: mal=%v ani=%v, want [1] and [42]", name, m.MALKeys, m.AniListKeys)
		}
	}
}

func TestLoadOrFetch_DiffBetweenVersions(t *testing.T) {
	t.Parallel()

	// v1 has MAL 1, 2, 3 and AniList 100.
	v1 := zstdBytes(`{
		"mal:1": { "tvdb_show:101:s1": { "1": "1" } },
		"mal:2": { "tvdb_show:102:s1": { "1": "1" } },
		"mal:3": { "tvdb_show:103:s1": { "1": "1" } },
		"anilist:100": { "tvdb_show:200:s1": { "1": "1" } }
	}`)
	// v2 drops MAL 3, adds MAL 4 and AniList 200.
	v2 := zstdBytes(`{
		"mal:1": { "tvdb_show:101:s1": { "1": "1" } },
		"mal:2": { "tvdb_show:102:s1": { "1": "1" } },
		"mal:4": { "tvdb_show:104:s1": { "1": "1" } },
		"anilist:100": { "tvdb_show:200:s1": { "1": "1" } },
		"anilist:200": { "tvdb_show:201:s1": { "1": "1" } }
	}`)

	up := newUpstream(t, `"v1"`, v1)
	path := filepath.Join(t.TempDir(), "mapping.json.zst")

	for _, step := range []struct {
		etag             string
		payload          []byte
		wantMAL, wantAni int
	}{
		{`"v1"`, v1, 3, 1},
		{`"v2"`, v2, 3, 2},
	} {
		up.set(step.etag, step.payload)
		_, meta, err := LoadOrFetch(context.Background(), path, up.URL)
		if err != nil {
			t.Fatalf("%s: LoadOrFetch: %v", step.etag, err)
		}
		if len(meta.MALKeys) != step.wantMAL || len(meta.AniListKeys) != step.wantAni {
			t.Errorf("%s: got %d MAL / %d AniList keys, want %d / %d",
				step.etag, len(meta.MALKeys), len(meta.AniListKeys), step.wantMAL, step.wantAni)
		}
	}
}

// --- Diff logging -----------------------------------------------------------

// TestLogMappingUpdate is not parallel because it replaces the default logger.
func TestLogMappingUpdate(t *testing.T) {
	tests := []struct {
		name        string
		prev, curr  Metadata
		wantMsg     string
		wantAdded   int64
		wantRemoved int64
	}{
		{
			name:    "fresh install",
			curr:    Metadata{MALKeys: []int{1, 2}, AniListKeys: []int{3}},
			wantMsg: "Loaded anibridge database",
		},
		{
			name:        "added and removed",
			prev:        Metadata{MALKeys: []int{1, 2, 3}, AniListKeys: []int{10, 20}},
			curr:        Metadata{MALKeys: []int{1, 2, 4}, AniListKeys: []int{10, 30}},
			wantMsg:     "Updated anibridge database",
			wantAdded:   2,
			wantRemoved: 2,
		},
		{
			// MAL 100 and AniList 100 are different shows and must not cancel out.
			name:        "MAL and AniList IDs do not collide",
			prev:        Metadata{MALKeys: []int{100}},
			curr:        Metadata{AniListKeys: []int{100}},
			wantMsg:     "Updated anibridge database",
			wantAdded:   1,
			wantRemoved: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := testutil.CaptureLogs(t, slog.LevelInfo)
			malN, aniN := len(tt.curr.MALKeys), len(tt.curr.AniListKeys)
			logMappingUpdate(tt.prev, tt.curr, malN, aniN, time.Millisecond)

			rec, ok := logs.Find(tt.wantMsg)
			if !ok {
				t.Fatalf("no %q log; got %+v", tt.wantMsg, logs.Records())
			}
			if got := rec.Attrs["total_entries"].Int64(); got != int64(malN+aniN) {
				t.Errorf("total_entries = %d, want %d", got, malN+aniN)
			}
			if tt.wantMsg != "Updated anibridge database" {
				return
			}
			if got := rec.Attrs["new"].Int64(); got != tt.wantAdded {
				t.Errorf("new = %d, want %d", got, tt.wantAdded)
			}
			if got := rec.Attrs["removals"].Int64(); got != tt.wantRemoved {
				t.Errorf("removals = %d, want %d", got, tt.wantRemoved)
			}
		})
	}
}

// --- helpers ----------------------------------------------------------------

// upstream is a fake anibridge host that serves ETag and MD5 headers on both
// HEAD and GET, counting each method.
type upstream struct {
	*httptest.Server
	heads, gets atomic.Int64

	mu      sync.Mutex
	etag    string
	payload []byte
}

func newUpstream(t *testing.T, etag string, payload []byte) *upstream {
	t.Helper()
	u := &upstream{etag: etag, payload: payload}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		etag, payload := u.etag, u.payload
		u.mu.Unlock()

		w.Header().Set("ETag", etag)
		w.Header().Set(md5Header, md5B64(payload))
		switch r.Method {
		case http.MethodHead:
			u.heads.Add(1)
		case http.MethodGet:
			u.gets.Add(1)
			w.Write(payload)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) set(etag string, payload []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.etag, u.payload = etag, payload
}

func newStatusServer(t *testing.T, code int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, http.StatusText(code), code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedCache writes raw bytes to path and, when meta is non-nil, the sidecar.
func seedCache(t *testing.T, path string, raw []byte, meta *Metadata) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if meta != nil {
		if err := WriteMetadata(metaPath(path), *meta); err != nil {
			t.Fatal(err)
		}
	}
}

func writeZstdFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, zstdBytes(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func zstdBytes(content string) []byte {
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		panic(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func md5B64(data []byte) string {
	sum := md5.Sum(data)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}
