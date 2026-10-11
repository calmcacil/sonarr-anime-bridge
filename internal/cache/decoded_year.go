package cache

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
)

const (
	defaultDecodedYearCapacity = 8
	defaultDecodedYearBytes    = 32 << 20
)

// DecodedYear distinguishes missing, corrupt and valid (including empty) rows.
// Shows owns its top-level array; all nested data must remain read-only.
type DecodedYear struct {
	Shows     []anilist.Show
	Fresh     bool
	Present   bool
	DecodeErr error
}

type decodedYearEntry struct {
	year  int
	raw   []byte
	shows []anilist.Show
	size  int64
}

type decodeGate struct {
	token chan struct{}
	refs  int
}

type decodedYearCache struct {
	mu         sync.Mutex
	generation uint64
	closed     bool
	entries    map[int]*list.Element
	lru        list.List
	bytes      int64
	maxYears   int
	maxBytes   int64
	gates      map[int]*decodeGate
	decode     func([]byte) ([]anilist.Show, error)

	// Package tests use barriers to exercise commit/invalidation orderings.
	beforeInvalidate func()
}

func newDecodedYearCache() *decodedYearCache {
	return &decodedYearCache{
		entries:  make(map[int]*list.Element),
		gates:    make(map[int]*decodeGate),
		maxYears: defaultDecodedYearCapacity,
		maxBytes: defaultDecodedYearBytes,
		decode:   DecodeYearData,
	}
}

// DecodeYearData validates persisted JSON and returns an independently decoded slice.
func DecodeYearData(data []byte) ([]anilist.Show, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("year data must be a JSON array")
	}
	var shows []anilist.Show
	if err := json.Unmarshal(trimmed, &shows); err != nil {
		return nil, err
	}
	return shows, nil
}

// GetDecodedYearContext performs one SQLite access with the usual hit/retention
// accounting, then reuses an unchanged decoding. Nested fields are read-only.
func (c *Cache) GetDecodedYearContext(ctx context.Context, year int) (DecodedYear, error) {
	return c.readDecodedYearContext(ctx, year, true)
}

// PeekDecodedYearContext hydrates/reuses decoding without counting user access.
// Nested fields are read-only, just as with GetDecodedYearContext.
func (c *Cache) PeekDecodedYearContext(ctx context.Context, year int) (DecodedYear, error) {
	return c.readDecodedYearContext(ctx, year, false)
}

func (c *Cache) readDecodedYearContext(ctx context.Context, year int, recordAccess bool) (DecodedYear, error) {
	if err := ctx.Err(); err != nil {
		return DecodedYear{}, err
	}
	d := c.decoded
	d.mu.Lock()
	generation := d.generation
	d.mu.Unlock()
	raw, fresh, present, err := c.readYearContext(ctx, year, recordAccess)
	if err != nil {
		return DecodedYear{}, err
	}
	result := DecodedYear{Fresh: fresh, Present: present}
	shows, hit := d.lookup(year, raw, present, generation)
	if !present {
		return result, ctx.Err()
	}
	if !hit {
		release, err := d.acquire(ctx, year)
		if err != nil {
			return DecodedYear{}, err
		}
		defer release()
		if err := ctx.Err(); err != nil {
			return DecodedYear{}, err
		}
		shows, hit = d.lookup(year, raw, true, generation)
		if !hit {
			shows, result.DecodeErr = d.decode(raw)
			if err := ctx.Err(); err != nil {
				return DecodedYear{}, err
			}
			if result.DecodeErr == nil {
				d.admit(year, raw, shows, generation)
			}
		}
	}
	if result.DecodeErr == nil {
		// Filtering compacts/appends to this array, never the resident array.
		result.Shows = make([]anilist.Show, len(shows))
		copy(result.Shows, shows)
	}
	if err := ctx.Err(); err != nil {
		return DecodedYear{}, err
	}
	return result, nil
}

