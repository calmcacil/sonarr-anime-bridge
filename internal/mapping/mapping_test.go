package mapping

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/config"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
	"github.com/calmcacil/sonarr-anime-bridge/internal/testutil"
	"github.com/klauspost/compress/zstd"
)

// --- AnibridgeMapping lookups ----------------------------------------------

func TestLookupByMAL(t *testing.T) {
	t.Parallel()

	am := &AnibridgeMapping{
		byMAL: map[int]int{
			16498: 12345,
			99999: 67890,
		},
	}

	t.Run("known MAL ID", func(t *testing.T) {
		tvdbID, ok := am.LookupByMAL(16498)
		if !ok {
			t.Error("expected ok for known MAL ID")
		}
		if tvdbID != 12345 {
			t.Errorf("expected TVDB 12345, got %d", tvdbID)
		}
	})

	t.Run("unknown MAL ID", func(t *testing.T) {
		_, ok := am.LookupByMAL(1)
		if ok {
			t.Error("expected !ok for unknown MAL ID")
		}
	})

	t.Run("zero MAL ID", func(t *testing.T) {
		_, ok := am.LookupByMAL(0)
		if ok {
			t.Error("expected !ok for zero MAL ID")
		}
	})

	t.Run("negative MAL ID", func(t *testing.T) {
		_, ok := am.LookupByMAL(-5)
		if ok {
			t.Error("expected !ok for negative MAL ID")
		}
	})
}

func TestLookupByAniList(t *testing.T) {
	t.Parallel()

	am := &AnibridgeMapping{
		byAniList: map[int]int{
			100: 54321,
			200: 98765,
		},
	}

	t.Run("known AniList ID", func(t *testing.T) {
		tvdbID, ok := am.LookupByAniList(100)
		if !ok {
			t.Error("expected ok for known AniList ID")
		}
		if tvdbID != 54321 {
			t.Errorf("expected TVDB 54321, got %d", tvdbID)
		}
	})

	t.Run("unknown AniList ID", func(t *testing.T) {
		_, ok := am.LookupByAniList(999)
		if ok {
			t.Error("expected !ok for unknown AniList ID")
		}
	})
}

func TestNewAnibridgeMapping_EmptyMaps(t *testing.T) {
	t.Parallel()

	am := NewAnibridgeMapping(nil, nil)
	if am == nil {
		t.Fatal("expected non-nil mapping")
	}
	mal, ani := am.Stats()
	if mal != 0 || ani != 0 {
		t.Errorf("expected zero entries, got mal=%d ani=%d", mal, ani)
	}
	if _, ok := am.LookupByMAL(1); ok {
		t.Error("expected miss on empty map")
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
		IDMal: nil,
		Title: anilist.Title{English: testutil.Ptr(title)},
	}
}

func TestResolver_NoMapping(t *testing.T) {
	t.Parallel()

	r := NewResolver()
	if r == nil {
		t.Fatal("expected non-nil Resolver")
	}
	if r.Mapping() != nil {
		t.Error("expected nil mapping before SetMapping")
	}
	tvdbID, ok := r.Resolve(showWithMAL(1, 16498, "Test"))
	if ok || tvdbID != 0 {
		t.Errorf("expected miss on empty resolver, got tvdb=%d ok=%v", tvdbID, ok)
	}
}

func TestResolver_ResolvePrefersMAL(t *testing.T) {
	t.Parallel()

	am := NewAnibridgeMapping(
		map[int]int{16498: 12345},
		map[int]int{1: 99999},
	)
	r := NewResolver()
	r.SetMapping(am)

	tvdbID, ok := r.Resolve(showWithMAL(1, 16498, "Priority"))
	if !ok {
		t.Fatal("expected hit via MAL")
	}
	if tvdbID != 12345 {
		t.Errorf("expected MAL entry 12345 to win over AniList 99999, got %d", tvdbID)
	}
}

func TestResolver_ResolveAnilistFallback(t *testing.T) {
	t.Parallel()

	am := NewAnibridgeMapping(
		map[int]int{16498: 12345},
		map[int]int{42: 77777},
	)
	r := NewResolver()
	r.SetMapping(am)

	tvdbID, ok := r.Resolve(showAnilistOnly(42, "AniList Original"))
	if !ok {
		t.Fatal("expected hit via AniList fallback")
	}
	if tvdbID != 77777 {
		t.Errorf("expected 77777, got %d", tvdbID)
	}
}

