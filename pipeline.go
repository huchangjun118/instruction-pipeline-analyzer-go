package ipa

import (
	"fmt"
	"io"
	"strings"
)

// StageSpan is the half-open cycle range [Start, End) an instruction
// occupies one pipeline stage.
type StageSpan struct {
	Stage string
	Start int
	End   int
}

// InstrTimeline is the reconstructed schedule of one instruction.
type InstrTimeline struct {
	Rec    Record
	Spans  []StageSpan
	Stalls []StallReason // one entry per stalled cycle, attributed
}

// inst is an in-flight instruction.
type inst struct {
	id            int
	rec           Record
	enter         []int // entry cycle per stage index
	spans         []StageSpan
	stalls        []StallReason
	stallLeft     int // remaining miss-penalty cycles at current stage
	curStage      int
	hazardCounted bool
}

// Sim reconstructs a pipeline timeline from a record stream.
// It is deterministic: same input and config always produce the same
// timeline and metrics.
type Sim struct {
	cfg        Config
	depth      int
	width      int
	resolveIdx int
	memIdx     int
	stages     [][]*inst
	src        func() (Record, bool)
	srcDone    bool
	queue      []Record
	cycle      int
	stats      Stats
	icache     *Cache
	dcache     *Cache
	branches   []*inst // fetched but unresolved branches
	opts       SimOptions
	nextID     int
	memUsed    int // memory ports already used this cycle
}

// SimOptions carries optional outputs. Nil writers disable the
// corresponding stream, keeping memory bounded for large traces.
type SimOptions struct {
	InstrTimelineW io.Writer // per-instruction stage spans at retire
	CycleTimelineW io.Writer // per-cycle stage occupancy
	ParseErrorW    io.Writer // rejected lines with line numbers
}

// NewSim creates a simulator.
func NewSim(cfg Config, opts SimOptions) (*Sim, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	resolve := cfg.BranchResolveStage
	if resolve == 0 {
		resolve = 3
	}
	s := &Sim{
		cfg:        cfg,
		depth:      cfg.PipelineDepth,
		width:      cfg.IssueWidth,
		resolveIdx: resolve - 1,
		memIdx:     cfg.PipelineDepth - 2,
		stages:     make([][]*inst, cfg.PipelineDepth),
		stats:      NewStats(),
		icache:     NewCache(cfg.ICache),
		dcache:     NewCache(cfg.DCache),
		opts:       opts,
	}
	return s, nil
}

// stageName names stage index i: IF, ID, EX1..EXk, MEM, WB.
func (s *Sim) stageName(i int) string {
	switch {
	case i == 0:
		return "IF"
	case i == 1:
		return "ID"
	case i == s.memIdx:
		return "MEM"
	case i == s.depth-1:
		return "WB"
	default:
		return fmt.Sprintf("EX%d", i-1)
	}
}

// Step buffers one record for simulation. For large inputs prefer Run,
// which pulls records on demand and keeps memory bounded.
func (s *Sim) Step(rec Record) { s.queue = append(s.queue, rec) }

// Finish marks input complete and drains the pipeline.
func (s *Sim) Finish() {
	s.srcDone = true
	s.runLoop()
}

// Run pulls records from p until EOF, then drains the pipeline.
// Parse errors are counted and reported without aborting.
func (s *Sim) Run(p *Parser) {
	s.src = func() (Record, bool) {
		for p.Next() {
			if err := p.Err(); err != nil {
				s.stats.ParseErrors++
				if s.opts.ParseErrorW != nil {
					fmt.Fprintln(s.opts.ParseErrorW, err)
				}
				continue
			}
			return p.Record(), true
		}
		s.srcDone = true
		return Record{}, false
	}
	s.runLoop()
}

func (s *Sim) runLoop() {
	for !s.done() {
		s.stepCycle()
	}
	s.stats.Cycles = uint64(s.cycle)
	s.stats.ICacheHits, s.stats.ICacheMisses = s.icache.Counts()
	s.stats.DCacheHits, s.stats.DCacheMisses = s.dcache.Counts()
}

func (s *Sim) done() bool {
	if !s.srcDone || len(s.queue) > 0 {
		return false
	}
	for _, st := range s.stages {
		if len(st) > 0 {
			return false
		}
	}
	return true
}

// Stats returns aggregated metrics.
func (s *Sim) Stats() Stats { return s.stats }

func (s *Sim) next() (Record, bool) {
	if len(s.queue) > 0 {
		r := s.queue[0]
		s.queue = s.queue[1:]
		return r, true
	}
	if s.src != nil && !s.srcDone {
		return s.src()
	}
	return Record{}, false
}

