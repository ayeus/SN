package main

import (
	"testing"
	"time"
)

func TestPickPrefersLeastOutstanding(t *testing.T) {
	r := newRouter(nil, 15*time.Second)
	cands := []replica{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	r.acquire("a")
	r.acquire("a")
	r.acquire("b")

	got, ok := r.pick(cands, nil)
	if !ok || got.ID != "c" {
		t.Fatalf("expected idle replica c, got %+v", got)
	}

	r.acquire("c")
	r.acquire("c")
	got, _ = r.pick(cands, nil)
	if got.ID != "b" {
		t.Fatalf("expected b (1 outstanding), got %s", got.ID)
	}

	r.release("a")
	r.release("a")
	got, _ = r.pick(cands, nil)
	if got.ID != "a" {
		t.Fatalf("expected a after release, got %s", got.ID)
	}
}

func TestPickSkipsExcludedAndRecentlyFailed(t *testing.T) {
	r := newRouter(nil, 15*time.Second)
	cands := []replica{{ID: "a"}, {ID: "b"}}

	r.markFailed("a")
	for i := 0; i < 20; i++ {
		got, ok := r.pick(cands, nil)
		if !ok || got.ID != "b" {
			t.Fatalf("failed replica must be skipped during cooldown, got %+v", got)
		}
	}

	if _, ok := r.pick(cands, map[string]bool{"b": true}); ok {
		t.Fatal("no replica should be eligible when the only healthy one is excluded")
	}

	r.markHealthy("a")
	if got, ok := r.pick(cands, map[string]bool{"b": true}); !ok || got.ID != "a" {
		t.Fatalf("healthy replica should be eligible again, got %+v", got)
	}
}

func TestPickSpreadsTies(t *testing.T) {
	r := newRouter(nil, 15*time.Second)
	cands := []replica{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	seen := map[string]int{}
	for i := 0; i < 300; i++ {
		got, _ := r.pick(cands, nil)
		seen[got.ID]++
	}
	for _, id := range []string{"a", "b", "c"} {
		if seen[id] < 50 {
			t.Fatalf("ties should be broken randomly, distribution %v", seen)
		}
	}
}

func TestDeltaCounterCountsTokensAcrossReads(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	// Feed in awkward slices to prove events split across reads still count.
	for _, size := range []int{1, 7, 64, len(stream)} {
		var c deltaCounter
		for i := 0; i < len(stream); i += size {
			end := min(i+size, len(stream))
			c.write([]byte(stream[i:end]))
		}
		if c.n != 2 {
			t.Fatalf("chunk size %d: counted %d deltas, want 2", size, c.n)
		}
	}
}

func TestEstimatePromptTokens(t *testing.T) {
	if n := estimatePromptTokens([]byte(`{"messages":[{"role":"user","content":"12345678"}]}`)); n < 2 || n > 4 {
		t.Fatalf("estimate = %d", n)
	}
	if estimatePromptTokens([]byte(`not json`)) != 0 {
		t.Fatal("invalid body must estimate 0")
	}
}
