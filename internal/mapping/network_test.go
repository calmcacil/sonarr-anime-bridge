package mapping

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoaderETagComparison(t *testing.T) {
	for _, tt := range []struct {
		name, cached, upstream string
		wantGET                int64
		wantID                 int
	}{
		{"same tag", `"version-a"`, `"version-a"`, 0, 100},
		{"outer whitespace", " \t\"version-a\"\t ", `"version-a"`, 0, 100},
		{"case differs", `"version-a"`, `"version-A"`, 1, 200},
		{"weak tag case differs", `W/"version-a"`, `W/"version-A"`, 1, 200},
		{"weak and strong differ", `W/"version-a"`, `"version-a"`, 1, 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			up := newUpstream(t, tt.upstream, zstdBytes(strings.ReplaceAll(fixtureMAL1, "100", "200")))
			path := filepath.Join(t.TempDir(), "mapping.zst")
			seedCache(t, path, zstdBytes(fixtureMAL1), &Metadata{URL: up.URL, ETag: tt.cached})
			result, err := NewLoader().Load(context.Background(), path, up.URL)
			if err != nil {
				t.Fatal(err)
			}
			if id, ok := result.Mapping.LookupByMAL(1); !ok || id != tt.wantID {
				t.Errorf("MAL 1 = %d, %v; want %d, true", id, ok, tt.wantID)
			}
			if heads, gets := up.heads.Load(), up.gets.Load(); heads != 1 || gets != tt.wantGET {
				t.Errorf("HEAD=%d GET=%d; want 1 and %d", heads, gets, tt.wantGET)
			}
		})
	}
}