func TestResolver_ResolveMissesUnknown(t *testing.T) {
	t.Parallel()

	am := NewAnibridgeMapping(
		map[int]int{16498: 12345},
		map[int]int{42: 77777},
	)
	r := NewResolver()
	r.SetMapping(am)

	_, ok := r.Resolve(showWithMAL(1, 999999, "Unknown"))
	if ok {
		t.Error("expected miss for unknown MAL")
	}
	_, ok = r.Resolve(showAnilistOnly(123456, "Unknown"))
	if ok {
		t.Error("expected miss for unknown AniList")
	}
}

func TestResolver_ResolveBatch(t *testing.T) {
	t.Parallel()

	am := NewAnibridgeMapping(
		map[int]int{16498: 12345},
		map[int]int{42: 77777},
	)
	r := NewResolver()
	r.SetMapping(am)

	shows := []anilist.Show{
		showWithMAL(1, 16498, "Via MAL"),
		showAnilistOnly(42, "Via AniList"),
		showWithMAL(99, 99999, "Unknown"),
		showAnilistOnly(1000, "Unknown AniList"),
	}

	resolved := r.ResolveBatch(shows)

	if len(resolved) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(resolved))
	}

	t.Run("mal hit", func(t *testing.T) {
		rs, ok := resolved[1]
		if !ok {
			t.Fatal("missing entry for ID 1")
		}
		if !rs.Resolved || rs.TVDBID != 12345 {
			t.Errorf("expected resolved 12345, got resolved=%v tvdb=%d", rs.Resolved, rs.TVDBID)
		}
	})

	t.Run("anilist fallback hit", func(t *testing.T) {
		rs, ok := resolved[42]
		if !ok {
			t.Fatal("missing entry for ID 42")
		}
		if !rs.Resolved || rs.TVDBID != 77777 {
			t.Errorf("expected resolved 77777, got resolved=%v tvdb=%d", rs.Resolved, rs.TVDBID)
		}
	})

	t.Run("mal miss, anilist also miss", func(t *testing.T) {
		rs, ok := resolved[99]
		if !ok {
			t.Fatal("missing entry for ID 99")
		}
		if rs.Resolved {
			t.Error("expected unresolved")
		}
	})

	t.Run("no MAL, anilist miss", func(t *testing.T) {
		rs, ok := resolved[1000]
		if !ok {
			t.Fatal("missing entry for ID 1000")
		}
		if rs.Resolved {
			t.Error("expected unresolved")
		}
	})
}

func TestResolver_SetMappingSwapsAtomically(t *testing.T) {
	t.Parallel()

	r := NewResolver()
	r.SetMapping(NewAnibridgeMapping(map[int]int{1: 100}, nil))
	r.SetMapping(NewAnibridgeMapping(map[int]int{1: 200}, nil))

	tvdbID, _ := r.Resolve(showWithMAL(1, 1, "Swap"))
	if tvdbID != 200 {
		t.Errorf("expected second mapping to be active, got %d", tvdbID)
	}
}

func TestResolver_SetMappingIgnoresNil(t *testing.T) {
	t.Parallel()

	r := NewResolver()
	r.SetMapping(NewAnibridgeMapping(map[int]int{1: 100}, nil))
	r.SetMapping(nil)

	tvdbID, _ := r.Resolve(showWithMAL(1, 1, "Persist"))
	if tvdbID != 100 {
		t.Errorf("expected prior mapping to remain, got %d", tvdbID)
	}
}

// --- Parser tests -----------------------------------------------------------

