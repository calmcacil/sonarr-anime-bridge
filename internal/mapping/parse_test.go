package mapping

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/calmcacil/sonarr-anime-bridge/internal/testutil"
)

var parserParityFixtures = []string{
	`{}`,
	`null`, `[]`, `true`, `1`, `"root"`, ``, `{`, `{"mal:1":`,
	`1e700`, `{"mal:1":{""`, `{"anilist:1":{""`,
	`{"mal:1":null,"anilist:1":{"tvdb_show:2:s1":null}}`,
	`{"mal:1":[],"mal:1":{}}`,
	`{"mal:1":"invalid"}`, `{"mal:1":123}`, `{"mal:1":false}`,
	`{"mal:1":{"tvdb_show:100:s0":{"1-100":"ignored"},"tvdb_show:200:s1":{"1":null}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1-12":"wrong-destination"},"tvdb_show:200:s1":{"1-":"1","2":"2"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{},"tvdb_show:99:s1":null}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":null,"2-4":null,"5-":null}}}`,
	`{"mal:1":{"tvdb_show:100:":null}}`,
	`{"mal:1":{"tvdb_show:100:s1:extra":{"1":"1"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"":"1"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"0":"1"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"-1":"1"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"2-1":"1"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1-2-3":"1"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"9223372036854775808":"1"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1-9223372036854775807":"1","2-3":"1"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"+1":"1","01":"1","1-":"1"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":"first","1":"last"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":false,"1":"last"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":"first","1":false}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":null,"1":"last"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":"first","1":null}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":{}},"tvdb_show:100:s1":{"1":"valid"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":"valid"},"tvdb_show:100:s1":{"1":{}}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"invalid":"1"},"tvdb_show:100:s1":null}}`,
	`{"mal:1":{"tvdb_show:100:s1":false,"tvdb_show:100:s1":{}}}`,
	`{"mal:1":{"tvdb_show:100:s1":[],"tvdb_show:100:s1":{}}}`,
	`{"mal:1":{"tvdb_show:100:s1":[],"tvdb_show:100:s1":[]}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":"1"},"tvdb_show:200:s0":{"0":"1"}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":1e999}}}`,
	`{"mal:1":{"tvdb_show:100:s1":123}}`,
	`{"mal:1":{"tvdb_show:100:s1":"invalid"}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":["invalid"]}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":true}}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":"1"}},"mal:1":{}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":"1"}},"mal:1":null}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":"1"}},"mal:1":{"tvdb_show:200:s1":null}}`,
	`{"mal:1":{"tvdb_show:100:s1":{"1":"1"}},"mal:1":false}`,
	`{"mal:01":{"tvdb_show:100:s1":null},"mal:+1":{"tvdb_show:200:s1":null},"anilist:1":{"tvdb_show:300:s1":null}}`,
	`{"mal:0":false,"mal:-1":123,"mal:invalid":{},"anilist:":[1e999],"anidb:1":[null,true,false,{},[]]}`,
	`{"mal:1":{"tvdb_show:0:s1":false,"tvdb_show:invalid:s1":123,"tvdb_show:1":[],"imdb:any":{"invalid":{}}}}`,
	`{"mal\u003a1":{"tvdb_show:100:s\u0031":{"\u0031":"quote\" slash\\ newline\n"}}}`,
	`{"ignored":{"nest":[{"array":[true,false,null,"text",1e999]}]},"mal:1":{"tvdb_show:1:s1":null}}`,
	`{"ignored":[{"bad":]}}`,
	`{"ignored":"` + strings.Repeat("long\\n", 2000) + `","mal:1":{"tvdb_show:1:s1":null}}`,
	`{"$meta":null,"mal:1":{"tvdb_show:1:s1":null}}`,
	`{"$meta":false,"mal:1":{"tvdb_show:1:s1":null}}`,
	`{"$meta":[{},[]],"mal:1":{"tvdb_show:1:s1":null}}`,
	`{"$meta":{"schema_version":{},"unknown":[1e999],"generated_on":123},"mal:1":{"tvdb_show:1:s1":null}}`,
	`{"$meta":{"SCHEMA_VERSION":"first","schema_version":null,"generated_on":"time"},"mal:1":{"tvdb_show:1:s1":null}}`,
	`{"$meta":{"schema_version":false,"schema_version":"last"},"mal:1":{"tvdb_show:1:s1":null}}`,
	`{"$meta":{"schema_version":"unterminated}}`,
	`{"$meta":{"unknown":]}}`,
	`{"$meta":`,
	`{"$meta":{"unknown":null}} trailing invalid JSON`,
	`{"mal:1":{"tvdb_show:1:s1":null}} {"mal:2":false}`,
}

