package ipa

import (
	"context"
	"io"
	"sync"
)

// TraceInput is one named trace source.
type TraceInput struct {
	Name   string
	Reader io.Reader
}

// Result is the outcome of analyzing one trace.
type Result struct {
	Name     string
	Stats    Stats
	Err      error
	Canceled bool
}

// AnalyzeOne analyzes a single trace stream. It never fails on bad
// lines; those are counted in Stats.ParseErrors. A nil or canceled
// context aborts early and marks the result canceled.
func AnalyzeOne(ctx context.Context, name string, r io.Reader, cfg Config) Result {
	res := Result{Name: name, Stats: NewStats()}
	if err := cfg.Validate(); err != nil {
		res.Err = err
		return res
	}
	sim, err := NewSim(cfg, SimOptions{})
	if err != nil {
		res.Err = err
		return res
	}
	p := NewParser(&cancelReader{ctx: ctx, r: r})
	sim.Run(p)
	if ctx.Err() != nil {
		res.Canceled = true
		res.Err = ctx.Err()
		return res
	}
	res.Stats = sim.Stats()
	return res
}

// cancelReader aborts reads when ctx is done, so cancellation cannot
// block behind a slow source.
type cancelReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *cancelReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// AnalyzeAll analyzes each input independently with bounded
// concurrency. Results are returned in input order and are identical
// to serial execution: simulations share no state. One failing trace
// does not affect the others; cancellation stops scheduling and marks
// unfinished results canceled without hanging.
func AnalyzeAll(ctx context.Context, inputs []TraceInput, cfg Config, concurrency int) []Result {
	results := make([]Result, len(inputs))
	for i, in := range inputs {
		results[i] = Result{Name: in.Name, Stats: NewStats()}
	}
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, in := range inputs {
		if ctx.Err() != nil {
			for j := i; j < len(inputs); j++ {
				results[j].Canceled = true
				results[j].Err = ctx.Err()
			}
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, in TraceInput) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = AnalyzeOne(ctx, in.Name, in.Reader, cfg)
		}(i, in)
	}
	wg.Wait()
	return results
}