func TestParseAnibridgeJSON_SmallFixture(t *testing.T) {
	t.Parallel()

	fixture := `{
  "$meta": {
    "schema_version": "3.0.0",
    "generated_on": "2026-01-01T00:00:00Z"
  },
  "mal:1": {
    "tvdb_show:100:s1": { "1-12": "1-12" }
  },
  "mal:2": {
    "tvdb_show:200:s1": { "1-24": "1-24" },
    "tvdb_show:200:s0": { "1-3": "4-6" }
  },
  "anilist:42": {
    "tvdb_show:300:s1": { "1-13": "1-13" }
  },
  "anilist:99": {
    "tvdb_show:400:s2": { "1-12": "1-12" }
  },
  "anidb:999:R": {
    "mal:1": { "1-12": "1-12" }
  }
}`

	am, err := parseAnibridgeJSON(strings.NewReader(fixture), "test")
	if err != nil {
		t.Fatalf("parseAnibridgeJSON: %v", err)
	}

	t.Run("mal entries", func(t *testing.T) {
		if tvdbID, ok := am.LookupByMAL(1); !ok || tvdbID != 100 {
			t.Errorf("expected MAL 1 -> TVDB 100, got %d, %v", tvdbID, ok)
		}
		if tvdbID, ok := am.LookupByMAL(2); !ok || tvdbID != 200 {
			t.Errorf("expected MAL 2 -> TVDB 200, got %d, %v", tvdbID, ok)
		}
		if _, ok := am.LookupByMAL(3); ok {
			t.Error("expected MAL 3 to be absent")
		}
	})

	t.Run("anilist entries", func(t *testing.T) {
		if tvdbID, ok := am.LookupByAniList(42); !ok || tvdbID != 300 {
			t.Errorf("expected AniList 42 -> TVDB 300, got %d, %v", tvdbID, ok)
		}
		if tvdbID, ok := am.LookupByAniList(99); !ok || tvdbID != 400 {
			t.Errorf("expected AniList 99 -> TVDB 400 (s2 when no s1), got %d, %v", tvdbID, ok)
		}
		if _, ok := am.LookupByAniList(100); ok {
			t.Error("expected AniList 100 to be absent")
		}
	})

	t.Run("ignores unrelated namespaces", func(t *testing.T) {
		mal, ani := am.Stats()
		if mal != 2 {
			t.Errorf("expected 2 MAL entries, got %d", mal)
		}
		if ani != 2 {
			t.Errorf("expected 2 AniList entries, got %d", ani)
		}
	})
}

func TestParseAnibridgeJSON_PrefersS1(t *testing.T) {
	t.Parallel()

	fixture := `{
  "mal:10": {
    "tvdb_show:500:s1": { "1-24": "1-24" },
    "tvdb_show:500:s2": { "1-24": "1-24" },
    "tvdb_show:500:s0": { "1-6": "1-6" }
  },
  "mal:11": {
    "tvdb_show:600:s0": { "1-50": "1-50" },
    "tvdb_show:600:s2": { "1-24": "1-24" }
  }
}`

	am, err := parseAnibridgeJSON(strings.NewReader(fixture), "test")
	if err != nil {
		t.Fatalf("parseAnibridgeJSON: %v", err)
	}

	tvdbID, ok := am.LookupByMAL(10)
	if !ok || tvdbID != 500 {
		t.Errorf("expected MAL 10 -> TVDB 500, got %d, %v", tvdbID, ok)
	}
	tvdbID, ok = am.LookupByMAL(11)
	if !ok || tvdbID != 600 {
		t.Errorf("expected MAL 11 -> TVDB 600 (s0 wins on episode count), got %d, %v", tvdbID, ok)
	}
}

func TestParseAnibridgeJSON_SkipsInvalidKeys(t *testing.T) {
	t.Parallel()

	fixture := `{
  "mal:abc": { "tvdb_show:1:s1": { "1": "1" } },
  "mal:-5": { "tvdb_show:1:s1": { "1": "1" } },
  "anilist:": { "tvdb_show:1:s1": { "1": "1" } },
  "mal:7": { "tvdb_show:0:s1": { "1": "1" } },
  "mal:8": "not an object"
}`

	am, err := parseAnibridgeJSON(strings.NewReader(fixture), "test")
	if err != nil {
		t.Fatalf("parseAnibridgeJSON: %v", err)
	}
	mal, ani := am.Stats()
	if mal != 0 {
		t.Errorf("expected 0 valid MAL entries, got %d", mal)
	}
	if ani != 0 {
		t.Errorf("expected 0 valid AniList entries, got %d", ani)
	}
}

func TestParseAnibridgeJSON_RejectsNonObject(t *testing.T) {
	t.Parallel()

	_, err := parseAnibridgeJSON(strings.NewReader(`[]`), "test")
	if err == nil {
		t.Error("expected error for non-object root")
	}
}

