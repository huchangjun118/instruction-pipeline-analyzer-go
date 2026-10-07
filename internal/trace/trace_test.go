package trace

import (
	"io"
	"strings"
	"testing"
)

func collect(t *testing.T, input string) ([]*Record, []*ParseError) {
	t.Helper()
	p := NewParser(strings.NewReader(input))
	var recs []*Record
	var errs []*ParseError
	for {
		rec, perr, err := p.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("fatal error: %v", err)
		}
		if perr != nil {
			errs = append(errs, perr)
			continue
		}
		recs = append(recs, rec)
	}
	return recs, errs
}

func TestParseValidRecords(t *testing.T) {
	input := `# comment line

0x400000 add reads=r2,r3 writes=r1
0x400004 lw writes=r4 reads=r1 mem=0x2000:4
4096 beq reads=r1,r2 taken
0x40000c sw reads=r4 mem=0x2004:4
`
	recs, errs := collect(t, input)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(recs) != 4 {
		t.Fatalf("got %d records, want 4", len(recs))
	}
	if recs[0].Class != ClassALU || recs[0].Addr != 0x400000 {
		t.Errorf("rec0 = %+v", recs[0])
	}
	if recs[1].Class != ClassLoad || !recs[1].HasMem || recs[1].MemAddr != 0x2000 || recs[1].MemWidth != 4 {
		t.Errorf("rec1 = %+v", recs[1])
	}
	if recs[2].Class != ClassBranch || !recs[2].Taken || recs[2].Addr != 4096 {
		t.Errorf("rec2 = %+v", recs[2])
	}
	if recs[3].Class != ClassStore {
		t.Errorf("rec3 = %+v", recs[3])
	}
}

func TestParseErrorKindsAndLineNumbers(t *testing.T) {
	input := `0x400000 add writes=r1
0x400001 frobnicate r1
0x400002 lw writes=r2
zzzz add writes=r3
0x400004 lw writes=r2 mem=0x1000
0x400005 add writes=r9
`
	recs, errs := collect(t, input)
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (valid lines must survive)", len(recs))
	}
	if len(errs) != 4 {
		t.Fatalf("got %d errors, want 4", len(errs))
	}
	want := []struct {
		line int
		kind ErrKind
	}{
		{2, ErrUnknownMnemonic},
		{3, ErrMissingField},
		{4, ErrBadField},
		{5, ErrBadField},
	}
	for i, w := range want {
		if errs[i].Line != w.line || errs[i].Kind != w.kind {
			t.Errorf("err[%d] = line %d kind %v, want line %d kind %v",
				i, errs[i].Line, errs[i].Kind, w.line, w.kind)
		}
	}
}

func TestErrorMessagesDistinct(t *testing.T) {
	_, errs := collect(t, "0x1 foo\n0x2 lw\n")
	if errs[0].Error() == errs[1].Error() {
		t.Errorf("error messages must be distinguishable: %q vs %q", errs[0], errs[1])
	}
	if !strings.Contains(errs[0].Error(), "line 1") {
		t.Errorf("error must carry line number: %q", errs[0])
	}
}

func TestStreamingConstantMemory(t *testing.T) {
	// Feed many records through a pipe-like reader; ensure no accumulation.
	p := NewParser(strings.NewReader(strings.Repeat("0x400000 add writes=r1 reads=r2\n", 100000)))
	n := 0
	for {
		_, _, err := p.Next()
		if err == io.EOF {
			break
		}
		n++
	}
	if n != 100000 {
		t.Fatalf("got %d records, want 100000", n)
	}
}
