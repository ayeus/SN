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