func checkParserParity(t testing.TB, input string) {
	t.Helper()
	display := input
	if len(display) > 1024 {
		display = display[:1024] + "...(truncated)"
	}
	reference, refErr := referenceParseAnibridgeJSON(context.Background(), strings.NewReader(input), "reference")
	actual, err := parseAnibridgeJSON(context.Background(), strings.NewReader(input), "streaming")
	if (refErr == nil) != (err == nil) {
		t.Fatalf("acceptance mismatch: reference=%v streaming=%v input=%q", refErr, err, display)
	}
	if err != nil {
		if actual != nil || reference != nil {
			t.Fatal("failed parse returned partial mappings")
		}
		if !sameParserErrorContext(refErr, err) {
			t.Fatalf("error context mismatch: reference=%v streaming=%v input=%q", refErr, err, display)
		}
		return
	}
	if !maps.Equal(actual.byMAL, reference.byMAL) || !maps.Equal(actual.byAniList, reference.byAniList) {
		t.Fatalf("mapping mismatch: reference=%+v streaming=%+v input=%q", reference, actual, display)
	}
	mal, ani := actual.Stats()
	refMAL, refAni := reference.Stats()
	keysMAL, keysAni := actual.Keys()
	refKeysMAL, refKeysAni := reference.Keys()
	if mal != refMAL || ani != refAni || !reflect.DeepEqual(keysMAL, refKeysMAL) || !reflect.DeepEqual(keysAni, refKeysAni) {
		t.Fatal("statistics or sorted keys mismatch")
	}
}

// Map iteration may choose different failing descriptors when several survive.
// Check the source and stage; incidental stdlib text and that order are unstable.
func sameParserErrorContext(a, b error) bool {
	category := func(err error) string {
		for _, marker := range []string{"count episodes for", "decode targets", "skip value", "expected opening brace", "expected '{'"} {
			if strings.Contains(err.Error(), marker) {
				return marker
			}
		}
		return "root traversal"
	}
	if category(a) != category(b) {
		return false
	}
	for _, namespace := range []string{"mal:", "anilist:"} {
		i := strings.Index(a.Error(), "JSON: "+namespace)
		if i >= 0 {
			source, _, _ := strings.Cut(a.Error()[i+6:], ": ")
			if !strings.Contains(b.Error(), "JSON: "+source+": ") {
				return false
			}
		}
	}
	return true
}

func TestMappingParserParity(t *testing.T) {
	t.Parallel()
	for i, fixture := range parserParityFixtures {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			checkParserParity(t, fixture)
		})
	}
	for _, file := range []string{"../../testdata/pipeline-mappings.json"} {
		t.Run(file, func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			checkParserParity(t, string(data))
		})
	}
}

func TestMappingParserCandidateOrder(t *testing.T) {
	t.Parallel()
	candidates := []string{
		`"tvdb_show:3:s1":{"1-12":"1"}`,
		`"tvdb_show:2:s1":{"1-12":"1"}`,
		`"tvdb_show:1:s0":{"1-100":"1"}`,
		`"tvdb_show:4:s1":{"1-":"1"}`,
	}
	var permute func([]string, []string)
	permute = func(prefix, remaining []string) {
		if len(remaining) == 0 {
			input := `{"mal:1":{` + strings.Join(prefix, ",") + `}}`
			checkParserParity(t, input)
			m, err := parseAnibridgeJSON(context.Background(), strings.NewReader(input), "test")
			if err != nil || m.byMAL[1] != 2 {
				t.Fatalf("selection depends on order: %v %v", m, err)
			}
			return
		}
		for i, candidate := range remaining {
			next := append([]string(nil), remaining[:i]...)
			next = append(next, remaining[i+1:]...)
			permute(append(prefix, candidate), next)
		}
	}
	permute(nil, candidates)
}