func TestLoaderRejectsUnsafeURLDespiteCache(t *testing.T) {
	for _, upstream := range []string{"http://github.com/mapping.zst", "https://example.com/mapping.zst"} {
		t.Run(upstream, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "mapping.zst")
			seedCache(t, path, zstdBytes(fixtureMAL1), &Metadata{URL: upstream})
			l := NewLoader()
			if result, err := l.Load(context.Background(), path, upstream); err == nil || result.Mapping != nil || l.state != nil {
				t.Fatalf("unsafe URL used cached mapping: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestLoaderDNSFailureUsesCache(t *testing.T) {
	for _, etag := range []string{"", `"v1"`} {
		t.Run("etag="+etag, func(t *testing.T) {
			t.Parallel()
			const upstream = "https://github.com/anibridge/mapping.zst"
			path := filepath.Join(t.TempDir(), "mapping.zst")
			seedCache(t, path, zstdBytes(fixtureMAL1), &Metadata{URL: upstream, ETag: etag})
			l := NewLoader()
			lookups := 0
			l.client = mappingTestClient(func(context.Context, string, string) ([]net.IP, error) {
				lookups++
				return nil, errors.New("injected DNS outage")
			}, nil)
			result := mustLoad(t, l, path, upstream)
			if id, ok := result.Mapping.LookupByMAL(1); !ok || id != 100 || result.Disposition != Fallback {
				t.Fatalf("DNS outage did not use cached mapping: result=%+v id=%d", result, id)
			}
			wantLookups := 1
			if etag != "" {
				wantLookups = 2 // HEAD failure still attempts GET before falling back.
			}
			if lookups != wantLookups {
				t.Fatalf("lookups=%d, want %d", lookups, wantLookups)
			}
		})
	}
}

func TestLoaderDNSCancellation(t *testing.T) {
	t.Parallel()
	const upstream = "https://github.com/anibridge/mapping.zst"
	path := filepath.Join(t.TempDir(), "mapping.zst")
	seedCache(t, path, zstdBytes(fixtureMAL1), &Metadata{URL: upstream, ETag: `"v1"`})
	l := NewLoader()
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	l.client = mappingTestClient(func(ctx context.Context, _, _ string) ([]net.IP, error) {
		close(entered)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, errors.New("released stalled DNS lookup")
		}
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := l.Load(ctx, path, upstream)
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("DNS lookup did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) || l.state != nil {
			t.Fatalf("canceled lookup returned %v or published state", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DNS lookup ignored caller cancellation")
	}
	// Cancellation must release the loader gate without selecting the cache.
	l.client = mappingTestClient(func(context.Context, string, string) ([]net.IP, error) {
		return nil, errors.New("injected DNS outage")
	}, nil)
	if result := mustLoad(t, l, path, upstream); result.Disposition != Fallback {
		t.Fatalf("subsequent load = %s, want fallback", result.Disposition)
	}
}

func TestMappingDNSDeadline(t *testing.T) {
	for _, callerDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "client timeout", true: "caller deadline"}[callerDeadline], func(t *testing.T) {
			t.Parallel()
			client := mappingTestClient(func(ctx context.Context, _, _ string) ([]net.IP, error) {
				if _, ok := ctx.Deadline(); !ok {
					return nil, errors.New("DNS lookup has no deadline")
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}, nil)
			ctx := context.Background()
			if callerDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			} else {
				client.Timeout = 20 * time.Millisecond
			}
			_, err := head(ctx, client, "https://github.com/mapping.zst")
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("DNS deadline error = %v, want context.DeadlineExceeded", err)
			}
		})
	}
}

func TestMappingNetworkPolicy(t *testing.T) {
	for _, tt := range []struct {
		name, url string
		ips       []net.IP
		wantErr   string
	}{
		{"public", "https://github.com/mapping.zst", []net.IP{net.ParseIP("140.82.112.3")}, ""},
		{"private", "https://github.com/mapping.zst", []net.IP{net.ParseIP("10.0.0.1")}, "non-public IP"},
		{"mixed answers", "https://github.com/mapping.zst", []net.IP{net.ParseIP("140.82.112.3"), net.ParseIP("192.168.1.1")}, "non-public IP"},
		{"loopback", "https://github.com/mapping.zst", []net.IP{net.ParseIP("127.0.0.1")}, "loopback IP"},
		{"link local", "https://github.com/mapping.zst", []net.IP{net.ParseIP("169.254.169.254")}, "non-public IP"},
		{"IPv6 private", "https://github.com/mapping.zst", []net.IP{net.ParseIP("fd00::1")}, "non-public IP"},
		{"localhost opt in", "http://localhost/mapping.zst", []net.IP{net.ParseIP("::1")}, ""},
		{"localhost private answer", "http://localhost/mapping.zst", []net.IP{net.ParseIP("10.0.0.1")}, "non-public IP"},
		{"scheme", "http://github.com/mapping.zst", nil, "must use https"},
		{"host", "https://example.com/mapping.zst", nil, "not allowlisted"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			requests := 0
			client := mappingTestClient(func(context.Context, string, string) ([]net.IP, error) {
				return tt.ips, nil
			}, mappingRoundTripper(func(req *http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			}))
			_, err := head(context.Background(), client, tt.url)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || requests != 0 {
					t.Fatalf("unsafe request reached transport or wrong error: requests=%d err=%v", requests, err)
				}
			} else if err != nil || requests != 1 {
				t.Fatalf("safe request failed: requests=%d err=%v", requests, err)
			}
		})
	}
}

func TestMappingRedirectNetworkPolicy(t *testing.T) {
	t.Parallel()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://github.com/mapping.zst", http.StatusFound)
	}))
	t.Cleanup(up.Close)
	lookups := 0
	client := mappingTestClient(func(context.Context, string, string) ([]net.IP, error) {
		lookups++
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}, http.DefaultTransport)
	_, err := head(context.Background(), client, up.URL)
	if err == nil || !strings.Contains(err.Error(), "loopback IP") || lookups != 1 {
		t.Fatalf("redirect skipped network policy: lookups=%d err=%v", lookups, err)
	}
}

func mappingTestClient(lookup func(context.Context, string, string) ([]net.IP, error), base http.RoundTripper) *http.Client {
	client := secureHTTPClient()
	if base == nil {
		base = mappingRoundTripper(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("transport called after DNS failure")
		})
	}
	client.Transport = &mappingTransport{base: base, lookupIP: lookup}
	return client
}

type mappingRoundTripper func(*http.Request) (*http.Response, error)

func (f mappingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
