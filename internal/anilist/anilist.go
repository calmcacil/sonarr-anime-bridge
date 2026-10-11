package anilist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	apiBase             = "https://graphql.anilist.co"
	maxRetry            = 5
	rateLimitDelay      = 2300 * time.Millisecond
	defaultBatchSize    = 4
	maxPerPage          = 50
	maxPages            = 100
	maxRetryAfter       = 2 * time.Minute
	maxErrorBodyBytes   = 64 << 10
	maxResponseBodySize = 10 << 20
)

// Tag represents an AniList content tag with name and relevance rank.
type Tag struct {
	Name string `json:"name"`
}

// RelationEdge represents a related media entry.
type RelationEdge struct {
	RelationType string `json:"relationType"`
}

// RelationBlock holds the edges wrapper.
type RelationBlock struct {
	Edges []RelationEdge `json:"edges"`
}

// Show represents an anime show from the AniList API.
type Show struct {
	ID        int            `json:"id"`
	IDMal     *int           `json:"idMal"`
	Title     Title          `json:"title"`
	Format    string         `json:"format"`
	Duration  *int           `json:"duration"`
	Tags      []Tag          `json:"tags"`
	Season    string         `json:"season"`
	StartDate FuzzyDate      `json:"startDate"`
	Relations *RelationBlock `json:"relations,omitempty"`
}

// IsNew returns true if the show is not a sequel or spin-off of an existing franchise.
// A show is considered "new" if it has no PREQUEL or PARENT relations.
func (s Show) IsNew() bool {
	if s.Relations == nil {
		return true
	}
	for _, e := range s.Relations.Edges {
		if e.RelationType == "PREQUEL" || e.RelationType == "PARENT" {
			return false
		}
	}
	return true
}

// SkipByDuration returns true if the show should be skipped because its
// per-episode duration is ≤ 10 minutes.
func (s Show) SkipByDuration() bool {
	return s.Duration != nil && *s.Duration <= 10
}

// HasTag returns true if the show has a tag matching the given name
// (case-insensitive).
func (s Show) HasTag(name string) bool {
	for _, t := range s.Tags {
		if strings.EqualFold(t.Name, name) {
			return true
		}
	}
	return false
}

// FuzzyDate represents a partial date (year, month, day) from AniList.
type FuzzyDate struct {
	Year  *int `json:"year"`
	Month *int `json:"month"`
	Day   *int `json:"day"`
}

// Title holds the english and romaji titles.
type Title struct {
	English *string `json:"english"`
	Romaji  *string `json:"romaji"`
}

// IsWithinMonths returns true if the show's start date is within the given
// number of months from now. If the start date is unknown, returns true
// (don't filter out shows with unknown dates).
func (s Show) IsWithinMonths(months int) bool {
	if s.StartDate.Year == nil || s.StartDate.Month == nil {
		return true
	}
	start := time.Date(*s.StartDate.Year, time.Month(*s.StartDate.Month), 1, 0, 0, 0, 0, time.UTC)
	return !start.After(time.Now().AddDate(0, months, 0))
}

// DisplayTitle returns the English title if available, falling back to romaji.
func (s Show) DisplayTitle() string {
	if s.Title.English != nil && *s.Title.English != "" {
		return *s.Title.English
	}
	if s.Title.Romaji != nil {
		return *s.Title.Romaji
	}
	return fmt.Sprintf("Anime #%d", s.ID)
}

// Each root alias owns one Page and one media field, as required by AniList.
const yearQueryTemplate = `query($y: Int, $p0: Int, $p1: Int, $p2: Int, $p3: Int, $perPage: Int) {
 page0: Page(page: $p0, perPage: $perPage) {
  pageInfo { hasNextPage currentPage }
  media(seasonYear: $y, type: ANIME, sort: POPULARITY_DESC) { ...YearMedia }
 }
 page1: Page(page: $p1, perPage: $perPage) {
  pageInfo { hasNextPage currentPage }
  media(seasonYear: $y, type: ANIME, sort: POPULARITY_DESC) { ...YearMedia }
 }
 page2: Page(page: $p2, perPage: $perPage) {
  pageInfo { hasNextPage currentPage }
  media(seasonYear: $y, type: ANIME, sort: POPULARITY_DESC) { ...YearMedia }
 }
 page3: Page(page: $p3, perPage: $perPage) {
  pageInfo { hasNextPage currentPage }
  media(seasonYear: $y, type: ANIME, sort: POPULARITY_DESC) { ...YearMedia }
 }
}
fragment YearMedia on Media {
 id
 idMal
 title { romaji english }
 format
 duration
 tags { name }
 season
 startDate { year month day }
 relations { edges { relationType } }
}`

type graphqlError struct {
	Message string `json:"message"`
}

// pageInfo holds pagination metadata from AniList.
type pageInfo struct {
	HasNextPage *bool `json:"hasNextPage"`
	CurrentPage int   `json:"currentPage"`
}

type mediaPage struct {
	PageInfo *pageInfo `json:"pageInfo"`
	Media    []Show    `json:"media"`
}

