package regexpset

import (
	"container/list"
	"regexp/syntax"
	"sort"
	"sync"
	"sync/atomic"
	"unicode/utf8"
	"unsafe"
)

const internShardCount = 32
const edgeBudgetBytes = 64 // conservative map-entry allowance, not a heap-size measurement

type internShard struct {
	mu     sync.Mutex
	states map[uint64][]*dfaState
}

type cacheGeneration struct {
	evictMu   sync.RWMutex // eviction coordinates interns and edge publications
	memberMu  sync.Mutex
	members   map[*dfaState]*list.Element // only when EvictLRU is enabled
	lru       list.List                   // front is most recently used, back is victim
	evictions atomic.Int64
	initial   sync.Map // syntax.EmptyOp -> *dfaState
	shards    [internShardCount]internShard
	states    atomic.Int64
	edges     atomic.Int64
	bytes     atomic.Int64 // tracked state/edge reservations (not full process RSS)
}

type machine struct {
	inst         []syntax.Inst
	starts       []uint32
	matchIDs     []int
	literalTries map[literalAnchor]*literalNode
	compiled     map[string][]uint32
	aliases      map[uint32][]int
	count        int
	ruleCount    int
	prefilter    *prefilter // retained for existing internal/test usage

	maxStates, maxTransitions int
	maxBytes                  int64
	resetOnFull               bool
	evictLRU                  bool
	resets                    atomic.Int64
	generation                atomic.Pointer[cacheGeneration]
	resetMu                   sync.Mutex
	asciiClass                [128]uint8
	asciiClasses              int
	unicodeBounds             []rune
	unicodeClass              []int32
	unicodeClasses            int

	startMu    sync.Mutex
	startCache sync.Map // syntax.EmptyOp -> immutable *dfaState
	pool       sync.Pool
}

type edgeKey struct {
	symbol  int32
	context syntax.EmptyOp
}

type dfaState struct {
	ready   []uint32
	matches []int
	cached  bool
	owner   *cacheGeneration
	mu      sync.RWMutex
	edges   map[edgeKey]*dfaState
	evicted atomic.Bool
}

func newMachine(count, rules, maxStates, maxTransitions int, maxBytes int64, resetOnFull, evictLRU bool) *machine {
	m := &machine{
		count: count, ruleCount: rules, maxStates: maxStates,
		maxTransitions: maxTransitions, maxBytes: maxBytes,
		resetOnFull: resetOnFull,
		evictLRU:    evictLRU,
		compiled:    make(map[string][]uint32),
		aliases:     make(map[uint32][]int),
	}
	m.generation.Store(new(cacheGeneration))
	return m
}

func (m *machine) cacheStats() CacheStats {
	g := m.generation.Load()
	return CacheStats{
		States: int(g.states.Load()), Transitions: int(g.edges.Load()),
		Bytes: g.bytes.Load(), ASCIIClasses: m.asciiClasses,
		UnicodeClasses: m.unicodeClasses, Resets: int(m.resets.Load()), Evictions: int(g.evictions.Load()),
	}
}

func nextRune(text string, pos int) rune {
	if pos >= len(text) {
		return -1
	}
	if text[pos] < utf8.RuneSelf {
		return rune(text[pos])
	}
	r, _ := utf8.DecodeRuneInString(text[pos:])
	return r
}

func consumeRune(text string, pos int) (rune, int) {
	if text[pos] < utf8.RuneSelf {
		return rune(text[pos]), 1
	}
	return utf8.DecodeRuneInString(text[pos:])
}

func (m *machine) matchAny(text string) bool {
	if m.prefilter != nil && !m.prefilter.containsAny(text) {
		return false
	}
	sc := m.pool.Get().(*scratch)
	defer m.pool.Put(sc)
	state := m.begin(nextRune(text, 0), sc)
	if len(state.matches) != 0 {
		return true
	}
	for pos := 0; pos < len(text); {
		r, width := consumeRune(text, pos)
		pos += width
		state = m.advance(state, r, syntax.EmptyOpContext(r, nextRune(text, pos)), sc)
		if len(state.matches) != 0 {
			return true
		}
	}
	return false
}