func TestMappingParserLargeEntry(t *testing.T) {
	t.Parallel()
	var entry strings.Builder
	entry.WriteString(`{"mal:1":{`)
	for i := range 200 {
		fmt.Fprintf(&entry, `"ignored:%d":{"nested":[%q,1e999,null,{"escaped":"\u0031"}]},`, i, strings.Repeat("unused value ", 100))
	}
	entry.WriteString(`"tvdb_show:1:s1":{`)
	for i := range 1000 {
		fmt.Fprintf(&entry, `"%d":"unused",`, i+1)
	}
	entry.WriteString(`"1001":"unused"},"tvdb_show:2:s0":{"1-9999":"1"}}}`)
	checkParserParity(t, entry.String())
	checkParserParity(t, `{"ignored":`+strings.Repeat(`[`, 2000)+`null`+strings.Repeat(`]`, 2000)+`}`)
}

func TestMappingParserRepresentativeParity(t *testing.T) {
	t.Parallel()
	checkParserParity(t, parserBenchmarkDocument(100))
}

func TestMappingParserDepthParity(t *testing.T) {
	t.Parallel()
	for _, depth := range []int{9998, 9999, 10000, 10001} {
		value := strings.Repeat(`[`, depth) + `null` + strings.Repeat(`]`, depth)
		for _, input := range []string{
			`{"ignored":` + value + `}`,
			`{"$meta":` + value + `}`,
			`{"$meta":{"ignored":` + value + `}}`,
			`{"mal:1":{"ignored":` + value + `}}`,
			`{"mal:1":{"tvdb_show:1:s1":` + value + `,"tvdb_show:1:s1":null}}`,
			`{"mal:1":{"tvdb_show:1:s1":{"1":` + value + `},"tvdb_show:1:s1":null}}`,
		} {
			checkParserParity(t, input)
		}
	}
}

func TestMappingParserMetadataLogs(t *testing.T) {
	for _, tt := range []struct {
		input, schema, generated string
		warning                  bool
	}{
		{input: `{"$meta":{"SCHEMA_VERSION":"old","schema_version":"3","schema_version":null,"GENERATED_ON":"today","unknown":[1e999]}}`, schema: "3", generated: "today"},
		{input: `{"$meta":{"schema_version":false,"schema_version":"3"},"mal:1":{"tvdb_show:1:s1":null}}`, warning: true},
		{input: `{"$meta":[1e999],"mal:1":{"tvdb_show:1:s1":null}}`, warning: true},
	} {
		t.Run(tt.input, func(t *testing.T) {
			logs := testutil.CaptureLogs(t, slog.LevelDebug)
			m, err := parseAnibridgeJSON(context.Background(), strings.NewReader(tt.input), "metadata")
			if err != nil || m == nil {
				t.Fatalf("informational metadata rejected entries: %v", err)
			}
			found := false
			for _, rec := range logs.Records() {
				if rec.Msg == "anibridge dataset metadata" || rec.Msg == "anibridge dataset metadata unreadable; continuing with mapping entries" {
					found = true
					if (rec.Level == slog.LevelWarn) != tt.warning || (!tt.warning && (rec.Attrs["schema_version"].String() != tt.schema || rec.Attrs["generated_on"].String() != tt.generated)) {
						t.Fatalf("incorrect metadata log: %+v", rec)
					}
				}
			}
			if !found {
				t.Fatal("missing metadata diagnostic")
			}
		})
	}
}

func FuzzMappingParserParity(f *testing.F) {
	for _, fixture := range parserParityFixtures {
		f.Add(fixture)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 64<<10 {
			t.Skip()
		}
		checkParserParity(t, input)
	})
}

type failingMappingReader struct{ err error }

func (r failingMappingReader) Read([]byte) (int, error) { return 0, r.err }

func TestMappingParserReaderErrors(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("reader failed")
	for _, prefix := range []string{``, `{"mal:1":{"tvdb_show:1:s1":null},"ignored":[`, `{"mal:1":{"tvdb_show:1:s1":{"1":`, `{"$meta":{"unknown":[`} {
		r := io.MultiReader(strings.NewReader(prefix), failingMappingReader{sentinel})
		m, err := parseAnibridgeJSON(context.Background(), r, "reader")
		if !errors.Is(err, sentinel) || m != nil {
			t.Fatalf("reader error was lost or returned partial mapping: %v %v", m, err)
		}
	}
}

type cancelAfterChecks struct {
	context.Context
	remaining int
}

