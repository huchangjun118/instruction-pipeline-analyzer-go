// Package pipeline reconstructs a cycle-accurate in-order pipeline timeline
// from a stream of executed instructions.
//
// Pipeline shape for depth D (>= 4) and issue width W (>= 1):
//
//	stage 0      IF   instruction fetch (icache access)
//	stage 1      ID   decode + register read + hazard detection
//	stage 2      EX1  execute; branches resolve when leaving this stage
//	stages 3..D-2 EXn  (only when D > 4)
//	stage D-2    MEM  data memory access (dcache access)
//	stage D-1    WB   writeback
//
// For D == 4, EX and MEM coincide in stage 2. Memory is treated as
// physically addressed and coherent with the pipeline: a dcache access
// happens on the cycle an instruction enters MEM, an icache access on the
// cycle an instruction is fetched.
//
// Stall attribution rules:
//   - a load whose result is consumed before it reaches WB stalls the
//     consumer in ID: StallLoadUse (the classic load-use hazard);
//   - with Forwarding enabled, ALU->consumer RAW dependencies never stall
//     and are counted in Stats.Forwarded; with Forwarding disabled they
//     stall in ID as StallDataRAW until the producer reaches WB;
//   - a taken branch or any jump blocks fetch until it leaves the resolve
//     stage, producing StallControl bubbles (fetch redirection);
//   - an icache miss keeps the instruction in IF for MissPenalty extra
//     cycles (StallICacheMiss); a dcache miss keeps a load/store in MEM
//     (StallDCacheMiss);
//   - when memory ports or execution units are exhausted the loser waits
//     with StallStructural; back-pressure from any stalled stage propagates
//     the downstream root cause to upstream instructions.
//
// The simulator is streaming: it pulls records one at a time and keeps only
// pipeline state plus aggregate counters, so memory use is bounded by the
// pipeline configuration, not the trace length. Timelines are emitted
// through OnEvent as they are produced; they are retained in memory only
// when Config.KeepTimeline is set.
package pipeline

import (
	"errors"
	"fmt"
	"io"

	"github.com/huchangjun118/instruction-pipeline-analyzer-go/internal/cache"
	"github.com/huchangjun118/instruction-pipeline-analyzer-go/internal/trace"
)

// StallReason identifies why an instruction could not advance.
type StallReason int

const (
	StallNone StallReason = iota
	StallLoadUse
	StallDataRAW
	StallControl
	StallICacheMiss
	StallDCacheMiss
	StallStructural
)

// AllReasons lists every stall reason in stable order.
var AllReasons = []StallReason{
	StallLoadUse, StallDataRAW, StallControl,
	StallICacheMiss, StallDCacheMiss, StallStructural,
}

func (r StallReason) String() string {
	switch r {
	case StallNone:
		return "none"
	case StallLoadUse:
		return "load-use"
	case StallDataRAW:
		return "data-raw"
	case StallControl:
		return "control"
	case StallICacheMiss:
		return "icache-miss"
	case StallDCacheMiss:
		return "dcache-miss"
	case StallStructural:
		return "structural"
	}
	return "unknown"
}

// Config controls the simulated machine.
type Config struct {
	Depth        int // pipeline stages, >= 4
	Width        int // issue width, >= 1
	MemPorts     int // memory ports shared by IF and MEM; 0 = unlimited
	ALUUnits     int // execution units usable per cycle; 0 = unlimited
	Forwarding   bool
	ICache       cache.Config
	DCache       cache.Config
	KeepTimeline bool // retain the full timeline in memory
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if c.Depth < 4 {
		return fmt.Errorf("pipeline depth must be >= 4, got %d", c.Depth)
	}
	if c.Width < 1 {
		return fmt.Errorf("issue width must be >= 1, got %d", c.Width)
	}
	if c.MemPorts < 0 {
		return fmt.Errorf("memory ports must be >= 0, got %d", c.MemPorts)
	}
	if c.ALUUnits < 0 {
		return fmt.Errorf("ALU units must be >= 0, got %d", c.ALUUnits)
	}
	if err := c.ICache.Validate(); err != nil {
		return fmt.Errorf("icache: %w", err)
	}
	if err := c.DCache.Validate(); err != nil {
		return fmt.Errorf("dcache: %w", err)
	}
	return nil
}

