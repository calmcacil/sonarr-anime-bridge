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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
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
	malKeys = make([]int, 0, len(m.byMAL))
	for k := range m.byMAL {
		malKeys = append(malKeys, k)
	}
	aniListKeys = make([]int, 0, len(m.byAniList))
	for k := range m.byAniList {
		aniListKeys = append(aniListKeys, k)
	}
	return malKeys, aniListKeys
}

// LoadOrFetch loads the mapping from path, downloading from upstream only when
// the local cache is missing or stale. If a download fails, falls back to the
// cached file when possible.
func LoadOrFetch(ctx context.Context, path, url string) (*AnibridgeMapping, Metadata, error) {
	if path == "" {
		path = config.DefaultAnibridgeMappingPath
	}
	if url == "" {
		url = config.DefaultAnibridgeURL
	}
	var err error
	path, err = validateDataPath(path)
	if err != nil {
		return nil, Metadata{}, err
	}
	if err := validateRemoteURL(url); err != nil {
		return nil, Metadata{}, err
	}

	metadataPath, err := validateDataPath(metaPath(path))
	if err != nil {
		return nil, Metadata{}, err
	}
	meta, metaErr := ReadMetadata(metadataPath)
	if metaErr != nil {
		slog.Warn("failed to read anibridge sidecar metadata", "error", metaErr, "path", metadataPath)
	}
	haveCache := false
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		haveCache = true
	}

	urlChanged := haveCache && meta.URL != "" && meta.URL != url
	if urlChanged {
		slog.Info("anibridge URL changed, ignoring cached mapping",
			"old_url", meta.URL, "new_url", url)
		meta = Metadata{}
	}
	canUseCache := haveCache && !urlChanged

	if canUseCache && meta.ETag != "" {
		slog.Debug("checking anibridge upstream for updates", "path", path)
		upstream, fetchErr := Head(ctx, url)
		switch {
		case fetchErr != nil:
			slog.Warn("anibridge HEAD failed, using cached mapping", "error", fetchErr)
		case strings.EqualFold(strings.TrimSpace(upstream.ETag), strings.TrimSpace(meta.ETag)):
			slog.Info("anibridge mapping is up to date (ETag match)", "etag", meta.ETag)
			m, parseErr := parseAnibridgeFileContext(ctx, path)
			if parseErr == nil {
				return m, meta, nil
			}
			slog.Warn("cached anibridge file is corrupt, re-downloading", "error", parseErr)
		default:
			slog.Info("anibridge mapping is stale, refreshing",
				"cached_etag", meta.ETag, "upstream_etag", upstream.ETag)
		}
	}

	data, newMeta, err := Fetch(ctx, url)
	if err != nil {
		if canUseCache {
			slog.Warn("anibridge fetch failed, using cached mapping", "error", err)
			m, parseErr := parseAnibridgeFileContext(ctx, path)
			if parseErr != nil {
				return nil, meta, fmt.Errorf("fetch failed and cached mapping is unreadable: %w", parseErr)
			}
			return m, meta, nil
		}
		return nil, Metadata{}, fmt.Errorf("anibridge mapping not found and download failed: %w", err)
	}

	if canUseCache && meta.MD5 != "" && newMeta.MD5 != "" && meta.MD5 == newMeta.MD5 {
		slog.Info("anibridge mapping is unchanged (MD5 match), refreshing in-memory only")
		m, parseErr := parseAnibridgeFileContext(ctx, path)
		if parseErr == nil {
			malKeys, aniKeys := m.Keys()
			newMeta.MALKeys = malKeys
			newMeta.AniListKeys = aniKeys
			if err := WriteMetadata(metadataPath, newMeta); err != nil {
				slog.Warn("failed to update anibridge sidecar metadata", "error", err)
			}
			return m, newMeta, nil
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, newMeta, err
	}
	if err := writeAnibridgeFile(path, data); err != nil {
		return nil, newMeta, fmt.Errorf("write anibridge cache: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, newMeta, err
	}

	m, err := parseAnibridgeBytesContext(ctx, data)
	if err != nil {
		return nil, newMeta, fmt.Errorf("parse anibridge mapping: %w", err)
	}

	malKeys, aniKeys := m.Keys()
	newMeta.MALKeys = malKeys
	newMeta.AniListKeys = aniKeys

	if err := WriteMetadata(metadataPath, newMeta); err != nil {
		slog.Warn("failed to write anibridge sidecar metadata", "error", err, "path", metadataPath)
	}

	malN, aniN := m.Stats()
	logMappingUpdate(meta, newMeta, malN, aniN)
	return m, newMeta, nil
}

// logMappingUpdate emits a single human-friendly line summarising the
// result of a successful anibridge load. The format is intentionally
// simple so it surfaces cleanly in `docker logs`:
//
//	Updated anibridge database, 12 new, 3 removals, 18091 total entries
//
// MAL and AniList IDs are tracked in separate namespaces since the same
// numeric value in each represents a different show.
func logMappingUpdate(prev, curr Metadata, malTotal, aniTotal int) {
	total := malTotal + aniTotal
	if len(prev.MALKeys) == 0 && len(prev.AniListKeys) == 0 {
		slog.Info("Loaded anibridge database",
			"mal_entries", malTotal,
			"anilist_entries", aniTotal,
			"total_entries", total,
		)
		return
	}

	prevMAL := keySet(prev.MALKeys)
	prevAni := keySet(prev.AniListKeys)
	currMAL := keySet(curr.MALKeys)
	currAni := keySet(curr.AniListKeys)

	var added, removed int
	for k := range currMAL {
		if !prevMAL[k] {
			added++
		}
	}
	for k := range currAni {
		if !prevAni[k] {
			added++
		}
	}
	for k := range prevMAL {
		if !currMAL[k] {
			removed++
		}
	}
	for k := range prevAni {
		if !currAni[k] {
			removed++
		}
	}

	slog.Info("Updated anibridge database",
		"new", added,
		"removals", removed,
		"total_entries", total,
	)
}

func keySet(keys []int) map[int]bool {
	s := make(map[int]bool, len(keys))
	for _, k := range keys {
		s[k] = true
	}
	return s
}

// Head performs a HEAD against the upstream URL, following redirects. It
// returns the current ETag, Last-Modified, and MD5 as exposed by the final
// response.
func Head(ctx context.Context, url string) (Metadata, error) {
	if err := validateRemoteURL(url); err != nil {
		return Metadata{}, err
	}
	client := secureHTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return Metadata{}, fmt.Errorf("create HEAD request: %w", err)
	}
	req.Header.Set("User-Agent", "sonarr-anime-bridge/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return Metadata{}, fmt.Errorf("HEAD anibridge: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Metadata{}, fmt.Errorf("HEAD anibridge: HTTP %d", resp.StatusCode)
	}

	md5FromHeader := ""
	if raw := resp.Header.Get("x-ms-blob-content-md5"); raw != "" {
		if rawBytes, decErr := base64.StdEncoding.DecodeString(raw); decErr == nil {
			md5FromHeader = hex.EncodeToString(rawBytes)
		} else {
			slog.Warn("invalid anibridge MD5 header", "error", decErr)
		}
	}
	return Metadata{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		MD5:          md5FromHeader,
		URL:          url,
		FetchedAt:    time.Now().UTC(),
	}, nil
}

