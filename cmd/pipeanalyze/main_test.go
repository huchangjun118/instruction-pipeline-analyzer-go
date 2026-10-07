package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleTrace = `# comment
0x1000 add writes=r1 reads=r2,r3
0x1004 lw writes=r4 reads=r1 mem=0x2000:4
0x1008 sub writes=r5 reads=r4,r6
`

func TestStdinTextOutput(t *testing.T) {
	var out, errBuf bytes.Buffer
	err := run([]string{"-dcache", "128:2:16:lru:4"}, strings.NewReader(sampleTrace), &out, &errBuf)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, errBuf.String())
	}
	s := out.String()
	for _, want := range []string{"instructions: 3", "CPI:", "dcache:", "load-use:"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

func TestMultiFileAndJSON(t *testing.T) {
	dir := t.TempDir()
	var files []string
	for i := 0; i < 3; i++ {
		f := filepath.Join(dir, "t"+strings.Repeat("x", i+1)+".trace")
		if err := os.WriteFile(f, []byte(sampleTrace), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	args := append([]string{"-format", "json", "-concurrency", "3"}, files...)
	var out bytes.Buffer
	if err := run(args, nil, &out, &bytes.Buffer{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	var results []map[string]any
	if err := json.Unmarshal(out.Bytes(), &results); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out.String())
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	// Identical traces must produce identical stats.
	for i := 1; i < 3; i++ {
		if results[i]["stats"].(map[string]any)["cycles"] != results[0]["stats"].(map[string]any)["cycles"] {
			t.Errorf("result %d differs from result 0", i)
		}
	}
}

func TestParseErrorsReportedNotFatal(t *testing.T) {
	input := "0x1 frobnicate\n0x2 lw writes=r1\n0x3 add writes=r2\n"
	var out bytes.Buffer
	err := run([]string{}, strings.NewReader(input), &out, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parse errors must not fail the run: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "parse errors: 2") || !strings.Contains(s, "instructions: 1") {
		t.Errorf("expected 2 parse errors and 1 surviving instruction:\n%s", s)
	}
	if !strings.Contains(s, "line 1") || !strings.Contains(s, "line 2") {
		t.Errorf("errors must carry line numbers:\n%s", s)
	}
}

func TestBadConfigFails(t *testing.T) {
	var errBuf bytes.Buffer
	err := run([]string{"-depth", "2"}, strings.NewReader(""), &bytes.Buffer{}, &errBuf)
	if err == nil {
		t.Fatal("invalid depth must fail")
	}
	err = run([]string{"-icache", "100:3:7:lru:2"}, strings.NewReader(""), &bytes.Buffer{}, &errBuf)
	if err == nil {
		t.Fatal("invalid cache spec must fail")
	}
}

func TestTimeoutDoesNotHang(t *testing.T) {
	huge := strings.Repeat("0x1000 add writes=r1 reads=r2\n", 3000000)
	var out bytes.Buffer
	err := run([]string{"-timeout", "50ms"}, strings.NewReader(huge), &out, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected failure due to timeout")
	}
	if !strings.Contains(out.String(), "FAILED") {
		t.Errorf("timeout must be reported per trace:\n%s", out.String())
	}
}

func TestTimelineFlag(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"-timeline"}, strings.NewReader(sampleTrace), &out, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "timeline (cycle: stage occupants):") {
		t.Errorf("timeline view missing:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "stages: IF | ID | EX1 | MEM | WB") {
		t.Errorf("stage legend missing:\n%s", out.String())
	}
}
