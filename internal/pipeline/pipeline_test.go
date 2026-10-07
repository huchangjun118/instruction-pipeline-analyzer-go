package pipeline

import (
	"strings"
	"testing"

	"github.com/huchangjun118/instruction-pipeline-analyzer-go/internal/cache"
	"github.com/huchangjun118/instruction-pipeline-analyzer-go/internal/trace"
)

func run(t *testing.T, cfg Config, input string) (Stats, []StageOccupancy) {
	t.Helper()
	cfg.KeepTimeline = true
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var perrs []*trace.ParseError
	st := s.Run(trace.NewParser(strings.NewReader(input)), func(e *trace.ParseError) {
		perrs = append(perrs, e)
	})
	if len(perrs) != 0 {
		t.Fatalf("parse errors: %v", perrs)
	}
	if s.Err() != nil {
		t.Fatalf("fatal: %v", s.Err())
	}
	return st, s.Timeline()
}

func baseCfg() Config {
	return Config{Depth: 5, Width: 1, Forwarding: true}
}

// checkOccupancyInvariant verifies no stage is occupied beyond issue width
// in any cycle.
func checkOccupancyInvariant(t *testing.T, tl []StageOccupancy, width int) {
	t.Helper()
	occ := map[[2]int]int{}
	for _, ev := range tl {
		k := [2]int{ev.Cycle, ev.Stage}
		occ[k]++
		if occ[k] > width {
			t.Fatalf("stage %d cycle %d occupied %d times (width %d)",
				ev.Stage, ev.Cycle, occ[k], width)
		}
	}
}

func TestIndependentInstructionsNoStalls(t *testing.T) {
	st, tl := run(t, baseCfg(), `0x1000 add writes=r1 reads=r2,r3
0x1004 sub writes=r4 reads=r5,r6
0x1008 and writes=r7 reads=r8,r9
`)
	if st.Instructions != 3 {
		t.Fatalf("instructions = %d", st.Instructions)
	}
	if st.Cycles != 7 { // 3 instr + 4 drain
		t.Errorf("cycles = %d, want 7", st.Cycles)
	}
	if st.StallCycles() != 0 {
		t.Errorf("unexpected stalls: %v", st.Stalls)
	}
	checkOccupancyInvariant(t, tl, 1)
	// First instruction must walk IF@1 ID@2 EX@3 MEM@4 WB@5.
	want := []int{1, 2, 3, 4, 5}
	for stage, cyc := range want {
		found := false
		for _, ev := range tl {
			if ev.Seq == 1 && ev.Stage == stage && ev.Cycle == cyc {
				found = true
			}
		}
		if !found {
			t.Errorf("seq1 missing stage %d at cycle %d", stage, cyc)
		}
	}
}

func TestLoadUseStall(t *testing.T) {
	st, _ := run(t, baseCfg(), `0x1000 lw writes=r1 reads=r2 mem=0x2000:4
0x1004 add reads=r1,r3 writes=r4
`)
	if st.Stalls[StallLoadUse] != 1 {
		t.Errorf("load-use stalls = %d, want 1", st.Stalls[StallLoadUse])
	}
	if st.Cycles != 7 { // 2 instr + 4 drain + 1 stall
		t.Errorf("cycles = %d, want 7", st.Cycles)
	}
}

func TestForwardingAvoidsStall(t *testing.T) {
	st, _ := run(t, baseCfg(), `0x1000 add writes=r1 reads=r2,r3
0x1004 sub reads=r1,r5 writes=r6
`)
	if st.Forwarded != 1 {
		t.Errorf("forwarded = %d, want 1", st.Forwarded)
	}
	if st.StallCycles() != 0 {
		t.Errorf("forwardable dependency must not stall: %v", st.Stalls)
	}
}

func TestNoForwardingCausesRAWStall(t *testing.T) {
	cfg := baseCfg()
	cfg.Forwarding = false
	st, _ := run(t, cfg, `0x1000 add writes=r1 reads=r2,r3
0x1004 sub reads=r1,r5 writes=r6
`)
	if st.Stalls[StallDataRAW] == 0 {
		t.Error("disabling forwarding must produce data-raw stalls")
	}
	if st.Forwarded != 0 {
		t.Error("no forwarding may be counted when disabled")
	}
}

func TestTakenBranchControlStall(t *testing.T) {
	st, _ := run(t, baseCfg(), `0x1000 beq reads=r1,r2 taken
0x2000 add writes=r3 reads=r4
`)
	if st.Stalls[StallControl] != 2 {
		t.Errorf("control stalls = %d, want 2", st.Stalls[StallControl])
	}
}

func TestUntakenBranchNoStall(t *testing.T) {
	st, _ := run(t, baseCfg(), `0x1000 beq reads=r1,r2
0x1004 add writes=r3 reads=r4
`)
	if st.Stalls[StallControl] != 0 {
		t.Errorf("untaken branch must not stall: %v", st.Stalls)
	}
}

