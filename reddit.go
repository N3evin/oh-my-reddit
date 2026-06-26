package main

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rivo/uniseg"
)

// oh-my-reddit reads Reddit's public Atom feeds — no OAuth, no app, no
// credentials. Reddit gates its JSON/OAuth API hard now, but the .rss feeds
// still serve anonymously; they just rate-limit (429), which fetchBytes
// rides out with simple backoff.
//
// A realistic browser User-Agent gets served where bot-looking ones get 403.
const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

var httpClient = &http.Client{Timeout: 15 * time.Second}

var (
	tagRe        = regexp.MustCompile(`<[^>]*>`)
	wsRe         = regexp.MustCompile(`\s+`)
	blankLinesRe = regexp.MustCompile(`\n{3,}`)
)

// --- Atom parsing ----------------------------------------------------------

type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID      string `xml:"id"`
	Title   string `xml:"title"`
	Updated string `xml:"updated"`
	Content string `xml:"content"`
	Author  struct {
		Name string `xml:"name"`
	} `xml:"author"`
	Links []struct {
		Href string `xml:"href,attr"`
		Rel  string `xml:"rel,attr"`
	} `xml:"link"`
}

// cleanContent turns a comment's escaped HTML into plain text.
func cleanContent(s string) string {
	s = tagRe.ReplaceAllString(s, " ") // drop tags (already XML-unescaped)
	s = html.UnescapeString(s)         // decode &#39; &amp; etc.
	s = wsRe.ReplaceAllString(s, " ")
	return stripControl(strings.TrimSpace(s))
}

// stripControl removes terminal control bytes (C0/C1, except newline and tab)
// from untrusted text — titles, comment bodies, usernames — so a crafted post
// can't smuggle ANSI/OSC escape sequences onto the screen when we render it.
// Note: html.UnescapeString above can DECODE &#27; into a real ESC, so this must
// run after any entity decoding.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return -1 // drop C0 and C1 controls (incl. ESC, BEL, 8-bit CSI/ST)
		default:
			return r
		}
	}, s)
}

// parseTime parses an RFC3339 timestamp, returning the zero time on failure
// rather than time.Now(): a bad timestamp sorts as oldest (and so won't be
// mistaken for a just-arrived comment) instead of fabricating "now".
func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// --- HTTP with backoff -----------------------------------------------------

// cachedResp lets us make conditional requests: Reddit returns 304 (no body)
// when nothing changed, which is cheap and much less likely to be throttled.
type cachedResp struct {
	etag    string
	lastMod string
	body    []byte
}

var (
	cacheMu   sync.Mutex
	respCache = map[string]cachedResp{}
)

// fetchBytes GETs url with conditional caching, honoring Retry-After on 429/5xx
// and falling back to jittered backoff.
func fetchBytes(rawURL string) ([]byte, error) {
	cacheMu.Lock()
	cached := respCache[rawURL]
	cacheMu.Unlock()

	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff(attempt))
		}

		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", browserUA)
		req.Header.Set("Accept", "application/atom+xml, text/xml, */*")
		// Ride your logged-in browser session for its higher rate budget. The
		// cookie is read fresh each attempt so a mid-flight renewal takes effect.
		if cookie := currentCookie(); cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		if cached.etag != "" {
			req.Header.Set("If-None-Match", cached.etag)
		}
		if cached.lastMod != "" {
			req.Header.Set("If-Modified-Since", cached.lastMod)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusNotModified:
			return cached.body, nil // unchanged since last poll
		case resp.StatusCode == http.StatusOK:
			if readErr != nil {
				return nil, readErr
			}
			cached = cachedResp{
				etag:    resp.Header.Get("ETag"),
				lastMod: resp.Header.Get("Last-Modified"),
				body:    body,
			}
			cacheMu.Lock()
			respCache[rawURL] = cached
			cacheMu.Unlock()
			return body, nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			if d := retryAfter(resp.Header.Get("Retry-After")); d > 0 {
				time.Sleep(d)
			}
			lastErr = fmt.Errorf("reddit busy (%s)", resp.Status)
			continue
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
			// Likely a missing/stale session — try to (re)acquire one from the
			// browser and retry immediately; otherwise surface the error.
			if changed, _ := renewCookie(); changed {
				continue // silently rode a fresh browser session; retry
			}
			return nil, errAuth // expired and unrenewable — prompt re-auth
		case resp.StatusCode == http.StatusNotFound:
			return nil, errNotFound // callers turn this into a friendly message
		default:
			return nil, fmt.Errorf("reddit returned %s", resp.Status)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("request failed")
	}
	return nil, lastErr
}