// Fetch performs a full GET against the upstream URL, following redirects.
// The returned data is the raw bytes (still zstd-compressed) ready to be
// written to disk and parsed.
func Fetch(ctx context.Context, url string) ([]byte, Metadata, error) {
	if err := validateRemoteURL(url); err != nil {
		return nil, Metadata{}, err
	}
	client := secureHTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, Metadata{}, fmt.Errorf("create GET request: %w", err)
	}
	req.Header.Set("User-Agent", "sonarr-anime-bridge/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, Metadata{}, fmt.Errorf("GET anibridge: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, Metadata{}, fmt.Errorf("GET anibridge: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCompressedMappingBytes+1))
	if err != nil {
		return nil, Metadata{}, fmt.Errorf("read anibridge body: %w", err)
	}
	if len(data) > maxCompressedMappingBytes {
		return nil, Metadata{}, fmt.Errorf("anibridge body exceeds %d bytes", maxCompressedMappingBytes)
	}

	meta := Metadata{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		URL:          url,
		FetchedAt:    time.Now().UTC(),
	}

	if expectedB64 := resp.Header.Get("x-ms-blob-content-md5"); expectedB64 != "" {
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

func parseAnibridgeFile(path string) (*AnibridgeMapping, error) {
	return parseAnibridgeFileContext(context.Background(), path)
}

func parseAnibridgeFileContext(ctx context.Context, path string) (*AnibridgeMapping, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open anibridge mapping: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only, error not useful

	zr, err := zstd.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("create zstd reader: %w", err)
	}
	defer zr.Close()

	return parseAnibridgeJSON(ctx, io.LimitReader(zr, maxDecodedMappingBytes+1), path)
}

func parseAnibridgeBytesContext(ctx context.Context, data []byte) (*AnibridgeMapping, error) {
	zr, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create zstd reader: %w", err)
	}
	defer zr.Close()
	return parseAnibridgeJSON(ctx, io.LimitReader(zr, maxDecodedMappingBytes+1), "<bytes>")
}

func writeFileAtomic(path string, data []byte) error {
	var err error
	path, err = validateDataPath(path)
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
		tmp.Close() //nolint:errcheck // cleanup on error path
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

func writeAnibridgeFile(path string, data []byte) error {
	return writeFileAtomic(path, data)
}

func metaPath(mappingPath string) string {
	return filepath.Join(filepath.Dir(mappingPath), filepath.Base(mappingPath)+".meta.json")
}

func validateDataPath(path string) (string, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("path must be absolute: %s", path)
	}
	for _, base := range []string{"/data", os.TempDir()} {
		rel, err := filepath.Rel(base, cleaned)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return cleaned, nil
		}
	}
	return "", fmt.Errorf("path must be under /data: %s", path)
}

func validateRemoteURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse anibridge URL: %w", err)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("anibridge URL must include a host")
	}
	if u.Scheme != "https" {
		if !allowInsecureMappingURL(u) {
			return fmt.Errorf("anibridge URL must use https")
		}
	}
	if !allowedMappingHost(u.Hostname()) && !allowInsecureMappingURL(u) {
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
		if ip.IsLoopback() && !allowInsecureMappingURL(u) {
			return fmt.Errorf("anibridge URL host resolved to loopback IP")
		}
	}
	return nil
}

func allowInsecureMappingURL(u *url.URL) bool {
	if os.Getenv("ALLOW_INSECURE_MAPPING_URL") != "1" && !strings.HasSuffix(os.Args[0], ".test") {
		return false
	}
	host := u.Hostname()
	return u.Scheme == "http" && (host == "127.0.0.1" || host == "localhost" || host == "::1")
}

func allowedMappingHost(host string) bool {
	switch strings.ToLower(host) {
	case "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com":
		return true
	default:
		return false
	}
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

		switch {
		case strings.HasPrefix(key, "mal:"):
			id, convErr := strconv.Atoi(key[4:])
			if convErr != nil || id <= 0 {
				if err := skipValue(dec); err != nil {
					return nil, err
				}
				continue
			}
			if tvdbID, ok, err := extractTVDB(dec); err != nil {
				return nil, fmt.Errorf("parse anibridge JSON: %s: %w", key, err)
			} else if ok {
				byMAL[id] = tvdbID
			}

		case strings.HasPrefix(key, "anilist:"):
			id, convErr := strconv.Atoi(key[8:])
			if convErr != nil || id <= 0 {
				if err := skipValue(dec); err != nil {
					return nil, err
				}
				continue
			}
			if tvdbID, ok, err := extractTVDB(dec); err != nil {
				return nil, fmt.Errorf("parse anibridge JSON: %s: %w", key, err)
			} else if ok {
				byAniList[id] = tvdbID
			}

		case key == "$meta":
			var meta anibridgeMeta
			if err := dec.Decode(&meta); err != nil {
				slog.Warn("failed to decode anibridge metadata", "error", err)
			} else {
				slog.Info("anibridge dataset",
					"schema_version", meta.SchemaVersion,
					"generated_on", meta.GeneratedOn)
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

	slog.Info("parsed anibridge mapping",
		"mal_entries", len(byMAL), "anilist_entries", len(byAniList),
		"parse_ms", time.Since(start).Milliseconds(),
		"source", src)

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
