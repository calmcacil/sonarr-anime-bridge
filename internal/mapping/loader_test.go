package mapping

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/testutil"
)

func countingLoader() (*Loader, *atomic.Int32) {
	l := NewLoader()
	count := new(atomic.Int32)
	l.parse = func(ctx context.Context, r io.Reader, src string) (*AnibridgeMapping, error) {
		count.Add(1)
		return parseAnibridge(ctx, r, src)
	}
	return l, count
}

func mustLoad(t *testing.T, l *Loader, path, url string) LoadResult {
	t.Helper()
	result, err := l.Load(context.Background(), path, url)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestLoaderVerifiedLocalReuseAndRestart(t *testing.T) {
	logs := testutil.CaptureLogs(t, slog.LevelDebug)
	up := newUpstream(t, `"v1"`, zstdBytes(fixtureMAL1))
	path := filepath.Join(t.TempDir(), "mapping.zst")
	seedCache(t, path, up.payload, &Metadata{ETag: `"v1"`, URL: up.URL})
	l, parses := countingLoader()
	first := mustLoad(t, l, path, up.URL)
	second := mustLoad(t, l, filepath.Join(filepath.Dir(path), ".", filepath.Base(path)), up.URL)
	if first.Disposition != Loaded || second.Disposition != Reused || second.Mapping != first.Mapping || parses.Load() != 1 {
		t.Fatalf("first=%s second=%s parses=%d; want one parse and identical pointer", first.Disposition, second.Disposition, parses.Load())
	}
	if up.gets.Load() != 0 || up.heads.Load() != 2 {
		t.Fatalf("GET=%d HEAD=%d; unchanged local refresh should only HEAD", up.gets.Load(), up.heads.Load())
	}
	parseLogs, reuseLogs := 0, 0
	for _, rec := range logs.Records() {
		if rec.Attrs["task"].String() == "mapping_parse" && rec.Attrs["outcome"].String() == "succeeded" {
			parseLogs++
		}
		if rec.Attrs["reason"].String() == "unchanged_content" {
			reuseLogs++
		}
	}
	if parseLogs != 1 || reuseLogs != 1 {
		t.Fatalf("parse logs=%d reuse logs=%d", parseLogs, reuseLogs)
	}
	restarted, restartedParses := countingLoader()
	third := mustLoad(t, restarted, path, up.URL)
	if third.Disposition != Loaded || restartedParses.Load() != 1 || third.Mapping == first.Mapping {
		t.Fatal("restart trusted persisted metadata without parsing")
	}
}

func TestLoaderConcurrentRefreshesParseOnce(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, `"v1"`, zstdBytes(fixtureMAL1))
	path := filepath.Join(t.TempDir(), "mapping.zst")
	seedCache(t, path, up.payload, &Metadata{ETag: `"v1"`, URL: up.URL})
	l, parses := countingLoader()
	const callers = 8
	results := make(chan LoadResult, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			result, err := l.Load(context.Background(), path, up.URL)
			if err != nil {
				t.Error(err)
			}
			results <- result
		})
	}
	wg.Wait()
	close(results)
	loaded, reused := 0, 0
	for result := range results {
		if result.Mapping != l.state.mapping {
			t.Fatal("concurrent refreshes published different mappings")
		}
		if result.Disposition == Loaded {
			loaded++
		} else if result.Disposition == Reused {
			reused++
		}
	}
	if loaded != 1 || reused != callers-1 || parses.Load() != 1 {
		t.Fatalf("loaded=%d reused=%d parses=%d", loaded, reused, parses.Load())
	}
}