// stepCycle advances the whole pipeline by one cycle.
func (s *Sim) stepCycle() {
	s.cycle++
	c := s.cycle
	s.memUsed = 0
	D := s.depth

	// WB: retire all occupants.
	for _, x := range s.stages[D-1] {
		s.leave(x, D-1, c)
		s.retire(x)
	}
	s.stages[D-1] = s.stages[D-1][:0]

	// MEM.
	s.advance(s.memIdx, c, StallDCacheMiss, nil)

	// EX stages, last to first.
	for i := D - 3; i >= 2; i-- {
		s.advance(i, c, "", nil)
	}

	// ID -> EX1: hazard and execution-unit checks.
	s.advanceID(c)

	// IF -> ID.
	s.advance(0, c, StallICacheMiss, nil)

	// Fetch new instructions into IF.
	s.fetch(c)

	s.emitCycle(c)
}

// advance moves occupants of stage `stage` into the next stage while
// capacity allows. waitReason, when non-empty, is the stall reason for
// occupants still waiting out a cache-miss penalty.
func (s *Sim) advance(stage, c int, waitReason StallReason, ok func(*inst) (StallReason, bool)) {
	from := s.stages[stage]
	to := stage + 1
	kept := from[:0]
	blocked := false
	for _, x := range from {
		if x.stallLeft > 0 {
			// Waiting out a cache-miss penalty. The pipeline is
			// in-order: younger occupants of this stage may not
			// overtake x, otherwise a producer could be stranded
			// behind its own consumer (deadlock).
			x.stallLeft--
			s.addStall(x, waitReason)
			blocked = true
			kept = append(kept, x)
			continue
		}
		if blocked || len(s.stages[to]) >= s.width {
			blocked = true
			kept = append(kept, x)
			continue
		}
		if ok != nil {
			if reason, stop := ok(x); stop {
				s.addStall(x, reason)
				blocked = true
				kept = append(kept, x)
				continue
			}
		}
		s.leave(x, stage, c)
		s.enterStage(x, to, c)
		s.stages[to] = append(s.stages[to], x)
	}
	s.stages[stage] = kept
}

// advanceID handles the ID -> EX1 boundary with hazard detection.
func (s *Sim) advanceID(c int) {
	s.advance(1, c, "", func(x *inst) (StallReason, bool) {
		if reason, block := s.hazardCheck(x); block {
			return reason, true
		}
		if s.cfg.EXUnits > 0 && s.exOccupancy() >= s.cfg.EXUnits {
			s.stats.Hazards[HazardStructural]++
			return StallEXUnit, true
		}
		return "", false
	})
}

// hazardCheck reports whether x must stay in ID this cycle due to a
// data dependency, and why. The hazard itself is counted once.
func (s *Sim) hazardCheck(x *inst) (StallReason, bool) {
	depFound := false
	block := false
	blockLoad := false
	forwarding := s.cfg.Forwarding
	for _, r := range x.rec.Reads {
		if r == "x0" || r == "zero" {
			continue
		}
		p := s.youngestProducer(x, r)
		if p == nil {
			continue
		}
		depFound = true
		// With forwarding the consumer needs the value at the start of
		// its first EX cycle, so the producer must have moved PAST the
		// stage where its result becomes ready. Without forwarding the
		// register file is read in ID, which may coincide with the
		// producer's WB cycle (write first half, read second half).
		ready := s.depth - 3 // ALU result ready at end of last EX stage
		if p.rec.Class == ClassLoad {
			ready = s.memIdx // load data ready at end of MEM
		}
		if !forwarding {
			if p.curStage < s.depth-1 {
				block = true
			}
			continue
		}
		if p.curStage <= ready {
			block = true
			if p.rec.Class == ClassLoad {
				blockLoad = true
			}
		}
	}
	if !x.hazardCounted {
		x.hazardCounted = true
		if depFound {
			switch {
			case block && blockLoad:
				s.stats.Hazards[HazardLoadUse]++
			case block:
				s.stats.Hazards[HazardDataStall]++
			default:
				s.stats.Hazards[HazardDataBypassed]++
			}
		}
	}
	if !block {
		return "", false
	}
	switch {
	case !forwarding:
		return StallDataNoForward, true
	case blockLoad:
		return StallLoadUse, true
	default:
		return StallDataBypass, true
	}
}

