package mapping

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type benchmarkUpstream struct {
	payload []byte
	etag    string
}

func (u *benchmarkUpstream) RoundTrip(req *http.Request) (*http.Response, error) {
	body := []byte(nil)
	if req.Method == http.MethodGet {
		body = u.payload
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Etag": []string{u.etag}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}, nil
}

func benchmarkMappingPayload(tvdbOffset int) []byte {
	var doc strings.Builder
	doc.WriteString(`{"$meta":{"schema_version":"1"}`)
	for _, namespace := range []string{"mal", "anilist"} {
		for id := 1; id <= 9000; id++ {
			fmt.Fprintf(&doc, `,"%s:%d":{"tvdb_show:%d:s1":{"1-12":"1-12"},"tvdb_show:%d:s0":{"1":"1"},"imdb:ignored":{"note":"unused target"}}`, namespace, id, id+tvdbOffset, id+20000)
		}
	}
	doc.WriteByte('}')
	return zstdBytes(doc.String())
}

// Fixed transport responses exclude network latency while retaining the HTTP
// decision tree. Fixture writes and startup priming are outside refresh timing.
func BenchmarkMappingRefresh(b *testing.B) {
	previousTransport, previousLogger := http.DefaultTransport, slog.Default()
	defer func() {
		http.DefaultTransport = previousTransport
		slog.SetDefault(previousLogger)
	}()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	payloads := [][]byte{benchmarkMappingPayload(100), benchmarkMappingPayload(101)}
	keys := make([]int, 9000)
	for i := range keys {
		keys[i] = i + 1
	}
	for _, scenario := range []string{"UnchangedLocal", "ChangedLocal", "IdenticalGET", "InitialLoad"} {
		b.Run(scenario, func(b *testing.B) {
			up := &benchmarkUpstream{payload: payloads[0], etag: `"v1"`}
			http.DefaultTransport = up
			path := filepath.Join(b.TempDir(), "mapping.zst")
			const upstreamURL = "http://127.0.0.1/mapping.zst"
			loader := NewLoader()
			load := func() error {
				_, err := loader.Load(context.Background(), path, upstreamURL)
				return err
			}
			if err := os.WriteFile(path, payloads[0], 0o600); err != nil {
				b.Fatal(err)
			}
			if err := WriteMetadata(metaPath(path), Metadata{ETag: up.etag, URL: upstreamURL, MALKeys: keys, AniListKeys: keys}); err != nil {
				b.Fatal(err)
			}
			if err := load(); err != nil {
				b.Fatal(err)
			}
			if scenario == "IdenticalGET" {
				up.etag = ""
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if scenario == "InitialLoad" {
					loader = NewLoader()
				}
				if scenario == "ChangedLocal" {
					b.StopTimer()
					if err := os.WriteFile(path, payloads[(i+1)%2], 0o600); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
				}
				if err := load(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkMappingFingerprint(b *testing.B) {
	payload := benchmarkMappingPayload(100)
	path := filepath.Join(b.TempDir(), "mapping.zst")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := hashMappingFile(context.Background(), path); err != nil {
			b.Fatal(err)
		}
	}
}
