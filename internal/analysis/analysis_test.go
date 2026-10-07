package analysis

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/huchangjun118/instruction-pipeline-analyzer-go/internal/cache"
	"github.com/huchangjun118/instruction-pipeline-analyzer-go/internal/pipeline"
)

func testOptions() Options {
	return Options{
		Pipeline: pipeline.Config{
			Depth: 5, Width: 1, Forwarding: true,
			DCache: cache.Config{SizeBytes: 128, Assoc: 2, LineBytes: 16, MissPenalty: 4},
		},
		Concurrency:  4,
		WantTimeline: true,
	}
}

func makeInputs(n, length int) []Input {
	inputs := make([]Input, n)
	for i := 0; i < n; i++ {
		var b strings.Builder
		for j := 0; j < length; j++ {
			switch j % 5 {
			case 0:
				fmt.Fprintf(&b, "0x%x lw writes=r1 reads=r2 mem=0x%x:4\n", 0x1000+4*j, 0x2000+4*(j%37))
			case 1:
				fmt.Fprintf(&b, "0x%x add reads=r1 writes=r3\n", 0x1000+4*j)
			case 2:
				fmt.Fprintf(&b, "0x%x beq reads=r3,r4 taken\n", 0x1000+4*j)
			case 3:
				fmt.Fprintf(&b, "0x%x sw reads=r3 mem=0x%x:4\n", 0x1000+4*j, 0x3000+4*(j%29))
			default:
				fmt.Fprintf(&b, "0x%x sub writes=r5 reads=r6\n", 0x1000+4*j)
			}
		}
		inputs[i] = Input{Name: fmt.Sprintf("trace-%d", i), R: strings.NewReader(b.String())}
	}
	return inputs
}

func TestConcurrencyMatchesSerial(t *testing.T) {
	serial := Run(context.Background(), testOptions(), makeInputs(6, 200))
	parOpts := testOptions()
	parOpts.Concurrency = 6
	parallel := Run(context.Background(), parOpts, makeInputs(6, 200))
	if len(serial) != len(parallel) {
		t.Fatal("result count mismatch")
	}
	for i := range serial {
		if serial[i].Err != nil || parallel[i].Err != nil {
			t.Fatalf("trace %d errored: %v / %v", i, serial[i].Err, parallel[i].Err)
		}
		if !reflect.DeepEqual(serial[i].Stats, parallel[i].Stats) {
			t.Errorf("trace %d: stats differ between serial and concurrent", i)
		}
		if !reflect.DeepEqual(serial[i].Timeline, parallel[i].Timeline) {
			t.Errorf("trace %d: timeline differs between serial and concurrent", i)
		}
	}
}

func TestReproducibility(t *testing.T) {
	a := Run(context.Background(), testOptions(), makeInputs(4, 150))
	b := Run(context.Background(), testOptions(), makeInputs(4, 150))
	for i := range a {
		if !reflect.DeepEqual(a[i].Stats, b[i].Stats) ||
			!reflect.DeepEqual(a[i].Timeline, b[i].Timeline) {
			t.Errorf("trace %d: identical inputs must reproduce identical results", i)
		}
	}
}

func TestOneTraceFailureDoesNotAffectOthers(t *testing.T) {
	opts := testOptions()
	opts.Pipeline.Depth = 2 // invalid: every trace with this config fails
	bad := Run(context.Background(), opts, makeInputs(3, 10))
	for i, r := range bad {
		if r.Err == nil {
			t.Errorf("trace %d: expected config error", i)
		}
	}
	// A trace with parse errors still yields stats for its valid records.
	inputs := []Input{
		{Name: "bad", R: strings.NewReader("0x1 frobnicate\nzzzz add\n0x2 add writes=r1\n")},
		{Name: "good", R: strings.NewReader("0x10 add writes=r1\n")},
	}
	res := Run(context.Background(), testOptions(), inputs)
	if res[0].TotalErrors != 2 || res[0].Stats.Instructions != 1 {
		t.Errorf("bad trace: errors=%d instr=%d", res[0].TotalErrors, res[0].Stats.Instructions)
	}
	if res[1].Err != nil || res[1].Stats.Instructions != 1 {
		t.Errorf("good trace affected: %+v", res[1])
	}
}

func TestCancellationStopsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before anything starts
	inputs := makeInputs(8, 50000)
	start := time.Now()
	res := Run(ctx, testOptions(), inputs)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancelled run took %v", elapsed)
	}
	for i, r := range res {
		if r.Err == nil {
			t.Errorf("trace %d: expected cancellation error", i)
		}
	}
}

func TestTimeoutDuringRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// Huge single trace: the Cancel hook must abort the simulator itself.
	var b strings.Builder
	for i := 0; i < 5000000; i++ {
		b.WriteString("0x1000 add writes=r1 reads=r2\n")
	}
	inputs := []Input{{Name: "huge", R: strings.NewReader(b.String())}}
	done := make(chan []TraceResult, 1)
	go func() { done <- Run(ctx, testOptions(), inputs) }()
	select {
	case res := <-done:
		if res[0].Err == nil {
			t.Error("expected timeout error on huge trace")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop after timeout: process would hang")
	}
}