func (m *machine) matchInto(text string, dst []int) []int {
	if m.prefilter != nil && !m.prefilter.containsAny(text) {
		return dst
	}
	sc := m.pool.Get().(*scratch)
	defer m.pool.Put(sc)
	sc.ids = sc.ids[:0]
	state := m.begin(nextRune(text, 0), sc)
	m.collect(sc, state.matches)
	for pos := 0; pos < len(text) && len(sc.ids) < m.ruleCount; {
		r, width := consumeRune(text, pos)
		pos += width
		state = m.advance(state, r, syntax.EmptyOpContext(r, nextRune(text, pos)), sc)
		m.collect(sc, state.matches)
	}
	dst = append(dst, sc.ids...)
	for _, id := range sc.ids {
		sc.matched[id] = false
	}
	return dst
}

// visit streams each distinct accepting pattern ID to emit. Returning true
// stops matching immediately, without materializing or sorting an ID list.
func (m *machine) visit(text string, emit func(int) bool) bool {
	sc := m.pool.Get().(*scratch)
	defer func() {
		for _, id := range sc.ids {
			sc.matched[id] = false
		}
		sc.ids = sc.ids[:0]
		m.pool.Put(sc)
	}()
	sc.ids = sc.ids[:0]
	collect := func(state *dfaState) bool {
		previous := len(sc.ids)
		m.collect(sc, state.matches)
		for _, id := range sc.ids[previous:] {
			if emit(id) {
				return true
			}
		}
		return false
	}
	state := m.begin(nextRune(text, 0), sc)
	if collect(state) {
		return true
	}
	for pos := 0; pos < len(text) && len(sc.ids) < m.ruleCount; {
		r, width := consumeRune(text, pos)
		pos += width
		state = m.advance(state, r, syntax.EmptyOpContext(r, nextRune(text, pos)), sc)
		if collect(state) {
			return true
		}
	}
	return false
}

func (m *machine) begin(next rune, sc *scratch) *dfaState {
	ctx := syntax.EmptyOpContext(-1, next)
	g := m.generation.Load()
	if cached, ok := g.initial.Load(ctx); ok {
		found := cached.(*dfaState)
		m.touch(found)
		return found
	}
	state := m.intern(m.closure(nil, ctx, sc))
	if state.cached && state.owner == m.generation.Load() {
		g := state.owner
		if m.evictLRU {
			g.evictMu.RLock()
		}
		if !state.evicted.Load() && g == m.generation.Load() {
			old, _ := g.initial.LoadOrStore(ctx, state)
			if m.evictLRU {
				g.evictMu.RUnlock()
			}
			return old.(*dfaState)
		}
		if m.evictLRU {
			g.evictMu.RUnlock()
		}
	}
	return state
}

func (m *machine) symbol(r rune) int32 {
	if r >= 0 && r < 128 {
		return int32(m.asciiClass[r])
	}
	if len(m.unicodeBounds) > 0 {
		i := sort.Search(len(m.unicodeBounds), func(i int) bool { return m.unicodeBounds[i] > r }) - 1
		return 128 + m.unicodeClass[i]
	}
	return int32(128 + r)
}

