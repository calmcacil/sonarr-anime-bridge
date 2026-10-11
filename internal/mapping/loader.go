package mapping

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
	"github.com/calmcacil/sonarr-anime-bridge/internal/datapath"
)

type Disposition string

const (
	Loaded   Disposition = "loaded"
	Reused   Disposition = "reused"
	Fallback Disposition = "fallback"
)

type LoadResult struct {
	Mapping     *AnibridgeMapping
	Metadata    Metadata
	Disposition Disposition
}

type parsedSource struct {
	path, url string
	digest    [sha256.Size]byte
	mapping   *AnibridgeMapping
	metadata  Metadata
}

type selectedSource struct {
	result LoadResult
	digest [sha256.Size]byte
}

// Loader retains one parsed source. Its admission gate serializes I/O and
// source-state changes; resolver readers do not use the gate.
// Construct loaders with NewLoader.
type Loader struct {
	gate  chan struct{}
	state *parsedSource
	parse func(context.Context, io.Reader, string) (*AnibridgeMapping, error)
	write func(string, []byte) error
}

func NewLoader() *Loader {
	return &Loader{
		gate: make(chan struct{}, 1), parse: parseAnibridge, write: writeFileAtomic,
	}
}

func (l *Loader) Load(ctx context.Context, path, url string) (LoadResult, error) {
	select {
	case l.gate <- struct{}{}:
		defer func() { <-l.gate }()
	case <-ctx.Done():
		return LoadResult{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return LoadResult{}, err
	}
	if path == "" {
		path = config.DefaultAnibridgeMappingPath
	}
	if url == "" {
		url = config.DefaultAnibridgeURL
	}
	var err error
	path, err = datapath.Validate(path)
	if err != nil {
		return LoadResult{}, err
	}
	if err := validateRemoteURL(url); err != nil {
		return LoadResult{}, mappingLogError(err)
	}
	source, err := l.load(ctx, path, url)
	if err != nil {
		return source.result, mappingLogError(err)
	}
	if err := ctx.Err(); err != nil {
		return LoadResult{}, err
	}
	l.state = &parsedSource{path: path, url: url, digest: source.digest, mapping: source.result.Mapping, metadata: source.result.Metadata}
	return source.result, nil
}

func (l *Loader) matches(path, url string, digest [sha256.Size]byte) bool {
	return l.state != nil && l.state.path == path && l.state.url == url && l.state.digest == digest
}

func (l *Loader) load(ctx context.Context, path, url string) (selectedSource, error) {
	start := time.Now()
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
	// In-memory ownership survives missing or edited sidecar metadata.
	urlChanged := haveCache && ((meta.URL != "" && meta.URL != url) ||
		(l.state != nil && l.state.path == path && l.state.url != url))
	if urlChanged {
		slog.Debug("anibridge cache bypassed", "type", "resolver",
			"task", "mapping_load", "outcome", "skipped", "reason", "url_changed", "action", "download")
		meta = Metadata{}
	}
	canUseCache := haveCache && !urlChanged
	if canUseCache && meta.ETag != "" {
		slog.Debug("checking anibridge upstream for updates", "type", "resolver", "task", "mapping_load", "path", path)
		upstream, fetchErr := Head(ctx, url)
		switch {
		case fetchErr != nil:
			if ctx.Err() == nil {
				slog.Warn("anibridge HEAD failed; continuing with download", "type", "resolver",
					"task", "mapping_download", "outcome", "degraded", "error", mappingLogError(fetchErr),
					"action", "download", "consequence", "freshness_check_unavailable")
			}
		case strings.EqualFold(strings.TrimSpace(upstream.ETag), strings.TrimSpace(meta.ETag)):
			source, parseErr := l.local(ctx, path, url, meta, Loaded)
			if parseErr == nil {
				slog.Debug("anibridge download skipped", "type", "resolver",
					"task", "mapping_download", "outcome", "skipped", "reason", "etag_match",
					"source", "cache", "duration_ms", time.Since(start).Milliseconds())
				return source, nil
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
	if err := ctx.Err(); err != nil {
		return selectedSource{}, err
	}
	data, newMeta, err := Fetch(ctx, url)
	if err != nil {
		if ctx.Err() != nil {
			return selectedSource{}, ctx.Err()
		}
		if canUseCache {
			source, parseErr := l.local(ctx, path, url, meta, Fallback)
			if parseErr != nil {
				return selectedSource{}, fmt.Errorf("fetch failed and cached mapping is unreadable: %w", parseErr)
			}
			logMappingFallback(source.result.Mapping, err, "download_failed", start)
			return source, nil
		}
		return selectedSource{}, fmt.Errorf("anibridge mapping not found and download failed: %w", mappingLogError(err))
	}
	if err := ctx.Err(); err != nil {
		return selectedSource{}, err
	}
	digest := sha256.Sum256(data)
	if l.matches(path, url, digest) {
		// Even a verified download cannot skip repairing missing or changed disk
		// content. A sidecar checksum alone is not proof that the file is intact.
		localDigest, hashErr := hashMappingFile(ctx, path)
		if err := ctx.Err(); err != nil {
			return selectedSource{}, err
		}
		if hashErr != nil || localDigest != digest {
			if err := l.write(path, data); err != nil {
				return selectedSource{}, fmt.Errorf("write anibridge cache: %w", err)
			}
		}
		m := l.state.mapping
		saveKeySnapshot(metadataPath, m, &newMeta)
		logMappingReuse("download", start)
		return selectedSource{LoadResult{m, newMeta, Reused}, digest}, nil
	}
	// Preserve the existing MD5-selected local-cache policy, but verify the
	// selected file's actual bytes before any in-memory reuse.
	if canUseCache && meta.MD5 != "" && meta.MD5 == newMeta.MD5 {
		if source, parseErr := l.local(ctx, path, url, newMeta, Loaded); parseErr == nil {
			saveKeySnapshot(metadataPath, source.result.Mapping, &source.result.Metadata)
			slog.Debug("anibridge cache rewrite skipped", "type", "resolver",
				"task", "mapping_download", "outcome", "skipped", "reason", "md5_match",
				"source", "cache", "duration_ms", time.Since(start).Milliseconds())
			return source, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return selectedSource{}, err
	}
	m, err := l.parse(ctx, &contextReader{ctx: ctx, reader: bytes.NewReader(data)}, "<bytes>")
	if err != nil {
		if ctx.Err() != nil {
			return selectedSource{}, ctx.Err()
		}
		if canUseCache {
			if cached, cacheErr := l.local(ctx, path, url, meta, Fallback); cacheErr == nil {
				logMappingFallback(cached.result.Mapping, err, "invalid_download", start)
				return cached, nil
			}
		}
		return selectedSource{}, fmt.Errorf("parse anibridge mapping: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return selectedSource{}, err
	}
	if err := l.write(path, data); err != nil {
		return selectedSource{}, fmt.Errorf("write anibridge cache: %w", err)
	}
	saveKeySnapshot(metadataPath, m, &newMeta)
	malN, aniN := m.Stats()
	logMappingUpdate(meta, newMeta, malN, aniN, time.Since(start))
	return selectedSource{LoadResult{m, newMeta, Loaded}, digest}, nil
}

func (l *Loader) local(ctx context.Context, path, url string, meta Metadata, disposition Disposition) (selectedSource, error) {
	start := time.Now()
	f, err := os.Open(path)
	if err != nil {
		return selectedSource{}, fmt.Errorf("open anibridge mapping: %w", err)
	}
	defer closeMappingFile(f)
	digest, err := hashMapping(ctx, f)
	if err != nil {
		return selectedSource{}, err
	}
	if l.matches(path, url, digest) {
		if disposition != Fallback {
			disposition = Reused
			logMappingReuse("cache", start)
		}
		return selectedSource{LoadResult{l.state.mapping, meta, disposition}, digest}, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return selectedSource{}, fmt.Errorf("rewind anibridge mapping: %w", err)
	}
	hash := sha256.New()
	reader := io.TeeReader(&contextReader{ctx: ctx, reader: f}, hash)
	m, err := l.parse(ctx, reader, path)
	if err != nil {
		return selectedSource{}, err
	}
	// The JSON parser can stop before compressed EOF. Include every byte in
	// the identity and reject in-place changes between inspection and parsing.
	if _, err := io.CopyBuffer(io.Discard, reader, make([]byte, mappingHashBufferSize)); err != nil {
		return selectedSource{}, fmt.Errorf("finish anibridge fingerprint: %w", err)
	}
	if !bytes.Equal(digest[:], hash.Sum(nil)) {
		return selectedSource{}, fmt.Errorf("anibridge mapping changed during inspection")
	}
	return selectedSource{LoadResult{m, meta, disposition}, digest}, nil
}

const mappingHashBufferSize = 32 * 1024

func hashMappingFile(ctx context.Context, path string) ([sha256.Size]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer closeMappingFile(f)
	return hashMapping(ctx, f)
}

func hashMapping(ctx context.Context, r io.Reader) ([sha256.Size]byte, error) {
	hash := sha256.New()
	if _, err := io.CopyBuffer(hash, &contextReader{ctx: ctx, reader: r}, make([]byte, mappingHashBufferSize)); err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("fingerprint anibridge mapping: %w", err)
	}
	var digest [sha256.Size]byte
	hash.Sum(digest[:0])
	return digest, nil
}

func closeMappingFile(f *os.File) {
	if err := f.Close(); err != nil {
		slog.Debug("close anibridge mapping failed", "type", "resolver", "task", "mapping_load", "path", f.Name(), "error", err)
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}

func logMappingReuse(source string, start time.Time) {
	slog.Debug("anibridge mapping reused", "type", "resolver", "task", "mapping_load",
		"outcome", "succeeded", "reason", "unchanged_content", "source", source,
		"duration_ms", time.Since(start).Milliseconds())
}

func logMappingFallback(m *AnibridgeMapping, err error, reason string, start time.Time) {
	malN, aniN := m.Stats()
	slog.Warn("anibridge refresh failed; serving cached mapping", "type", "resolver",
		"task", "mapping_load", "outcome", "degraded", "source", "cache", "reason", reason,
		"error", mappingLogError(err), "action", "use_cache", "consequence", "mapping_may_be_stale",
		"mal_entries", malN, "anilist_entries", aniN, "duration_ms", time.Since(start).Milliseconds())
}
