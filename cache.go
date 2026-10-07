package ipa

import "fmt"

// Cache models a set-associative cache with LRU or FIFO replacement.
// All behavior is deterministic for a given access sequence.
type Cache struct {
	cfg      CacheConfig
	sets     [][]uint64 // tags per set
	recency  [][]int    // per-set way ages (LRU) or insertion order (FIFO)
	lines    int
	assoc    int
	hits     uint64
	misses   uint64
	lineMask uint64
}

// NewCache builds a cache from cfg. Returns nil when disabled.
// Invalid geometry yields an error via ValidateCacheConfig.
func NewCache(cfg CacheConfig) *Cache {
	if !cfg.Enabled {
		return nil
	}
	sets := cfg.Capacity / (cfg.LineSize * cfg.Assoc)
	if sets < 1 {
		sets = 1
	}
	c := &Cache{
		cfg:     cfg,
		sets:    make([][]uint64, sets),
		recency: make([][]int, sets),
		lines:   sets,
		assoc:   cfg.Assoc,
	}
	for i := range c.sets {
		c.sets[i] = make([]uint64, 0, cfg.Assoc)
		c.recency[i] = make([]int, 0, cfg.Assoc)
	}
	return c
}

// ValidateCacheConfig checks cache geometry.
func ValidateCacheConfig(cfg CacheConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if cfg.Capacity <= 0 || cfg.LineSize <= 0 || cfg.Assoc <= 0 {
		return fmt.Errorf("cache capacity, line size and assoc must be positive")
	}
	if cfg.Capacity%(cfg.LineSize*cfg.Assoc) != 0 {
		return fmt.Errorf("cache capacity %d not divisible by lineSize*assoc (%d)",
			cfg.Capacity, cfg.LineSize*cfg.Assoc)
	}
	if cfg.Replacement != "lru" && cfg.Replacement != "fifo" {
		return fmt.Errorf("unknown replacement policy %q (want lru|fifo)", cfg.Replacement)
	}
	if cfg.MissPenalty < 0 {
		return fmt.Errorf("miss penalty must be >= 0")
	}
	return nil
}

// Access records an access to addr; returns true on hit.
func (c *Cache) Access(addr uint64) bool {
	if c == nil {
		return true
	}
	line := addr / uint64(c.cfg.LineSize)
	setIdx := int(line % uint64(len(c.sets)))
	tags := c.sets[setIdx]
	for i, t := range tags {
		if t == line {
			c.hits++
			c.touch(setIdx, i)
			return true
		}
	}
	c.misses++
	c.insert(setIdx, line)
	return false
}

// touch updates replacement state after a hit.
func (c *Cache) touch(set, way int) {
	if c.cfg.Replacement != "lru" {
		return
	}
	rec := c.recency[set]
	age := rec[way]
	for i := range rec {
		if rec[i] > age {
			rec[i]--
		}
	}
	rec[way] = len(rec) - 1
}

// insert adds a line, evicting per policy when the set is full.
func (c *Cache) insert(set int, line uint64) {
	tags := c.sets[set]
	rec := c.recency[set]
	if len(tags) < c.assoc {
		c.sets[set] = append(tags, line)
		c.recency[set] = append(rec, len(tags))
		return
	}
	// Evict: LRU picks lowest recency; FIFO picks index 0 (oldest insert).
	victim := 0
	if c.cfg.Replacement == "lru" {
		for i := range rec {
			if rec[i] < rec[victim] {
				victim = i
			}
		}
	}
	tags[victim] = line
	if c.cfg.Replacement == "lru" {
		age := rec[victim]
		for i := range rec {
			if rec[i] > age {
				rec[i]--
			}
		}
		rec[victim] = len(rec) - 1
	} else {
		// FIFO: rotate so index 0 is always the oldest.
		copy(tags[0:], tags[1:])
		tags[len(tags)-1] = line
		c.sets[set] = tags
	}
}

// Counts returns (hits, misses).
func (c *Cache) Counts() (hits, misses uint64) {
	if c == nil {
		return 0, 0
	}
	return c.hits, c.misses
}
