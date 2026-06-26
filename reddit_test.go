package main

import "testing"

func TestParseThreadsJSON(t *testing.T) {
	body := []byte(`{"data":{"children":[
		{"data":{"id":"a1","title":"  Hot one  ","permalink":"/r/x/comments/a1/","author":"u1","num_comments":5,"stickied":false}},
		{"data":{"id":"p1","title":"Pinned","permalink":"/r/x/comments/p1/","author":"mod","num_comments":2,"stickied":true}}
	],"after":"t3_next"}}`)
	threads, after, err := parseThreadsJSON(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if after != "t3_next" {
		t.Errorf("after = %q, want t3_next", after)
	}
	if len(threads) != 2 {
		t.Fatalf("want 2 threads, got %d", len(threads))
	}
	// Stickied first.
	if !threads[0].stickied || threads[0].id != "p1" {
		t.Errorf("stickied thread should sort first, got %q", threads[0].id)
	}
	// Title trimmed, permalink absolutized, counts carried.
	if threads[1].title != "Hot one" {
		t.Errorf("title not trimmed: %q", threads[1].title)
	}
	if threads[1].permalink != "https://www.reddit.com/r/x/comments/a1/" {
		t.Errorf("permalink not absolutized: %q", threads[1].permalink)
	}
	if threads[1].numComments != 5 || threads[1].author != "u1" {
		t.Errorf("thread fields wrong: %+v", threads[1])
	}
}

func TestParseThreadsJSONEmpty(t *testing.T) {
	if _, _, err := parseThreadsJSON([]byte(`{"data":{"children":[]}}`), true); err == nil {
		t.Error("empty listing should error")
	}
}

func TestParseThreadsJSONAfter(t *testing.T) {
	body := []byte(`{"data":{"after":"t3_abc123","children":[
		{"data":{"id":"a1","title":"One","permalink":"/r/x/comments/a1/","author":"u1","num_comments":1,"stickied":false}}
	]}}`)
	_, after, err := parseThreadsJSON(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if after != "t3_abc123" {
		t.Errorf("after = %q, want t3_abc123", after)
	}
}

func TestParseThreadJSON(t *testing.T) {
	body := []byte(`[
		{"data":{"children":[{"data":{
			"id":"x","title":"OP title","author":"op","selftext":"hello **world**","score":42,"created_utc":1700000000,"is_self":true
		}}]}},
		{"data":{"children":[
			{"kind":"t1","data":{"id":"c1","author":"a","body":"top comment","score":10,"created_utc":1700000100,"parent_id":"t3_x","replies":{"data":{"children":[
				{"kind":"t1","data":{"id":"c2","author":"b","body":"a reply","score":3,"created_utc":1700000050,"parent_id":"t1_c1","replies":""}}
			]}}}},
			{"kind":"more","data":{"id":"more1","children":["c4","c5"]}},
			{"kind":"t1","data":{"id":"c3","author":"c","body":"[deleted]","score":0,"created_utc":1700000200,"parent_id":"t3_x"}}
		]}}
	]`)

	tc, err := parseThreadJSON(body)
	if err != nil {
		t.Fatal(err)
	}

	// OP from listings[0].
	if tc.post == nil {
		t.Fatal("expected an OP")
	}
	if tc.post.title != "OP title" || tc.post.author != "op" || tc.post.score != 42 || !tc.post.hasScore {
		t.Errorf("OP fields wrong: %+v", tc.post)
	}
	if tc.postID != "t3_x" {
		t.Errorf("postID = %q, want t3_x", tc.postID)
	}

	// "more" stub collected; [deleted] dropped; nested reply kept.
	if len(tc.moreIDs) != 2 || tc.moreIDs[0] != "c4" {
		t.Errorf("moreIDs = %v, want [c4 c5]", tc.moreIDs)
	}
	if len(tc.comments) != 2 {
		t.Fatalf("want 2 comments (c1 + nested c2), got %d", len(tc.comments))
	}
	// Newest-first: c1 (1700000100) before c2 (1700000050).
	if tc.comments[0].id != "t1_c1" || tc.comments[1].id != "t1_c2" {
		t.Errorf("comments not sorted newest-first: %q, %q", tc.comments[0].id, tc.comments[1].id)
	}
	// Reply threading is preserved.
	if tc.comments[1].parentID != "t1_c1" {
		t.Errorf("nested reply parentID wrong: %q", tc.comments[1].parentID)
	}
	for _, c := range tc.comments {
		if c.body == "[deleted]" {
			t.Error("[deleted] comment should be dropped")
		}
	}
}

func TestParseMoreChildrenJSON(t *testing.T) {
	body := []byte(`{"json":{"data":{"things":[
		{"kind":"t1","data":{"id":"m1","author":"u","body":"older comment","score":1,"created_utc":1700000000,"parent_id":"t3_x"}},
		{"kind":"more","data":{"id":"more2","children":["x"]}}
	]}}}`)
	cs, nested, err := parseMoreChildrenJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].id != "t1_m1" {
		t.Errorf("got comments %+v, want one t1_m1", cs)
	}
	if len(nested) != 1 || nested[0] != "x" {
		t.Errorf("nested more = %v, want [x]", nested)
	}
}