func (m *machine) advance(state *dfaState, r rune, ctx syntax.EmptyOp, sc *scratch) *dfaState {
	key := edgeKey{symbol: m.symbol(r), context: ctx}
	if state.cached {
		state.mu.RLock()
		found := state.edges[key]
		state.mu.RUnlock()
		if found != nil {
			m.touch(found)
			return found
		}
	}
	sc.raw = sc.raw[:0]
	for _, pc := range state.ready {
		inst := &m.inst[pc]
		if inst.MatchRune(r) {
			sc.raw = append(sc.raw, inst.Out)
		}
	}
	next := m.intern(m.closure(sc.raw, ctx, sc))
	if state.cached && next.cached && state.owner == next.owner && state.owner == m.generation.Load() {
		g := state.owner
		edgeFull := false
		if m.evictLRU {
			g.evictMu.RLock()
		}
		if !state.evicted.Load() && !next.evicted.Load() && m.generation.Load() == g {
			state.mu.Lock()
			if existing := state.edges[key]; existing != nil {
				next = existing
			} else if m.reserveEdge(g) {
				if state.edges == nil {
					state.edges = make(map[edgeKey]*dfaState)
				}
				state.edges[key] = next
			} else {
				edgeFull = true
				if m.resetOnFull && !m.evictLRU && m.maxTransitions > 0 {
					m.resetGeneration(g)
				}
			}
			state.mu.Unlock()
		}
		if m.evictLRU {
			g.evictMu.RUnlock()
			if edgeFull {
				m.evictOne(g)
			} // create room for a future edge
		}
	}
	m.touch(next)
	return next
}

func hashState(state *dfaState) uint64 {
	const prime uint64 = 1099511628211
	h := uint64(14695981039346656037)
	for _, pc := range state.ready {
		h = (h ^ uint64(pc+1)) * prime
	}
	h = (h ^ uint64(0xffffffff)) * prime
	for _, id := range state.matches {
		h = (h ^ uint64(id+1)) * prime
	}
	return h
}

func equalState(a, b *dfaState) bool {
	if len(a.ready) != len(b.ready) || len(a.matches) != len(b.matches) {
		return false
	}
	for i, pc := range a.ready {
		if pc != b.ready[i] {
			return false
		}
	}
	for i, id := range a.matches {
		if id != b.matches[i] {
			return false
		}
	}
	return true
}

// touch maintains exact LRU order with an O(1) linked-list move. This is an
// opt-in synchronized hot-path cost. Eviction still requires removing incoming
// cached transitions, so the COMPLETE eviction is not O(1).
func (m *machine) touch(state *dfaState) {
	if !m.evictLRU || !state.cached {
		return
	}
	g := state.owner
	g.memberMu.Lock()
	if e := g.members[state]; e != nil {
		g.lru.MoveToFront(e)
	}
	g.memberMu.Unlock()
}

// intern first attempts a sharded lookup. On cache exhaustion and with opt-in
// EvictLRU, one old state is removed, including every cached incoming edge.
// This ensures the intern table never retains state-to-evicted-state pointers.
func (m *machine) intern(candidate *dfaState) *dfaState {
	if m.maxStates == 0 {
		return candidate
	}
	key := hashState(candidate)
	for attempt := 0; ; attempt++ {
		g := m.generation.Load()
		if m.evictLRU {
			g.evictMu.RLock()
		}
		shard := &g.shards[int(key%internShardCount)]
		shard.mu.Lock()
		for _, existing := range shard.states[key] {
			if equalState(candidate, existing) {
				shard.mu.Unlock()
				if m.evictLRU {
					g.evictMu.RUnlock()
				}
				m.touch(existing)
				return existing
			}
		}
		if m.reserveState(g, candidate) {
			cached := &dfaState{
				ready:   append([]uint32(nil), candidate.ready...),
				matches: append([]int(nil), candidate.matches...),
				cached:  true, owner: g,
			}
			if shard.states == nil {
				shard.states = make(map[uint64][]*dfaState)
			}
			shard.states[key] = append(shard.states[key], cached)
			if m.evictLRU {
				g.memberMu.Lock()
				if g.members == nil {
					g.members = make(map[*dfaState]*list.Element)
				}
				g.members[cached] = g.lru.PushFront(cached)
				g.memberMu.Unlock()
			}
			shard.mu.Unlock()
			if m.evictLRU {
				g.evictMu.RUnlock()
			}
			m.touch(cached)
			return cached
		}
		shard.mu.Unlock()
		if m.evictLRU {
			g.evictMu.RUnlock()
		}
		if m.evictLRU && g == m.generation.Load() && attempt < m.maxStates && m.canFitState(candidate) && m.evictOne(g) {
			continue
		}
		if m.resetOnFull {
			m.resetGeneration(g)
		}
		return candidate
	}
}

