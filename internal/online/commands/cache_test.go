package commands

import "testing"

func TestResultCache_PutGet(t *testing.T) {
	c := NewResultCache(4)
	c.Put("a", CachedResult{Status: "ok"})
	got, ok := c.Get("a")
	if !ok || got.Status != "ok" {
		t.Fatalf("Get(a) = %+v, %v; want {Status:ok}, true", got, ok)
	}
}

func TestResultCache_LRUEvicts(t *testing.T) {
	c := NewResultCache(2)
	c.Put("a", CachedResult{Status: "ok"})
	c.Put("b", CachedResult{Status: "ok"})
	c.Put("c", CachedResult{Status: "ok"}) // evicts "a"

	if _, ok := c.Get("a"); ok {
		t.Fatal("expected a to be evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("expected b to be retained")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("expected c to be retained")
	}
}

func TestResultCache_GetPromotes(t *testing.T) {
	c := NewResultCache(2)
	c.Put("a", CachedResult{Status: "ok"})
	c.Put("b", CachedResult{Status: "ok"})
	_, _ = c.Get("a")                       // promote a → MRU
	c.Put("c", CachedResult{Status: "ok"}) // should now evict b, not a

	if _, ok := c.Get("a"); !ok {
		t.Fatal("expected a to survive (was just promoted)")
	}
	if _, ok := c.Get("b"); ok {
		t.Fatal("expected b to be evicted")
	}
}

func TestResultCache_PutOverwrites(t *testing.T) {
	c := NewResultCache(4)
	c.Put("a", CachedResult{Status: "ok"})
	c.Put("a", CachedResult{Status: "error", ErrorMessage: "oops"})

	got, _ := c.Get("a")
	if got.Status != "error" || got.ErrorMessage != "oops" {
		t.Fatalf("got %+v, want overwritten value", got)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
}