// StageOccupancy records which instruction occupied a stage in one cycle.
type StageOccupancy struct {
	Cycle    int
	Stage    int
	Seq      uint64 // instruction sequence number, 1-based in fetch order
	Addr     uint64
	Mnemonic string
	Stall    StallReason // why the instruction held this stage (None = normal flow)
}

// Stats aggregates the whole run.
type Stats struct {
	Instructions uint64
	Cycles       uint64
	Stalls       map[StallReason]uint64 // stall cycles per reason
	Forwarded    uint64                 // RAW dependencies resolved by bypassing
	ICache       cache.Stats
	DCache       cache.Stats
}

// CPI returns cycles per instruction, or 0 for an empty run.
func (s Stats) CPI() float64 {
	if s.Instructions == 0 {
		return 0
	}
	return float64(s.Cycles) / float64(s.Instructions)
}

// StallCycles returns the total stall cycles across all reasons.
func (s Stats) StallCycles() uint64 {
	var n uint64
	for _, v := range s.Stalls {
		n += v
	}
	return n
}

type inst struct {
	seq        uint64
	rec        *trace.Record
	stage      int
	reason     StallReason // why it held its stage (set when a stall is decided)
	stallLeft  int         // remaining forced stall cycles from a cache miss
	missReason StallReason
	fwdSeen    map[uint64]bool // producer seqs already counted as forwarded
}

// Simulator consumes records and produces the timeline.
type Simulator struct {
	cfg      Config
	icache   *cache.Cache
	dcache   *cache.Cache
	stages   [][]*inst
	writerOf map[string]*inst

	cycle      int
	seqCounter uint64
	pending    *inst
	eof        bool
	err        error

	stats    Stats
	timeline []StageOccupancy

	// OnEvent, if set, receives every occupancy record as it is produced.
	OnEvent func(StageOccupancy)

	// Cancel, if set, is polled once per simulated cycle; returning true
	// aborts the run promptly with ErrCancelled.
	Cancel func() bool
}

// ErrCancelled marks a run aborted through the Cancel hook.
var ErrCancelled = errors.New("simulation cancelled")

// New creates a simulator for cfg.
func New(cfg Config) (*Simulator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	s := &Simulator{
		cfg:      cfg,
		icache:   cache.New(cfg.ICache),
		dcache:   cache.New(cfg.DCache),
		stages:   make([][]*inst, cfg.Depth),
		writerOf: map[string]*inst{},
		stats: Stats{
			Stalls: map[StallReason]uint64{},
		},
	}
	for _, r := range AllReasons {
		s.stats.Stalls[r] = 0
	}
	return s, nil
}

// StageNames returns the human-readable name of each stage.
func (s *Simulator) StageNames() []string {
	d := s.cfg.Depth
	names := make([]string, d)
	names[0] = "IF"
	names[1] = "ID"
	for i := 2; i < d-1; i++ {
		if i == d-2 {
			names[i] = "MEM"
		} else {
			names[i] = fmt.Sprintf("EX%d", i-1)
		}
	}
	names[d-1] = "WB"
	return names
}

// Err returns a fatal input error, if one stopped the run.
func (s *Simulator) Err() error { return s.err }

// Timeline returns the retained occupancy entries (nil unless
// Config.KeepTimeline was set).
func (s *Simulator) Timeline() []StageOccupancy { return s.timeline }

// Stats returns aggregated counters so far.
func (s *Simulator) Stats() Stats { return s.stats }