func (c *cancelAfterChecks) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestMappingParserCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if m, err := parseAnibridgeJSON(ctx, strings.NewReader(`{}`), "already-canceled"); !errors.Is(err, context.Canceled) || m != nil {
		t.Fatalf("already-canceled parse returned %v %v", m, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	if m, err := parseAnibridgeJSON(ctx, interruptedReader{cancel: cancel}, "read-canceled"); !errors.Is(err, context.Canceled) || m != nil {
		t.Fatalf("cancellation during read returned %v %v", m, err)
	}
	for _, input := range []string{
		`{"mal:1":{"tvdb_show:1:s1":null},"ignored":[` + strings.Repeat(`[{"nested":[null]}],`, 1000) + `null]}`,
		`{"mal:1":{"tvdb_show:1:s1":{` + strings.Repeat(`"1":"1",`, 1000) + `"2":"2"}}}`,
		`{"$meta":{"ignored":[` + strings.Repeat(`null,`, 1000) + `null]},"mal:1":{}}`,
	} {
		ctx := &cancelAfterChecks{Context: context.Background(), remaining: 100}
		m, err := parseAnibridgeJSON(ctx, strings.NewReader(input), "canceled")
		if !errors.Is(err, context.Canceled) || m != nil {
			t.Fatalf("cancellation during traversal returned %v %v", m, err)
		}
	}
}

func TestMappingParserFailurePreservesLoaderCache(t *testing.T) {
	t.Parallel()
	payload := zstdBytes(fixtureMAL1)
	up := newUpstream(t, `"v1"`, payload)
	path := filepath.Join(t.TempDir(), "mapping.zst")
	loader, parses := countingLoader()
	first := mustLoad(t, loader, path, up.URL)
	// The second candidate cannot win, but its surviving semantic error must
	// reject the whole download before any cache replacement or publication.
	up.set(`"v2"`, zstdBytes(`{"mal:1":{"tvdb_show:200:s1":{"1":"1"},"tvdb_show:300:s0":{"invalid":"1"}}}`))
	second := mustLoad(t, loader, path, up.URL)
	if second.Disposition != Fallback || second.Mapping != first.Mapping || parses.Load() != 2 {
		t.Fatalf("failed parser changed active mapping: disposition=%s parses=%d", second.Disposition, parses.Load())
	}
	cached, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(cached, payload) {
		t.Fatalf("failed parser replaced valid cache: %v", err)
	}
	meta, err := ReadMetadata(metaPath(path))
	if err != nil || meta.ETag != `"v1"` {
		t.Fatalf("failed parser advanced cache metadata: %v %v", meta, err)
	}
}

func TestMappingParserCompressedParity(t *testing.T) {
	t.Parallel()
	input := `{"mal:1":{"tvdb_show:1:s1":{"1":"1"}},"ignored":[{"nested":null}]}`
	payload := zstdBytes(input)
	for _, data := range [][]byte{payload, payload[:len(payload)/2], payload[:len(payload)-1], []byte("invalid zstd")} {
		zr, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		ref, refErr := referenceParseAnibridgeJSON(context.Background(), zr, "reference")
		zr.Close()
		m, err := parseAnibridge(context.Background(), bytes.NewReader(data), "streaming")
		if (err == nil) != (refErr == nil) || !reflect.DeepEqual(m, ref) || (err != nil && m != nil) {
			t.Fatalf("compressed mismatch: reference=%v %v streaming=%v %v", ref, refErr, m, err)
		}
	}
}

type repeatedMappingByte struct{ value byte }

func (r repeatedMappingByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.value
	}
	return len(p), nil
}

func TestMappingParserDecodedLimit(t *testing.T) {
	t.Parallel()
	// Whitespace inside an ignored array exercises the real 250 MiB bound without
	// retaining an enormous test fixture or decoded string.
	r := io.MultiReader(strings.NewReader(`{"ignored":[`), io.LimitReader(repeatedMappingByte{' '}, maxDecodedMappingBytes), strings.NewReader(`]}`))
	m, err := parseAnibridgeJSON(context.Background(), r, "limit")
	if err == nil || !strings.Contains(err.Error(), "decoded anibridge mapping exceeds") || m != nil {
		t.Fatalf("ignored data bypassed limit: %v %v", m, err)
	}
	input := `{"ignored":[null],"mal:1":{"tvdb_show:1:s1":null}}`
	for _, size := range []int{len(input) - 1, len(input), len(input) + 1} {
		r := &limitReader{Reader: strings.NewReader(input), limit: int64(size)}
		m, err := parseAnibridgeJSON(context.Background(), r, "boundary")
		if (err == nil) != (size >= len(input)) || (err != nil && m != nil) {
			t.Fatalf("size=%d mapping=%v error=%v", size, m, err)
		}
	}
}