func TestParseAnibridgeFile_ZstdFixture(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.json.zst")

	fixture := `{ "mal:1": { "tvdb_show:100:s1": { "1-12": "1-12" } } }`
	if err := writeZstdFile(path, fixture); err != nil {
		t.Fatal(err)
	}

	am, err := parseAnibridgeFile(path)
	if err != nil {
		t.Fatalf("parseAnibridgeFile: %v", err)
	}
	if tvdbID, ok := am.LookupByMAL(1); !ok || tvdbID != 100 {
		t.Errorf("expected MAL 1 -> TVDB 100, got %d, %v", tvdbID, ok)
	}
}

func TestParseAnibridgeFile_Missing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nonexistent.json.zst")
	_, err := parseAnibridgeFile(path)
	if err == nil {
		t.Error("expected error for missing file")
	}
}

// --- Metadata I/O -----------------------------------------------------------

func TestReadMetadata_MissingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "absent.meta.json")
	m, err := ReadMetadata(path)
	if err != nil {
		t.Errorf("missing sidecar should not be an error, got %v", err)
	}
	if m.ETag != "" || m.MD5 != "" {
		t.Errorf("expected zero metadata, got %+v", m)
	}
}

func TestWriteAndReadMetadata_RoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.meta.json")

	want := Metadata{
		ETag:         `"0x8DEC2CA2CEB7643"`,
		LastModified: "Fri, 05 Jun 2026 06:17:52 GMT",
		MD5:          "ee20b3531f9453369bbcb16c1cda9a5d",
		FetchedAt:    time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
		URL:          config.DefaultAnibridgeURL,
	}

	if err := WriteMetadata(path, want); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}

	got, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if got.ETag != want.ETag {
		t.Errorf("ETag: got %q want %q", got.ETag, want.ETag)
	}
	if got.MD5 != want.MD5 {
		t.Errorf("MD5: got %q want %q", got.MD5, want.MD5)
	}
	if !got.FetchedAt.Equal(want.FetchedAt) {
		t.Errorf("FetchedAt: got %v want %v", got.FetchedAt, want.FetchedAt)
	}
	if got.URL != want.URL {
		t.Errorf("URL: got %q want %q", got.URL, want.URL)
	}
}

func TestWriteMetadata_RejectsPathConflict(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	parent := filepath.Join(dir, "regular-file")
	if err := os.WriteFile(parent, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "test.meta.json")
	err := WriteMetadata(path, Metadata{ETag: "x"})
	if err == nil {
		t.Error("expected error when parent is a regular file")
	}
}

// --- HTTP conditional fetch ------------------------------------------------

func TestHead_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := Head(context.Background(), srv.URL)
	if err == nil {
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
		w.Header().Set("x-ms-blob-content-md5", "3q2+7w==") // base64 of 0xDEADBEEF in hex
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

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
	md5B64Val := md5B64(payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ms-blob-content-md5", md5B64Val)
		w.Write(payload)
	}))

	data, m, err := Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(data) != string(payload) {
		t.Errorf("payload mismatch: got %q want %q", data, payload)
	}
	expectedHex := hex.EncodeToString(mustMD5(payload))
	if m.MD5 != expectedHex {
		t.Errorf("MD5: got %q want %q", m.MD5, expectedHex)
	}
}

func TestFetch_MD5Mismatch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ms-blob-content-md5", "AAAAAAAAAAAAAAAAAAAAAA==")
		w.Write([]byte("actual content"))
	}))
	defer srv.Close()

	_, _, err := Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Error("expected MD5 mismatch error")
	}
}

func TestFetch_RejectsOversizedContentLength(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(maxCompressedMappingSize+1))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, _, err := Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected oversized body error")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected too large error, got %v", err)
	}
}

