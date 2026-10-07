// Package analysis orchestrates single- and multi-trace runs.
//
// Every input trace is analyzed by an independent simulator instance, so
// results are deterministic per trace regardless of how many traces run
// concurrently. A failure in one trace never affects the others. When the
// context is cancelled, scheduling stops promptly, in-flight simulators are
// aborted through their Cancel hook, and every result carries either data
// or an error — Run never blocks forever and never returns partial rows
// without an explanation.
package analysis

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"

	"github.com/huchangjun118/instruction-pipeline-analyzer-go/internal/pipeline"
	"github.com/huchangjun118/instruction-pipeline-analyzer-go/internal/trace"
)

// maxStoredErrors bounds how many parse errors are kept per trace; the
// total is always reported so bounded memory never hides problems.
const maxStoredErrors = 1024

// Options controls one analysis session.
type Options struct {
	Pipeline     pipeline.Config
	Concurrency  int  // max traces analyzed in parallel; 0 = NumCPU
	WantTimeline bool // keep per-cycle timeline in the result
}

// Input names one trace source.
type Input struct {
	Name string
	R    io.Reader
}

// TraceResult is the outcome for one input.
type TraceResult struct {
	Name        string
	Stats       pipeline.Stats
	Timeline    []pipeline.StageOccupancy
	Errors      []error // first parse problems, non-fatal
	TotalErrors int     // full parse-error count, even if truncated
	Err         error   // fatal failure for this trace only
}

// Run analyzes every input independently and returns one result per input,
// in input order. Results are identical for any concurrency level.
func Run(ctx context.Context, opts Options, inputs []Input) []TraceResult {
	results := make([]TraceResult, len(inputs))
	for i, in := range inputs {
		results[i].Name = in.Name
	}
	if len(inputs) == 0 {
		return results
	}

	workers := opts.Concurrency
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers > len(inputs) {
		workers = len(inputs)
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = analyze(ctx, opts, inputs[i])
			}
		}()
	}
sched:
	for i := range inputs {
		select {
		case <-ctx.Done():
			for j := i; j < len(inputs); j++ {
				results[j].Err = ctx.Err()
			}
			break sched
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	return results
}

// analyze runs one trace end to end. It never touches shared state.
func analyze(ctx context.Context, opts Options, in Input) (res TraceResult) {
	res.Name = in.Name
	cfg := opts.Pipeline
	cfg.KeepTimeline = opts.WantTimeline
	sim, err := pipeline.New(cfg)
	if err != nil {
		res.Err = err
		return res
	}
	sim.Cancel = func() bool { return ctx.Err() != nil }
	res.Stats = sim.Run(trace.NewParser(in.R), func(pe *trace.ParseError) {
		res.TotalErrors++
		if len(res.Errors) < maxStoredErrors {
			res.Errors = append(res.Errors, pe)
		}
	})
	if opts.WantTimeline {
		res.Timeline = sim.Timeline()
	}
	if err := sim.Err(); err != nil {
		if errors.Is(err, pipeline.ErrCancelled) && ctx.Err() != nil {
			res.Err = ctx.Err()
		} else {
			res.Err = err
		}
	}
	return res
}