func TestLoaderDetectsLocalChanges(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "in-place same size and timestamp", true: "atomic replacement"}[replacement], func(t *testing.T) {
			t.Parallel()
			up := newUpstream(t, `"v1"`, zstdBytes(fixtureMAL1))
			path := filepath.Join(t.TempDir(), "mapping.zst")
			seedCache(t, path, up.payload, &Metadata{ETag: `"v1"`, URL: up.URL})
			l, parses := countingLoader()
			first := mustLoad(t, l, path, up.URL)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := zstdBytes(strings.ReplaceAll(fixtureMAL1, "100", "200"))
			if len(changed) != len(up.payload) {
				t.Fatal("fixture must preserve compressed size")
			}
			writePath := path
			if replacement {
				writePath += ".replacement"
			}
			if err := os.WriteFile(writePath, changed, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(writePath, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if replacement {
				if err := os.Rename(writePath, path); err != nil {
					t.Fatal(err)
				}
			}
			second := mustLoad(t, l, path, up.URL)
			id, ok := second.Mapping.LookupByMAL(1)
			if second.Disposition != Loaded || second.Mapping == first.Mapping || parses.Load() != 2 || !ok || id != 200 {
				t.Fatalf("changed local source not parsed: disposition=%s parses=%d id=%d", second.Disposition, parses.Load(), id)
			}
			if again := mustLoad(t, l, path, up.URL); again.Mapping != second.Mapping || again.Disposition != Reused || parses.Load() != 2 {
				t.Fatal("new local content not retained for reuse")
			}
		})
	}
}

func TestLoaderIdenticalGETRepairsCache(t *testing.T) {
	for _, damage := range []string{"none", "missing", "different", "corrupt"} {
		t.Run(damage, func(t *testing.T) {
			t.Parallel()
			payload := zstdBytes(fixtureMAL1)
			// No MD5 or ETag: byte identity must work without remote metadata.
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) }))
			t.Cleanup(up.Close)
			path := filepath.Join(t.TempDir(), "mapping.zst")
			l, parses := countingLoader()
			first := mustLoad(t, l, path, up.URL)
			switch damage {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "different", "corrupt":
				data := []byte("corrupt")
				if damage == "different" {
					data = zstdBytes(strings.ReplaceAll(fixtureMAL1, "100", "200"))
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if damage == "none" {
				l.write = func(string, []byte) error { return errors.New("identical verified file must not be rewritten") }
			}
			second := mustLoad(t, l, path, up.URL)
			onDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if second.Disposition != Reused || first.Mapping != second.Mapping || parses.Load() != 1 || !bytes.Equal(payload, onDisk) {
				t.Fatalf("download reuse/repair failed: disposition=%s parses=%d", second.Disposition, parses.Load())
			}
			meta, err := ReadMetadata(metaPath(path))
			if err != nil || len(meta.MALKeys) != 1 || meta.MALKeys[0] != 1 {
				t.Fatalf("key snapshot missing after reuse: %+v %v", meta, err)
			}
		})
	}
}

