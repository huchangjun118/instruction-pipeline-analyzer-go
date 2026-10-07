package ipa

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runTrace simulates a trace and returns stats plus both timelines.
func runTrace(t *testing.T, cfg Config, trace string) (Stats, string, string) {
	t.Helper()
	var instrW, cycleW, errW strings.Builder
	sim, err := NewSim(cfg, SimOptions{
		InstrTimelineW: &instrW,
		CycleTimelineW: &cycleW,
		ParseErrorW:    &errW,
	})
	if err != nil {
		t.Fatalf("NewSim: %v", err)
	}
	sim.Run(NewParser(strings.NewReader(trace)))
	return sim.Stats(), instrW.String(), cycleW.String()
}

// spans extracts the stage spans of one instruction-timeline line.
func spans(t *testing.T, instrTL, op string, id int) map[string][2]int {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(instrTL), "\n") {
		if strings.HasPrefix(line, fmt.Sprintf("id=%d ", id)) {
			out := map[string][2]int{}
			re := regexp.MustCompile(`(\w+)=(\d+)-(\d+)`)
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				start, _ := strconv.Atoi(m[2])
				end, _ := strconv.Atoi(m[3])
				out[m[1]] = [2]int{start, end}
			}
			return out
		}
	}
	t.Fatalf("instruction id=%d not found in timeline:\n%s", id, instrTL)
	return nil
}

func TestSimIndependentInstructions(t *testing.T) {
	trace := `addr=0x1000 op=add w=r1 r=r2,r3
addr=0x1004 op=sub w=r4 r=r5,r6
addr=0x1008 op=xor w=r7 r=r8,r9
`
	st, instrTL, _ := runTrace(t, DefaultConfig(), trace)
	if st.Instructions != 3 {
		t.Fatalf("want 3 instructions, got %d", st.Instructions)
	}
	if st.TotalStalls() != 0 {
		t.Fatalf("want no stalls, got %v", st.Stalls)
	}
	s0 := spans(t, instrTL, "add", 0)
	want := map[string][2]int{"IF": {1, 2}, "ID": {2, 3}, "EX1": {3, 4}, "MEM": {4, 5}, "WB": {5, 6}}
	if !reflect.DeepEqual(s0, want) {
		t.Fatalf("want %v, got %v", want, s0)
	}
	if st.Cycles != 8 {
		t.Fatalf("want 8 cycles, got %d", st.Cycles)
	}
}

func TestSimDataHazardBypassed(t *testing.T) {
	trace := `addr=0x1000 op=add w=r1 r=r2,r3
addr=0x1004 op=sub w=r4 r=r1,r5
`
	st, _, _ := runTrace(t, DefaultConfig(), trace)
	if st.Hazards[HazardDataBypassed] != 1 {
		t.Fatalf("want 1 bypassed hazard, got %v", st.Hazards)
	}
	if st.TotalStalls() != 0 {
		t.Fatalf("bypass must avoid stalls, got %v", st.Stalls)
	}
}

func TestSimLoadUseStall(t *testing.T) {
	trace := `addr=0x1000 op=lw w=r1 r=r2 mem=0x2000 width=4
addr=0x1004 op=add w=r3 r=r1,r4
`
	st, instrTL, _ := runTrace(t, DefaultConfig(), trace)
	if st.Stalls[StallLoadUse] != 1 {
		t.Fatalf("want 1 load_use stall cycle, got %v", st.Stalls)
	}
	if st.Hazards[HazardLoadUse] != 1 {
		t.Fatalf("want 1 load_use hazard, got %v", st.Hazards)
	}
	s1 := spans(t, instrTL, "add", 1)
	if s1["ID"] != [2]int{3, 5} {
		t.Fatalf("consumer must wait 1 cycle in ID, got %v", s1["ID"])
	}
	// Load MEM and consumer EX1 must not overlap.
	s0 := spans(t, instrTL, "lw", 0)
	if s1["EX1"][0] <= s0["MEM"][1]-1 {
		t.Fatalf("consumer EX1 %v overlaps load MEM %v", s1["EX1"], s0["MEM"])
	}
}

