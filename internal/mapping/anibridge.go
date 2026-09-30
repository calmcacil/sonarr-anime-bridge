package mapping

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
	"github.com/calmcacil/sonarr-anime-bridge/internal/datapath"
	"github.com/calmcacil/sonarr-anime-bridge/internal/mappingurl"
)

const (
	defaultAnibridgeHTTPTimeout = 60 * time.Second
	maxCompressedMappingBytes   = 50 << 20
	maxDecodedMappingBytes      = 250 << 20
)

// Metadata is persisted next to the cached mapping file so that subsequent
// loads can ask the upstream "is the file still what I have?" with a
// conditional HEAD request instead of re-downloading.
type Metadata struct {
	ETag         string    `json:"etag"`
	LastModified string    `json:"last_modified,omitempty"`
	MD5          string    `json:"md5,omitempty"`
	FetchedAt    time.Time `json:"fetched_at"`
	URL          string    `json:"url"`

	// MALKeys and AniListKeys snapshot the keys present in the last
	// successful download. They are used to compute the new/removed
	// counts logged on each refresh. The lists are compact — an
	// anibridge dataset of ~18k entries serializes to ~200 KB.
	MALKeys     []int `json:"mal_keys,omitempty"`
	AniListKeys []int `json:"anilist_keys,omitempty"`
}

type anibridgeMeta struct {
	SchemaVersion string `json:"schema_version"`
	GeneratedOn   string `json:"generated_on"`
}

type AnibridgeMapping struct {
	byMAL     map[int]int
	byAniList map[int]int
}

func NewAnibridgeMapping(byMAL, byAniList map[int]int) *AnibridgeMapping {
	return &AnibridgeMapping{byMAL: byMAL, byAniList: byAniList}
}

func (m *AnibridgeMapping) LookupByMAL(malID int) (int, bool) {
	tvdbID, ok := m.byMAL[malID]
	return tvdbID, ok
}

func (m *AnibridgeMapping) LookupByAniList(anilistID int) (int, bool) {
	tvdbID, ok := m.byAniList[anilistID]
	return tvdbID, ok
}

func (m *AnibridgeMapping) Stats() (malEntries, aniListEntries int) {
	return len(m.byMAL), len(m.byAniList)
}

// Keys returns sorted snapshots of the MAL and AniList key sets. The
// returned slices are fresh copies; callers may retain or persist them.
func (m *AnibridgeMapping) Keys() (malKeys, aniListKeys []int) {
	return slices.Sorted(maps.Keys(m.byMAL)), slices.Sorted(maps.Keys(m.byAniList))
}