func TestICacheMissStall(t *testing.T) {
	cfg := baseCfg()
	cfg.ICache = cache.Config{SizeBytes: 64, Assoc: 1, LineBytes: 16, MissPenalty: 3}
	st, tl := run(t, cfg, "0x1000 nop\n")
	if st.Stalls[StallICacheMiss] != 3 {
		t.Errorf("icache stalls = %d, want 3", st.Stalls[StallICacheMiss])
	}
	if st.ICache.Misses != 1 {
		t.Errorf("icache misses = %d, want 1", st.ICache.Misses)
	}
	// The instruction must occupy IF for 1+3 cycles, and every stall cycle
	// counted in stats must appear in the timeline.
	ifCycles := 0
	stallEntries := 0
	for _, ev := range tl {
		if ev.Stage == 0 {
			ifCycles++
		}
		if ev.Stall == StallICacheMiss {
			stallEntries++
		}
	}
	if ifCycles != 4 {
		t.Errorf("IF occupancy = %d cycles, want 4", ifCycles)
	}
	if uint64(stallEntries) != st.Stalls[StallICacheMiss] {
		t.Errorf("timeline stall entries %d != stats %d", stallEntries, st.Stalls[StallICacheMiss])
	}
}

func TestDCacheMissStallAndTimelineConsistency(t *testing.T) {
	cfg := baseCfg()
	cfg.DCache = cache.Config{SizeBytes: 64, Assoc: 1, LineBytes: 16, MissPenalty: 5}
	st, tl := run(t, cfg, "0x1000 lw writes=r1 reads=r2 mem=0x2000:4\n")
	if st.Stalls[StallDCacheMiss] != 5 {
		t.Errorf("dcache stalls = %d, want 5", st.Stalls[StallDCacheMiss])
	}
	if st.DCache.Misses != 1 {
		t.Errorf("dcache misses = %d, want 1", st.DCache.Misses)
	}
	memCycles := 0
	stallEntries := 0
	for _, ev := range tl {
		if ev.Stage == 3 { // MEM for depth 5
			memCycles++
		}
		if ev.Stall == StallDCacheMiss {
			stallEntries++
		}
	}
	if memCycles != 6 {
		t.Errorf("MEM occupancy = %d cycles, want 6", memCycles)
	}
	if uint64(stallEntries) != st.Stalls[StallDCacheMiss] {
		t.Errorf("timeline %d != stats %d", stallEntries, st.Stalls[StallDCacheMiss])
	}
}

func TestStructuralMemPortStall(t *testing.T) {
	cfg := baseCfg()
	cfg.MemPorts = 1
	st, tl := run(t, cfg, `0x1000 lw writes=r1 reads=r2 mem=0x2000:4
0x1004 nop
0x1008 nop
0x100c nop
`)
	if st.Stalls[StallStructural] != 1 {
		t.Errorf("structural stalls = %d, want 1 (IF yields its port to MEM)",
			st.Stalls[StallStructural])
	}
	checkOccupancyInvariant(t, tl, 1)
}

func TestWideIssue(t *testing.T) {
	cfg := baseCfg()
	cfg.Width = 2
	st, tl := run(t, cfg, `0x1000 add writes=r1 reads=r2
0x1004 add writes=r3 reads=r4
0x1008 add writes=r5 reads=r6
0x100c add writes=r7 reads=r8
`)
	if st.Cycles != 6 { // 2 fetch cycles + 4 drain
		t.Errorf("cycles = %d, want 6", st.Cycles)
	}
	checkOccupancyInvariant(t, tl, 2)
}

func TestConfigChangesTimelineDeterministically(t *testing.T) {
	input := `0x1000 lw writes=r1 reads=r2 mem=0x2000:4
0x1004 add reads=r1 writes=r3
0x1008 beq reads=r3,r4 taken
0x2000 sub writes=r5 reads=r6
`
	_, tl1 := run(t, baseCfg(), input)
	_, tl1again := run(t, baseCfg(), input)
	if len(tl1) != len(tl1again) {
		t.Fatal("same config must reproduce identical timeline length")
	}
	for i := range tl1 {
		if tl1[i] != tl1again[i] {
			t.Fatalf("timeline diverges at %d: %+v vs %+v", i, tl1[i], tl1again[i])
		}
	}
	deeper := baseCfg()
	deeper.Depth = 7
	_, tl2 := run(t, deeper, input)
	if len(tl2) == len(tl1) {
		t.Error("changing depth must change the timeline")
	}
}

func TestStreamingLargeTrace(t *testing.T) {
	// 200k instructions through a pipe: must complete with bounded memory
	// (timeline retention off).
	var b strings.Builder
	for i := 0; i < 200000; i++ {
		b.WriteString("0x1000 add writes=r1 reads=r2,r3\n")
	}
	cfg := baseCfg()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st := s.Run(trace.NewParser(strings.NewReader(b.String())), nil)
	if st.Instructions != 200000 {
		t.Fatalf("instructions = %d", st.Instructions)
	}
	if st.Cycles != 200000+4 {
		t.Errorf("cycles = %d, want %d", st.Cycles, 200000+4)
	}
	if s.Timeline() != nil {
		t.Error("timeline must not be retained when KeepTimeline is off")
	}
}