func TestLoaderSidecarDoesNotProveIdentity(t *testing.T) {
	for _, sidecar := range []string{"missing", "corrupt", "edited"} {
		t.Run(sidecar, func(t *testing.T) {
			t.Parallel()
			up := newUpstream(t, `"v1"`, zstdBytes(fixtureMAL1))
			path := filepath.Join(t.TempDir(), "mapping.zst")
			l, parses := countingLoader()
			first := mustLoad(t, l, path, up.URL)
			switch sidecar {
			case "missing":
				if err := os.Remove(metaPath(path)); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(metaPath(path), []byte("broken JSON"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "edited":
				if err := WriteMetadata(metaPath(path), Metadata{ETag: `"v1"`, URL: up.URL, MD5: "forged"}); err != nil {
					t.Fatal(err)
				}
			}
			second := mustLoad(t, l, path, up.URL)
			if second.Mapping != first.Mapping || second.Disposition != Reused || parses.Load() != 1 {
				t.Fatal("unchanged bytes should reuse despite unusable freshness metadata")
			}
			// Matching metadata must not hide a subsequent local edit.
			changed := zstdBytes(strings.ReplaceAll(fixtureMAL1, "100", "200"))
			if err := os.WriteFile(path, changed, 0o600); err != nil {
				t.Fatal(err)
			}
			third := mustLoad(t, l, path, up.URL)
			if id, _ := third.Mapping.LookupByMAL(1); id != 200 || parses.Load() != 2 {
				t.Fatal("sidecar hid changed bytes")
			}
		})
	}
}

func TestLoaderRetainsOnlyCurrentSource(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, `"v1"`, zstdBytes(fixtureMAL1))
	other := newUpstream(t, `"v1"`, up.payload)
	path := filepath.Join(t.TempDir(), "mapping.zst")
	otherPath := filepath.Join(t.TempDir(), "mapping.zst")
	l, parses := countingLoader()
	first := mustLoad(t, l, path, up.URL)
	second := mustLoad(t, l, otherPath, up.URL)
	if second.Mapping == first.Mapping || parses.Load() != 2 {
		t.Fatal("path change reused previous source")
	}
	third := mustLoad(t, l, path, up.URL)
	if third.Mapping == first.Mapping || parses.Load() != 3 {
		t.Fatal("loader retained historical source")
	}
	// Simulate missing source ownership on disk; memory still knows the URL.
	if err := os.Remove(metaPath(path)); err != nil {
		t.Fatal(err)
	}
	fourth := mustLoad(t, l, path, other.URL)
	if fourth.Mapping == third.Mapping || fourth.Disposition != Loaded || parses.Load() != 4 || other.gets.Load() != 1 || other.heads.Load() != 0 {
		t.Fatal("changed URL reused old parsing or old cache metadata")
	}
	if l.state.path != path || l.state.url != other.URL || l.state.mapping != fourth.Mapping {
		t.Fatal("current source state not replaced")
	}
}

func TestLoaderChangedURLCannotFallbackAfterSidecarEdit(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, `"v1"`, zstdBytes(fixtureMAL1))
	failed := newStatusServer(t, http.StatusServiceUnavailable)
	path := filepath.Join(t.TempDir(), "mapping.zst")
	l, _ := countingLoader()
	first := mustLoad(t, l, path, up.URL)
	if err := WriteMetadata(metaPath(path), Metadata{ETag: `"v1"`, URL: failed.URL}); err != nil {
		t.Fatal(err)
	}
	if result, err := l.Load(context.Background(), path, failed.URL); err == nil || result.Mapping != nil || l.state.mapping != first.Mapping || l.state.url != up.URL {
		t.Fatal("changed URL fell back to old cache or advanced reusable state")
	}
}

func TestLoaderFailureFallbackIsNotVerifiedReuse(t *testing.T) {
	logs := testutil.CaptureLogs(t, slog.LevelDebug)
	up := newUpstream(t, `"v1"`, zstdBytes(fixtureMAL1))
	path := filepath.Join(t.TempDir(), "mapping.zst")
	l, parses := countingLoader()
	first := mustLoad(t, l, path, up.URL)
	up.Close()
	second := mustLoad(t, l, path, up.URL)
	if second.Disposition != Fallback || second.Mapping != first.Mapping || parses.Load() != 1 {
		t.Fatal("network failure should reuse verified cache as a fallback")
	}
	fallbacks, reuses := 0, 0
	for _, rec := range logs.Records() {
		if rec.Attrs["action"].String() == "use_cache" {
			fallbacks++
			if rec.Attrs["outcome"].String() != "degraded" || rec.Level != slog.LevelWarn {
				t.Fatal("fallback falsely reported freshness")
			}
		}
		if rec.Attrs["reason"].String() == "unchanged_content" {
			reuses++
		}
		for _, attr := range rec.Attrs {
			if strings.Contains(attr.String(), up.URL) {
				t.Fatalf("configured URL leaked: %v", rec)
			}
		}
	}
	if fallbacks != 1 || reuses != 0 {
		t.Fatalf("fallback logs=%d fresh reuse logs=%d", fallbacks, reuses)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if result, err := l.Load(context.Background(), path, up.URL); err == nil || result.Mapping != nil || l.state.mapping != first.Mapping {
		t.Fatal("remembered parsing concealed missing cache and network failure")
	}
}

func TestLoaderFailedWriteDoesNotAdvanceState(t *testing.T) {
	for _, identical := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed download", true: "identical download repairs missing cache"}[identical], func(t *testing.T) {
			t.Parallel()
			up := newUpstream(t, `"v1"`, zstdBytes(fixtureMAL1))
			path := filepath.Join(t.TempDir(), "mapping.zst")
			l, parses := countingLoader()
			first := mustLoad(t, l, path, up.URL)
			before := l.state
			if identical {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				up.set(`"v2"`, zstdBytes(strings.ReplaceAll(fixtureMAL1, "100", "200")))
			}
			writeErr := errors.New("injected persistent write failure")
			l.write = func(string, []byte) error { return writeErr }
			if _, err := l.Load(context.Background(), path, up.URL); !errors.Is(err, writeErr) || l.state != before {
				t.Fatalf("failed write advanced source state: %v", err)
			}
			if identical && parses.Load() != 1 {
				t.Fatal("identical bytes unnecessarily parsed")
			}
			if !identical {
				cached, err := parseAnibridgeFileContext(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
				if id, _ := cached.LookupByMAL(1); id != 100 {
					t.Fatal("failed write replaced persisted cache")
				}
			}
			l.write = writeFileAtomic
			second := mustLoad(t, l, path, up.URL)
			if identical && (second.Mapping != first.Mapping || second.Disposition != Reused) {
				t.Fatal("repair retry failed to reuse")
			}
			if !identical {
				if id, _ := second.Mapping.LookupByMAL(1); id != 200 || second.Disposition != Loaded || parses.Load() != 3 {
					t.Fatal("retry used candidate state from failed write")
				}
			}
		})
	}
}