// graphqlResponse is the top-level response from AniList.
type graphqlResponse struct {
	Data struct {
		Page0 *mediaPage `json:"page0"`
		Page1 *mediaPage `json:"page1"`
		Page2 *mediaPage `json:"page2"`
		Page3 *mediaPage `json:"page3"`
	} `json:"data"`
	Errors []graphqlError `json:"errors,omitempty"`
}

// Client fetches data from the AniList GraphQL API.
type Client struct {
	http    *http.Client
	baseURL string
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error

	rateLimitMu   sync.Mutex
	interval      time.Duration
	lastAdmission time.Time
	cooldownUntil time.Time
}

// NewWithTimeout creates a new AniList client with the given HTTP timeout.
func NewWithTimeout(timeout time.Duration) *Client {
	return NewWithHTTPClient(apiBase, &http.Client{Timeout: timeout})
}

func NewWithHTTPClient(baseURL string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = apiBase
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		http:     httpClient,
		baseURL:  baseURL,
		now:      time.Now,
		interval: rateLimitDelay,
		sleep:    sleepContext,
	}
}

// jitter returns d randomly varied by ±25% to prevent synchronized retry storms.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	quarter := d / 4
	offset := time.Duration(rand.Int64N(int64(2*quarter+1))) - quarter
	return d + offset
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Admission is assigned only after waiting. Old reservations cannot burst when
// a cooldown expires, and each waiter observes the latest upstream constraints.
func (c *Client) throttle(ctx context.Context, retryAt time.Time) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.rateLimitMu.Lock()
		now := c.now()
		ready := c.lastAdmission.Add(c.interval)
		if c.cooldownUntil.After(ready) {
			ready = c.cooldownUntil
		}
		if retryAt.After(ready) {
			ready = retryAt
		}
		if !ready.After(now) {
			c.lastAdmission = now
			c.rateLimitMu.Unlock()
			return nil
		}
		delay := ready.Sub(now)
		reason := "pacing"
		if c.cooldownUntil.After(now) {
			reason = "cooldown"
		}
		c.rateLimitMu.Unlock()
		if delay > maxRetryAfter {
			return fmt.Errorf("AniList admission wait exceeds %s", maxRetryAfter)
		}
		slog.Debug("AniList admission waiting", "type", "fetch", "task", "year_fetch",
			"outcome", "waiting", "reason", reason, "wait_ms", delay.Milliseconds())
		if err := c.sleep(ctx, delay); err != nil {
			return err
		}
	}
}

// observeAllowance owns shared upstream constraints. A recovered allowance can
// restore default pacing, but cannot erase an existing cooldown deadline.
func (c *Client) observeAllowance(resp *http.Response) (string, error) {
	c.rateLimitMu.Lock()
	defer c.rateLimitMu.Unlock()
	now := c.now()
	if limit, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Limit"), 10, 32); err == nil && limit > 0 {
		interval := time.Duration(float64(time.Minute) / (0.9 * float64(limit)))
		c.interval = max(rateLimitDelay, interval)
	}
	state := "not_applicable"
	limited := resp.StatusCode == http.StatusTooManyRequests
	remaining, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Remaining"), 10, 64)
	if !limited && (err != nil || remaining != 0) {
		return state, nil
	}
	var deadline time.Time
	if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > now.Unix() {
		deadline = now.Add(headerDuration(reset - now.Unix()))
		if reset-now.Unix() <= int64((1<<63-1)/time.Second) {
			deadline = time.Unix(reset, 0)
		}
	}
	if limited {
		state = "missing"
		if value := resp.Header.Get("Retry-After"); value != "" {
			state = "invalid"
			if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
				state = "valid"
				delay := headerDuration(seconds)
				if retry := now.Add(delay); retry.After(deadline) {
					deadline = retry
				}
			}
		}
	}
	if deadline.IsZero() {
		deadline = now.Add(time.Minute)
	}
	if deadline.After(c.cooldownUntil) {
		c.cooldownUntil = deadline
	}
	if limited && c.cooldownUntil.After(now.Add(maxRetryAfter)) {
		return "exceeds_bound", fmt.Errorf("AniList cooldown exceeds %s", maxRetryAfter)
	}
	return state, nil
}

