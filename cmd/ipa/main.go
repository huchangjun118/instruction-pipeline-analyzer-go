// Command ipa reconstructs a pipeline timeline from instruction
// execution traces and reports cache hit rates and stall reasons.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	ipa "github.com/huchangjun118/instruction-pipeline-analyzer-go"
)

func main() {
	var (
		depth       = flag.Int("depth", 5, "pipeline depth (>=5)")
		width       = flag.Int("width", 1, "issue width (>=1)")
		forwarding  = flag.Bool("forwarding", true, "enable bypass forwarding")
		branchStage = flag.Int("branch-stage", 0, "1-based branch resolve stage (0=EX1)")
		memPorts    = flag.Int("mem-ports", 0, "memory ports shared by IF/MEM (0=unlimited)")
		exUnits     = flag.Int("ex-units", 0, "execution units (0=unlimited)")
		icacheSpec  = flag.String("icache", "", "icache cap:line:assoc:lru|fifo:penalty")
		dcacheSpec  = flag.String("dcache", "", "dcache cap:line:assoc:lru|fifo:penalty")
		concurrency = flag.Int("concurrency", 4, "max traces analyzed in parallel")
		timeout     = flag.Duration("timeout", 0, "overall timeout (0=none)")
		instrTL     = flag.String("instr-timeline", "", "per-instruction timeline output file (- for stdout)")
		cycleTL     = flag.String("cycle-timeline", "", "per-cycle timeline output file (- for stdout)")
	)
	flag.Parse()

	cfg := ipa.DefaultConfig()
	cfg.PipelineDepth = *depth
	cfg.IssueWidth = *width
	cfg.Forwarding = *forwarding
	cfg.BranchResolveStage = *branchStage
	cfg.MemPorts = *memPorts
	cfg.EXUnits = *exUnits
	var err error
	if cfg.ICache, err = parseCacheSpec("icache", *icacheSpec); err != nil {
		fatal(err)
	}
	if cfg.DCache, err = parseCacheSpec("dcache", *dcacheSpec); err != nil {
		fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		fatal(err)
	}

	ctx := context.Background()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	args := flag.Args()
	if (*instrTL != "" || *cycleTL != "") && len(args) > 1 {
		fatal(fmt.Errorf("timeline output requires a single input"))
	}

	if len(args) == 0 {
		res := analyzeStream(ctx, "stdin", os.Stdin, cfg, *instrTL, *cycleTL)
		printResult(res)
		if res.Err != nil && !res.Canceled {
			os.Exit(1)
		}
		return
	}

	inputs := make([]ipa.TraceInput, 0, len(args))
	files := make([]*os.File, 0, len(args))
	for _, path := range args {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ipa: %s: %v\n", path, err)
			for _, of := range files {
				of.Close()
			}
			os.Exit(1)
		}
		files = append(files, f)
		inputs = append(inputs, ipa.TraceInput{Name: path, Reader: f})
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()

	if len(inputs) == 1 && (*instrTL != "" || *cycleTL != "") {
		res := analyzeStream(ctx, inputs[0].Name, inputs[0].Reader, cfg, *instrTL, *cycleTL)
		printResult(res)
		if res.Err != nil && !res.Canceled {
			os.Exit(1)
		}
		return
	}

	results := ipa.AnalyzeAll(ctx, inputs, cfg, *concurrency)
	exit := 0
	for _, res := range results {
		printResult(res)
		if res.Err != nil && !res.Canceled {
			exit = 1
		}
	}
	os.Exit(exit)
}

