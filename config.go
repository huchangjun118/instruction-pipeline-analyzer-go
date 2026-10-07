package ipa

import "fmt"

// CacheConfig describes one cache level.
type CacheConfig struct {
	Enabled     bool
	Capacity    int    // bytes
	LineSize    int    // bytes per line
	Assoc       int    // ways
	Replacement string // "lru" or "fifo"
	MissPenalty int    // extra cycles per miss
}

// Config controls pipeline reconstruction and cache modeling.
type Config struct {
	PipelineDepth      int // number of stages, >= 5
	IssueWidth         int // instructions issued per cycle, >= 1
	Forwarding         bool
	BranchResolveStage int // 1-based stage where branches resolve; 0 = default (EX1)
	MemPorts           int // memory ports shared by IF and MEM; 0 = unlimited
	EXUnits            int // execution units; 0 = unlimited
	ICache             CacheConfig
	DCache             CacheConfig
}

// DefaultConfig returns a classic 5-stage single-issue configuration.
func DefaultConfig() Config {
	return Config{
		PipelineDepth: 5,
		IssueWidth:    1,
		Forwarding:    true,
		MemPorts:      0,
		EXUnits:       0,
		ICache:        CacheConfig{Enabled: false},
		DCache:        CacheConfig{Enabled: false},
	}
}

// Validate checks the configuration for consistency.
func (c Config) Validate() error {
	if c.PipelineDepth < 5 {
		return fmt.Errorf("pipeline depth must be >= 5, got %d", c.PipelineDepth)
	}
	if c.IssueWidth < 1 {
		return fmt.Errorf("issue width must be >= 1, got %d", c.IssueWidth)
	}
	resolve := c.BranchResolveStage
	if resolve == 0 {
		resolve = 3
	}
	if resolve < 3 || resolve > c.PipelineDepth-2 {
		return fmt.Errorf("branch resolve stage %d out of range [3, %d]",
			resolve, c.PipelineDepth-2)
	}
	if c.MemPorts < 0 {
		return fmt.Errorf("memory ports must be >= 0")
	}
	if c.EXUnits < 0 {
		return fmt.Errorf("execution units must be >= 0")
	}
	if err := ValidateCacheConfig(c.ICache); err != nil {
		return fmt.Errorf("icache: %w", err)
	}
	if err := ValidateCacheConfig(c.DCache); err != nil {
		return fmt.Errorf("dcache: %w", err)
	}
	return nil
}
