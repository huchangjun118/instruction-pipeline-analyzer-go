package ipa

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// OpClass categorizes an instruction.
type OpClass int

const (
	ClassALU OpClass = iota
	ClassLoad
	ClassStore
	ClassBranch
)

// Record is one parsed trace line.
type Record struct {
	Line     int
	Addr     uint64
	Op       string
	Class    OpClass
	Reads    []string
	Writes   []string
	HasMem   bool
	MemAddr  uint64
	MemWidth int
}

// ParseError describes a rejected trace line.
type ParseError struct {
	Line   int
	Reason string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("line %d: %s", e.Line, e.Reason)
}

// mnemonic table: known opcodes and their classes.
var opClasses = map[string]OpClass{
	// ALU / integer
	"add": ClassALU, "addi": ClassALU, "sub": ClassALU, "and": ClassALU,
	"andi": ClassALU, "or": ClassALU, "ori": ClassALU, "xor": ClassALU,
	"xori": ClassALU, "sll": ClassALU, "srl": ClassALU, "sra": ClassALU,
	"mul": ClassALU, "div": ClassALU, "rem": ClassALU, "mv": ClassALU,
	"not": ClassALU, "neg": ClassALU, "slt": ClassALU, "li": ClassALU,
	"nop": ClassALU,
	// loads
	"lw": ClassLoad, "lh": ClassLoad, "lb": ClassLoad, "ld": ClassLoad,
	"lbu": ClassLoad, "lhu": ClassLoad, "load": ClassLoad,
	// stores
	"sw": ClassStore, "sh": ClassStore, "sb": ClassStore, "sd": ClassStore,
	"store": ClassStore,
	// branches / jumps
	"beq": ClassBranch, "bne": ClassBranch, "blt": ClassBranch,
	"bge": ClassBranch, "beqz": ClassBranch, "bnez": ClassBranch,
	"j": ClassBranch, "jal": ClassBranch, "jalr": ClassBranch,
	"ret": ClassBranch, "call": ClassBranch,
}

// Parser streams records from a reader, tolerating blank lines and
// '#' comments. Per-line failures are reported via Err without
// aborting the stream.
type Parser struct {
	sc       *bufio.Scanner
	line     int
	rec      Record
	err      error
	errShown bool // read error already reported
}

// NewParser creates a streaming parser over r.
func NewParser(r io.Reader) *Parser {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return &Parser{sc: sc}
}

// Next advances to the next meaningful line. Returns false at EOF.
// After a true return, Err reports a per-line problem (the line is
// skipped) or Record yields the parsed instruction.
func (p *Parser) Next() bool {
	p.rec = Record{}
	p.err = nil
	for p.sc.Scan() {
		p.line++
		line := strings.TrimSpace(p.sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rec, err := parseLine(line, p.line)
		if err != nil {
			p.err = err
			return true
		}
		p.rec = rec
		return true
	}
	if serr := p.sc.Err(); serr != nil && !p.errShown {
		p.errShown = true
		p.err = &ParseError{Line: 0, Reason: "read error: " + serr.Error()}
		return true
	}
	return false
}

// Record returns the current parsed record; valid only when Err == nil.
func (p *Parser) Record() Record { return p.rec }

// Err returns the parse problem of the current line, if any.
func (p *Parser) Err() error { return p.err }

// parseLine parses one record line of the form:
//
//	addr=0x1000 op=add w=r1 r=r2,r3 mem=0x2000 width=4
func parseLine(line string, lineNo int) (Record, error) {
	fail := func(reason string) (Record, error) {
		return Record{}, &ParseError{Line: lineNo, Reason: reason}
	}
	var rec Record
	rec.Line = lineNo
	seen := map[string]bool{}
	for _, field := range strings.Fields(line) {
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 {
			return fail("malformed field " + strconv.Quote(field) + " (want key=value)")
		}
		key, val := kv[0], kv[1]
		if seen[key] {
			return fail("duplicate field " + strconv.Quote(key))
		}
		seen[key] = true
		switch key {
		case "addr":
			v, err := parseUint(val)
			if err != nil {
				return fail("bad addr: " + err.Error())
			}
			rec.Addr = v
		case "op":
			cls, ok := opClasses[val]
			if !ok {
				return fail("unknown mnemonic " + strconv.Quote(val))
			}
			rec.Op = val
			rec.Class = cls
		case "w":
			rec.Writes = splitList(val)
		case "r":
			rec.Reads = splitList(val)
		case "mem":
			v, err := parseUint(val)
			if err != nil {
				return fail("bad mem address: " + err.Error())
			}
			rec.HasMem = true
			rec.MemAddr = v
		case "width":
			v, err := parseUint(val)
			if err != nil || v == 0 {
				return fail("bad mem width " + strconv.Quote(val))
			}
			rec.MemWidth = int(v)
		default:
			return fail("unknown field " + strconv.Quote(key))
		}
	}
	if !seen["addr"] {
		return fail("missing field \"addr\"")
	}
	if !seen["op"] {
		return fail("missing field \"op\"")
	}
	switch rec.Class {
	case ClassLoad, ClassStore:
		if !rec.HasMem {
			return fail("missing field \"mem\" for " + rec.Op)
		}
		if rec.MemWidth == 0 {
			return fail("missing field \"width\" for " + rec.Op)
		}
	}
	return rec, nil
}

func parseUint(s string) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(s), 0, 64)
}

func splitList(s string) []string {
	if s == "" || s == "-" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