func TestSimNoForwarding(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Forwarding = false
	trace := `addr=0x1000 op=add w=r1 r=r2,r3
addr=0x1004 op=sub w=r4 r=r1,r5
`
	st, instrTL, _ := runTrace(t, cfg, trace)
	if st.Stalls[StallDataNoForward] != 1 {
		t.Fatalf("want 1 data_no_forward stall cycle, got %v", st.Stalls)
	}
	if st.Hazards[HazardDataStall] != 1 {
		t.Fatalf("want 1 data_stall hazard, got %v", st.Hazards)
	}
	s1 := spans(t, instrTL, "sub", 1)
	s0 := spans(t, instrTL, "add", 0)
	// Consumer may leave ID once the producer reaches WB.
	if s1["EX1"][0] != s0["WB"][0] {
		t.Fatalf("consumer EX1 %v must coincide with producer WB %v", s1["EX1"], s0["WB"])
	}
}

func TestSimControlHazard(t *testing.T) {
	trace := `addr=0x1000 op=beq r=r1,r2
addr=0x1004 op=add w=r3 r=r4,r5
`
	st, instrTL, _ := runTrace(t, DefaultConfig(), trace)
	// Branch resolves at end of EX1: fetch restarts 2 cycles later.
	if st.Stalls[StallControl] != 2 {
		t.Fatalf("want 2 control stall cycles, got %v", st.Stalls)
	}
	if st.Hazards[HazardControl] != 1 {
		t.Fatalf("want 1 control hazard, got %v", st.Hazards)
	}
	s1 := spans(t, instrTL, "add", 1)
	if s1["IF"] != [2]int{4, 5} {
		t.Fatalf("fetch must restart at cycle 4, got %v", s1["IF"])
	}
}

func TestSimDCacheMissStall(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DCache = CacheConfig{Enabled: true, Capacity: 64, LineSize: 32, Assoc: 2, Replacement: "lru", MissPenalty: 3}
	trace := `addr=0x1000 op=lw w=r1 r=r2 mem=0x2000 width=4
addr=0x1004 op=lw w=r3 r=r4 mem=0x2004 width=4
`
	st, instrTL, _ := runTrace(t, cfg, trace)
	if st.DCacheMisses != 1 || st.DCacheHits != 1 {
		t.Fatalf("want 1 miss + 1 hit, got %d/%d", st.DCacheMisses, st.DCacheHits)
	}
	if st.Stalls[StallDCacheMiss] != 3 {
		t.Fatalf("want 3 dcache stall cycles, got %v", st.Stalls)
	}
	// Timeline must agree: first load occupies MEM for 1+3 cycles.
	s0 := spans(t, instrTL, "lw", 0)
	if got := s0["MEM"][1] - s0["MEM"][0]; got != 4 {
		t.Fatalf("load must occupy MEM 4 cycles, got %d", got)
	}
}

func TestSimICacheMissStall(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ICache = CacheConfig{Enabled: true, Capacity: 64, LineSize: 32, Assoc: 2, Replacement: "lru", MissPenalty: 2}
	trace := `addr=0x1000 op=add w=r1 r=r2,r3
addr=0x1004 op=sub w=r4 r=r5,r6
`
	st, instrTL, _ := runTrace(t, cfg, trace)
	if st.ICacheMisses != 1 || st.ICacheHits != 1 {
		t.Fatalf("want 1 miss + 1 hit, got %d/%d", st.ICacheMisses, st.ICacheHits)
	}
	if st.Stalls[StallICacheMiss] != 2 {
		t.Fatalf("want 2 icache stall cycles, got %v", st.Stalls)
	}
	s0 := spans(t, instrTL, "add", 0)
	if got := s0["IF"][1] - s0["IF"][0]; got != 3 {
		t.Fatalf("first fetch must occupy IF 3 cycles, got %d", got)
	}
}

func TestSimStructuralMemPort(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MemPorts = 1
	trace := `addr=0x1000 op=lw w=r1 r=r2 mem=0x2000 width=4
addr=0x1004 op=add w=r3 r=r4,r5
addr=0x1008 op=add w=r6 r=r7,r8
addr=0x100c op=add w=r9 r=r10,r11
`
	st, _, _ := runTrace(t, cfg, trace)
	if st.Stalls[StallMemPort] != 1 {
		t.Fatalf("want 1 mem-port stall cycle, got %v", st.Stalls)
	}
	if st.Hazards[HazardStructural] != 1 {
		t.Fatalf("want 1 structural hazard, got %v", st.Hazards)
	}
}