// backoff returns a jittered, growing delay capped at 8s.
func backoff(attempt int) time.Duration {
	base := time.Duration(attempt) * 1500 * time.Millisecond
	if base > 8*time.Second {
		base = 8 * time.Second
	}
	jitter := time.Duration(time.Now().UnixNano()%500) * time.Millisecond
	return base + jitter
}

// retryAfter parses a Retry-After header (seconds or HTTP-date), capped at 30s.
func retryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil {
		d := time.Duration(secs) * time.Second
		if d > 30*time.Second {
			d = 30 * time.Second
		}
		return d
	}
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d > 30*time.Second {
			d = 30 * time.Second
		}
		if d > 0 {
			return d
		}
	}
	return 0
}

// postForm POSTs form-encoded data with the same retry/backoff as fetchBytes.
func postForm(rawURL string, form url.Values) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff(attempt))
		}
		req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", browserUA)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		if cookie := currentCookie(); cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			if readErr != nil {
				return nil, readErr
			}
			return body, nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			if d := retryAfter(resp.Header.Get("Retry-After")); d > 0 {
				time.Sleep(d)
			}
			lastErr = fmt.Errorf("reddit busy (%s)", resp.Status)
			continue
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
			if changed, _ := renewCookie(); changed {
				continue
			}
			return nil, errAuth
		case resp.StatusCode == http.StatusNotFound:
			return nil, errNotFound
		default:
			return nil, fmt.Errorf("reddit returned %s", resp.Status)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("request failed")
	}
	return nil, lastErr
}

// --- public API ------------------------------------------------------------

func cleanSubreddit(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "/")
	s = strings.TrimPrefix(s, "r/")
	return strings.Trim(s, "/ ")
}

// rssURL turns a thread URL or bare path into its Atom feed URL.
func rssURL(raw string) string {
	path := strings.TrimSpace(raw)
	if u, err := url.Parse(path); err == nil && u.Host != "" {
		path = u.Path
	}
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".rss")
	path = strings.TrimSuffix(path, "/")
	return "https://www.reddit.com" + path + "/.rss"
}

