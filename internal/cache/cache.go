// Package cache models a configurable set-associative cache.
//
// An access is classified by the line containing its start address; an
// access wider than one line is treated as touching that single line.
// A zero SizeBytes disables the cache: every access is a hit and no
// miss penalty is ever applied.
package cache

import "fmt"

// Replacement selects the eviction policy.
type Replacement int

const (
	LRU Replacement = iota
	FIFO
)

func (r Replacement) String() string {
	if r == FIFO {
		return "fifo"
	}
	return "lru"
}

// Config describes one cache instance.
type Config struct {
	SizeBytes   int
	Assoc       int
	LineBytes   int
	Replacement Replacement
	MissPenalty int // stall cycles added on a miss
}

// Validate checks the configuration for consistency.
func (c Config) Validate() error {
	if c.SizeBytes == 0 {
		return nil // disabled
	}
	if c.LineBytes <= 0 {
		return fmt.Errorf("line size must be positive, got %d", c.LineBytes)
	}
	if c.Assoc <= 0 {
		return fmt.Errorf("associativity must be positive, got %d", c.Assoc)
	}
	if c.SizeBytes%(c.LineBytes*c.Assoc) != 0 {
		return fmt.Errorf("size %d not divisible by line*assoc (%d)",
			c.SizeBytes, c.LineBytes*c.Assoc)
	}
	if c.MissPenalty < 0 {
		return fmt.Errorf("miss penalty must be >= 0, got %d", c.MissPenalty)
	}
	return nil
}

// Result is the per-access classification.
type Result struct {
	Hit bool
}

type line struct {
	tag   uint64
	valid bool
	age   uint64 // LRU: last touch; FIFO: insertion time
}

// Cache is a single set-associative cache model. Its memory footprint is
// bounded by the configured capacity, independent of trace length.
type Cache struct {
	cfg   Config
	sets  [][]line
	nsets uint64
	clock uint64
	stats Stats
}

// New builds a cache from cfg. Invalid configs are normalized to a
// disabled cache so a bad flag never crashes a run.
func New(cfg Config) *Cache {
	c := &Cache{cfg: cfg}
	if cfg.Validate() != nil || cfg.SizeBytes == 0 {
		return c // disabled: Access always hits
	}
	c.nsets = uint64(cfg.SizeBytes / (cfg.LineBytes * cfg.Assoc))
	c.sets = make([][]line, c.nsets)
	for i := range c.sets {
		c.sets[i] = make([]line, cfg.Assoc)
	}
	return c
}

// Enabled reports whether the cache models misses at all.
func (c *Cache) Enabled() bool { return c.sets != nil }

// MissPenalty returns the configured stall cycles per miss (0 if disabled).
func (c *Cache) MissPenalty() int {
	if !c.Enabled() {
		return 0
	}
	return c.cfg.MissPenalty
}

// Access classifies one access to addr spanning width bytes and updates
// replacement state.
func (c *Cache) Access(addr uint64, width int) Result {
	c.stats.Accesses++
	if !c.Enabled() {
		c.stats.Hits++
		return Result{Hit: true}
	}
	c.clock++
	lineAddr := addr / uint64(c.cfg.LineBytes)
	idx := lineAddr % c.nsets
	tag := lineAddr / c.nsets
	set := c.sets[idx]
	for i := range set {
		if set[i].valid && set[i].tag == tag {
			if c.cfg.Replacement == LRU {
				set[i].age = c.clock
			}
			c.stats.Hits++
			return Result{Hit: true}
		}
	}
	// Miss: pick victim (invalid first, then oldest by policy).
	victim := 0
	for i := range set {
		if !set[i].valid {
			victim = i
			break
		}
		if set[i].age < set[victim].age {
			victim = i
		}
	}
	set[victim] = line{tag: tag, valid: true, age: c.clock}
	c.stats.Misses++
	return Result{Hit: false}
}

// Stats summarizes all accesses so far.
type Stats struct {
	Accesses uint64
	Hits     uint64
	Misses   uint64
}

// HitRate returns hits/accesses, or 1 when there were no accesses.
func (s Stats) HitRate() float64 {
	if s.Accesses == 0 {
		return 1
	}
	return float64(s.Hits) / float64(s.Accesses)
}

// Stats returns cumulative counters.
func (c *Cache) Stats() Stats { return c.stats }
