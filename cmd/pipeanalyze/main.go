// Command pipeanalyze reconstructs pipeline timelines from instruction
// traces and reports cache hit rates and stall attribution.
//
// Usage:
//
//	pipeanalyze [flags] [trace-file ...]
//
// With no files (or "-"), the trace is read from standard input. All
// processing is local and in-process; no external services are used.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/huchangjun118/instruction-pipeline-analyzer-go/internal/analysis"
	"github.com/huchangjun118/instruction-pipeline-analyzer-go/internal/cache"
	"github.com/huchangjun118/instruction-pipeline-analyzer-go/internal/pipeline"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "pipeanalyze:", err)
		os.Exit(1)
	}
}

type options struct {
	depth       int
	width       int
	memPorts    int
	aluUnits    int
	noFwd       bool
	icacheSpec  string
	dcacheSpec  string
	concurrency int
	timeout     time.Duration
	format      string
	timeline    bool
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	var o options
	fs := flag.NewFlagSet("pipeanalyze", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.IntVar(&o.depth, "depth", 5, "pipeline depth (>= 4)")
	fs.IntVar(&o.width, "width", 1, "issue width (>= 1)")
	fs.IntVar(&o.memPorts, "mem-ports", 0, "memory ports shared by IF and MEM (0 = unlimited)")
	fs.IntVar(&o.aluUnits, "alu-units", 0, "execution units per cycle (0 = unlimited)")
	fs.BoolVar(&o.noFwd, "no-forwarding", false, "disable result forwarding")
	fs.StringVar(&o.icacheSpec, "icache", "off", "icache spec size:assoc:line:repl:penalty or off")
	fs.StringVar(&o.dcacheSpec, "dcache", "off", "dcache spec size:assoc:line:repl:penalty or off")
	fs.IntVar(&o.concurrency, "concurrency", 0, "traces analyzed in parallel (0 = NumCPU)")
	fs.DurationVar(&o.timeout, "timeout", 0, "overall deadline (0 = none)")
	fs.StringVar(&o.format, "format", "text", "output format: text or json")
	fs.BoolVar(&o.timeline, "timeline", false, "print the per-cycle timeline")
	if err := fs.Parse(args); err != nil {
		return err
	}

	icfg, err := parseCacheSpec(o.icacheSpec)
	if err != nil {
		return fmt.Errorf("icache: %w", err)
	}
	dcfg, err := parseCacheSpec(o.dcacheSpec)
	if err != nil {
		return fmt.Errorf("dcache: %w", err)
	}
	pcfg := pipeline.Config{
		Depth:      o.depth,
		Width:      o.width,
		MemPorts:   o.memPorts,
		ALUUnits:   o.aluUnits,
		Forwarding: !o.noFwd,
		ICache:     icfg,
		DCache:     dcfg,
	}
	if err := pcfg.Validate(); err != nil {
		return err
	}

	ctx := context.Background()
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}

	files := fs.Args()
	var inputs []analysis.Input
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()
	if len(files) == 0 {
		files = []string{"-"}
	}
	for _, f := range files {
		if f == "-" {
			inputs = append(inputs, analysis.Input{Name: "<stdin>", R: stdin})
			continue
		}
		fh, err := os.Open(f)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		closers = append(closers, fh)
		inputs = append(inputs, analysis.Input{Name: f, R: fh})
	}

	results := analysis.Run(ctx, analysis.Options{
		Pipeline:     pcfg,
		Concurrency:  o.concurrency,
		WantTimeline: o.timeline,
	}, inputs)

	switch o.format {
	case "json":
		return printJSON(stdout, results)
	case "text":
		return printText(stdout, results, o.timeline, pcfg.Depth)
	default:
		return fmt.Errorf("unknown format %q", o.format)
	}
}

// parseCacheSpec parses "size:assoc:line:repl:penalty" or "off".
func parseCacheSpec(spec string) (cache.Config, error) {
	if spec == "" || spec == "off" {
		return cache.Config{}, nil
	}
	parts := strings.Split(spec, ":")
	if len(parts) != 5 {
		return cache.Config{}, fmt.Errorf("spec %q needs size:assoc:line:repl:penalty", spec)
	}
	nums := make([]int, 3)
	for i, p := range []string{parts[0], parts[1], parts[2]} {
		v, err := strconv.Atoi(p)
		if err != nil {
			return cache.Config{}, fmt.Errorf("bad number %q", p)
		}
		nums[i] = v
	}
	var repl cache.Replacement
	switch parts[3] {
	case "lru":
		repl = cache.LRU
	case "fifo":
		repl = cache.FIFO
	default:
		return cache.Config{}, fmt.Errorf("unknown replacement %q (lru|fifo)", parts[3])
	}
	pen, err := strconv.Atoi(parts[4])
	if err != nil {
		return cache.Config{}, fmt.Errorf("bad miss penalty %q", parts[4])
	}
	cfg := cache.Config{
		SizeBytes: nums[0], Assoc: nums[1], LineBytes: nums[2],
		Replacement: repl, MissPenalty: pen,
	}
	if err := cfg.Validate(); err != nil {
		return cache.Config{}, err
	}
	return cfg, nil
}

