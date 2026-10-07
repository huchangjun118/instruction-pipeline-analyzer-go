// Package trace parses instruction execution traces in a streaming fashion.
//
// Line format (blank lines and lines starting with '#' are ignored):
//
//	<addr> <mnemonic> [reads=r1,r2] [writes=r3] [mem=0xADDR:WIDTH] [taken]
//
// addr may be hex (0x…) or decimal. mem is required for loads and stores.
// taken marks a branch/jump whose execution redirected the fetch stream.
package trace

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Class categorizes an instruction for pipeline behavior.
type Class int

const (
	ClassALU Class = iota
	ClassLoad
	ClassStore
	ClassBranch
	ClassJump
)

// Record is one parsed trace line.
type Record struct {
	Line     int    // 1-based source line number
	Addr     uint64 // instruction address
	Mnemonic string
	Class    Class
	Reads    []string // registers read
	Writes   []string // registers written
	HasMem   bool     // whether a memory access is present
	MemAddr  uint64   // data memory address
	MemWidth int      // data memory access width in bytes
	Taken    bool     // branch/jump taken (redirects fetch)
}

// ErrKind distinguishes parse failure reasons.
type ErrKind int

const (
	ErrUnknownMnemonic ErrKind = iota
	ErrMissingField
	ErrBadField
)

func (k ErrKind) String() string {
	switch k {
	case ErrUnknownMnemonic:
		return "unknown mnemonic"
	case ErrMissingField:
		return "missing field"
	case ErrBadField:
		return "bad field"
	}
	return "unknown error"
}

// ParseError describes a single malformed line. Parsing continues after it.
type ParseError struct {
	Line   int
	Kind   ErrKind
	Detail string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("line %d: %s: %s", e.Line, e.Kind, e.Detail)
}

var mnemonics = map[string]Class{
	"add": ClassALU, "sub": ClassALU, "mul": ClassALU, "div": ClassALU,
	"and": ClassALU, "or": ClassALU, "xor": ClassALU, "sll": ClassALU,
	"srl": ClassALU, "nop": ClassALU,
	"lw": ClassLoad, "ld": ClassLoad, "lb": ClassLoad, "lh": ClassLoad,
	"sw": ClassStore, "sd": ClassStore, "sb": ClassStore, "sh": ClassStore,
	"beq": ClassBranch, "bne": ClassBranch, "blt": ClassBranch, "bge": ClassBranch,
	"j": ClassJump, "jal": ClassJump, "jr": ClassJump, "ret": ClassJump,
}

// Parser streams records from an underlying reader.
type Parser struct {
	sc   *bufio.Scanner
	line int
}

// NewParser wraps r. Records are produced one at a time; memory use is O(1)
// per record regardless of trace size.
func NewParser(r io.Reader) *Parser {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &Parser{sc: sc}
}

// Next returns the next record. It returns io.EOF when the stream is
// exhausted. Malformed lines are reported through the returned *ParseError
// (with a nil Record) and never abort the stream.
func (p *Parser) Next() (*Record, *ParseError, error) {
	for p.sc.Scan() {
		p.line++
		text := strings.TrimSpace(p.sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		rec, perr := parseLine(p.line, text)
		if perr != nil {
			return nil, perr, nil
		}
		return rec, nil, nil
	}
	if err := p.sc.Err(); err != nil {
		return nil, nil, err
	}
	return nil, nil, io.EOF
}

func parseLine(line int, text string) (*Record, *ParseError) {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return nil, &ParseError{Line: line, Kind: ErrMissingField,
			Detail: "need at least address and mnemonic"}
	}
	rec := &Record{Line: line}
	addr, err := strconv.ParseUint(fields[0], 0, 64)
	if err != nil {
		return nil, &ParseError{Line: line, Kind: ErrBadField,
			Detail: fmt.Sprintf("bad address %q", fields[0])}
	}
	rec.Addr = addr
	rec.Mnemonic = strings.ToLower(fields[1])
	class, ok := mnemonics[rec.Mnemonic]
	if !ok {
		return nil, &ParseError{Line: line, Kind: ErrUnknownMnemonic,
			Detail: fmt.Sprintf("mnemonic %q", fields[1])}
	}
	rec.Class = class

	for _, f := range fields[2:] {
		if f == "taken" {
			rec.Taken = true
			continue
		}
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			return nil, &ParseError{Line: line, Kind: ErrBadField,
				Detail: fmt.Sprintf("field %q is not key=value", f)}
		}
		switch strings.ToLower(kv[0]) {
		case "reads":
			rec.Reads = splitList(kv[1])
		case "writes":
			rec.Writes = splitList(kv[1])
		case "mem":
			a, w, err := parseMem(kv[1])
			if err != nil {
				return nil, &ParseError{Line: line, Kind: ErrBadField, Detail: err.Error()}
			}
			rec.HasMem, rec.MemAddr, rec.MemWidth = true, a, w
		default:
			return nil, &ParseError{Line: line, Kind: ErrBadField,
				Detail: fmt.Sprintf("unknown key %q", kv[0])}
		}
	}
	if (class == ClassLoad || class == ClassStore) && !rec.HasMem {
		return nil, &ParseError{Line: line, Kind: ErrMissingField,
			Detail: fmt.Sprintf("%s requires mem=ADDR:WIDTH", rec.Mnemonic)}
	}
	return rec, nil
}

func splitList(s string) []string {
	if s == "" || s == "-" {
		return nil
	}
	return strings.Split(s, ",")
}

func parseMem(s string) (uint64, int, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("mem field %q needs ADDR:WIDTH", s)
	}
	addr, err := strconv.ParseUint(parts[0], 0, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("bad mem address %q", parts[0])
	}
	width, err := strconv.Atoi(parts[1])
	if err != nil || width <= 0 {
		return 0, 0, fmt.Errorf("bad mem width %q", parts[1])
	}
	return addr, width, nil
}