// LoadOrFetch loads the mapping from path, downloading from upstream only when
// the local cache is missing or stale. If a download fails, falls back to the
// cached file when possible.
func LoadOrFetch(ctx context.Context, path, url string) (*AnibridgeMapping, Metadata, error) {
	start := time.Now()
	if path == "" {
		path = config.DefaultAnibridgeMappingPath
	}
	if url == "" {
		url = config.DefaultAnibridgeURL
	}
	var err error
	path, err = datapath.Validate(path)
	if err != nil {
		return nil, Metadata{}, err
	}
	if err := validateRemoteURL(url); err != nil {
		return nil, Metadata{}, err
	}

	metadataPath := metaPath(path)
	meta, metaErr := ReadMetadata(metadataPath)
	if metaErr != nil {
		slog.Warn("anibridge metadata unavailable; continuing with cache inspection", "type", "resolver",
			"task", "mapping_load", "outcome", "degraded", "error", metaErr, "path", metadataPath,
			"action", "inspect_cache", "consequence", "upstream_freshness_check_unavailable")
	}
	haveCache := false
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		haveCache = true
	}

	urlChanged := haveCache && meta.URL != "" && meta.URL != url
	if urlChanged {
		slog.Debug("anibridge cache bypassed", "type", "resolver",
			"task", "mapping_load", "outcome", "skipped", "reason", "url_changed", "action", "download")
		meta = Metadata{}
	}
	canUseCache := haveCache && !urlChanged

	if canUseCache && meta.ETag != "" {
		slog.Debug("checking anibridge upstream for updates", "type", "resolver",
			"task", "mapping_load", "path", path)
		upstream, fetchErr := Head(ctx, url)
		switch {
		case fetchErr != nil:
			if ctx.Err() == nil {
				slog.Warn("anibridge HEAD failed; continuing with download", "type", "resolver",
					"task", "mapping_download", "outcome", "degraded", "error", mappingLogError(fetchErr),
					"action", "download", "consequence", "freshness_check_unavailable")
			}
		case strings.EqualFold(strings.TrimSpace(upstream.ETag), strings.TrimSpace(meta.ETag)):
			m, parseErr := parseAnibridgeFileContext(ctx, path)
			if parseErr == nil {
				slog.Debug("anibridge download skipped", "type", "resolver",
					"task", "mapping_download", "outcome", "skipped", "reason", "etag_match",
					"source", "cache", "duration_ms", time.Since(start).Milliseconds())
				return m, meta, nil
			}
			if ctx.Err() == nil {
				slog.Warn("cached anibridge mapping unreadable; continuing with download", "type", "resolver",
					"task", "mapping_load", "outcome", "degraded", "error", parseErr,
					"action", "download", "consequence", "cached_mapping_unavailable")
			}
		default:
			slog.Debug("anibridge mapping needs refresh", "type", "resolver",
				"task", "mapping_download", "reason", "etag_changed", "action", "download")
		}
	}

	data, newMeta, err := Fetch(ctx, url)
	if err != nil {
		if canUseCache {
			m, parseErr := parseAnibridgeFileContext(ctx, path)
			if parseErr != nil {
				return nil, meta, fmt.Errorf("fetch failed and cached mapping is unreadable: %w", parseErr)
			}
			malN, aniN := m.Stats()
			slog.Warn("anibridge download failed; serving cached mapping", "type", "resolver",
				"task", "mapping_load", "outcome", "degraded", "source", "cache",
				"error", mappingLogError(err), "action", "use_cache", "consequence", "mapping_may_be_stale",
				"mal_entries", malN, "anilist_entries", aniN, "duration_ms", time.Since(start).Milliseconds())
			return m, meta, nil
		}
		return nil, Metadata{}, fmt.Errorf("anibridge mapping not found and download failed: %w", err)
	}

	if canUseCache && meta.MD5 != "" && meta.MD5 == newMeta.MD5 {
		if m, parseErr := parseAnibridgeFileContext(ctx, path); parseErr == nil {
			saveKeySnapshot(metadataPath, m, &newMeta)
			slog.Debug("anibridge cache rewrite skipped", "type", "resolver",
				"task", "mapping_download", "outcome", "skipped", "reason", "md5_match",
				"source", "cache", "duration_ms", time.Since(start).Milliseconds())
			return m, newMeta, nil
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, newMeta, err
	}
	if err := writeFileAtomic(path, data); err != nil {
		return nil, newMeta, fmt.Errorf("write anibridge cache: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, newMeta, err
	}

	m, err := parseAnibridge(ctx, bytes.NewReader(data), "<bytes>")
	if err != nil {
		return nil, newMeta, fmt.Errorf("parse anibridge mapping: %w", err)
	}
	saveKeySnapshot(metadataPath, m, &newMeta)

	malN, aniN := m.Stats()
	logMappingUpdate(meta, newMeta, malN, aniN, time.Since(start))
	return m, newMeta, nil
}

// saveKeySnapshot records m's keys in meta and persists the sidecar.
func saveKeySnapshot(metadataPath string, m *AnibridgeMapping, meta *Metadata) {
	meta.MALKeys, meta.AniListKeys = m.Keys()
	if err := WriteMetadata(metadataPath, *meta); err != nil {
		slog.Warn("anibridge metadata persistence failed; next refresh may download again", "type", "resolver",
			"task", "mapping_load", "outcome", "degraded", "error", err, "path", metadataPath,
			"action", "continue", "consequence", "next_refresh_may_download_again")
	}
}

// logMappingUpdate reports key-set changes as diagnostic detail. MAL and
// AniList IDs are separate namespaces even when their numeric values match.
func logMappingUpdate(prev, curr Metadata, malTotal, aniTotal int, duration time.Duration) {
	total := malTotal + aniTotal
	if len(prev.MALKeys) == 0 && len(prev.AniListKeys) == 0 {
		slog.Debug("anibridge mapping key snapshot", "type", "resolver", "task", "mapping_load",
			"mal_entries", malTotal,
			"anilist_entries", aniTotal,
			"total_entries", total,
			"duration_ms", duration.Milliseconds(),
		)
		return
	}

	added, removed := diffKeyCounts(prev, curr)
	slog.Debug("anibridge mapping key changes", "type", "resolver", "task", "mapping_load",
		"new", added,
		"removals", removed,
		"total_entries", total,
		"duration_ms", duration.Milliseconds(),
	)
}

// diffKeyCounts counts keys added and removed between two snapshots. MAL and
// AniList IDs are separate namespaces.
func diffKeyCounts(prev, curr Metadata) (added, removed int) {
	added = countMissing(curr.MALKeys, prev.MALKeys) + countMissing(curr.AniListKeys, prev.AniListKeys)
	removed = countMissing(prev.MALKeys, curr.MALKeys) + countMissing(prev.AniListKeys, curr.AniListKeys)
	return added, removed
}

// countMissing returns how many distinct keys in from are absent from in.
func countMissing(from, in []int) int {
	present := make(map[int]bool, len(in))
	for _, k := range in {
		present[k] = true
	}
	missing := make(map[int]bool)
	for _, k := range from {
		if !present[k] {
			missing[k] = true
		}
	}
	return len(missing)
}

// URL errors include the configured upstream URL in their text. Keep the
// underlying diagnostic without exposing that URL in recoverable warnings.
func mappingLogError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return mappingLogError(urlErr.Err)
	}
	return err
}