// analyzeStream runs one trace with optional timeline outputs.
func analyzeStream(ctx context.Context, name string, r io.Reader, cfg ipa.Config, instrTL, cycleTL string) ipa.Result {
	res := ipa.Result{Name: name, Stats: ipa.NewStats()}
	var opts ipa.SimOptions
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()
	open := func(path string) (io.Writer, error) {
		if path == "" {
			return nil, nil
		}
		if path == "-" {
			return os.Stdout, nil
		}
		f, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		closers = append(closers, f)
		return f, nil
	}
	var err error
	if opts.InstrTimelineW, err = open(instrTL); err != nil {
		res.Err = err
		return res
	}
	if opts.CycleTimelineW, err = open(cycleTL); err != nil {
		res.Err = err
		return res
	}
	opts.ParseErrorW = os.Stderr
	sim, err := ipa.NewSim(cfg, opts)
	if err != nil {
		res.Err = err
		return res
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// Context expired; the parser's reader aborts below.
		case <-done:
		}
	}()
	defer close(done)
	sim.Run(ipa.NewParser(ctxReader{ctx, r}))
	if ctx.Err() != nil {
		res.Canceled = true
		res.Err = ctx.Err()
		return res
	}
	res.Stats = sim.Stats()
	return res
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func printResult(res ipa.Result) {
	fmt.Printf("== %s ==\n", res.Name)
	if res.Canceled {
		fmt.Printf("  canceled: %v\n", res.Err)
		return
	}
	if res.Err != nil {
		fmt.Printf("  error: %v\n", res.Err)
		return
	}
	s := res.Stats
	fmt.Printf("  instructions: %d\n", s.Instructions)
	fmt.Printf("  cycles:       %d\n", s.Cycles)
	if s.Instructions > 0 {
		fmt.Printf("  cpi:          %.3f\n", float64(s.Cycles)/float64(s.Instructions))
	}
	fmt.Printf("  parse errors: %d\n", s.ParseErrors)
	printCache := func(label string, hits, misses uint64) {
		total := hits + misses
		if total == 0 {
			return
		}
		fmt.Printf("  %s: hits=%d misses=%d hit-rate=%.2f%%\n",
			label, hits, misses, 100*float64(hits)/float64(total))
	}
	printCache("icache", s.ICacheHits, s.ICacheMisses)
	printCache("dcache", s.DCacheHits, s.DCacheMisses)
	if len(s.Stalls) > 0 {
		fmt.Println("  stall cycles:")
		for _, k := range sortedKeys(s.Stalls) {
			fmt.Printf("    %-16s %d\n", k, s.Stalls[k])
		}
	}
	if len(s.Hazards) > 0 {
		fmt.Println("  hazards:")
		keys := make([]string, 0, len(s.Hazards))
		for k := range s.Hazards {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("    %-16s %d\n", k, s.Hazards[k])
		}
	}
}

func sortedKeys(m map[ipa.StallReason]uint64) []ipa.StallReason {
	keys := make([]ipa.StallReason, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// parseCacheSpec parses "cap:line:assoc:policy:penalty".
func parseCacheSpec(name, spec string) (ipa.CacheConfig, error) {
	cfg := ipa.CacheConfig{}
	if spec == "" {
		return cfg, nil
	}
	parts := strings.Split(spec, ":")
	if len(parts) != 5 {
		return cfg, fmt.Errorf("%s: want cap:line:assoc:lru|fifo:penalty, got %q", name, spec)
	}
	nums := make([]int, 3)
	for i, p := range parts[:3] {
		v, err := strconv.Atoi(p)
		if err != nil {
			return cfg, fmt.Errorf("%s: bad number %q", name, p)
		}
		nums[i] = v
	}
	pen, err := strconv.Atoi(parts[4])
	if err != nil {
		return cfg, fmt.Errorf("%s: bad penalty %q", name, parts[4])
	}
	cfg = ipa.CacheConfig{
		Enabled:     true,
		Capacity:    nums[0],
		LineSize:    nums[1],
		Assoc:       nums[2],
		Replacement: parts[3],
		MissPenalty: pen,
	}
	if err := ipa.ValidateCacheConfig(cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", name, err)
	}
	return cfg, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ipa:", err)
	os.Exit(2)
}