func TestLoaderInvalidDownloadPreservesActiveAndCachedMapping(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, `"v1"`, zstdBytes(fixtureMAL1))
	path := filepath.Join(t.TempDir(), "mapping.zst")
	l, parses := countingLoader()
	first := mustLoad(t, l, path, up.URL)
	up.set(`"v2"`, zstdBytes(`{"mal:2":`))
	second := mustLoad(t, l, path, up.URL)
	if second.Mapping != first.Mapping || second.Disposition != Fallback || parses.Load() != 2 {
		t.Fatal("invalid download did not fall back without reparsing known cache")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(onDisk, zstdBytes(fixtureMAL1)) {
		t.Fatalf("invalid download overwrote usable cache: %v", err)
	}
}

func TestLoaderUnusableLocalCacheCannotReuseMemory(t *testing.T) {
	for _, damage := range []string{"corrupt", "unreadable"} {
		t.Run(damage, func(t *testing.T) {
			t.Parallel()
			up := newUpstream(t, `"v1"`, zstdBytes(fixtureMAL1))
			path := filepath.Join(t.TempDir(), "mapping.zst")
			l, _ := countingLoader()
			first := mustLoad(t, l, path, up.URL)
			up.Close()
			if damage == "corrupt" {
				if err := os.WriteFile(path, []byte("broken zstd"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
				if f, err := os.Open(path); err == nil {
					_ = f.Close()
					t.Skip("runtime can read files despite mode 000")
				}
			}
			if result, err := l.Load(context.Background(), path, up.URL); err == nil || result.Mapping != nil || l.state.mapping != first.Mapping {
				t.Fatal("remembered mapping concealed unusable local cache and failed upstream")
			}
		})
	}
}

func TestLoaderReuseStillVerifiesDownloadChecksum(t *testing.T) {
	t.Parallel()
	payload := zstdBytes(fixtureMAL1)
	var badChecksum atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if badChecksum.Load() {
			w.Header().Set(md5Header, md5B64([]byte("wrong checksum")))
		} else {
			w.Header().Set(md5Header, md5B64(payload))
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write(payload)
		}
	}))
	t.Cleanup(up.Close)
	path := filepath.Join(t.TempDir(), "mapping.zst")
	l, parses := countingLoader()
	first := mustLoad(t, l, path, up.URL)
	badChecksum.Store(true)
	second := mustLoad(t, l, path, up.URL)
	if second.Mapping != first.Mapping || second.Disposition != Fallback || parses.Load() != 1 {
		t.Fatal("identical download with bad checksum was treated as verified reuse")
	}
}