func (m *machine) canFitState(state *dfaState) bool {
	return m.maxBytes <= 0 || stateCost(state) <= m.maxBytes
}

// evictOne removes the least recently used retained state from the intern
// table and all other states' cached outgoing transitions. No cache locks are
// held when calling this method. Readers with transient pointers stay valid.
func (m *machine) evictOne(g *cacheGeneration) bool {
	g.evictMu.Lock()
	defer g.evictMu.Unlock()
	if m.generation.Load() != g {
		return false
	}
	g.memberMu.Lock()
	tail := g.lru.Back()
	if tail == nil {
		g.memberMu.Unlock()
		return false
	}
	victim := tail.Value.(*dfaState)
	g.lru.Remove(tail)
	delete(g.members, victim)
	victim.evicted.Store(true)
	others := make([]*dfaState, 0, len(g.members))
	for st := range g.members {
		others = append(others, st)
	}
	g.memberMu.Unlock()

	key := hashState(victim)
	shard := &g.shards[int(key%internShardCount)]
	shard.mu.Lock()
	bucket := shard.states[key]
	for i, st := range bucket {
		if st == victim {
			bucket = append(bucket[:i], bucket[i+1:]...)
			break
		}
	}
	if len(bucket) == 0 {
		delete(shard.states, key)
	} else {
		shard.states[key] = bucket
	}
	shard.mu.Unlock()
	g.initial.Range(func(k, v any) bool {
		if v == victim {
			g.initial.Delete(k)
		}
		return true
	})
	var removed int64
	for _, st := range others {
		st.mu.Lock()
		for k, to := range st.edges {
			if to == victim {
				delete(st.edges, k)
				removed++
			}
		}
		st.mu.Unlock()
	}
	victim.mu.Lock()
	removed += int64(len(victim.edges))
	victim.edges = nil
	victim.mu.Unlock()
	g.states.Add(-1)
	g.edges.Add(-removed)
	g.bytes.Add(-stateCost(victim) - removed*edgeBudgetBytes)
	g.evictions.Add(1)
	return true
}

func reserve(limit int64, count *atomic.Int64, delta int64) bool {
	if limit <= 0 {
		return false
	}
	for {
		old := count.Load()
		if delta > limit-old {
			return false
		}
		if count.CompareAndSwap(old, old+delta) {
			return true
		}
	}
}

func stateCost(state *dfaState) int64 {
	return int64(unsafe.Sizeof(dfaState{})) + int64(len(state.ready))*4 + int64(len(state.matches))*int64(unsafe.Sizeof(int(0))) + 64
}

func (m *machine) reserveState(g *cacheGeneration, state *dfaState) bool {
	if m.maxBytes < 0 || !reserve(int64(m.maxStates), &g.states, 1) {
		return false
	}
	// Account the state object, frontier slice allocations, and a fixed
	// map/allocator allowance. This is not a Go heap/RSS measurement.
	cost := stateCost(state)
	if m.maxBytes > 0 && !reserve(m.maxBytes, &g.bytes, cost) {
		g.states.Add(-1)
		return false
	}
	if m.maxBytes <= 0 {
		g.bytes.Add(cost)
	}
	return true
}

func (m *machine) reserveEdge(g *cacheGeneration) bool {
	if !reserve(int64(m.maxTransitions), &g.edges, 1) {
		return false
	}
	if m.maxBytes > 0 && !reserve(m.maxBytes, &g.bytes, edgeBudgetBytes) {
		g.edges.Add(-1)
		return false
	}
	if m.maxBytes <= 0 {
		g.bytes.Add(edgeBudgetBytes)
	}
	return true
}

// A reset swaps in a new generation without touching old maps or edges.
// Readers holding the old generation remain safe and can finish matching.
func (m *machine) resetGeneration(old *cacheGeneration) {
	m.resetMu.Lock()
	if m.generation.Load() == old {
		m.generation.Store(new(cacheGeneration))
		m.resets.Add(1)
	}
	m.resetMu.Unlock()
}
