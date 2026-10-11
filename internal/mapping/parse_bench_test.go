package mapping

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func parserBenchmarkDocument(offset int) string {
	var doc strings.Builder
	doc.WriteString(`{"$meta":{"schema_version":"1"}`)
	for _, namespace := range []string{"mal", "anilist"} {
		for id := 1; id <= 9000; id++ {
			fmt.Fprintf(&doc, `,"%s:%d":{"tvdb_show:%d:s1":{"1-12":"1-12"},"tvdb_show:%d:s0":{"1":"1"},"imdb:ignored":{"note":"unused target"}}`, namespace, id, id+offset, id+20000)
		}
	}
	doc.WriteByte('}')
	return doc.String()
}

// Every iteration parses directly. Alternating documents also prevents source
// reuse from concealing parser work if this harness is used with the loader.
func BenchmarkMappingParse(b *testing.B) {
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(previous)
	docs := []string{parserBenchmarkDocument(100), parserBenchmarkDocument(101)}
	compressed := [][]byte{zstdBytes(docs[0]), zstdBytes(docs[1])}
	for _, mode := range []string{"JSON", "Zstd"} {
		for _, implementation := range []string{"Reference", "Streaming"} {
			b.Run(mode+"/"+implementation, func(b *testing.B) {
				parse := parseAnibridgeJSON
				if implementation == "Reference" {
					parse = referenceParseAnibridgeJSON
				}
				b.ReportAllocs()
				b.SetBytes(int64(len(docs[0])))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var r io.Reader = strings.NewReader(docs[i%2])
					if mode == "Zstd" {
						zr, err := zstd.NewReader(bytes.NewReader(compressed[i%2]))
						if err != nil {
							b.Fatal(err)
						}
						m, err := parse(context.Background(), zr, "benchmark")
						zr.Close()
						if err != nil || m == nil {
							b.Fatalf("parse: %v", err)
						}
					} else if _, err := parse(context.Background(), r, "benchmark"); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// Heap sampling runs separately from timing benchmarks. The opt-in report keeps
// runtime.ReadMemStats overhead out of normal tests and benchmark measurements.
func TestMappingParserHeapReport(t *testing.T) {
	if os.Getenv("MAPPING_HEAP_REPORT") != "1" {
		t.Skip("set MAPPING_HEAP_REPORT=1 to sample parser heap")
	}
	doc := parserBenchmarkDocument(100)
	payload := zstdBytes(doc)
	for _, mode := range []string{"JSON", "Zstd"} {
		for _, implementation := range []string{"Reference", "Streaming"} {
			parse := parseAnibridgeJSON
			if implementation == "Reference" {
				parse = referenceParseAnibridgeJSON
			}
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			stop, sampled := make(chan struct{}), make(chan uint64)
			go func() {
				var peak uint64
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					var stats runtime.MemStats
					runtime.ReadMemStats(&stats)
					peak = max(peak, stats.HeapAlloc)
					select {
					case <-stop:
						sampled <- peak
						return
					case <-ticker.C:
					}
				}
			}()
			var r io.Reader = strings.NewReader(doc)
			var zr *zstd.Decoder
			if mode == "Zstd" {
				var err error
				zr, err = zstd.NewReader(bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}
				r = zr
			}
			m, err := parse(context.Background(), r, "heap-report")
			if zr != nil {
				zr.Close()
			}
			close(stop)
			peak := <-sampled
			if err != nil {
				t.Fatal(err)
			}
			runtime.GC()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			runtime.KeepAlive(m)
			runtime.KeepAlive(doc)
			runtime.KeepAlive(payload)
			t.Logf("%s/%s sampled_peak_extra_bytes=%d retained_extra_bytes=%d", mode, implementation, int64(peak)-int64(before.HeapAlloc), int64(after.HeapAlloc)-int64(before.HeapAlloc))
		}
	}
}
