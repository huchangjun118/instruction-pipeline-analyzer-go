package ipa

import (
	"strings"
	"testing"
)

// collect parses all lines and returns records and per-line errors.
func collect(input string) ([]Record, []error) {
	p := NewParser(strings.NewReader(input))
	var recs []Record
	var errs []error
	for p.Next() {
		if err := p.Err(); err != nil {
			errs = append(errs, err)
			continue
		}
		recs = append(recs, p.Record())
	}
	return recs, errs
}

func TestParserBasic(t *testing.T) {
	input := `
# comment line

addr=0x1000 op=add w=r1 r=r2,r3
addr=0x1004 op=lw w=r5 r=r2 mem=0x2000 width=4
addr=0x1008 op=sw r=r1,r2 mem=0x2004 width=4
addr=0x100c op=beq r=r1,r2
`
	recs, errs := collect(input)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(recs) != 4 {
		t.Fatalf("want 4 records, got %d", len(recs))
	}
	if recs[0].Class != ClassALU || recs[1].Class != ClassLoad ||
		recs[2].Class != ClassStore || recs[3].Class != ClassBranch {
		t.Fatalf("bad classes: %+v", recs)
	}
	lw := recs[1]
	if !lw.HasMem || lw.MemAddr != 0x2000 || lw.MemWidth != 4 {
		t.Fatalf("bad mem fields: %+v", lw)
	}
	if len(lw.Writes) != 1 || lw.Writes[0] != "r5" || len(lw.Reads) != 1 || lw.Reads[0] != "r2" {
		t.Fatalf("bad reg lists: %+v", lw)
	}
	if lw.Line != 5 {
		t.Fatalf("want line 5, got %d", lw.Line)
	}
}

func TestParserErrorsArePerLine(t *testing.T) {
	input := `addr=0x1000 op=add w=r1 r=r2
op=sub w=r1 r=r2
addr=0x1004 op=xyzzy w=r1
addr=0x1008 op=lw w=r1 r=r2
addr=0x100c op=lw w=r1 r=r2 mem=0x2000
addr=zz op=add
addr=0x1010 op=add w=r1 r=r2 bogus
addr=0x1014 op=add w=r1 r=r2 extra=1
addr=0x1018 op=add w=r1 r=r2
`
	recs, errs := collect(input)
	if len(recs) != 2 {
		t.Fatalf("want 2 good records, got %d", len(recs))
	}
	if len(errs) != 7 {
		t.Fatalf("want 7 errors, got %d: %v", len(errs), errs)
	}
	// Each error must carry a line number and a distinguishable reason.
	wantReasons := []string{
		"missing field \"addr\"",
		"unknown mnemonic",
		"missing field \"mem\"",
		"missing field \"width\"",
		"bad addr",
		"malformed field",
		"unknown field",
	}
	for i, want := range wantReasons {
		pe, ok := errs[i].(*ParseError)
		if !ok {
			t.Fatalf("error %d not a ParseError: %T", i, errs[i])
		}
		if pe.Line != i+2 {
			t.Fatalf("error %d: want line %d, got %d", i, i+2, pe.Line)
		}
		if !strings.Contains(pe.Reason, want) {
			t.Fatalf("error %d: want reason containing %q, got %q", i, want, pe.Reason)
		}
	}
}

func TestParserErrorDoesNotAbortStream(t *testing.T) {
	input := `addr=0x1 op=add
bad line here
addr=0x2 op=sub
`
	recs, errs := collect(input)
	if len(recs) != 2 || len(errs) != 1 {
		t.Fatalf("got %d records, %d errors", len(recs), len(errs))
	}
	if recs[1].Addr != 0x2 {
		t.Fatalf("stream did not continue after error: %+v", recs)
	}
}

func TestParserStreamsLargeInput(t *testing.T) {
	var b strings.Builder
	const n = 200000
	for i := 0; i < n; i++ {
		b.WriteString("addr=0x1000 op=add w=r1 r=r2,r3\n")
	}
	p := NewParser(strings.NewReader(b.String()))
	count := 0
	for p.Next() {
		if p.Err() != nil {
			t.Fatalf("unexpected error: %v", p.Err())
		}
		count++
	}
	if count != n {
		t.Fatalf("want %d records, got %d", n, count)
	}
}