func TestLoaderSidecarWriteFailureDoesNotPreventLoad(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, `"v1"`, zstdBytes(fixtureMAL1))
	path := filepath.Join(t.TempDir(), "mapping.zst")
	if err := os.Mkdir(metaPath(path), 0o700); err != nil {
		t.Fatal(err)
	}
	l, parses := countingLoader()
	first := mustLoad(t, l, path, up.URL)
	second := mustLoad(t, l, path, up.URL)
	if first.Disposition != Loaded || second.Disposition != Reused || first.Mapping != second.Mapping || parses.Load() != 1 {
		t.Fatal("best-effort sidecar write prevented load or reusable state")
	}
}

func TestLoaderLocalInspectionUsesOpenedSnapshot(t *testing.T) {
	for _, atomicReplacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "in-place mutation rejected", true: "atomic replacement uses opened file"}[atomicReplacement], func(t *testing.T) {
			t.Parallel()
			up := newStatusServer(t, http.StatusServiceUnavailable)
			path := filepath.Join(t.TempDir(), "mapping.zst")
			seedCache(t, path, zstdBytes(fixtureMAL1), nil)
			l := NewLoader()
			entered, resume := make(chan struct{}), make(chan struct{})
			l.parse = func(ctx context.Context, r io.Reader, src string) (*AnibridgeMapping, error) {
				close(entered)
				<-resume
				return parseAnibridge(ctx, r, src)
			}
			finished := make(chan error, 1)
			go func() {
				_, err := l.Load(context.Background(), path, up.URL)
				finished <- err
			}()
			<-entered
			changed := zstdBytes(strings.ReplaceAll(fixtureMAL1, "100", "200"))
			if atomicReplacement {
				if err := writeFileAtomic(path, changed); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, changed, 0o600); err != nil {
				t.Fatal(err)
			}
			close(resume)
			err := <-finished
			if atomicReplacement {
				if err != nil {
					t.Fatal(err)
				}
				if id, _ := l.state.mapping.LookupByMAL(1); id != 100 {
					t.Fatal("replacement changed the opened snapshot")
				}
			} else if err == nil || l.state != nil {
				t.Fatal("in-place mutation published inconsistent source")
			}
			l.parse = parseAnibridge
			if id, _ := mustLoad(t, l, path, up.URL).Mapping.LookupByMAL(1); id != 200 {
				t.Fatal("next refresh did not observe changed file")
			}
		})
	}
}

func TestLoaderCanceledAdmissionAndParseReleaseGate(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, `"v1"`, zstdBytes(fixtureMAL1))
	path := filepath.Join(t.TempDir(), "mapping.zst")
	l := NewLoader()
	entered := make(chan struct{})
	l.parse = func(ctx context.Context, _ io.Reader, _ string) (*AnibridgeMapping, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := l.Load(ctx, path, up.URL)
		finished <- err
	}()
	<-entered
	waitCtx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if _, err := l.Load(waitCtx, path, up.URL); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter: %v", err)
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) || l.state != nil {
			t.Fatalf("canceled parse published state: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled loader did not release gate")
	}
	l.parse = parseAnibridge
	if result := mustLoad(t, l, path, up.URL); result.Disposition != Loaded {
		t.Fatal("subsequent load did not succeed")
	}
}

type interruptedReader struct {
	cancel context.CancelFunc
	err    error
}

func (r interruptedReader) Read(p []byte) (int, error) {
	if r.cancel != nil {
		r.cancel()
	}
	p[0] = 1
	return 1, r.err
}

func TestMappingFingerprintPropagatesCancellationAndReadFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := hashMapping(ctx, interruptedReader{cancel: cancel}); !errors.Is(err, context.Canceled) {
		t.Fatalf("fingerprint ignored cancellation: %v", err)
	}
	readErr := errors.New("injected read failure")
	if _, err := hashMapping(context.Background(), interruptedReader{err: readErr}); !errors.Is(err, readErr) {
		t.Fatalf("fingerprint ignored read error: %v", err)
	}
}
