package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFuzzyFilter(t *testing.T) {
	threads := []thread{
		{title: "random other thread"},
		{title: "Match Thread: France vs Senegal"},
		{title: "match preview"},
	}
	got := fuzzyFilter(threads, "match")
	for _, th := range got {
		if th.title == "random other thread" {
			t.Error("non-matching thread should be filtered out")
		}
	}
	// A contiguous substring match outranks a scattered subsequence match.
	ranked := fuzzyFilter([]thread{
		{title: "m-a-t-c-h scattered"},
		{title: "match contiguous"},
	}, "match")
	if len(ranked) != 2 || ranked[0].title != "match contiguous" {
		t.Errorf("contiguous match should rank first, got %v", ranked)
	}
}

func TestParseThreadsJSONStickyStability(t *testing.T) {
	body := []byte(`{"data":{"children":[
		{"data":{"id":"1","title":"sticky A","permalink":"/a","stickied":true}},
		{"data":{"id":"2","title":"hot X","permalink":"/x","stickied":false}},
		{"data":{"id":"3","title":"sticky B","permalink":"/b","stickied":true}},
		{"data":{"id":"4","title":"hot Y","permalink":"/y","stickied":false}}
	]}}`)
	out, _, err := parseThreadsJSON(body, true)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{out[0].id, out[1].id, out[2].id, out[3].id}
	want := []string{"1", "3", "2", "4"} // stickies first, original relative order preserved
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"5", 5 * time.Second},
		{"100", 30 * time.Second}, // capped
		{"", 0},
		{"garbage", 0},
	}
	for _, c := range cases {
		if got := retryAfter(c.in); got != c.want {
			t.Errorf("retryAfter(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSparkline(t *testing.T) {
	if got := sparkline([]int{0, 0, 0}, 0); got != "▁▁▁" {
		t.Errorf("sparkline(zeros) = %q, want ▁▁▁", got)
	}
	got := []rune(sparkline([]int{0, 10}, 10))
	if got[0] != '▁' || got[1] != '█' {
		t.Errorf("sparkline = %q, want ▁ then █", string(got))
	}
}

func TestPushSub(t *testing.T) {
	subs := pushSub([]string{"a", "b"}, "b") // move b to front, dedup
	if len(subs) != 2 || subs[0] != "b" {
		t.Errorf("pushSub dedup/front = %v", subs)
	}
	var many []string
	for i := 0; i < maxRecents+3; i++ {
		many = pushSub(many, fmt.Sprintf("s%d", i))
	}
	if len(many) != maxRecents {
		t.Errorf("pushSub len = %d, want cap %d", len(many), maxRecents)
	}
	if many[0] != fmt.Sprintf("s%d", maxRecents+2) {
		t.Errorf("newest should be at front, got %q", many[0])
	}
}

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.1.1", "v0.1.2", true},
		{"v0.1.1", "v0.1.1", false},
		{"v0.1.2", "v0.1.1", false},
		{"v0.1.1", "v0.2.0", true},
		{"v0.1.1", "v1.0.0", true},
		{"v0.9.9", "v0.10.0", true},     // numeric compare, not lexical
		{"v1.2.3", "v1.2.3-rc1", false}, // pre-release suffix ignored → equal
	}
	for _, c := range cases {
		if got := newerVersion(c.a, c.b); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestPushThread(t *testing.T) {
	var ts []recentThread
	ts = pushThread(ts, recentThread{URL: "u1", Title: "one"})
	ts = pushThread(ts, recentThread{URL: "u2", Title: "two"})
	ts = pushThread(ts, recentThread{URL: "u1", Title: "one again"}) // dedup by URL, move to front
	if len(ts) != 2 || ts[0].URL != "u1" {
		t.Errorf("pushThread dedup/front = %v", ts)
	}
}

func TestFuzzyScore(t *testing.T) {
	if _, ok := fuzzyScore("hello world", "hlo"); !ok {
		t.Error("hlo should match hello world as a subsequence")
	}
	if _, ok := fuzzyScore("hello", "xyz"); ok {
		t.Error("xyz should not match hello")
	}
	// A contiguous run should score higher than the same chars scattered.
	cont, _ := fuzzyScore("abcdef", "abc")
	scat, _ := fuzzyScore("axbxcx", "abc")
	if cont <= scat {
		t.Errorf("contiguous score %d should beat scattered %d", cont, scat)
	}
	// Regression: non-ASCII titles must match (the old byte-vs-rune comparison
	// silently dropped any thread with accented or non-Latin characters).
	if _, ok := fuzzyScore("café", "é"); !ok {
		t.Error("é should match café (unicode)")
	}
	if _, ok := fuzzyScore("münchen", "mü"); !ok {
		t.Error("mü should match münchen (unicode)")
	}
}

func TestURLBuilders(t *testing.T) {
	if got := cleanSubreddit("  /r/Soccer/ "); got != "Soccer" {
		t.Errorf("cleanSubreddit = %q, want Soccer", got)
	}
	if got := cleanSubreddit("r/golang"); got != "golang" {
		t.Errorf("cleanSubreddit = %q, want golang", got)
	}

	if got := threadsJSONURL("soccer", sortNew, ""); got != "https://www.reddit.com/r/soccer/new.json?limit=100&raw_json=1" {
		t.Errorf("threadsJSONURL new = %q", got)
	}
	if got := threadsJSONURL("soccer", sortTop, ""); !strings.Contains(got, "/top.json") || !strings.Contains(got, "t=day") {
		t.Errorf("threadsJSONURL top = %q, want /top.json and t=day", got)
	}
	if got := threadsJSONURL("soccer", sortNew, "t3_abc"); got != "https://www.reddit.com/r/soccer/new.json?limit=100&raw_json=1&after=t3_abc" {
		t.Errorf("threadsJSONURL with after = %q", got)
	}
	if got := threadsRSSURL("soccer", sortHot); got != "https://www.reddit.com/r/soccer/.rss" {
		t.Errorf("threadsRSSURL hot = %q", got)
	}
	if got := threadsRSSURL("soccer", sortNew); got != "https://www.reddit.com/r/soccer/new/.rss" {
		t.Errorf("threadsRSSURL new = %q", got)
	}

	// jsonURL accepts a full thread URL or a bare path and normalizes suffixes.
	want := "https://www.reddit.com/r/soccer/comments/abc/title.json?sort=new&limit=100&raw_json=1"
	if got := jsonURL("https://www.reddit.com/r/soccer/comments/abc/title/"); got != want {
		t.Errorf("jsonURL = %q, want %q", got, want)
	}
	if got := rssURL("/r/soccer/comments/abc/title/"); got != "https://www.reddit.com/r/soccer/comments/abc/title/.rss" {
		t.Errorf("rssURL = %q", got)
	}
}

func TestCleanContent(t *testing.T) {
	if got := cleanContent("<b>hi</b>  &amp; bye"); got != "hi & bye" {
		t.Errorf("cleanContent = %q, want %q", got, "hi & bye")
	}
}

func TestCleanSelftext(t *testing.T) {
	// Decodes entities and collapses 3+ blank lines to one, but keeps a paragraph break.
	got := cleanSelftext("para1\n\n\n\npara2 &amp; more")
	want := "para1\n\npara2 & more"
	if got != want {
		t.Errorf("cleanSelftext = %q, want %q", got, want)
	}
}

func TestEmojiReserveRestore(t *testing.T) {
	in := "goal 🎉 and ⚽ but text arrows ↑ ↓ → stay"
	reserved, picked := reserveEmoji(in)
	if len(picked) != 2 {
		t.Errorf("picked %d emoji, want 2 (🎉 ⚽); arrows must not count: %v", len(picked), picked)
	}
	if got := restoreEmoji(reserved, picked); got != in {
		t.Errorf("emoji round-trip = %q, want %q", got, in)
	}
}

func TestIsEmojiCluster(t *testing.T) {
	cases := []struct {
		r    rune
		want bool
	}{
		{'🎉', true},  // pictographic supplement
		{'⚽', true},  // misc symbols
		{'★', true},  // black star (in 0x2600–0x27BF)
		{'↑', false}, // text arrow, must stay
		{'→', false}, // text arrow, must stay
		{'a', false},
	}
	for _, c := range cases {
		if got := isEmojiCluster([]rune{c.r}); got != c.want {
			t.Errorf("isEmojiCluster(%q) = %v, want %v", c.r, got, c.want)
		}
	}
}

func TestDemoPoolShuffleBag(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e"}
	p := newPool(lines...)
	n := len(lines)

	var prev string
	for pass := 0; pass < 2; pass++ {
		seen := map[string]int{}
		for i := 0; i < n; i++ {
			got := p.next()
			if pass == 1 && i == 0 && got == prev {
				t.Errorf("line %q repeated across the reshuffle boundary", got)
			}
			seen[got]++
			prev = got
		}
		for _, l := range lines {
			if seen[l] != 1 {
				t.Errorf("pass %d: %q appeared %d times, want exactly 1 (not a permutation)", pass, l, seen[l])
			}
		}
	}
}

func TestEnqueueDedupAndCap(t *testing.T) {
	newM := func() *model {
		return &model{seen: map[string]bool{}, byName: map[string]comment{}, scores: map[string]int{}}
	}

	m := newM()
	m.enqueue([]comment{{id: "a"}, {id: "b"}})
	m.enqueue([]comment{{id: "b"}, {id: "c"}}) // b is a duplicate
	if len(m.comments) != 2 {
		t.Errorf("comments = %d, want 2 (first batch shown instantly)", len(m.comments))
	}
	if len(m.pending) != 1 || m.pending[0].id != "c" {
		t.Errorf("pending = %v, want [c] only (b deduped)", m.pending)
	}

	// The first batch is capped to initialCap; the newest instantBurst show immediately.
	m2 := newM()
	big := make([]comment, initialCap+10)
	for i := range big {
		big[i] = comment{id: fmt.Sprintf("c%d", i), postedAt: time.Unix(int64(i), 0)}
	}
	m2.enqueue(big)
	if len(m2.comments) != instantBurst {
		t.Errorf("instant comments = %d, want %d", len(m2.comments), instantBurst)
	}
	wantPending := initialCap - instantBurst
	if len(m2.pending) != wantPending {
		t.Errorf("pending after burst = %d, want %d", len(m2.pending), wantPending)
	}
	if m2.comments[0].id != fmt.Sprintf("c%d", initialCap+9) {
		t.Errorf("newest shown = %q, want c%d", m2.comments[0].id, initialCap+9)
	}
	if m2.pending[0].id != "c31" {
		t.Errorf("next pending = %q, want c31", m2.pending[0].id)
	}
}

func TestReleaseOneNewestFirst(t *testing.T) {
	m := &model{seen: map[string]bool{}, byName: map[string]comment{}, scores: map[string]int{}}
	// Pending is newest-first (index 0 = newest); drain appends below.
	m.pending = []comment{
		{id: "new", postedAt: time.Unix(3, 0)},
		{id: "mid", postedAt: time.Unix(2, 0)},
		{id: "old", postedAt: time.Unix(1, 0)},
	}
	for range 3 {
		m.releaseOne()
	}
	if len(m.comments) != 3 {
		t.Fatalf("comments = %d, want 3", len(m.comments))
	}
	want := []string{"new", "mid", "old"}
	for i, id := range want {
		if m.comments[i].id != id {
			t.Errorf("comments[%d].id = %q, want %q (newest-first)", i, m.comments[i].id, id)
		}
	}
}

func TestInstantBurst(t *testing.T) {
	m := &model{seen: map[string]bool{}, byName: map[string]comment{}, scores: map[string]int{}}
	batch := make([]comment, 15)
	for i := range batch {
		batch[i] = comment{id: fmt.Sprintf("c%d", i), postedAt: time.Unix(int64(i), 0)}
	}
	m.enqueue(batch)
	if len(m.comments) != instantBurst {
		t.Fatalf("instant comments = %d, want %d", len(m.comments), instantBurst)
	}
	if m.comments[0].id != "c14" {
		t.Errorf("first shown = %q, want c14 (newest)", m.comments[0].id)
	}
	if m.comments[instantBurst-1].id != fmt.Sprintf("c%d", 15-instantBurst) {
		t.Errorf("last of burst = %q, want c%d", m.comments[instantBurst-1].id, 15-instantBurst)
	}
	if len(m.pending) != 15-instantBurst {
		t.Errorf("pending after burst = %d, want %d", len(m.pending), 15-instantBurst)
	}
}

func TestReleaseOneLivePrepend(t *testing.T) {
	m := &model{
		seen:     map[string]bool{},
		byName:   map[string]comment{},
		scores:   map[string]int{},
		comments: []comment{{id: "shown", postedAt: time.Unix(5, 0)}},
	}
	m.pending = []comment{{id: "live", postedAt: time.Unix(10, 0)}}
	m.releaseOne()
	if len(m.comments) != 2 || m.comments[0].id != "live" {
		t.Errorf("live comment should prepend: %+v", m.comments)
	}
}

func TestMergeMoreIDsSkipsFetched(t *testing.T) {
	m := &model{fetchedMoreIDs: map[string]bool{"a": true}}
	m.mergeMoreIDs([]string{"a", "b", "b", "c"})
	if len(m.commentMoreIDs) != 2 || m.commentMoreIDs[0] != "b" || m.commentMoreIDs[1] != "c" {
		t.Errorf("queue = %v, want [b c]", m.commentMoreIDs)
	}
	m.markMoreFetched([]string{"b"})
	m.mergeMoreIDs([]string{"a", "b", "c"})
	if len(m.commentMoreIDs) != 2 {
		t.Fatalf("queue = %v, want [c] only added once", m.commentMoreIDs)
	}
	if !m.commentsHasMore {
		t.Error("commentsHasMore should be true while c is queued")
	}
}

func TestAppendOlderCommentsDedupes(t *testing.T) {
	m := &model{
		seen:     map[string]bool{"t1_old": true},
		byName:   map[string]comment{},
		scores:   map[string]int{},
		comments: []comment{{id: "t1_old"}},
	}
	n := m.appendOlderComments([]comment{
		{id: "t1_old"},
		{id: "t1_new"},
	})
	if n != 1 {
		t.Errorf("added = %d, want 1", n)
	}
	if len(m.comments) != 2 || m.comments[1].id != "t1_new" {
		t.Errorf("comments = %+v", m.comments)
	}
}

func TestAppendOlderCommentsSorted(t *testing.T) {
	now := time.Now()
	sixH := now.Add(-6 * time.Hour)
	fiveH := now.Add(-5 * time.Hour)
	m := &model{
		seen:     map[string]bool{},
		byName:   map[string]comment{},
		scores:   map[string]int{},
		comments: []comment{{id: "six", postedAt: sixH}},
	}
	n := m.appendOlderComments([]comment{{id: "five", postedAt: fiveH}})
	if n != 1 {
		t.Fatalf("added = %d, want 1", n)
	}
	if len(m.comments) != 2 || m.comments[0].id != "five" || m.comments[1].id != "six" {
		t.Errorf("comments = %+v, want [five six] newest-first", m.comments)
	}
}

func TestAppendOlderCommentsSortedBatch(t *testing.T) {
	now := time.Now()
	m := &model{
		seen:     map[string]bool{},
		byName:   map[string]comment{},
		scores:   map[string]int{},
		comments: []comment{{id: "c3", postedAt: now.Add(-3 * time.Hour)}},
	}
	n := m.appendOlderComments([]comment{
		{id: "c5", postedAt: now.Add(-5 * time.Hour)},
		{id: "c4", postedAt: now.Add(-4 * time.Hour)},
		{id: "c6", postedAt: now.Add(-6 * time.Hour)},
	})
	if n != 3 {
		t.Fatalf("added = %d, want 3", n)
	}
	want := []string{"c3", "c4", "c5", "c6"}
	for i, id := range want {
		if m.comments[i].id != id {
			t.Errorf("comments[%d] = %q, want %q", i, m.comments[i].id, id)
		}
	}
}