// Run pulls records from p until exhaustion and returns the final stats.
// Parse errors are reported through onErr and never abort the run.
func (s *Simulator) Run(p *trace.Parser, onErr func(*trace.ParseError)) Stats {
	s.pull(p, onErr)
	s.fetch(nil, nil, 0, p, onErr) // prime IF for cycle 1
	for {
		if s.eof && s.pending == nil && s.allEmpty() {
			break
		}
		if s.Cancel != nil && s.Cancel() {
			s.err = ErrCancelled
			break
		}
		s.cycle++
		s.emit()
		s.advance(p, onErr)
	}
	s.stats.Cycles = uint64(s.cycle)
	s.stats.ICache = s.icache.Stats()
	s.stats.DCache = s.dcache.Stats()
	return s.stats
}

func (s *Simulator) allEmpty() bool {
	for _, st := range s.stages {
		if len(st) > 0 {
			return false
		}
	}
	return true
}

// pull fetches the next record into the pending slot.
func (s *Simulator) pull(p *trace.Parser, onErr func(*trace.ParseError)) {
	if s.pending != nil || s.eof {
		return
	}
	for {
		rec, perr, err := p.Next()
		if err == io.EOF {
			s.eof = true
			return
		}
		if err != nil {
			s.eof = true
			s.err = err
			return
		}
		if perr != nil {
			if onErr != nil {
				onErr(perr)
			}
			continue
		}
		s.seqCounter++
		s.pending = &inst{seq: s.seqCounter, rec: rec}
		s.stats.Instructions++
		return
	}
}

// emit records the occupancy of the current cycle.
func (s *Simulator) emit() {
	for i, st := range s.stages {
		for _, in := range st {
			ev := StageOccupancy{
				Cycle:    s.cycle,
				Stage:    i,
				Seq:      in.seq,
				Addr:     in.rec.Addr,
				Mnemonic: in.rec.Mnemonic,
				Stall:    in.reason,
			}
			if s.OnEvent != nil {
				s.OnEvent(ev)
			}
			if s.cfg.KeepTimeline {
				s.timeline = append(s.timeline, ev)
			}
		}
	}
}

// redirects reports whether the record redirects the fetch stream.
func redirects(rec *trace.Record) bool {
	return rec.Class == trace.ClassJump ||
		(rec.Class == trace.ClassBranch && rec.Taken)
}

// advance computes and applies all movements for one cycle.
func (s *Simulator) advance(p *trace.Parser, onErr func(*trace.ParseError)) {
	d := s.cfg.Depth
	stay := make([][]*inst, d)
	arrivals := make([][]*inst, d)

	// WB completes: retire register writers.
	for _, in := range s.stages[d-1] {
		for _, r := range in.rec.Writes {
			if s.writerOf[r] == in {
				delete(s.writerOf, r)
			}
		}
	}

	// Stages D-2 down to 0: try to move each instruction one stage up.
	for i := d - 2; i >= 0; i-- {
		for _, in := range s.stages[i] {
			if in.stallLeft > 0 {
				in.stallLeft--
				in.reason = in.missReason
				s.stats.Stalls[in.missReason]++
				stay[i] = append(stay[i], in)
				continue
			}
			if reason, blocked := s.advanceBlocked(in, i, stay, arrivals); blocked {
				in.reason = reason
				s.stats.Stalls[reason]++
				stay[i] = append(stay[i], in)
				continue
			}
			in.reason = StallNone
			in.stage = i + 1
			arrivals[i+1] = append(arrivals[i+1], in)
		}
	}

	// Data cache accesses for instructions entering MEM.
	memStage := d - 2
	memAccesses := 0
	for _, in := range arrivals[memStage] {
		if in.rec.HasMem {
			memAccesses++
			if !s.dcache.Access(in.rec.MemAddr, in.rec.MemWidth).Hit {
				in.stallLeft = s.dcache.MissPenalty()
				in.missReason = StallDCacheMiss
			}
		}
	}

	// Register writers become visible to hazards when entering ID.
	for _, in := range arrivals[1] {
		for _, r := range in.rec.Writes {
			s.writerOf[r] = in
		}
	}

	s.fetch(stay, arrivals, memAccesses, p, onErr)

	for i := 0; i < d; i++ {
		s.stages[i] = append(stay[i], arrivals[i]...)
	}
}

