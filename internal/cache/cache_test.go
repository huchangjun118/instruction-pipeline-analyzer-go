package cache

import "testing"

func cfg(size, assoc, line int, repl Replacement) Config {
	return Config{SizeBytes: size, Assoc: assoc, LineBytes: line,
		Replacement: repl, MissPenalty: 10}
}

func TestDisabledCacheAlwaysHits(t *testing.T) {
	c := New(Config{})
	for i := 0; i < 100; i++ {
		if !c.Access(uint64(i*64), 4).Hit {
			t.Fatal("disabled cache must always hit")
		}
	}
	if c.MissPenalty() != 0 {
		t.Fatal("disabled cache must have zero miss penalty")
	}
}

func TestCompulsoryAndConflictMisses(t *testing.T) {
	// 2 sets, 1-way (direct mapped), 64B lines.
	c := New(cfg(128, 1, 64, LRU))
	if c.Access(0, 4).Hit {
		t.Error("first access must miss (compulsory)")
	}
	if !c.Access(0, 4).Hit {
		t.Error("second access to same line must hit")
	}
	// Address 256 maps to same set (256/64=4, 4%2=0): evicts address 0.
	c.Access(256, 4)
	if c.Access(0, 4).Hit {
		t.Error("address 0 must have been evicted by conflict")
	}
	s := c.Stats()
	if s.Accesses != 4 || s.Hits != 1 || s.Misses != 3 {
		t.Errorf("stats = %+v", s)
	}
}

func TestLRUVsFIFO(t *testing.T) {
	// 1 set of 2 ways, 64B lines: addresses 0, 64, 128 all map to set 0.
	lru := New(cfg(128, 2, 64, LRU))
	lru.Access(0, 4)
	lru.Access(64, 4)
	lru.Access(0, 4)   // touch 0 → 64 is now LRU victim
	lru.Access(128, 4) // evicts 64
	if !lru.Access(0, 4).Hit {
		t.Error("LRU must keep recently used line 0")
	}
	if lru.Access(64, 4).Hit {
		t.Error("LRU must have evicted line 64")
	}

	fifo := New(cfg(128, 2, 64, FIFO))
	fifo.Access(0, 4)
	fifo.Access(64, 4)
	fifo.Access(0, 4)   // touch does NOT refresh under FIFO
	fifo.Access(128, 4) // evicts 0 (oldest inserted)
	if !fifo.Access(64, 4).Hit {
		t.Error("FIFO must keep line 64")
	}
	if fifo.Access(0, 4).Hit {
		t.Error("FIFO must have evicted line 0")
	}
}

func TestDeterminism(t *testing.T) {
	run := func() Stats {
		c := New(cfg(256, 2, 32, LRU))
		for i := 0; i < 1000; i++ {
			c.Access(uint64((i*37)%512), 4)
		}
		return c.Stats()
	}
	if run() != run() {
		t.Fatal("same access sequence must give identical stats")
	}
}

func TestInvalidConfigNormalizesToDisabled(t *testing.T) {
	c := New(Config{SizeBytes: 100, Assoc: 3, LineBytes: 7})
	if c.Enabled() {
		t.Fatal("invalid config must normalize to disabled cache")
	}
	if !c.Access(0, 4).Hit {
		t.Fatal("disabled cache must hit")
	}
}