// youngestProducer finds the youngest in-flight instruction older than
// x that writes reg, or nil.
func (s *Sim) youngestProducer(x *inst, reg string) *inst {
	for st := 1; st < s.depth; st++ {
		occ := s.stages[st]
		for i := len(occ) - 1; i >= 0; i-- {
			y := occ[i]
			if y.id >= x.id {
				continue
			}
			if writesReg(y, reg) {
				return y
			}
		}
	}
	return nil
}

func writesReg(x *inst, reg string) bool {
	for _, w := range x.rec.Writes {
		if w == reg {
			return true
		}
	}
	return false
}

// exOccupancy counts instructions currently in EX stages.
func (s *Sim) exOccupancy() int {
	n := 0
	for i := 2; i <= s.depth-3; i++ {
		n += len(s.stages[i])
	}
	return n
}

// fetch pulls new records into IF, honoring branch redirects, memory
// ports and issue width.
func (s *Sim) fetch(c int) {
	if len(s.branches) > 0 {
		// Fetch redirected: wait until the branch resolves.
		s.addStall(s.branches[0], StallControl)
		return
	}
	for len(s.stages[0]) < s.width {
		if s.cfg.MemPorts > 0 && s.memUsed >= s.cfg.MemPorts {
			s.stats.Stalls[StallMemPort]++
			s.stats.Hazards[HazardStructural]++
			return
		}
		rec, ok := s.next()
		if !ok {
			return
		}
		x := &inst{id: s.nextID, rec: rec, enter: make([]int, s.depth)}
		s.nextID++
		for i := range x.enter {
			x.enter[i] = -1
		}
		s.memUsed++ // instruction fetch consumes a memory port
		if s.icache != nil && !s.icache.Access(rec.Addr) {
			x.stallLeft = s.cfg.ICache.MissPenalty
		}
		if rec.Class == ClassBranch {
			s.branches = append(s.branches, x)
			s.stats.Hazards[HazardControl]++
		}
		x.curStage = 0
		x.enter[0] = c
		s.stages[0] = append(s.stages[0], x)
	}
}

// leave closes the span of x at stage `stage` in cycle c.
func (s *Sim) leave(x *inst, stage, c int) {
	x.spans = append(x.spans, StageSpan{Stage: s.stageName(stage), Start: x.enter[stage], End: c})
	if stage == s.resolveIdx && x.rec.Class == ClassBranch {
		// Branch resolved: fetch may restart. Oldest first.
		for i, b := range s.branches {
			if b == x {
				s.branches = append(s.branches[:i], s.branches[i+1:]...)
				break
			}
		}
	}
}

// enterStage records x entering stage `stage` in cycle c and performs
// the data-cache access for MEM.
func (s *Sim) enterStage(x *inst, stage, c int) {
	x.curStage = stage
	x.enter[stage] = c
	if stage == s.memIdx && x.rec.HasMem {
		// The data-memory access consumes a port whether or not a
		// cache is modeled.
		s.memUsed++
		if s.dcache != nil && !s.dcache.Access(x.rec.MemAddr) {
			x.stallLeft = s.cfg.DCache.MissPenalty
		}
	}
}

// retire completes x at the end of WB.
func (s *Sim) retire(x *inst) {
	s.stats.Instructions++
	if s.opts.InstrTimelineW != nil {
		var b strings.Builder
		fmt.Fprintf(&b, "id=%d addr=%#x op=%s", x.id, x.rec.Addr, x.rec.Op)
		for _, sp := range x.spans {
			fmt.Fprintf(&b, " %s=%d-%d", sp.Stage, sp.Start, sp.End)
		}
		if len(x.stalls) > 0 {
			b.WriteString(" stalls=")
			for i, st := range x.stalls {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(string(st))
			}
		}
		fmt.Fprintln(s.opts.InstrTimelineW, b.String())
	}
}

// emitCycle writes the per-cycle occupancy view.
func (s *Sim) emitCycle(c int) {
	if s.opts.CycleTimelineW == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "cycle %d:", c)
	for i, occ := range s.stages {
		fmt.Fprintf(&b, " %s=[", s.stageName(i))
		for j, x := range occ {
			if j > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "i%d", x.id)
		}
		b.WriteByte(']')
	}
	fmt.Fprintln(s.opts.CycleTimelineW, b.String())
}

// addStall attributes one stalled cycle to x and the global stats.
func (s *Sim) addStall(x *inst, reason StallReason) {
	if reason == "" {
		return
	}
	x.stalls = append(x.stalls, reason)
	s.stats.Stalls[reason]++
}