// Head performs a HEAD against the upstream URL, following redirects. It
// returns the current ETag, Last-Modified, and MD5 as exposed by the final
// response.
func Head(ctx context.Context, url string) (Metadata, error) {
	resp, meta, err := doMappingRequest(ctx, http.MethodHead, url)
	if err != nil {
		return Metadata{}, err
	}
	defer resp.Body.Close()

	if raw := resp.Header.Get(md5Header); raw != "" {
		if sum, decErr := base64.StdEncoding.DecodeString(raw); decErr == nil {
			meta.MD5 = hex.EncodeToString(sum)
		} else {
			slog.Warn("invalid anibridge MD5 header; continuing without checksum metadata", "type", "resolver",
				"task", "mapping_download", "outcome", "degraded", "error", decErr,
				"action", "continue", "consequence", "head_checksum_unavailable")
		}
	}
	return meta, nil
}

// Fetch performs a full GET against the upstream URL, following redirects.
// The returned data is the raw bytes (still zstd-compressed) ready to be
// written to disk and parsed.
func Fetch(ctx context.Context, url string) ([]byte, Metadata, error) {
	resp, meta, err := doMappingRequest(ctx, http.MethodGet, url)
	if err != nil {
		return nil, Metadata{}, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCompressedMappingBytes+1))
	if err != nil {
		return nil, Metadata{}, fmt.Errorf("read anibridge body: %w", err)
	}
	if len(data) > maxCompressedMappingBytes {
		return nil, Metadata{}, fmt.Errorf("anibridge body exceeds %d bytes", maxCompressedMappingBytes)
	}

	if expectedB64 := resp.Header.Get(md5Header); expectedB64 != "" {
		expectedRaw, decErr := base64.StdEncoding.DecodeString(expectedB64)
		if decErr != nil {
			return nil, meta, fmt.Errorf("invalid anibridge MD5 header: %w", decErr)
		}
		sum := md5.Sum(data)
		got := hex.EncodeToString(sum[:])
		want := hex.EncodeToString(expectedRaw)
		if !strings.EqualFold(got, want) {
			return nil, meta, fmt.Errorf("anibridge MD5 mismatch: got %s, want %s", got, want)
		}
		meta.MD5 = got
	}
	return data, meta, nil
}

const md5Header = "x-ms-blob-content-md5"

// doMappingRequest sends a validated request and returns a 200 response along
// with the metadata derived from its headers. Callers must close the body.
func doMappingRequest(ctx context.Context, method, url string) (*http.Response, Metadata, error) {
	if err := validateRemoteURL(url); err != nil {
		return nil, Metadata{}, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, Metadata{}, fmt.Errorf("create %s request: %w", method, err)
	}
	req.Header.Set("User-Agent", "sonarr-anime-bridge/1.0")

	resp, err := secureHTTPClient().Do(req)
	if err != nil {
		return nil, Metadata{}, fmt.Errorf("%s anibridge: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, Metadata{}, fmt.Errorf("%s anibridge: HTTP %d", method, resp.StatusCode)
	}
	return resp, Metadata{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		URL:          url,
		FetchedAt:    time.Now().UTC(),
	}, nil
}

func parseAnibridgeFileContext(ctx context.Context, path string) (*AnibridgeMapping, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open anibridge mapping: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Debug("close anibridge mapping failed", "type", "resolver", "task", "mapping_parse", "path", path, "error", err)
		}
	}()
	return parseAnibridge(ctx, f, path)
}