// advanceBlocked decides whether in may leave stage i this cycle.
func (s *Simulator) advanceBlocked(in *inst, i int, stay, arrivals [][]*inst) (StallReason, bool) {
	next := i + 1
	if len(stay[next])+len(arrivals[next]) >= s.cfg.Width {
		// Back-pressure: inherit the root cause from whoever is stuck ahead.
		if len(stay[next]) > 0 && stay[next][0].reason != StallNone {
			return stay[next][0].reason, true
		}
		return StallStructural, true
	}
	if i == 1 { // ID -> EX1 boundary
		if reason, blocked := s.hazardBlocked(in); blocked {
			return reason, true
		}
		if s.cfg.ALUUnits > 0 && len(stay[2])+len(arrivals[2]) >= s.cfg.ALUUnits {
			return StallStructural, true
		}
	}
	return StallNone, false
}

// hazardBlocked checks RAW dependencies of an instruction sitting in ID.
func (s *Simulator) hazardBlocked(in *inst) (StallReason, bool) {
	wbStage := s.cfg.Depth - 1
	for _, r := range in.rec.Reads {
		p := s.writerOf[r]
		if p == nil {
			continue
		}
		if p.rec.Class == trace.ClassLoad {
			// Load data is only usable once the load reaches WB.
			if p.stage < wbStage {
				return StallLoadUse, true
			}
			continue
		}
		if s.cfg.Forwarding {
			if p.stage < wbStage {
				if in.fwdSeen == nil {
					in.fwdSeen = map[uint64]bool{}
				}
				if !in.fwdSeen[p.seq] {
					in.fwdSeen[p.seq] = true
					s.stats.Forwarded++
				}
			}
			continue
		}
		if p.stage < wbStage {
			return StallDataRAW, true
		}
	}
	return StallNone, false
}

// fetch fills the IF stage for the next cycle. stay/arrivals describe the
// next-cycle state already decided by the move phase; they may be nil when
// priming an empty pipeline.
func (s *Simulator) fetch(stay, arrivals [][]*inst, memAccesses int, p *trace.Parser, onErr func(*trace.ParseError)) {
	w := s.cfg.Width
	occupied := 0
	if stay != nil {
		occupied = len(stay[0]) + len(arrivals[0])
	}
	fetches := 0
	for occupied < w {
		// Pull first: bubbles are only counted when an instruction is
		// actually waiting to be fetched, never for an exhausted trace.
		s.pull(p, onErr)
		if s.pending == nil {
			return // input exhausted
		}
		if s.controlBlocked(stay, arrivals) {
			s.stats.Stalls[StallControl]++
			return
		}
		if s.cfg.MemPorts > 0 && memAccesses+fetches >= s.cfg.MemPorts {
			s.stats.Stalls[StallStructural]++
			return
		}
		in := s.pending
		s.pending = nil
		in.stage = 0
		if !s.icache.Access(in.rec.Addr, 4).Hit {
			in.stallLeft = s.icache.MissPenalty()
			in.missReason = StallICacheMiss
		}
		if arrivals == nil {
			s.stages[0] = append(s.stages[0], in)
		} else {
			arrivals[0] = append(arrivals[0], in)
		}
		occupied++
		fetches++
	}
}

// controlBlocked reports whether a redirecting branch/jump will sit at or
// before the resolve stage in the next cycle, forcing a fetch bubble.
func (s *Simulator) controlBlocked(stay, arrivals [][]*inst) bool {
	if stay == nil {
		return false
	}
	resolve := 2
	if m := s.cfg.Depth - 2; m < resolve {
		resolve = m
	}
	for i := 0; i <= resolve; i++ {
		for _, in := range stay[i] {
			if redirects(in.rec) {
				return true
			}
		}
		for _, in := range arrivals[i] {
			if redirects(in.rec) {
				return true
			}
		}
	}
	return false
}
