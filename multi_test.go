package ipa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sampleTrace(seed int, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "addr=%#x op=lw w=r1 r=r2 mem=%#x width=4\n", 0x1000+i*4, (i*seed%23)*32)
		fmt.Fprintf(&b, "addr=%#x op=add w=r3 r=r1,r4\n", 0x2000+i*4)
		fmt.Fprintf(&b, "addr=%#x op=beq r=r3,r2\n", 0x3000+i*4)
	}
	return b.String()
}

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.DCache = CacheConfig{Enabled: true, Capacity: 256, LineSize: 32, Assoc: 2, Replacement: "lru", MissPenalty: 3}
	return cfg
}

func TestAnalyzeAllMatchesSerial(t *testing.T) {
	cfg := testConfig()
	traces := []string{sampleTrace(1, 100), sampleTrace(2, 150), sampleTrace(3, 80)}
	// Serial reference.
	want := make([]Stats, len(traces))
	for i, tr := range traces {
		res := AnalyzeOne(context.Background(), fmt.Sprintf("t%d", i), strings.NewReader(tr), cfg)
		if res.Err != nil {
			t.Fatalf("serial run %d: %v", i, res.Err)
		}
		want[i] = res.Stats
	}
	// Concurrent, various degrees.
	for _, conc := range []int{1, 2, 8} {
		inputs := make([]TraceInput, len(traces))
		for i, tr := range traces {
			inputs[i] = TraceInput{Name: fmt.Sprintf("t%d", i), Reader: strings.NewReader(tr)}
		}
		results := AnalyzeAll(context.Background(), inputs, cfg, conc)
		if len(results) != len(traces) {
			t.Fatalf("conc=%d: want %d results, got %d", conc, len(traces), len(results))
		}
		for i, res := range results {
			if res.Err != nil {
				t.Fatalf("conc=%d result %d: %v", conc, i, res.Err)
			}
			if res.Name != fmt.Sprintf("t%d", i) {
				t.Fatalf("conc=%d: result order broken at %d: %s", conc, i, res.Name)
			}
			if !reflect.DeepEqual(res.Stats, want[i]) {
				t.Fatalf("conc=%d result %d differs from serial:\n%+v\n%+v", conc, i, res.Stats, want[i])
			}
		}
	}
}

// errReader fails after a few bytes.
type errReader struct{ n int }

func (r *errReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, errors.New("boom")
	}
	r.n--
	p[0] = '#'
	return 1, nil
}

func TestAnalyzeAllFailureIsolation(t *testing.T) {
	cfg := testConfig()
	inputs := []TraceInput{
		{Name: "good1", Reader: strings.NewReader(sampleTrace(1, 20))},
		{Name: "broken", Reader: &errReader{n: 10}},
		{Name: "good2", Reader: strings.NewReader(sampleTrace(2, 20))},
	}
	results := AnalyzeAll(context.Background(), inputs, cfg, 3)
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
	// The broken reader is contained: good traces still succeed.
	for _, i := range []int{0, 2} {
		if results[i].Err != nil || results[i].Stats.Instructions == 0 {
			t.Fatalf("good trace %d affected: %+v", i, results[i])
		}
	}
	// The broken trace reports its read failure as a parse error.
	if results[1].Stats.ParseErrors == 0 {
		t.Fatalf("broken trace must report parse errors: %+v", results[1].Stats)
	}
}

func TestAnalyzeAllCancellation(t *testing.T) {
	cfg := testConfig()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before scheduling
	inputs := []TraceInput{
		{Name: "a", Reader: strings.NewReader(sampleTrace(1, 10))},
		{Name: "b", Reader: strings.NewReader(sampleTrace(2, 10))},
	}
	done := make(chan []Result, 1)
	go func() { done <- AnalyzeAll(ctx, inputs, cfg, 2) }()
	select {
	case results := <-done:
		for _, r := range results {
			if !r.Canceled {
				t.Fatalf("result %s must be canceled: %+v", r.Name, r)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AnalyzeAll hung after cancellation")
	}
}

// infiniteReader yields an endless stream of valid records.
type infiniteReader struct{ i int }

func (r *infiniteReader) Read(p []byte) (int, error) {
	s := fmt.Sprintf("addr=%#x op=add w=r1 r=r2,r3\n", 0x1000+(r.i%100)*4)
	r.i++
	if r.i%8 != 0 {
		// Dribble out line fragments to exercise the scanner.
	}
	return copy(p, s), nil
}

func TestAnalyzeAllTimeout(t *testing.T) {
	cfg := testConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	inputs := []TraceInput{
		{Name: "endless", Reader: &infiniteReader{}},
		{Name: "finite", Reader: strings.NewReader(sampleTrace(1, 10))},
	}
	done := make(chan []Result, 1)
	go func() { done <- AnalyzeAll(ctx, inputs, cfg, 2) }()
	select {
	case results := <-done:
		if !results[0].Canceled {
			t.Fatalf("endless trace must be canceled: %+v", results[0])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AnalyzeAll hung after timeout")
	}
}

func TestAnalyzeOneInvalidConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PipelineDepth = 2
	res := AnalyzeOne(context.Background(), "bad", strings.NewReader(""), cfg)
	if res.Err == nil {
		t.Fatal("want config error")
	}
}

var _ io.Reader = &infiniteReader{}