func headerDuration(seconds int64) time.Duration {
	if seconds > int64((1<<63-1)/time.Second) {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(seconds) * time.Second
}

// FetchYear returns all anime (all seasons, all formats) for the given year.
// Paginates through AniList's 50-per-page limit until all pages are fetched.
func (c *Client) FetchYear(ctx context.Context, year int) ([]Show, error) {
	var allShows []Show
	for page := 1; page <= maxPages; page += defaultBatchSize {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		payload := map[string]any{
			"query": yearQueryTemplate,
			"variables": map[string]any{
				"y":       year,
				"p0":      page,
				"p1":      page + 1,
				"p2":      page + 2,
				"p3":      page + 3,
				"perPage": maxPerPage,
			},
		}

		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal payload: %w", err)
		}

		started := c.now()
		slog.Debug("AniList batch started", "type", "fetch", "task", "year_fetch",
			"year", year, "batch_start", page, "batch_end", page+defaultBatchSize-1)
		var resp graphqlResponse
		if err := c.doRequest(ctx, body, &resp, year, page); err != nil {
			return nil, fmt.Errorf("fetch year %d (page %d): %w", year, page, err)
		}

		if len(resp.Errors) > 0 {
			return nil, fmt.Errorf("AniList GraphQL response contains %d errors", len(resp.Errors))
		}

		pages := [defaultBatchSize]*mediaPage{resp.Data.Page0, resp.Data.Page1, resp.Data.Page2, resp.Data.Page3}
		terminal := defaultBatchSize
		for i, p := range pages {
			if p == nil || p.PageInfo == nil || p.PageInfo.HasNextPage == nil || p.PageInfo.CurrentPage != page+i || p.Media == nil {
				return nil, fmt.Errorf("fetch year %d (page %d): incomplete or mismatched AniList page", year, page+i)
			}
			if i > terminal && (*p.PageInfo.HasNextPage || len(p.Media) != 0) {
				return nil, fmt.Errorf("fetch year %d (page %d): inconsistent terminal AniList page", year, page+i)
			}
			if !*p.PageInfo.HasNextPage && terminal == defaultBatchSize {
				terminal = i
			}
		}
		for i, p := range pages {
			if i > terminal {
				break
			}
			allShows = append(allShows, p.Media...)
			slog.Debug("AniList page fetched", "type", "fetch", "task", "year_fetch",
				"year", year, "page", page+i, "shows", len(p.Media), "total_shows", len(allShows),
				"has_next_page", *p.PageInfo.HasNextPage)
		}
		slog.Debug("AniList batch fetched", "type", "fetch", "task", "year_fetch", "outcome", "completed",
			"year", year, "batch_start", page, "batch_end", page+defaultBatchSize-1,
			"pages", min(terminal+1, defaultBatchSize), "elapsed_ms", c.now().Sub(started).Milliseconds())
		if terminal < defaultBatchSize {
			return allShows, nil
		}
	}
	return nil, fmt.Errorf("fetch year %d exceeded max pages %d", year, maxPages)
}

// doRequest sends a POST request with retries and exponential backoff.
func (c *Client) doRequest(ctx context.Context, payload []byte, dst any, year, page int) error {
	var lastErr error
	var attempts, retryStatus int
	var retryReason, retryAfterState string
	for attempt := range maxRetry {
		var retryAt time.Time
		if attempt > 0 {
			// Exponential backoff: 2s, 4s, 8s, 16s (+ jitter).
			delay := jitter(time.Duration(1<<attempt) * time.Second)
			if ctx.Err() == nil {
				slog.Warn("AniList request retry scheduled", "type", "fetch", "task", "year_fetch", "outcome", "retrying",
					"year", year, "page", page, "attempt", attempts, "next_attempt", attempt+1, "max_attempts", maxRetry,
					"reason", retryReason, "status", retryStatus, "retry_in_ms", delay.Milliseconds(),
					"retry_after", retryAfterState, "action", "retry", "consequence", "year_fetch_delayed")
			}
			retryAt = c.now().Add(delay)
		}

		if err := c.throttle(ctx, retryAt); err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL,
			bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "sonarr-anime-bridge/1.0")

		attempts++
		slog.Debug("AniList HTTP attempt", "type", "fetch", "task", "year_fetch",
			"year", year, "batch_start", page, "attempt", attempts)
		retryStatus = 0
		retryAfterState = "not_applicable"
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = fmt.Errorf("AniList HTTP request failed")
			retryReason = "network_error"
			continue
		}
		retryStatus = resp.StatusCode
		var allowanceErr error
		retryAfterState, allowanceErr = c.observeAllowance(resp)
		if allowanceErr != nil {
			resp.Body.Close()
			return allowanceErr
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			retryReason = "rate_limited"
			lastErr = fmt.Errorf("rate limited (attempt %d)", attempt+1)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
			resp.Body.Close()
			if readErr != nil {
				lastErr = fmt.Errorf("API error (HTTP %d): failed to read response body: %w", resp.StatusCode, readErr)
			} else {
				lastErr = fmt.Errorf("API error (HTTP %d)", resp.StatusCode)
			}
			// Client errors (4xx except 429) won't self-heal; break.
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				break
			}
			retryReason = "http_error"
			continue
		}

		err = decodeResponse(resp.Body, dst)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("decode response: %w", err)
			// Malformed JSON won't self-heal on retry; break.
			break
		}

		slog.Debug("AniList HTTP batch completed", "type", "fetch", "task", "year_fetch",
			"year", year, "batch_start", page, "http_attempts", attempts, "retries", attempts-1)
		return nil
	}

	return fmt.Errorf("giving up after %d attempts: %w", attempts, lastErr)
}

func decodeResponse(body io.Reader, dst any) error {
	bounded := &io.LimitedReader{R: body, N: maxResponseBodySize + 1}
	decoder := json.NewDecoder(bounded)
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	err := decoder.Decode(&trailing)
	if bounded.N == 0 {
		return fmt.Errorf("response exceeds %d bytes", maxResponseBodySize)
	}
	if err != io.EOF {
		return fmt.Errorf("unexpected trailing response data: %v", err)
	}
	return nil
}