// parseAnibridge decompresses a zstd mapping stream and parses its JSON.
func parseAnibridge(ctx context.Context, r io.Reader, src string) (*AnibridgeMapping, error) {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("create zstd reader: %w", err)
	}
	defer zr.Close()
	return parseAnibridgeJSON(ctx, zr, src)
}

func writeFileAtomic(path string, data []byte) error {
	var err error
	path, err = datapath.Validate(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename file: %w", err)
	}
	cleanup = false
	return nil
}

func metaPath(mappingPath string) string {
	return filepath.Join(filepath.Dir(mappingPath), filepath.Base(mappingPath)+".meta.json")
}

func validateRemoteURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse anibridge URL: %w", err)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("anibridge URL must include a host")
	}
	insecureOK := mappingurl.InsecureLoopbackAllowed(u)
	if u.Scheme != "https" && !insecureOK {
		return fmt.Errorf("anibridge URL must use https")
	}
	if !mappingurl.AllowedHost(u.Hostname()) && !insecureOK {
		return fmt.Errorf("anibridge URL host is not allowlisted: %s", u.Hostname())
	}
	ips, err := lookupMappingHost(u.Hostname())
	if err != nil {
		return fmt.Errorf("resolve anibridge URL host: %w", err)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) && !ip.IsLoopback() {
			return fmt.Errorf("anibridge URL host resolved to non-public IP")
		}
		if ip.IsLoopback() && !insecureOK {
			return fmt.Errorf("anibridge URL host resolved to loopback IP")
		}
	}
	return nil
}

func lookupMappingHost(host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	return net.LookupIP(host)
}

func secureHTTPClient() *http.Client {
	return &http.Client{
		Timeout: defaultAnibridgeHTTPTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return validateRemoteURL(req.URL.String())
		},
	}
}

func isPublicIP(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsUnspecified()
}

type limitReader struct {
	io.Reader
	limit int64
	read  int64
}

func (r *limitReader) Read(p []byte) (int, error) {
	if r.read >= r.limit {
		return 0, fmt.Errorf("decoded anibridge mapping exceeds %d bytes", r.limit)
	}
	if int64(len(p)) > r.limit-r.read {
		p = p[:r.limit-r.read]
	}
	n, err := r.Reader.Read(p)
	r.read += int64(n)
	return n, err
}

// ReadMetadata loads sidecar metadata from disk. A missing file is not an
// error — it returns a zero Metadata so callers can detect "no cache yet".
func ReadMetadata(path string) (Metadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Metadata{}, nil
		}
		return Metadata{}, fmt.Errorf("read anibridge metadata: %w", err)
	}
	var m Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return Metadata{}, fmt.Errorf("parse anibridge metadata: %w", err)
	}
	return m, nil
}

// WriteMetadata atomically writes sidecar metadata to disk.
func WriteMetadata(path string, m Metadata) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal anibridge metadata: %w", err)
	}
	return writeFileAtomic(path, data)
}