func printText(w io.Writer, results []analysis.TraceResult, timeline bool, depth int) error {
	failed := false
	for _, r := range results {
		fmt.Fprintf(w, "=== %s ===\n", r.Name)
		if r.Err != nil {
			failed = true
			fmt.Fprintf(w, "  FAILED: %v\n", r.Err)
			continue
		}
		st := r.Stats
		fmt.Fprintf(w, "  instructions: %d\n", st.Instructions)
		fmt.Fprintf(w, "  cycles:       %d\n", st.Cycles)
		fmt.Fprintf(w, "  CPI:          %.3f\n", st.CPI())
		if st.Forwarded > 0 {
			fmt.Fprintf(w, "  forwarded:    %d (RAW dependencies resolved by bypassing)\n", st.Forwarded)
		}
		fmt.Fprintf(w, "  stall cycles: %d\n", st.StallCycles())
		for _, reason := range pipeline.AllReasons {
			if n := st.Stalls[reason]; n > 0 {
				fmt.Fprintf(w, "    %-12s %d\n", reason.String()+":", n)
			}
		}
		fmt.Fprintf(w, "  icache: %d accesses, %d hits, %d misses (hit rate %.2f%%)\n",
			st.ICache.Accesses, st.ICache.Hits, st.ICache.Misses, 100*st.ICache.HitRate())
		fmt.Fprintf(w, "  dcache: %d accesses, %d hits, %d misses (hit rate %.2f%%)\n",
			st.DCache.Accesses, st.DCache.Hits, st.DCache.Misses, 100*st.DCache.HitRate())
		if r.TotalErrors > 0 {
			fmt.Fprintf(w, "  parse errors: %d (first %d shown)\n", r.TotalErrors, len(r.Errors))
			for _, e := range r.Errors {
				fmt.Fprintf(w, "    %v\n", e)
			}
		}
		if timeline {
			printTimeline(w, r.Timeline, depth)
		}
	}
	if failed {
		return fmt.Errorf("one or more traces failed")
	}
	return nil
}

// printTimeline renders a per-cycle, per-stage occupancy view.
func printTimeline(w io.Writer, tl []pipeline.StageOccupancy, depth int) {
	if len(tl) == 0 {
		return
	}
	names := stageNames(depth)
	fmt.Fprintf(w, "  timeline (cycle: stage occupants):\n")
	byCycle := map[int][]pipeline.StageOccupancy{}
	maxCycle := 0
	for _, ev := range tl {
		byCycle[ev.Cycle] = append(byCycle[ev.Cycle], ev)
		if ev.Cycle > maxCycle {
			maxCycle = ev.Cycle
		}
	}
	for c := 1; c <= maxCycle; c++ {
		events := byCycle[c]
		if len(events) == 0 {
			continue
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "  %6d |", c)
		stage := 0
		for _, ev := range events {
			for ; stage < ev.Stage; stage++ {
				fmt.Fprintf(&sb, " %*s |", stageColWidth, "-")
			}
			cell := fmt.Sprintf("#%d %s", ev.Seq, ev.Mnemonic)
			if ev.Stall != pipeline.StallNone {
				cell += " (" + ev.Stall.String() + ")"
			}
			fmt.Fprintf(&sb, " %-*s |", stageColWidth, cell)
			stage++
		}
		for ; stage < depth; stage++ {
			fmt.Fprintf(&sb, " %*s |", stageColWidth, "-")
		}
		fmt.Fprintln(w, sb.String())
	}
	fmt.Fprintf(w, "  stages: %s\n", strings.Join(names, " | "))
}

const stageColWidth = 18

func stageNames(depth int) []string {
	names := make([]string, depth)
	names[0] = "IF"
	names[1] = "ID"
	for i := 2; i < depth-1; i++ {
		if i == depth-2 {
			names[i] = "MEM"
		} else {
			names[i] = fmt.Sprintf("EX%d", i-1)
		}
	}
	names[depth-1] = "WB"
	return names
}

type jsonStats struct {
	Instructions uint64            `json:"instructions"`
	Cycles       uint64            `json:"cycles"`
	CPI          float64           `json:"cpi"`
	Forwarded    uint64            `json:"forwarded"`
	Stalls       map[string]uint64 `json:"stalls"`
	ICache       jsonCache         `json:"icache"`
	DCache       jsonCache         `json:"dcache"`
}

type jsonCache struct {
	Accesses uint64  `json:"accesses"`
	Hits     uint64  `json:"hits"`
	Misses   uint64  `json:"misses"`
	HitRate  float64 `json:"hit_rate"`
}

type jsonResult struct {
	Name        string                    `json:"name"`
	Error       string                    `json:"error,omitempty"`
	Stats       *jsonStats                `json:"stats,omitempty"`
	ParseErrors int                       `json:"parse_errors,omitempty"`
	Timeline    []pipeline.StageOccupancy `json:"timeline,omitempty"`
}

func printJSON(w io.Writer, results []analysis.TraceResult) error {
	out := make([]jsonResult, 0, len(results))
	failed := false
	for _, r := range results {
		jr := jsonResult{Name: r.Name, ParseErrors: r.TotalErrors, Timeline: r.Timeline}
		if r.Err != nil {
			failed = true
			jr.Error = r.Err.Error()
		} else {
			stalls := map[string]uint64{}
			for _, reason := range pipeline.AllReasons {
				stalls[reason.String()] = r.Stats.Stalls[reason]
			}
			jr.Stats = &jsonStats{
				Instructions: r.Stats.Instructions,
				Cycles:       r.Stats.Cycles,
				CPI:          r.Stats.CPI(),
				Forwarded:    r.Stats.Forwarded,
				Stalls:       stalls,
				ICache: jsonCache{
					Accesses: r.Stats.ICache.Accesses, Hits: r.Stats.ICache.Hits,
					Misses: r.Stats.ICache.Misses, HitRate: r.Stats.ICache.HitRate(),
				},
				DCache: jsonCache{
					Accesses: r.Stats.DCache.Accesses, Hits: r.Stats.DCache.Hits,
					Misses: r.Stats.DCache.Misses, HitRate: r.Stats.DCache.HitRate(),
				},
			}
		}
		out = append(out, jr)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return err
	}
	if failed {
		return fmt.Errorf("one or more traces failed")
	}
	return nil
}
