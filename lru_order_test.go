package regexpset

import "testing"

func TestLRUEvictsLeastRecentlyTouchedState(t *testing.T) {
	m := newMachine(0, 0, 2, 100, 0, false, true)
	first := m.intern(&dfaState{ready: []uint32{1}})
	second := m.intern(&dfaState{ready: []uint32{2}})
	if !first.cached || !second.cached {
		t.Fatal("states not cached")
	}
	m.touch(first)
	third := m.intern(&dfaState{ready: []uint32{3}})
	if !third.cached {
		t.Fatal("failed to cache third state after eviction")
	}
	if !second.evicted.Load() || first.evicted.Load() || third.evicted.Load() {
		t.Fatalf("wrong victim: first=%t second=%t third=%t", first.evicted.Load(), second.evicted.Load(), third.evicted.Load())
	}
	stats := m.cacheStats()
	if stats.States != 2 || stats.Evictions != 1 {
		t.Fatalf("wrong stats: %+v", stats)
	}
}
