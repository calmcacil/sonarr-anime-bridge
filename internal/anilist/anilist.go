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

	"golang.org/x/time/rate"
)

const (
	apiBase          = "https://graphql.anilist.co"
	maxRetry         = 5
	rateLimitDelay   = 700 * time.Millisecond
	rateLimitBackoff = 5 * time.Second
	maxPerPage       = 50
	maxErrorBodySize = 64 << 10
)

// Tag represents an AniList content tag with name and relevance rank.
type Tag struct {
	Name string `json:"name"`
}

// RelationEdge represents a related media entry.
type RelationEdge struct {
	Node         RelationNode `json:"node"`
	RelationType string       `json:"relationType"`
}

// RelationNode holds minimal data for a related media entry.
type RelationNode struct {
	ID    int   `json:"id"`
	IDMal *int  `json:"idMal"`
	Title Title `json:"title"`
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
	Episodes  *int           `json:"episodes"`
	Duration  *int           `json:"duration"`
	Genres    []string       `json:"genres"`
	Tags      []Tag          `json:"tags"`
	Status    string         `json:"status"`
	Season    string         `json:"season"`
	StartDate FuzzyDate      `json:"startDate"`
	Relations *RelationBlock `json:"relations,omitempty"`
}

// IsSeries returns true if the show is a series (TV, ONA) rather than a movie (MOVIE, OVA, SPECIAL).
func (s Show) IsSeries() bool {
	return s.Format == "TV" || s.Format == "ONA"
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
	lower := strings.ToLower(name)
	for _, t := range s.Tags {
		if strings.ToLower(t.Name) == lower {
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
	month := *s.StartDate.Month
	if month < 1 || month > 12 {
		return true
	}
	start := time.Date(*s.StartDate.Year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	return !start.After(time.Now().AddDate(0, months, 0))
}

// IsWinterStart returns true if the show's start date month falls within
// the winter anime season (December through March). Shows with unknown
// start dates are kept (returns true) since they cannot be ruled out.
// AniList's winter season spans December of the previous calendar year
// through March of the current year, so months 12, 1, 2, and 3 are valid.
func (s Show) IsWinterStart() bool {
	if s.StartDate.Month == nil {
		return true
	}
	m := *s.StartDate.Month
	return m == 12 || m == 1 || m == 2 || m == 3
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

// yearQueryTemplate fetches all anime for a given year regardless of season,
// across all formats. Local filtering applies the per-season and per-format
// filters on-the-fly from the cached data.
const yearQueryTemplate = `query($y: Int, $page: Int, $perPage: Int) {
	Page(page: $page, perPage: $perPage) {
		pageInfo {
			hasNextPage
			currentPage
		}
		media(
			seasonYear: $y,
			type: ANIME,
			sort: POPULARITY_DESC
		) {
			id
			idMal
			title { romaji english }
			format
			episodes
			duration
			genres
			tags { name }
			status
			season
			startDate { year month day }
			relations {
				edges {
					node { id idMal title { romaji english } }
					relationType
				}
			}
		}
	}
}`

type graphqlError struct {
	Message string `json:"message"`
}

// pageInfo holds pagination metadata from AniList.
type pageInfo struct {
	HasNextPage bool `json:"hasNextPage"`
	CurrentPage int  `json:"currentPage"`
}

// graphqlResponse is the top-level response from AniList.
type graphqlResponse struct {
	Data struct {
		Page struct {
			PageInfo pageInfo `json:"pageInfo"`
			Media    []Show   `json:"media"`
		} `json:"Page"`
	} `json:"data"`
	Errors []graphqlError `json:"errors,omitempty"`
}

// Client fetches data from the AniList GraphQL API.
type Client struct {
	http    *http.Client
	limiter *rate.Limiter

	rateLimitMu   sync.Mutex
	lastRateLimit time.Time
}

// New creates a new AniList client with a 30-second HTTP timeout.
func New() *Client {
	return NewWithTimeout(30 * time.Second)
}

// NewWithTimeout creates a new AniList client with the given HTTP timeout.
func NewWithTimeout(timeout time.Duration) *Client {
	return &Client{
		http:    &http.Client{Timeout: timeout},
		limiter: rate.NewLimiter(rate.Every(rateLimitDelay), 1),
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

// throttle ensures we don't exceed AniList rate limits using a token-bucket
// rate limiter. After a 429 response, the limit is tightened to 5s between
// requests for 30 seconds. The rate.Limiter is inherently goroutine-safe.
func (c *Client) throttle(ctx context.Context) error {
	c.rateLimitMu.Lock()
	limit := rate.Every(rateLimitDelay)
	if time.Since(c.lastRateLimit) < 30*time.Second {
		limit = rate.Every(rateLimitBackoff)
	}
	c.limiter.SetLimit(limit)
	c.rateLimitMu.Unlock()

	return c.limiter.Wait(ctx)
}

// FetchYear returns all anime (all seasons, all formats) for the given year.
// Paginates through AniList's 50-per-page limit until all pages are fetched.
func (c *Client) FetchYear(ctx context.Context, year int) ([]Show, error) {
	var allShows []Show
	page := 1

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		payload := map[string]any{
			"query": yearQueryTemplate,
			"variables": map[string]any{
				"y":       year,
				"page":    page,
				"perPage": maxPerPage,
			},
		}

		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal payload: %w", err)
		}

		var resp graphqlResponse
		if err := c.doRequest(ctx, body, &resp); err != nil {
			return nil, fmt.Errorf("fetch year %d (page %d): %w", year, page, err)
		}

		if len(resp.Errors) > 0 {
			msgs := make([]string, len(resp.Errors))
			for i, e := range resp.Errors {
				msgs[i] = e.Message
			}
			return nil, fmt.Errorf("AniList GraphQL errors: %s", strings.Join(msgs, "; "))
		}

		shows := resp.Data.Page.Media
		if shows == nil {
			shows = []Show{}
		}
		allShows = append(allShows, shows...)

		if !resp.Data.Page.PageInfo.HasNextPage {
			break
		}

		page++
	}

	return allShows, nil
}

// doRequest sends a POST request with retries and exponential backoff.
func (c *Client) doRequest(ctx context.Context, payload []byte, dst any) error {
	var lastErr error
	for attempt := range maxRetry {
		if attempt > 0 {
			// Exponential backoff: 2s, 4s, 8s, 16s (+ jitter)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(jitter(time.Duration(1<<attempt) * time.Second)):
			}
		}

		if err := c.throttle(ctx); err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase,
			bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "sonarr-anime-bridge/1.0")

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("http request: %w", err)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			c.rateLimitMu.Lock()
			c.lastRateLimit = time.Now()
			c.rateLimitMu.Unlock()
			retryAfter := resp.Header.Get("Retry-After")
			resp.Body.Close()
			if retryAfter != "" {
				if sec, err := strconv.Atoi(retryAfter); err == nil && sec > 0 {
					slog.Warn("rate limited, waiting retry-after", "seconds", sec)
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(time.Duration(sec) * time.Second):
					}
				}
			}
			lastErr = fmt.Errorf("rate limited (attempt %d)", attempt+1)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
			resp.Body.Close()
			if readErr != nil {
				lastErr = fmt.Errorf("API error (HTTP %d): failed to read response body: %w", resp.StatusCode, readErr)
			} else {
				lastErr = fmt.Errorf("API error (HTTP %d): %s", resp.StatusCode, string(respBody))
			}
			// Client errors (4xx except 429) won't self-heal; break.
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				break
			}
			continue
		}

		err = json.NewDecoder(resp.Body).Decode(dst)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("decode response: %w", err)
			// Malformed JSON won't self-heal on retry; break.
			break
		}

		return nil
	}

	return fmt.Errorf("giving up after %d retries: %w", maxRetry, lastErr)
}
