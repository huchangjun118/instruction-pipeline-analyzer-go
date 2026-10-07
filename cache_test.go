package ipa

import "testing"

func lruCfg() CacheConfig {
	return CacheConfig{
		Enabled:     true,
		Capacity:    128,
		LineSize:    32,
		Assoc:       2,
		Replacement: "lru",
		MissPenalty: 5,
	}
}

func TestCacheBasicHitMiss(t *testing.T) {
	c := NewCache(lruCfg())
	if c.Access(0) {
		t.Fatal("first access must miss")
	}
	if !c.Access(0) {
		t.Fatal("second access to same line must hit")
	}
	if !c.Access(31) {
		t.Fatal("same line (offset 31) must hit")
	}
	if c.Access(32) {
		t.Fatal("different line must miss")
	}
	h, m := c.Counts()
	if h != 2 || m != 2 {
		t.Fatalf("want hits=2 misses=2, got %d/%d", h, m)
	}
}

func TestCacheDisabled(t *testing.T) {
	if NewCache(CacheConfig{}) != nil {
		t.Fatal("disabled cache must be nil")
	}
	var c *Cache
	if !c.Access(0) {
		t.Fatal("nil cache must report hit (no stall)")
	}
}

func TestCacheCapacityEviction(t *testing.T) {
	c := NewCache(lruCfg()) // 2 sets x 2 ways, 32B lines
	// Set 0 lines: 0, 64, 128 (line = addr/32, set = line % 2)
	c.Access(0)    // line 0 -> set 0
	c.Access(2048) // line 64 -> set 0
	if !c.Access(0) {
		t.Fatal("line 0 must still be cached")
	}
	c.Access(4096) // line 128 -> set 0, evicts LRU (line 64)
	if !c.Access(0) {
		t.Fatal("line 0 must survive (most recently used)")
	}
	if c.Access(2048) {
		t.Fatal("line 64 must have been evicted (LRU)")
	}
}

func TestCacheFIFODiffersFromLRU(t *testing.T) {
	cfg := lruCfg()
	cfg.Replacement = "fifo"
	c := NewCache(cfg)
	c.Access(0)    // line 0
	c.Access(2048) // line 64
	c.Access(0)    // hit, but FIFO does not refresh
	c.Access(4096) // evicts line 0 (oldest inserted)
	if c.Access(0) {
		t.Fatal("FIFO must evict line 0 despite recent use")
	}
}

func TestCacheDeterministic(t *testing.T) {
	run := func() (uint64, uint64) {
		c := NewCache(lruCfg())
		for i := 0; i < 1000; i++ {
			c.Access(uint64((i*37)%509) * 4)
		}
		return c.Counts()
	}
	h1, m1 := run()
	h2, m2 := run()
	if h1 != h2 || m1 != m2 {
		t.Fatalf("non-deterministic: %d/%d vs %d/%d", h1, m1, h2, m2)
	}
}

func TestCacheConfigValidation(t *testing.T) {
	bad := lruCfg()
	bad.Capacity = 100 // not divisible by 32*2
	if ValidateCacheConfig(bad) == nil {
		t.Fatal("want geometry error")
	}
	bad = lruCfg()
	bad.Replacement = "random"
	if ValidateCacheConfig(bad) == nil {
		t.Fatal("want policy error")
	}
	if ValidateCacheConfig(lruCfg()) != nil {
		t.Fatal("valid config rejected")
	}
}