func TestSimStructuralEXUnit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PipelineDepth = 7 // EX1,EX2,EX3
	cfg.IssueWidth = 3
	cfg.EXUnits = 1
	trace := `addr=0x1000 op=add w=r1 r=r2,r3
addr=0x1004 op=sub w=r4 r=r5,r6
addr=0x1008 op=xor w=r7 r=r8,r9
`
	st, _, cycleTL := runTrace(t, cfg, trace)
	if st.Stalls[StallEXUnit] == 0 {
		t.Fatalf("want ex-unit stalls, got %v", st.Stalls)
	}
	assertStageCapacity(t, cycleTL, 3)
	assertEXOccupancy(t, cycleTL, 1)
}

// assertStageCapacity verifies no stage ever exceeds width occupants.
func assertStageCapacity(t *testing.T, cycleTL string, width int) {
	t.Helper()
	re := regexp.MustCompile(`\[([^\]]*)\]`)
	for _, line := range strings.Split(strings.TrimSpace(cycleTL), "\n") {
		for _, m := range re.FindAllStringSubmatch(line, -1) {
			if m[1] == "" {
				continue
			}
			if n := len(strings.Split(m[1], ",")); n > width {
				t.Fatalf("stage over capacity %d in %q", width, line)
			}
		}
	}
}

// assertEXOccupancy verifies the total EX population never exceeds max.
func assertEXOccupancy(t *testing.T, cycleTL string, max int) {
	t.Helper()
	stageRe := regexp.MustCompile(`(\w+)=\[([^\]]*)\]`)
	for _, line := range strings.Split(strings.TrimSpace(cycleTL), "\n") {
		total := 0
		for _, m := range stageRe.FindAllStringSubmatch(line, -1) {
			if strings.HasPrefix(m[1], "EX") && m[2] != "" {
				total += len(strings.Split(m[2], ","))
			}
		}
		if total > max {
			t.Fatalf("EX occupancy %d > %d in %q", total, max, line)
		}
	}
}

func TestSimDeterministic(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DCache = CacheConfig{Enabled: true, Capacity: 128, LineSize: 32, Assoc: 2, Replacement: "lru", MissPenalty: 4}
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "addr=%#x op=lw w=r1 r=r2 mem=%#x width=4\n", 0x1000+i*4, (i%17)*64)
		fmt.Fprintf(&b, "addr=%#x op=add w=r3 r=r1,r4\n", 0x2000+i*4)
		fmt.Fprintf(&b, "addr=%#x op=beq r=r3,r4\n", 0x3000+i*4)
	}
	st1, tl1, cl1 := runTrace(t, cfg, b.String())
	st2, tl2, cl2 := runTrace(t, cfg, b.String())
	if !reflect.DeepEqual(st1, st2) {
		t.Fatalf("stats differ between runs:\n%+v\n%+v", st1, st2)
	}
	if tl1 != tl2 || cl1 != cl2 {
		t.Fatal("timelines differ between runs")
	}
}

func TestSimConfigChangesTimeline(t *testing.T) {
	trace := `addr=0x1000 op=add w=r1 r=r2,r3
addr=0x1004 op=beq r=r1,r2
addr=0x1008 op=add w=r3 r=r4,r5
`
	cfg5 := DefaultConfig()
	cfg7 := DefaultConfig()
	cfg7.PipelineDepth = 7
	st5, tl5, _ := runTrace(t, cfg5, trace)
	st7, tl7, _ := runTrace(t, cfg7, trace)
	if st5.Cycles == st7.Cycles || tl5 == tl7 {
		t.Fatal("deeper pipeline must change the timeline")
	}
	// Deeper resolve stage -> larger branch penalty.
	if st7.Stalls[StallControl] <= st5.Stalls[StallControl] {
		t.Fatalf("deeper pipeline must increase control stalls: %v vs %v",
			st7.Stalls, st5.Stalls)
	}
}

