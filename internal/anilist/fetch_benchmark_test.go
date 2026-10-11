package anilist

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// Both protocols use identical fields and page boundaries. Fixture preparation,
// pacing, network latency, and copying response bodies are outside this benchmark.
func BenchmarkFetchYear(b *testing.B) {
	for _, n := range []int{0, 1, 482, 600, 5000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			raw, err := os.ReadFile("testdata/year-2025.json")
			if err != nil {
				b.Fatal(err)
			}
			var fixture []Show
			if err := json.Unmarshal(raw, &fixture); err != nil {
				b.Fatal(err)
			}
			shows := make([]Show, n)
			for i := range shows {
				shows[i] = fixture[i%len(fixture)]
			}
			last := max(1, (n+49)/50)
			pages := make([]string, ((last+3)/4)*4)
			for i := range pages {
				media, err := json.Marshal(shows[min(i*50, n):min((i+1)*50, n)])
				if err != nil {
					b.Fatal(err)
				}
				pages[i] = fmt.Sprintf(`{"pageInfo":{"currentPage":%d,"hasNextPage":%t},"media":%s}`, i+1, i+1 < last, media)
			}
			singles, batches := make(map[int]string), make(map[int]string)
			for i, p := range pages {
				singles[i+1] = `{"data":{"Page":` + p + `}}`
			}
			for i := 0; i < len(pages); i += 4 {
				batches[i+1] = fmt.Sprintf(`{"data":{"page0":%s,"page1":%s,"page2":%s,"page3":%s}}`, pages[i], pages[i+1], pages[i+2], pages[i+3])
			}
			attempts, responseBytes := 0, 0
			client := NewWithHTTPClient("https://example.invalid", &http.Client{Transport: benchmarkTransport(func(r *http.Request) (*http.Response, error) {
				var req struct{ Variables map[string]int }
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					return nil, err
				}
				body := singles[req.Variables["page"]]
				if page := req.Variables["p0"]; page != 0 {
					body = batches[page]
				}
				attempts++
				responseBytes += len(body)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			})})
			client.interval = 0
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				got, err := client.FetchYear(context.Background(), 2025)
				if err != nil || len(got) != n {
					b.Fatalf("fetch %d: %v", len(got), err)
				}
			}
			b.ReportMetric(float64(attempts)/float64(b.N), "requests/op")
			b.ReportMetric(float64(responseBytes)/float64(b.N), "response-B/op")
		})
	}
}

type benchmarkTransport func(*http.Request) (*http.Response, error)

func (f benchmarkTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