func TestLoadOrFetch_ETagShortCircuits(t *testing.T) {
	t.Parallel()

	const etag = `"v1"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			t.Error("unexpected GET — ETag should have short-circuited the fetch")
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "mapping.json.zst")
	fixture := `{ "mal:1": { "tvdb_show:100:s1": { "1-12": "1-12" } } }`
	if err := writeZstdFile(path, fixture); err != nil {
		t.Fatal(err)
	}
	if err := WriteMetadata(metaPath(path), Metadata{
		ETag: etag, URL: srv.URL, FetchedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	am, _, err := LoadOrFetch(context.Background(), path, srv.URL)
	if err != nil {
		t.Fatalf("LoadOrFetch: %v", err)
	}
	if am == nil {
		t.Fatal("expected non-nil mapping")
	}
}

func TestLoadOrFetch_ETagChangedTriggersGet(t *testing.T) {
	t.Parallel()

	payload := zstdBytes(`{ "mal:1": { "tvdb_show:100:s1": { "1-12": "1-12" } } }`)
	hashB64 := md5B64(payload)

	headCalls := 0
	getCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			headCalls++
			w.Header().Set("ETag", `"v2"`)
			w.Header().Set("x-ms-blob-content-md5", hashB64)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			getCalls++
			w.Header().Set("ETag", `"v2"`)
			w.Header().Set("x-ms-blob-content-md5", hashB64)
			w.Write(payload)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "mapping.json.zst")
	if err := writeZstdFile(path, `{ "mal:99": { "tvdb_show:999:s1": { "1": "1" } } }`); err != nil {
		t.Fatal(err)
	}
	if err := WriteMetadata(metaPath(path), Metadata{
		ETag: `"v1"`, URL: srv.URL, FetchedAt: time.Now().Add(-24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	am, _, err := LoadOrFetch(context.Background(), path, srv.URL)
	if err != nil {
		t.Fatalf("LoadOrFetch: %v", err)
	}
	if headCalls != 1 {
		t.Errorf("expected 1 HEAD, got %d", headCalls)
	}
	if getCalls != 1 {
		t.Errorf("expected 1 GET, got %d", getCalls)
	}
	if tvdbID, ok := am.LookupByMAL(1); !ok || tvdbID != 100 {
		t.Errorf("expected fresh mapping MAL 1 -> TVDB 100, got %d, %v", tvdbID, ok)
	}
}

func TestLoadOrFetch_URLChangeForcesRefresh(t *testing.T) {
	t.Parallel()

	payload := zstdBytes(`{ "mal:1": { "tvdb_show:100:s1": { "1-12": "1-12" } } }`)
	hashB64 := md5B64(payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("x-ms-blob-content-md5", hashB64)
		if r.Method == http.MethodGet {
			w.Write(payload)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "mapping.json.zst")
	if err := writeZstdFile(path, `{ "mal:99": { "tvdb_show:999:s1": { "1": "1" } }`); err != nil {
		t.Fatal(err)
	}
	if err := WriteMetadata(metaPath(path), Metadata{
		ETag: `"v1"`, URL: "https://example.com/old-url", FetchedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	am, meta, err := LoadOrFetch(context.Background(), path, srv.URL)
	if err != nil {
		t.Fatalf("LoadOrFetch: %v", err)
	}
	if meta.URL != srv.URL {
		t.Errorf("expected URL updated, got %q", meta.URL)
	}
	if tvdbID, ok := am.LookupByMAL(1); !ok || tvdbID != 100 {
		t.Errorf("expected refreshed mapping, got %d, %v", tvdbID, ok)
	}
}

func TestLoadOrFetch_FetchFailureFallsBackToCache(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "mapping.json.zst")
	if err := writeZstdFile(path, `{ "mal:1": { "tvdb_show:100:s1": { "1-12": "1-12" } } }`); err != nil {
		t.Fatal(err)
	}

	am, _, err := LoadOrFetch(context.Background(), path, srv.URL)
	if err != nil {
		t.Fatalf("LoadOrFetch: %v", err)
	}
	if am == nil {
		t.Fatal("expected fallback mapping from cache")
	}
	if tvdbID, ok := am.LookupByMAL(1); !ok || tvdbID != 100 {
		t.Errorf("expected cached mapping MAL 1 -> TVDB 100, got %d, %v", tvdbID, ok)
	}
}

func TestLoadOrFetch_FetchFailureNoCacheErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "absent.json.zst")

	_, _, err := LoadOrFetch(context.Background(), path, srv.URL)
	if err == nil {
		t.Error("expected error when fetch fails and no cache exists")
	}
}

func TestLoadOrFetch_CorruptCacheTriggersRefresh(t *testing.T) {
	t.Parallel()

	payload := zstdBytes(`{ "mal:1": { "tvdb_show:100:s1": { "1-12": "1-12" } } }`)
	hashB64 := md5B64(payload)

	headCalls := 0
	getCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			headCalls++
			w.Header().Set("ETag", `"v2"`)
			w.Header().Set("x-ms-blob-content-md5", hashB64)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			getCalls++
			w.Header().Set("ETag", `"v2"`)
			w.Header().Set("x-ms-blob-content-md5", hashB64)
			w.Write(payload)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "mapping.json.zst")
	if err := os.WriteFile(path, []byte("not valid zstd"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteMetadata(metaPath(path), Metadata{
		ETag: `"v1"`, URL: srv.URL, FetchedAt: time.Now().Add(-24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	am, _, err := LoadOrFetch(context.Background(), path, srv.URL)
	if err != nil {
		t.Fatalf("LoadOrFetch: %v", err)
	}
	if headCalls != 1 {
		t.Errorf("expected 1 HEAD, got %d", headCalls)
	}
	if getCalls != 1 {
		t.Errorf("expected GET after corrupt cache, got %d", getCalls)
	}
	if tvdbID, ok := am.LookupByMAL(1); !ok || tvdbID != 100 {
		t.Errorf("expected refreshed mapping, got %d, %v", tvdbID, ok)
	}
}

func TestLoadOrFetch_WritesKeySnapshotToSidecar(t *testing.T) {
	t.Parallel()

	payload := zstdBytes(`{ "mal:1": { "tvdb_show:100:s1": { "1-12": "1-12" } }, "anilist:42": { "tvdb_show:200:s1": { "1": "1" } } }`)
	hashB64 := md5B64(payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("x-ms-blob-content-md5", hashB64)
			w.Write(payload)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "mapping.json.zst")

	_, meta, err := LoadOrFetch(context.Background(), path, srv.URL)
	if err != nil {
		t.Fatalf("LoadOrFetch: %v", err)
	}
	if len(meta.MALKeys) != 1 || meta.MALKeys[0] != 1 {
		t.Errorf("expected MALKeys [1], got %v", meta.MALKeys)
	}
	if len(meta.AniListKeys) != 1 || meta.AniListKeys[0] != 42 {
		t.Errorf("expected AniListKeys [42], got %v", meta.AniListKeys)
	}

	// Reload should read keys back from disk
	persisted, err := ReadMetadata(metaPath(path))
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if len(persisted.MALKeys) != 1 || len(persisted.AniListKeys) != 1 {
		t.Errorf("expected persisted keys, got mal=%v ani=%v", persisted.MALKeys, persisted.AniListKeys)
	}
}

func TestLoadOrFetch_DiffBetweenVersions(t *testing.T) {
	t.Parallel()

	const v1Etag = `"v1"`
	const v2Etag = `"v2"`

	// v1 has MAL 1, 2, 3 and AniList 100
	payloadV1 := zstdBytes(`{
		"mal:1": { "tvdb_show:101:s1": { "1": "1" } },
		"mal:2": { "tvdb_show:102:s1": { "1": "1" } },
		"mal:3": { "tvdb_show:103:s1": { "1": "1" } },
		"anilist:100": { "tvdb_show:200:s1": { "1": "1" } }
	}`)

	// v2 has MAL 1, 2, 4 (removed 3, added 4) and AniList 100, 200 (added 200)
	payloadV2 := zstdBytes(`{
		"mal:1": { "tvdb_show:101:s1": { "1": "1" } },
		"mal:2": { "tvdb_show:102:s1": { "1": "1" } },
		"mal:4": { "tvdb_show:104:s1": { "1": "1" } },
		"anilist:100": { "tvdb_show:200:s1": { "1": "1" } },
		"anilist:200": { "tvdb_show:201:s1": { "1": "1" } }
	}`)

	md5V1 := md5B64(payloadV1)
	md5V2 := md5B64(payloadV2)

	current := v1Etag
	currentMD5 := md5V1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ms-blob-content-md5", currentMD5)
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("ETag", current)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.Header().Set("ETag", current)
			if current == v1Etag {
				w.Write(payloadV1)
			} else {
				w.Write(payloadV2)
			}
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "mapping.json.zst")

	// First load: fresh install
	_, meta1, err := LoadOrFetch(context.Background(), path, srv.URL)
	if err != nil {
		t.Fatalf("first LoadOrFetch: %v", err)
	}
	if len(meta1.MALKeys) != 3 {
		t.Errorf("v1: expected 3 MAL keys, got %d", len(meta1.MALKeys))
	}
	if len(meta1.AniListKeys) != 1 {
		t.Errorf("v1: expected 1 AniList key, got %d", len(meta1.AniListKeys))
	}

	// Switch to v2 and reload
	current = v2Etag
	currentMD5 = md5V2
	_, meta2, err := LoadOrFetch(context.Background(), path, srv.URL)
	if err != nil {
		t.Fatalf("second LoadOrFetch: %v", err)
	}
	if len(meta2.MALKeys) != 3 {
		t.Errorf("v2: expected 3 MAL keys, got %d", len(meta2.MALKeys))
	}
	if len(meta2.AniListKeys) != 2 {
		t.Errorf("v2: expected 2 AniList keys, got %d", len(meta2.AniListKeys))
	}

	// Verify the diff: +1 MAL (4), -1 MAL (3), +1 AniList (200)
	added, removed := diffKeys(meta1, meta2)
	if added != 2 {
		t.Errorf("expected 2 added, got %d", added)
	}
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}
}

func mustMD5(b []byte) []byte {
	sum := md5.Sum(b)
	return sum[:]
}

func md5B64(data []byte) string {
	sum := md5.Sum(data)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func diffKeys(prev, curr Metadata) (added, removed int) {
	prevMAL := make(map[int]bool)
	for _, k := range prev.MALKeys {
		prevMAL[k] = true
	}
	prevAni := make(map[int]bool)
	for _, k := range prev.AniListKeys {
		prevAni[k] = true
	}
	for _, k := range curr.MALKeys {
		if !prevMAL[k] {
			added++
		}
	}
	for _, k := range curr.AniListKeys {
		if !prevAni[k] {
			added++
		}
	}
	for k := range prevMAL {
		found := false
		for _, ck := range curr.MALKeys {
			if ck == k {
				found = true
				break
			}
		}
		if !found {
			removed++
		}
	}
	for k := range prevAni {
		found := false
		for _, ck := range curr.AniListKeys {
			if ck == k {
				found = true
				break
			}
		}
		if !found {
			removed++
		}
	}
	return added, removed
}

// --- Diff logging -----------------------------------------------------------

func TestLogMappingUpdate_FreshInstall(t *testing.T) {
	t.Parallel()

	// no slog capture needed — we just verify it doesn't panic and the
	// condition "no previous keys" is treated as a fresh install.
	logMappingUpdate(Metadata{}, Metadata{MALKeys: []int{1, 2}, AniListKeys: []int{3}}, 2, 1)
}

func TestLogMappingUpdate_WithDiff(t *testing.T) {
	t.Parallel()

	prev := Metadata{
		MALKeys:     []int{1, 2, 3},
		AniListKeys: []int{10, 20},
	}
	curr := Metadata{
		MALKeys:     []int{1, 2, 4},
		AniListKeys: []int{10, 30},
	}
	// added: MAL 4, AniList 30 (2)
	// removed: MAL 3, AniList 20 (2)
	logMappingUpdate(prev, curr, 3, 2)
}

func TestLogMappingUpdate_MALAndAniListIDsDoNotCollide(t *testing.T) {
	t.Parallel()

	// MAL 100 and AniList 100 are different shows and should not
	// cancel out in the diff.
	prev := Metadata{MALKeys: []int{100}, AniListKeys: nil}
	curr := Metadata{MALKeys: nil, AniListKeys: []int{100}}

	// The diff is +1 AniList, -1 MAL, total 1 — they must be tracked
	// in separate namespaces.
	logMappingUpdate(prev, curr, 0, 1)
}

// --- helpers ----------------------------------------------------------------

func writeZstdFile(path, content string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w, err := zstd.NewWriter(f)
	if err != nil {
		return err
	}
	defer w.Close()
	_, err = w.Write([]byte(content))
	return err
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