func parseAnibridgeJSON(ctx context.Context, r io.Reader, src string) (*AnibridgeMapping, error) {
	limited := &limitReader{Reader: r, limit: maxDecodedMappingBytes}
	dec := json.NewDecoder(limited)

	t, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("parse anibridge JSON: expected opening brace: %w", err)
	}
	if t != json.Delim('{') {
		return nil, fmt.Errorf("parse anibridge JSON: expected '{', got %T(%v)", t, t)
	}

	start := time.Now()
	byMAL := map[int]int{}
	byAniList := map[int]int{}

	for dec.More() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("parse anibridge JSON: key token: %w", err)
		}
		key, ok := t.(string)
		if !ok {
			return nil, fmt.Errorf("parse anibridge JSON: expected string key, got %T", t)
		}

		var target map[int]int
		var rawID string
		if rest, ok := strings.CutPrefix(key, "mal:"); ok {
			target, rawID = byMAL, rest
		} else if rest, ok := strings.CutPrefix(key, "anilist:"); ok {
			target, rawID = byAniList, rest
		}
		if target != nil {
			if id, convErr := strconv.Atoi(rawID); convErr == nil && id > 0 {
				tvdbID, ok, err := extractTVDB(dec)
				if err != nil {
					return nil, fmt.Errorf("parse anibridge JSON: %s: %w", key, err)
				}
				if ok {
					target[id] = tvdbID
				}
			} else if err := skipValue(dec); err != nil {
				return nil, err
			}
			continue
		}

		switch key {
		case "$meta":
			var meta anibridgeMeta
			if err := dec.Decode(&meta); err != nil {
				slog.Warn("anibridge dataset metadata unreadable; continuing with mapping entries", "type", "resolver",
					"task", "mapping_parse", "outcome", "degraded", "error", err,
					"action", "continue", "consequence", "dataset_metadata_unavailable")
			} else {
				slog.Debug("anibridge dataset metadata", "type", "resolver", "task", "mapping_parse",
					"schema_version", meta.SchemaVersion, "generated_on", meta.GeneratedOn)
			}

		default:
			if err := skipValue(dec); err != nil {
				return nil, err
			}
		}
	}

	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("parse anibridge JSON: expected closing brace: %w", err)
	}

	slog.Debug("parsed anibridge mapping", "type", "resolver", "task", "mapping_parse", "outcome", "succeeded",
		"mal_entries", len(byMAL), "anilist_entries", len(byAniList),
		"duration_ms", time.Since(start).Milliseconds(), "source", src)

	return &AnibridgeMapping{byMAL: byMAL, byAniList: byAniList}, nil
}

func skipValue(dec *json.Decoder) error {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("skip value: %w", err)
	}
	return nil
}

// extractTVDB chooses the best TVDB ID for a single anibridge entry. The
// anibridge data often lists the same show multiple times under different
// season scopes (e.g. `tvdb_show:123:s1` for the regular season and
// `tvdb_show:123:s0` for specials). We prefer s1 entries, and otherwise fall
// back to the scope with the highest source-episode count.
func extractTVDB(dec *json.Decoder) (int, bool, error) {
	var targets map[string]json.RawMessage
	if err := dec.Decode(&targets); err != nil {
		return 0, false, fmt.Errorf("decode targets: %w", err)
	}

	bestTVDB := 0
	bestEpCount := -1
	bestIsS1 := false

	for descriptor, rawValue := range targets {
		if !strings.HasPrefix(descriptor, "tvdb_show:") {
			continue
		}

		parts := strings.SplitN(descriptor, ":", 3)
		if len(parts) < 3 {
			continue
		}
		tvdbID, convErr := strconv.Atoi(parts[1])
		if convErr != nil || tvdbID <= 0 {
			continue
		}
		scope := parts[2]

		epCount, err := countSourceEpisodes(rawValue)
		if err != nil {
			return 0, false, fmt.Errorf("count episodes for %s: %w", descriptor, err)
		}

		isS1 := scope == "s1"
		if isS1 != bestIsS1 {
			if isS1 {
				bestTVDB = tvdbID
				bestEpCount = epCount
				bestIsS1 = true
			}
			continue
		}
		if epCount > bestEpCount {
			bestTVDB = tvdbID
			bestEpCount = epCount
		}
	}

	if bestTVDB > 0 {
		return bestTVDB, true, nil
	}
	return 0, false, nil
}

func countSourceEpisodes(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}

	var ranges map[string]string
	if err := json.Unmarshal(raw, &ranges); err != nil {
		return 0, err
	}

	var total int
	for srcRange := range ranges {
		if srcRange == "" {
			return 0, errors.New("empty episode range")
		}
		parts := strings.SplitN(srcRange, "-", 2)
		if len(parts) == 1 {
			ep, err := strconv.Atoi(parts[0])
			if err != nil || ep <= 0 {
				return 0, fmt.Errorf("invalid episode range %q", srcRange)
			}
			total++
			continue
		}
		start, err := strconv.Atoi(parts[0])
		if err != nil || start <= 0 {
			return 0, fmt.Errorf("invalid episode range %q", srcRange)
		}
		if parts[1] == "" {
			total++
			continue
		}
		end, err := strconv.Atoi(parts[1])
		if err != nil || end < start {
			return 0, fmt.Errorf("invalid episode range %q", srcRange)
		}
		total += end - start + 1
	}
	return total, nil
}