func TestSimParseErrorsCounted(t *testing.T) {
	trace := `addr=0x1000 op=add w=r1 r=r2,r3
this is garbage
addr=0x1004 op=add w=r1 r=r2,r3
op=unknown
`
	st, _, _ := runTrace(t, DefaultConfig(), trace)
	if st.Instructions != 2 {
		t.Fatalf("want 2 instructions, got %d", st.Instructions)
	}
	if st.ParseErrors != 2 {
		t.Fatalf("want 2 parse errors, got %d", st.ParseErrors)
	}
}

func TestSimStreamingLargeTrace(t *testing.T) {
	const n = 100000
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "addr=%#x op=add w=r1 r=r2,r3\n", 0x1000+(i%1000)*4)
	}
	// No timeline writers: memory stays bounded.
	sim, err := NewSim(DefaultConfig(), SimOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sim.Run(NewParser(strings.NewReader(b.String())))
	st := sim.Stats()
	if st.Instructions != n {
		t.Fatalf("want %d instructions, got %d", n, st.Instructions)
	}
}

func TestSimIssueWidth2(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IssueWidth = 2
	trace := `addr=0x1000 op=add w=r1 r=r2,r3
addr=0x1004 op=sub w=r4 r=r5,r6
addr=0x1008 op=xor w=r7 r=r8,r9
addr=0x100c op=and w=r10 r=r11,r12
`
	st, _, cycleTL := runTrace(t, cfg, trace)
	if st.Instructions != 4 {
		t.Fatalf("want 4 instructions, got %d", st.Instructions)
	}
	assertStageCapacity(t, cycleTL, 2)
	// Two independent pairs issue in parallel: fewer cycles than width 1.
	st1, _, _ := runTrace(t, DefaultConfig(), trace)
	if st.Cycles >= st1.Cycles {
		t.Fatalf("width 2 (%d cycles) must beat width 1 (%d cycles)", st.Cycles, st1.Cycles)
	}
}

func TestSimInvalidConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PipelineDepth = 3
	if _, err := NewSim(cfg, SimOptions{}); err == nil {
		t.Fatal("want error for depth < 5")
	}
	cfg = DefaultConfig()
	cfg.BranchResolveStage = 10
	if _, err := NewSim(cfg, SimOptions{}); err == nil {
		t.Fatal("want error for resolve stage out of range")
	}
}

// TestSimInOrderUnderCacheMiss is a regression test: an instruction
// waiting out an icache-miss penalty in IF must not let younger
// instructions overtake it. Otherwise a producer can be stranded in IF
// behind its own consumer in a full ID stage, deadlocking the sim.
func TestSimInOrderUnderCacheMiss(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PipelineDepth = 6
	cfg.IssueWidth = 2
	cfg.MemPorts = 1
	cfg.ICache = CacheConfig{Enabled: true, Capacity: 256, LineSize: 32, Assoc: 2, Replacement: "lru", MissPenalty: 2}
	cfg.DCache = CacheConfig{Enabled: true, Capacity: 256, LineSize: 32, Assoc: 2, Replacement: "lru", MissPenalty: 4}
	// Producer load followed closely by a dependent branch, with
	// enough surrounding traffic to fill ID while the load's fetch
	// waits out an icache miss.
	var b strings.Builder
	fmt.Fprintln(&b, "addr=0x1000 op=add w=r1 r=r2,r3")
	fmt.Fprintln(&b, "addr=0x1004 op=add w=r2 r=r3,r4")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "addr=%#x op=lw w=r5 r=r2 mem=%#x width=4\n", 0x2000+i*4, 0x3000+i*4)
		fmt.Fprintf(&b, "addr=%#x op=beq r=r4,r5\n", 0x2100+i*4)
		fmt.Fprintf(&b, "addr=%#x op=add w=r6 r=r7,r8\n", 0x2200+i*4)
	}
	done := make(chan struct{})
	var st Stats
	var instrTL, cycleTL string
	go func() {
		defer close(done)
		st, instrTL, cycleTL = runTrace(t, cfg, b.String())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("simulation deadlocked")
	}
	if st.Instructions != 122 {
		t.Fatalf("want 122 instructions, got %d", st.Instructions)
	}
	assertStageCapacity(t, cycleTL, 2)
	_ = instrTL
}
