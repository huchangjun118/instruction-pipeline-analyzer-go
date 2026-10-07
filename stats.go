package ipa

// StallReason classifies pipeline stalls.
type StallReason string

const (
	StallLoadUse       StallReason = "load_use"        // load immediately used
	StallDataBypass    StallReason = "data_bypass"     // RAW wait despite bypass (deep EX)
	StallDataNoForward StallReason = "data_no_forward" // RAW without bypass
	StallControl       StallReason = "control"         // branch redirect
	StallICacheMiss    StallReason = "icache_miss"
	StallDCacheMiss    StallReason = "dcache_miss"
	StallMemPort       StallReason = "struct_mem_port" // memory port conflict
	StallEXUnit        StallReason = "struct_ex_unit"  // execution unit conflict
)

// Hazard kinds counted in Stats.Hazards.
const (
	HazardDataBypassed = "data_bypassed" // RAW solved by forwarding
	HazardDataStall    = "data_stall"    // RAW requiring a stall
	HazardLoadUse      = "load_use"      // load-use interlock
	HazardControl      = "control"       // branch redirect
	HazardStructural   = "structural"    // resource conflict
)

// Stats aggregates one trace analysis.
type Stats struct {
	Instructions uint64
	Cycles       uint64
	ParseErrors  uint64
	Stalls       map[StallReason]uint64
	Hazards      map[string]uint64
	ICacheHits   uint64
	ICacheMisses uint64
	DCacheHits   uint64
	DCacheMisses uint64
}

// NewStats returns an initialized Stats.
func NewStats() Stats {
	return Stats{
		Stalls:  map[StallReason]uint64{},
		Hazards: map[string]uint64{},
	}
}

// Merge adds other into s.
func (s *Stats) Merge(other Stats) {
	s.Instructions += other.Instructions
	s.Cycles += other.Cycles
	s.ParseErrors += other.ParseErrors
	s.ICacheHits += other.ICacheHits
	s.ICacheMisses += other.ICacheMisses
	s.DCacheHits += other.DCacheHits
	s.DCacheMisses += other.DCacheMisses
	for k, v := range other.Stalls {
		s.Stalls[k] += v
	}
	for k, v := range other.Hazards {
		s.Hazards[k] += v
	}
}

// TotalStalls returns the sum of all stall cycles.
func (s Stats) TotalStalls() uint64 {
	var n uint64
	for _, v := range s.Stalls {
		n += v
	}
	return n
}