// jsonListing models a subreddit listing (any sort order). JSON (unlike RSS)
// exposes stickied posts and comment counts.
type jsonListing struct {
	Data struct {
		After    string `json:"after"` // pagination cursor; empty when done
		Children []struct {
			Data struct {
				ID          string `json:"id"`
				Title       string `json:"title"`
				Permalink   string `json:"permalink"`
				Author      string `json:"author"`
				NumComments int    `json:"num_comments"`
				Stickied    bool   `json:"stickied"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

// threadsResult is one page of a subreddit listing plus the cursor for the next.
type threadsResult struct {
	threads []thread
	after   string
}

// errNotFound is a 404 from reddit, surfaced so callers can show a friendly,
// context-specific message instead of a raw status line.
var errNotFound = errors.New("not found")

// errAuth is a 401/403 that a browser-cookie refresh couldn't fix — the reddit
// session has expired. Callers route this to the connect screen for re-auth.
var errAuth = errors.New("session expired")

// listSort is a subreddit listing order. Its string value is the Reddit URL
// path segment ("hot", "new", …), so the type doubles as that path component.
type listSort string

const (
	sortHot    listSort = "hot"
	sortNew    listSort = "new"
	sortRising listSort = "rising"
	sortTop    listSort = "top"
)

// listSorts are the orders we cycle through (←/→ on the list screen). "best" is
// deliberately absent — it's the logged-in home-feed sort, not a subreddit
// listing, so Reddit ignores it for /r/<sub>.
var listSorts = []listSort{sortHot, sortNew, sortRising, sortTop}

// fetchThreads lists a subreddit's posts in the given sort order. It prefers
// JSON (pins + comment counts) and falls back to RSS when JSON is blocked.
func fetchThreads(subreddit string, sort listSort) (threadsResult, error) {
	sub := cleanSubreddit(subreddit)
	if page, err := fetchThreadsJSON(sub, sort, ""); err == nil && len(page.threads) > 0 {
		return page, nil
	}
	ts, err := fetchThreadsRSS(sub, sort)
	if errors.Is(err, errNotFound) {
		return threadsResult{}, fmt.Errorf("no subreddit called r/%s", sub)
	}
	return threadsResult{threads: ts}, err
}

// fetchThreadsPage loads the next page of a subreddit listing using Reddit's
// "after" cursor. JSON only — RSS has no pagination.
func fetchThreadsPage(subreddit string, sort listSort, after string) (threadsResult, error) {
	sub := cleanSubreddit(subreddit)
	return fetchThreadsJSON(sub, sort, after)
}

// threadsJSONURL builds a subreddit listing endpoint for the given sort. "top"
// needs a time window; we default to the day so it tracks what's big right now.
func threadsJSONURL(sub string, sort listSort, after string) string {
	u := "https://www.reddit.com/r/" + sub + "/" + string(sort) + ".json?limit=100&raw_json=1"
	if sort == sortTop {
		u += "&t=day"
	}
	if after != "" {
		u += "&after=" + url.QueryEscape(after)
	}
	return u
}

func fetchThreadsJSON(sub string, sort listSort, after string) (threadsResult, error) {
	body, err := fetchBytes(threadsJSONURL(sub, sort, after))
	if err != nil {
		return threadsResult{}, err
	}
	ts, next, err := parseThreadsJSON(body, after == "")
	if err != nil {
		return threadsResult{}, err
	}
	return threadsResult{threads: ts, after: next}, nil
}

// parseThreadsJSON turns a subreddit listing into threads. On the first page
// (pinStickies), stickied posts float to the top; later pages keep Reddit order.
func parseThreadsJSON(body []byte, pinStickies bool) ([]thread, string, error) {
	var l jsonListing
	if err := json.Unmarshal(body, &l); err != nil {
		return nil, "", err
	}
	out := make([]thread, 0, len(l.Data.Children))
	for _, c := range l.Data.Children {
		d := c.Data
		out = append(out, thread{
			id:          d.ID,
			title:       stripControl(strings.TrimSpace(d.Title)),
			permalink:   "https://www.reddit.com" + d.Permalink,
			author:      stripControl(d.Author),
			numComments: d.NumComments,
			stickied:    d.Stickied,
		})
	}
	if len(out) == 0 {
		return nil, "", fmt.Errorf("empty listing")
	}
	if pinStickies {
		sort.SliceStable(out, func(i, j int) bool { return out[i].stickied && !out[j].stickied })
	}
	return out, l.Data.After, nil
}

// threadsRSSURL builds the Atom feed for a subreddit listing. Hot is the bare
// feed; other sorts live under their own path (top carries a time window).
func threadsRSSURL(sub string, sort listSort) string {
	if sort == sortHot {
		return "https://www.reddit.com/r/" + sub + "/.rss"
	}
	u := "https://www.reddit.com/r/" + sub + "/" + string(sort) + "/.rss"
	if sort == sortTop {
		u += "?t=day"
	}
	return u
}

func fetchThreadsRSS(sub string, sort listSort) ([]thread, error) {
	body, err := fetchBytes(threadsRSSURL(sub, sort))
	if err != nil {
		return nil, err
	}

	var feed atomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("could not parse r/%s feed", sub)
	}

	out := make([]thread, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		if !strings.HasPrefix(e.ID, "t3_") {
			continue
		}
		out = append(out, thread{
			id:        strings.TrimPrefix(e.ID, "t3_"),
			title:     stripControl(strings.TrimSpace(e.Title)),
			permalink: entryLink(e),
			author:    stripControl(strings.TrimPrefix(e.Author.Name, "/u/")),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no threads found in r/%s", sub)
	}
	return out, nil
}

// jsonComment models a node in the comment tree. Replies is "" (string) when
// empty, or a nested listing object — hence RawMessage. "more" stubs carry
// child IDs to expand via /api/morechildren.
type jsonComment struct {
	Kind string `json:"kind"`
	Data struct {
		ID         string          `json:"id"`
		Author     string          `json:"author"`
		Body       string          `json:"body"`
		Score      int             `json:"score"`
		CreatedUTC float64         `json:"created_utc"`
		ParentID   string          `json:"parent_id"`
		Replies    json.RawMessage `json:"replies"`
		Children   []string        `json:"children"` // "more" stub: comment IDs to fetch
	} `json:"data"`
}

type jsonCommentListing struct {
	Data struct {
		Children []jsonComment `json:"children"`
	} `json:"data"`
}

// jsonPostListing models listings[0] of a thread response: the OP submission
// itself (title, self-text, score), which we surface in the OP modal.
type jsonPostListing struct {
	Data struct {
		Children []struct {
			Data struct {
				ID         string  `json:"id"`
				Title      string  `json:"title"`
				Author     string  `json:"author"`
				Selftext   string  `json:"selftext"`
				Score      int     `json:"score"`
				CreatedUTC float64 `json:"created_utc"`
				URL        string  `json:"url"`
				IsSelf     bool    `json:"is_self"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

// parsePost pulls the OP out of listings[0]. Returns nil if it's absent or
// malformed — the feed still works without it.
func parsePost(raw json.RawMessage) *post {
	var pl jsonPostListing
	if json.Unmarshal(raw, &pl) != nil || len(pl.Data.Children) == 0 {
		return nil
	}
	d := pl.Data.Children[0].Data
	p := &post{
		title:    stripControl(strings.TrimSpace(d.Title)),
		author:   stripControl(d.Author),
		body:     cleanSelftext(d.Selftext),
		score:    d.Score,
		hasScore: true,
		postedAt: time.Unix(int64(d.CreatedUTC), 0),
	}
	if !d.IsSelf {
		p.link = strings.TrimSpace(d.URL) // external destination for link posts
	}
	return p
}

// cleanSelftext decodes entities and collapses runs of blank lines, but keeps
// paragraph breaks so the modal body reads like the original post.
func cleanSelftext(s string) string {
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = blankLinesRe.ReplaceAllString(s, "\n\n")
	return stripControl(strings.TrimSpace(s))
}

// emojiToken is the fixed-width slot we reserve for every emoji while glamour
// lays out (tables, wrapping). glamour mis-sizes wide glyphs but handles ASCII
// exactly, so we reserve two Private-Use runes — each width-1 to glamour, two
// cells total, matching how the terminal draws an emoji — then drop the real
// glyph back into that slot. PUA runes never appear in reddit text, so restore
// can't mis-target.
const emojiToken = ""

// reserveEmoji replaces each emoji grapheme cluster with emojiToken, returning
// the rewritten text and the emoji pulled out, in document order. Detection is
// generic (Unicode block, never a hardcoded list); text arrows (↑ ↓ → ↳) stay.
func reserveEmoji(s string) (string, []string) {
	var b strings.Builder
	b.Grow(len(s))
	var picked []string
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		if isEmojiCluster(g.Runes()) {
			picked = append(picked, g.Str())
			b.WriteString(emojiToken)
		} else {
			b.WriteString(g.Str())
		}
	}
	return b.String(), picked
}

// restoreEmoji swaps each token back to its emoji, left to right. A 2-cell emoji
// fills the same slot the token held, so glamour's column math still holds.
func restoreEmoji(s string, picked []string) string {
	for _, e := range picked {
		i := strings.Index(s, emojiToken)
		if i < 0 {
			break
		}
		s = s[:i] + e + s[i+len(emojiToken):]
	}
	return s
}

// isEmojiCluster reports whether a grapheme cluster is an emoji, by testing its
// runes against the Unicode blocks emoji live in. The whole cluster (incl. ZWJ
// sequences and skin tones) is handled as one unit.
func isEmojiCluster(runes []rune) bool {
	for _, r := range runes {
		switch {
		case r >= 0x1F000 && r <= 0x1FAFF, // emoji & pictographic supplements
			r >= 0x2600 && r <= 0x27BF, // misc symbols + dingbats
			r >= 0x2B00 && r <= 0x2BFF, // misc symbols & arrows (stars…)
			r >= 0x2300 && r <= 0x23FF, // misc technical (⌚ ⏱ ⏰…)
			r == 0x20E3,                // combining enclosing keycap
			r == 0x2049 || r == 0x203C: // ‼ ⁉
			return true
		}
	}
	return false
}

const commentFetchLimit = 100

// jsonURL builds the .json endpoint for a thread. sort=new returns the freshest
// comments first; limit caps each fetch so huge threads stay cheap.
func jsonURL(raw string) string {
	path := strings.TrimSpace(raw)
	if u, err := url.Parse(path); err == nil && u.Host != "" {
		path = u.Path
	}
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".json")
	path = strings.TrimSuffix(path, ".rss")
	path = strings.TrimSuffix(path, "/")
	return fmt.Sprintf("https://www.reddit.com%s.json?sort=new&limit=%d&raw_json=1", path, commentFetchLimit)
}

// fetchUsernameFor returns the reddit username for a given Cookie header (via
// api/me.json) and whether reddit was actually reached. reached is false only on
// a transport error, so callers can tell "definitely anonymous" (reached, name
// "") apart from "couldn't check" (a network blip) and not force re-auth on the
// latter.
func fetchUsernameFor(cookie string) (name string, reached bool) {
	if strings.TrimSpace(cookie) == "" {
		return "", true // no cookie is definitively anonymous
	}
	req, err := http.NewRequest(http.MethodGet, "https://www.reddit.com/api/me.json", nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Cookie", cookie)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", false // couldn't reach reddit — unknown, not "anonymous"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 401/403 = a definitive "not logged in". 429/5xx = rate-limited or down,
		// so we can't tell — report unreached so a busy server isn't mistaken for
		// an expired session (which would wrongly boot a logged-in user).
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", true
		}
		return "", false
	}
	var me struct {
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&me) != nil {
		return "", true
	}
	return me.Data.Name, true
}

// fetchUsername resolves the username for the currently-active cookie.
func fetchUsername() (string, bool) { return fetchUsernameFor(currentCookie()) }

// threadComments is the parsed result of a thread fetch, including pagination hints.
type threadComments struct {
	post     *post
	comments []comment
	postID   string   // "t3_…" for morechildren
	moreIDs  []string // child IDs from "more" stubs
}

// fetchComments pulls a thread's OP and comments, preferring JSON (score +
// parent_id for threading, plus the OP submission) and falling back to RSS when
// JSON is blocked. The post is nil on the RSS path (RSS carries no OP body).
func fetchComments(threadRef string) (threadComments, error) {
	if tc, err := fetchCommentsJSON(threadRef); err == nil && len(tc.comments) > 0 {
		return tc, nil
	}
	cs, err := fetchCommentsRSS(threadRef)
	if errors.Is(err, errNotFound) {
		return threadComments{}, fmt.Errorf("that thread isn't on reddit anymore (it may have been deleted)")
	}
	return threadComments{comments: cs}, err
}

func fetchCommentsJSON(threadRef string) (threadComments, error) {
	body, err := fetchBytes(jsonURL(threadRef))
	if err != nil {
		return threadComments{}, err
	}
	return parseThreadJSON(body)
}

const moreChildrenBatch = 20

// fetchMoreComments expands a batch of "more" stub child IDs via Reddit's
// morechildren endpoint. fetched lists the child IDs consumed; nestedMore carries
// any new "more" stubs returned in the response.
func fetchMoreComments(postID string, moreIDs []string) (comments []comment, remaining, fetched, nestedMore []string, err error) {
	if postID == "" || len(moreIDs) == 0 {
		return nil, moreIDs, nil, nil, nil
	}
	batch := moreIDs
	if len(batch) > moreChildrenBatch {
		batch = batch[:moreChildrenBatch]
	}
	remaining = moreIDs[len(batch):]
	fetched = append([]string(nil), batch...)

	form := url.Values{
		"link_id":  {postID},
		"children": {strings.Join(batch, ",")},
		"sort":     {"new"},
		"api_type": {"json"},
	}
	body, err := postForm("https://www.reddit.com/api/morechildren.json", form)
	if err != nil {
		return nil, moreIDs, nil, nil, err
	}
	comments, nestedMore, err = parseMoreChildrenJSON(body)
	if err != nil {
		return nil, moreIDs, nil, nil, err
	}
	sortCommentsNewestFirst(comments)
	return comments, remaining, fetched, nestedMore, nil
}

// parseMoreChildrenJSON extracts comments and nested "more" child IDs from a
// morechildren API response.
func parseMoreChildrenJSON(body []byte) ([]comment, []string, error) {
	var resp struct {
		JSON struct {
			Data struct {
				Things []jsonComment `json:"things"`
			} `json:"data"`
		} `json:"json"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil, err
	}
	var out []comment
	var nested []string
	for _, c := range resp.JSON.Data.Things {
		if c.Kind == "more" {
			nested = append(nested, c.Data.Children...)
			continue
		}
		if c.Kind != "t1" {
			continue
		}
		if cm := commentFromJSON(c); cm != nil {
			out = append(out, *cm)
		}
	}
	return out, nested, nil
}

func commentFromJSON(c jsonComment) *comment {
	b := stripControl(strings.TrimSpace(c.Data.Body))
	if b == "" || b == "[deleted]" || b == "[removed]" {
		return nil
	}
	return &comment{
		id:       "t1_" + c.Data.ID,
		author:   stripControl(c.Data.Author),
		body:     b,
		score:    c.Data.Score,
		hasScore: true,
		parentID: c.Data.ParentID,
		postedAt: time.Unix(int64(c.Data.CreatedUTC), 0),
	}
}

func sortCommentsNewestFirst(cs []comment) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].postedAt.After(cs[j].postedAt) })
}

// parseThreadJSON turns a thread response into its OP (listings[0]) and comments
// (listings[1]), walking the reply tree and collecting "more" stubs for pagination.
func parseThreadJSON(body []byte) (threadComments, error) {
	var listings []json.RawMessage
	if err := json.Unmarshal(body, &listings); err != nil {
		return threadComments{}, err
	}
	if len(listings) < 2 {
		return threadComments{}, fmt.Errorf("unexpected response shape")
	}
	op := parsePost(listings[0])
	postID := postIDFromListing(listings[0])
	var cl jsonCommentListing
	if err := json.Unmarshal(listings[1], &cl); err != nil {
		return threadComments{}, err
	}

	var out []comment
	var moreIDs []string
	var walk func(children []jsonComment)
	walk = func(children []jsonComment) {
		for _, c := range children {
			if c.Kind == "more" {
				moreIDs = append(moreIDs, c.Data.Children...)
				continue
			}
			if c.Kind != "t1" {
				continue
			}
			if cm := commentFromJSON(c); cm != nil {
				out = append(out, *cm)
			}
			if len(c.Data.Replies) > 0 && c.Data.Replies[0] == '{' {
				var rep jsonCommentListing
				if json.Unmarshal(c.Data.Replies, &rep) == nil {
					walk(rep.Data.Children)
				}
			}
		}
	}
	walk(cl.Data.Children)

	sortCommentsNewestFirst(out)
	return threadComments{post: op, comments: out, postID: postID, moreIDs: moreIDs}, nil
}

// postIDFromListing extracts the t3_ fullname from listings[0].
func postIDFromListing(raw json.RawMessage) string {
	var pl jsonPostListing
	if json.Unmarshal(raw, &pl) != nil || len(pl.Data.Children) == 0 {
		return ""
	}
	id := pl.Data.Children[0].Data.ID
	if id == "" {
		return ""
	}
	return "t3_" + id
}

// fetchCommentsRSS pulls a thread's comments via RSS, oldest-first.
func fetchCommentsRSS(threadRef string) ([]comment, error) {
	body, err := fetchBytes(rssURL(threadRef))
	if err != nil {
		return nil, err
	}

	var feed atomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("could not parse thread feed")
	}

	out := make([]comment, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		if !strings.HasPrefix(e.ID, "t1_") {
			continue // skip the post entry itself (t3_)
		}
		body := cleanContent(e.Content)
		if body == "" {
			continue
		}
		out = append(out, comment{
			id:       e.ID,
			author:   stripControl(strings.TrimPrefix(e.Author.Name, "/u/")),
			body:     body, // already stripped by cleanContent
			postedAt: parseTime(e.Updated),
		})
	}

	// RSS order isn't guaranteed; sort newest-first for display.
	sortCommentsNewestFirst(out)
	return out, nil
}

// entryLink returns the human (alternate) link for an entry.
func entryLink(e atomEntry) string {
	for _, l := range e.Links {
		if l.Rel == "alternate" || l.Rel == "" {
			return l.Href
		}
	}
	if len(e.Links) > 0 {
		return e.Links[0].Href
	}
	return ""
}