func (d *decodedYearCache) lookup(year int, raw []byte, present bool, generation uint64) ([]anilist.Show, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if element := d.entries[year]; element != nil {
		entry := element.Value.(*decodedYearEntry)
		if present && bytes.Equal(raw, entry.raw) {
			d.lru.MoveToFront(element)
			return entry.shows, true
		}
		// An old read must not evict a replacement admitted after a mutation.
		if generation == d.generation {
			d.remove(element)
		}
	}
	return nil, false
}

func (d *decodedYearCache) acquire(ctx context.Context, year int) (func(), error) {
	d.mu.Lock()
	gate := d.gates[year]
	if gate == nil {
		gate = &decodeGate{token: make(chan struct{}, 1)}
		gate.token <- struct{}{}
		d.gates[year] = gate
	}
	gate.refs++
	d.mu.Unlock()
	unref := func() {
		d.mu.Lock()
		gate.refs--
		if gate.refs == 0 {
			delete(d.gates, year)
		}
		d.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		unref()
		return nil, ctx.Err()
	case <-gate.token:
		return func() {
			gate.token <- struct{}{}
			unref()
		}, nil
	}
}

func (d *decodedYearCache) admit(year int, raw []byte, shows []anilist.Show, generation uint64) {
	size := decodedYearSize(raw, shows)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || generation != d.generation || d.maxYears <= 0 || size > d.maxBytes {
		return
	}
	if element := d.entries[year]; element != nil {
		d.remove(element)
	}
	for len(d.entries) >= d.maxYears || d.bytes+size > d.maxBytes {
		d.remove(d.lru.Back())
	}
	entry := &decodedYearEntry{year: year, raw: raw, shows: shows, size: size}
	d.entries[year] = d.lru.PushFront(entry)
	d.bytes += size
}

func (d *decodedYearCache) remove(element *list.Element) {
	entry := element.Value.(*decodedYearEntry)
	d.bytes -= entry.size
	delete(d.entries, entry.year)
	d.lru.Remove(element)
}

// invalidate runs after SQL commits and before the mutation reports success.
func (d *decodedYearCache) invalidate(year int, all, closed bool) {
	if d.beforeInvalidate != nil {
		d.beforeInvalidate()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.generation++
	d.closed = d.closed || closed
	if all {
		clear(d.entries)
		d.lru.Init()
		d.bytes = 0
	} else if element := d.entries[year]; element != nil {
		d.remove(element)
	}
}

// Account payload capacities and nested data, not allocator/map overhead,
// request copies, in-flight decodes or snapshots still held by readers.
func decodedYearSize(raw []byte, shows []anilist.Show) int64 {
	size := int64(unsafe.Sizeof(decodedYearEntry{})+unsafe.Sizeof(list.Element{})) + int64(cap(raw))
	size += int64(cap(shows)) * int64(unsafe.Sizeof(anilist.Show{}))
	for _, show := range shows {
		size += int64(len(show.Format) + len(show.Season))
		for _, number := range []*int{show.IDMal, show.Duration, show.StartDate.Year, show.StartDate.Month, show.StartDate.Day} {
			if number != nil {
				size += int64(unsafe.Sizeof(*number))
			}
		}
		for _, title := range []*string{show.Title.English, show.Title.Romaji} {
			if title != nil {
				size += int64(unsafe.Sizeof(*title)) + int64(len(*title))
			}
		}
		size += int64(cap(show.Tags)) * int64(unsafe.Sizeof(anilist.Tag{}))
		for _, tag := range show.Tags {
			size += int64(len(tag.Name))
		}
		if show.Relations != nil {
			size += int64(unsafe.Sizeof(*show.Relations))
			size += int64(cap(show.Relations.Edges)) * int64(unsafe.Sizeof(anilist.RelationEdge{}))
			for _, edge := range show.Relations.Edges {
				size += int64(len(edge.RelationType))
			}
		}
	}
	return size
}
