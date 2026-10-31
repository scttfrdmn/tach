// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Integration tests: tach runs against testdata/mock-dcgmi.
//
// Runs that end normally call cmdRun in-process, so they count toward
// coverage. Paths that end in os.Exit (flag errors, a missing dcgmi) and the
// commands that print to stdout re-exec this test binary as tach.

func TestMain(m *testing.M) {
	if os.Getenv("TACH_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// mockDcgmi writes a per-test wrapper around testdata/mock-dcgmi that sets the
// mock's environment. Parallel tests cannot share process environment, and
// each needs its own energy counter state.
func mockDcgmi(t *testing.T, env ...string) string {
	t.Helper()
	mock, err := filepath.Abs("testdata/mock-dcgmi")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&b, "MOCK_ENERGY=%q; export MOCK_ENERGY\n", filepath.Join(dir, "energy"))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		fmt.Fprintf(&b, "%s=%q; export %s\n", k, v, k)
	}
	fmt.Fprintf(&b, "exec %q \"$@\"\n", mock)
	path := filepath.Join(dir, "dcgmi")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

type runResult struct {
	code    int
	sum     *Summary
	header  []string
	rows    [][]string
	csvPath string
}

// runTach runs `tach --dcgmi <mock> --no-imds --csv <tmp> <args...>` in-process.
// args must include `-- <command>`.
func runTach(t *testing.T, mockEnv []string, args ...string) runResult {
	t.Helper()
	csvPath := filepath.Join(t.TempDir(), "run.csv")
	argv := append([]string{"--dcgmi", mockDcgmi(t, mockEnv...), "--no-imds", "--csv", csvPath}, args...)
	code := cmdRun(argv)
	sum, err := readSummary(csvPath + ".summary.json")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	f, err := os.Open(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("csv: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("csv has no header")
	}
	return runResult{code: code, sum: sum, header: recs[0], rows: recs[1:], csvPath: csvPath}
}

// execTach re-execs the test binary as tach and returns exit code, stdout and
// stderr.
func execTach(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "TACH_TEST_MAIN=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errb.String()
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func entity(t *testing.T, s *Summary, typ, id string) EntitySummary {
	t.Helper()
	for _, e := range s.Entities {
		if e.Type == typ && e.ID == id {
			return e
		}
	}
	t.Fatalf("no entity %s %s in %+v", typ, id, s.Entities)
	return EntitySummary{}
}

func hasCode(s *Summary, code string) bool {
	for _, f := range s.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

func TestRunFP64Scenario(t *testing.T) {
	t.Parallel()
	r := runTach(t, nil, "--interval", "200ms", "--work", "10", "--unit", "ns", "--price-per-hour", "2", "--label", "fp64", "--", "sleep", "1.1")
	s := r.sum
	if r.code != 0 || s.ExitCode != 0 {
		t.Fatalf("exit %d / %d", r.code, s.ExitCode)
	}
	if !s.DCP || s.Profile != "triage" || s.IntervalMs != 200 || s.Label != "fp64" {
		t.Fatalf("run metadata: %+v", s)
	}
	if s.TachVersion != version || strings.Join(s.Command, " ") != "sleep 1.1" {
		t.Fatalf("version %q command %q", s.TachVersion, s.Command)
	}
	if s.WallSeconds < 1.1 || s.WallSeconds > 5 {
		t.Fatalf("wall %v", s.WallSeconds)
	}

	// The mock prints constants, so every statistic is exact.
	g := entity(t, s, "GPU", "0")
	want := map[string]float64{"sm_active": 0.85, "sm_occupancy": 0.40, "tensor_active": 0.02, "dram_active": 0.30,
		"fp64_active": 0.20, "power_w": 250.5, "sm_clock_mhz": 2100, "fb_used_mib": 6000, "fb_total_mib": 46068,
		"gpu_util_pct": 100, "gpu_temp_c": 55}
	if len(g.Fields) != len(want) {
		t.Fatalf("fields %v", g.Fields)
	}
	for name, v := range want {
		st := g.Fields[name]
		// Like real DCGM, the mock's first row is N/A for profiling fields.
		wantMissing := 0
		if strings.HasSuffix(name, "_active") || name == "sm_occupancy" {
			wantMissing = 1
		}
		if st.N < 3 || st.Missing != wantMissing || !near(st.Mean, v, 1e-9) || st.Min != v || st.Max != v || st.P95 != v {
			t.Errorf("%s: %+v, want %v with %d missing", name, st, v, wantMissing)
		}
	}
	if g.Samples < 3 {
		t.Fatalf("only %d samples at 200ms over %.1fs", g.Samples, s.WallSeconds)
	}

	// Energy counter: the mock adds 500000 mJ between the two reads.
	if s.EnergyMethod != "counter" || s.EnergyJoules != 500 {
		t.Fatalf("energy %v %s", s.EnergyJoules, s.EnergyMethod)
	}
	pu := s.PerUnit
	if pu == nil || !near(pu.Seconds, s.WallSeconds/10, 1e-12) || pu.Joules != 50 || !near(pu.Dollars, 2*s.WallSeconds/3600/10, 1e-15) {
		t.Fatalf("per unit %+v (wall %v)", pu, s.WallSeconds)
	}

	if !hasCode(s, "fp64_required") || !hasCode(s, "memory_headroom") || hasCode(s, "starved") || hasCode(s, "tensor_idle") {
		t.Fatalf("findings %+v", s.Findings)
	}
	if s.Verdict != "FP64-bound hardware choice; oversized memory" {
		t.Fatalf("verdict %q", s.Verdict)
	}

	wantHdr := "timestamp,elapsed_s,entity_type,entity_id,sm_active,sm_occupancy,tensor_active,dram_active,fp64_active,power_w,sm_clock_mhz,fb_used_mib,fb_total_mib,gpu_util_pct,gpu_temp_c"
	if got := strings.Join(r.header, ","); got != wantHdr {
		t.Fatalf("csv header\n got %s\nwant %s", got, wantHdr)
	}
	if len(r.rows) != g.Samples {
		t.Fatalf("%d csv rows, %d samples", len(r.rows), g.Samples)
	}
	for i, row := range r.rows {
		// The first row's profiling cells are blank (N/A), not zero.
		wantSM := "0.85"
		if i == 0 {
			wantSM = ""
		}
		if len(row) != len(r.header) || row[2] != "GPU" || row[3] != "0" || row[4] != wantSM || row[9] != "250.5" {
			t.Fatalf("csv row %d: %v", i, row)
		}
	}
}

func TestRunStarvedPCIe(t *testing.T) {
	t.Parallel()
	r := runTach(t, []string{"MOCK_SCENARIO=starved"}, "--pcie", "--interval", "200ms", "--instance", "g7e.4xlarge", "--", "sleep", "1.1")
	s := r.sum
	if s.IntervalMs != 1000 {
		t.Fatalf("--pcie must raise the interval to 1s, got %dms", s.IntervalMs)
	}
	if s.InstanceType != "g7e.4xlarge" || s.PCIeLineRateGBs != 63 {
		t.Fatalf("instance %q line rate %v", s.InstanceType, s.PCIeLineRateGBs)
	}
	if !strings.Contains(strings.Join(r.header, ","), "fp64_active,pcie_tx_bps,pcie_rx_bps,power_w") {
		t.Fatalf("header %v", r.header)
	}
	if tx := entity(t, s, "GPU", "0").Fields["pcie_tx_bps"]; tx.Mean != 4.1e10 {
		t.Fatalf("pcie_tx_bps %+v", tx)
	}
	// p95 4.1e10 B/s is 65% of 63 GB/s.
	if !hasCode(s, "starved") || !hasCode(s, "pcie_bound") {
		t.Fatalf("findings %+v", s.Findings)
	}
}

func TestRunTensorIdle(t *testing.T) {
	t.Parallel()
	s := runTach(t, []string{"MOCK_SCENARIO=ai_fp32"}, "--", "sleep", "1.1").sum
	if !hasCode(s, "tensor_idle") || !hasCode(s, "fp64_unused") || hasCode(s, "fp64_required") {
		t.Fatalf("findings %+v", s.Findings)
	}
}

func TestRunHPCProfileFields(t *testing.T) {
	t.Parallel()
	r := runTach(t, nil, "--profile", "hpc", "--", "sleep", "1.1")
	if got := strings.Join(r.header[4:9], ","); got != "sm_active,sm_occupancy,dram_active,fp64_active,fp32_active" {
		t.Fatalf("hpc profile columns %s", got)
	}
	if r.sum.Profile != "hpc" || hasCode(r.sum, "tensor_idle") {
		t.Fatalf("%+v", r.sum)
	}
}

func TestRunNoProfilingFallback(t *testing.T) {
	t.Parallel()
	r := runTach(t, []string{"MOCK_NOPROF=1"}, "--", "sleep", "1.1")
	s := r.sum
	if s.DCP {
		t.Fatal("dcp should be false")
	}
	for _, h := range r.header {
		if strings.HasSuffix(h, "_active") || h == "sm_occupancy" {
			t.Fatalf("profiling column %s in fallback CSV", h)
		}
	}
	if _, ok := entity(t, s, "GPU", "0").Fields["power_w"]; !ok {
		t.Fatal("device fields missing in fallback")
	}
	// Absent profiling must not read as an idle GPU.
	if !hasCode(s, "no_dcp") || hasCode(s, "starved") {
		t.Fatalf("findings %+v", s.Findings)
	}
	if !strings.HasPrefix(s.Verdict, "oversized memory") || !strings.Contains(s.Verdict, "device-level only") {
		t.Fatalf("verdict %q", s.Verdict)
	}
}

func TestRunEnergyFallsBackToPowerIntegral(t *testing.T) {
	t.Parallel()
	r := runTach(t, []string{"MOCK_NOENERGY=1"}, "--interval", "200ms", "--", "sleep", "1.1")
	s := r.sum
	if s.EnergyMethod != "power-integral" {
		t.Fatalf("method %q", s.EnergyMethod)
	}
	// Constant 250.5 W over the sampled span.
	var first, last float64
	if _, err := fmt.Sscan(r.rows[0][1], &first); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Sscan(r.rows[len(r.rows)-1][1], &last); err != nil {
		t.Fatal(err)
	}
	if want := 250.5 * (last - first); want <= 0 || !near(s.EnergyJoules, want, 0.01*want) {
		t.Fatalf("energy %v J, want ≈ %v (span %v s)", s.EnergyJoules, want, last-first)
	}
}

func TestRunMIGEntities(t *testing.T) {
	t.Parallel()
	r := runTach(t, []string{"MOCK_MIG=1"}, "--entities", "i:0,i:1", "--", "sleep", "1.1")
	s := r.sum
	var order []string
	for _, e := range s.Entities {
		order = append(order, e.Type+" "+e.ID)
	}
	if strings.Join(order, ",") != "GPU 0,GPU-I 0,GPU-I 1" {
		t.Fatalf("entity order %v", order)
	}
	if st := entity(t, s, "GPU-I", "1").Fields["sm_active"]; st.N == 0 || st.Mean != 0.85 {
		t.Fatalf("per-instance sm_active %+v", st)
	}
	// Energy is whole-GPU: the instance rows' N/A counter is ignored.
	if s.EnergyMethod != "counter" || s.EnergyJoules != 500 {
		t.Fatalf("energy %v %s", s.EnergyJoules, s.EnergyMethod)
	}
	n := 0
	for _, row := range r.rows {
		if row[2] == "GPU-I" {
			n++
		}
	}
	if n == 0 {
		t.Fatal("no GPU-I rows in CSV")
	}
}

func TestRunExitCodePassthrough(t *testing.T) {
	t.Parallel()
	r := runTach(t, nil, "--", "sh", "-c", "exit 3")
	if r.code != 3 || r.sum.ExitCode != 3 {
		t.Fatalf("exit %d / summary %d", r.code, r.sum.ExitCode)
	}
}

func TestRunFlagErrors(t *testing.T) {
	t.Parallel()
	mock := mockDcgmi(t)
	dir := t.TempDir()
	cases := []struct {
		name string
		args []string
		code int
		msg  string
	}{
		{"no command", []string{"--dcgmi", mock}, 2, "usage:"},
		{"interval too short", []string{"--dcgmi", mock, "--interval", "50ms", "--", "true"}, 1, "at least 100ms"},
		{"unknown profile", []string{"--dcgmi", mock, "--profile", "cfd", "--", "true"}, 1, "unknown profile"},
		{"missing dcgmi", []string{"--dcgmi", filepath.Join(dir, "nope"), "--", "true"}, 1, "is DCGM installed?"},
		{"command not found", []string{"--dcgmi", mock, "--no-imds", "--csv", filepath.Join(dir, "x.csv"), "--", filepath.Join(dir, "no-such-cmd")}, 1, "start "},
	}
	for _, c := range cases {
		code, _, stderr := execTach(t, c.args...)
		if code != c.code || !strings.Contains(stderr, c.msg) {
			t.Errorf("%s: exit %d (want %d), stderr %q", c.name, code, c.code, stderr)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	t.Parallel()
	code, out, _ := execTach(t, "version")
	if code != 0 || !strings.HasPrefix(out, "tach "+version+" (commit ") {
		t.Fatalf("exit %d, %q", code, out)
	}
}

func TestAnnotateThenReport(t *testing.T) {
	t.Parallel()
	a := runTach(t, nil, "--label", "a", "--", "sleep", "1.1")
	b := runTach(t, []string{"MOCK_SCENARIO=ai_fp32"}, "--label", "b", "--", "sleep", "1.1")
	pa, pb := a.csvPath+".summary.json", b.csvPath+".summary.json"

	if code := cmdAnnotate([]string{"--work", "5", "--unit", "ns", "--price-per-hour", "3", pa}); code != 0 {
		t.Fatalf("annotate exit %d", code)
	}
	if code := cmdAnnotate([]string{"--work", "5", "--unit", "ns", "--price-per-hour", "300", "--label", "b2", pb}); code != 0 {
		t.Fatalf("annotate exit %d", code)
	}
	sa, err := readSummary(pa)
	if err != nil {
		t.Fatal(err)
	}
	if sa.Work != 5 || sa.Unit != "ns" || sa.PricePerHour != 3 || sa.Label != "a" || sa.PerUnit == nil ||
		!near(sa.PerUnit.Dollars, 3*sa.WallSeconds/3600/5, 1e-15) || sa.PerUnit.Joules != 100 {
		t.Fatalf("annotated %+v / %+v", sa, sa.PerUnit)
	}
	// Findings survive annotation unchanged.
	if !hasCode(sa, "fp64_required") {
		t.Fatalf("findings lost: %+v", sa.Findings)
	}

	code, out, stderr := execTach(t, "report", pb, pa)
	if code != 0 {
		t.Fatalf("report exit %d: %s", code, stderr)
	}
	lines := strings.Split(out, "\n")
	if lines[0] != "| run | instance | wall s | s/ns | $/ns | J/ns | SM active | tensor | FP64 | DRAM | peak mem GiB | verdict |" {
		t.Fatalf("report header %q", lines[0])
	}
	// Both runs are priced, so the cheaper one ($3/h) sorts first.
	if !strings.HasPrefix(lines[2], "| a |") || !strings.HasPrefix(lines[3], "| b2 |") {
		t.Fatalf("report order:\n%s", out)
	}
	if !strings.Contains(out, "Findings (heuristic") {
		t.Fatalf("no findings section:\n%s", out)
	}

	code, out, _ = execTach(t, "report", "--format", "csv", pa, pb)
	recs, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if code != 0 || err != nil || len(recs) != 3 || recs[1][0] != "a" {
		t.Fatalf("csv report exit %d err %v: %q", code, err, out)
	}

	if code, _, _ := execTach(t, "report"); code != 2 {
		t.Fatalf("report without files: exit %d", code)
	}
	if code, _, stderr := execTach(t, "report", "--format", "xml", pa); code != 1 || !strings.Contains(stderr, "unknown format") {
		t.Fatalf("bad format: exit %d %q", code, stderr)
	}
}

func TestRunDcgmiUnusable(t *testing.T) {
	t.Parallel()
	csvPath := filepath.Join(t.TempDir(), "run.csv")
	code, _, stderr := execTach(t, "--dcgmi", mockDcgmi(t, "MOCK_FAIL=1"), "--no-imds", "--csv", csvPath, "--", "true")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	for _, want := range []string{"cannot read the GPUs", "unable to establish a connection", "nv-hostengine"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	// It stops before the command starts: no CSV, no summary.
	if _, err := os.Stat(csvPath); !os.IsNotExist(err) {
		t.Fatalf("CSV written for a run that measured nothing: %v", err)
	}
}

func TestRunProfilingProbeErrorFallsBack(t *testing.T) {
	t.Parallel()
	// dcgmi answers device fields but errors on profiling ones: device-only, not fatal.
	r := runTach(t, []string{"MOCK_PROF_ERROR=1"}, "--", "sleep", "1.1")
	if r.code != 0 || r.sum.DCP || !hasCode(r.sum, "no_dcp") || len(r.sum.Entities) != 1 {
		t.Fatalf("exit %d dcp %v findings %+v", r.code, r.sum.DCP, r.sum.Findings)
	}
}

func TestRunNoSamplesIsFlagged(t *testing.T) {
	t.Parallel()
	r := runTach(t, []string{"MOCK_FAIL_STREAM=1"}, "--", "sleep", "1.1")
	s := r.sum
	if r.code != 0 || len(s.Entities) != 0 || len(r.rows) != 0 {
		t.Fatalf("exit %d, %d entities, %d rows", r.code, len(s.Entities), len(r.rows))
	}
	if !hasCode(s, "no_samples") || !strings.HasPrefix(s.Verdict, "no data") {
		t.Fatalf("findings %+v verdict %q", s.Findings, s.Verdict)
	}
	if len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "host engine connection lost") {
		t.Fatalf("notes %q", s.Notes)
	}
}

func TestRunSignalledChildExitCode(t *testing.T) {
	t.Parallel()
	r := runTach(t, nil, "--", "sh", "-c", "kill -TERM $$")
	if r.code != 143 || r.sum.ExitCode != 143 {
		t.Fatalf("exit %d / summary %d, want 143 (128+SIGTERM)", r.code, r.sum.ExitCode)
	}
}

func TestReportRefusesMixedUnits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, unit string) string {
		p := filepath.Join(dir, name+".summary.json")
		s := &Summary{Label: name, WallSeconds: 100, Work: 10, Unit: unit}
		s.computePerUnit()
		if err := writeSummary(p, s); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ns, ns2, samples := write("a", "ns"), write("b", "ns"), write("c", "samples")
	noWork := filepath.Join(dir, "d.summary.json")
	if err := writeSummary(noWork, &Summary{Label: "d", WallSeconds: 5, Unit: "frames"}); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := execTach(t, "report", ns, samples)
	if code != 1 || !strings.Contains(stderr, `different units ("ns", "samples")`) {
		t.Fatalf("mixed units: exit %d, %q", code, stderr)
	}
	// Same unit, plus a run with no per-unit figures (its unit is not compared).
	if code, out, stderr := execTach(t, "report", ns, ns2, noWork); code != 0 || !strings.Contains(out, "| s/ns |") {
		t.Fatalf("same units: exit %d %q %q", code, out, stderr)
	}
}

// TestCtrlCKeepsSampling sends SIGINT to tach's whole process group, as a
// terminal Ctrl-C does. The command takes 2 s to shut down; dmon, in its own
// group, must keep sampling through that.
func TestCtrlCKeepsSampling(t *testing.T) {
	t.Parallel()
	csvPath := filepath.Join(t.TempDir(), "run.csv")
	cmd := exec.Command(os.Args[0], "--dcgmi", mockDcgmi(t), "--no-imds", "--interval", "200ms", "--csv", csvPath,
		"--", "sh", "-c", `trap 'sleep 2; exit 0' INT; sleep 10 & wait`)
	cmd.Env = append(os.Environ(), "TACH_TEST_MAIN=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	sigAt := time.Now()
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("tach: %v", err)
	}
	s, err := readSummary(csvPath + ".summary.json")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(sigAt); d < 1500*time.Millisecond {
		t.Fatalf("tach exited %v after SIGINT; the command's 2 s shutdown did not run", d)
	}
	// Sampling must cover the shutdown: the last row lands at the end of the
	// run, not at the signal.
	b, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	last := strings.Split(lines[len(lines)-1], ",")
	var lastElapsed float64
	if _, err := fmt.Sscan(last[1], &lastElapsed); err != nil {
		t.Fatal(err)
	}
	// Relative, not absolute: startup probes shift when the command starts,
	// so wall time after the signal is the only stable quantity.
	if lastElapsed < s.WallSeconds-0.5 {
		t.Fatalf("sampling stopped at %.1fs of a %.1fs run (SIGINT at ~2s): dmon died with the Ctrl-C", lastElapsed, s.WallSeconds)
	}
}

// TestRunDropsUnsupportedField reproduces the L40S: DCGM rejects the whole
// watch set because fp64_active is unsupported. tach must drop that field
// and record everything else.
func TestRunDropsUnsupportedField(t *testing.T) {
	t.Parallel()
	r := runTach(t, []string{"MOCK_UNSUPPORTED=1006"}, "--", "sleep", "1.1")
	s := r.sum
	if r.code != 0 || !s.DCP || strings.Join(s.UnsupportedFields, ",") != "fp64_active" {
		t.Fatalf("exit %d dcp %v unsupported %v", r.code, s.DCP, s.UnsupportedFields)
	}
	for _, h := range r.header {
		if h == "fp64_active" {
			t.Fatal("unsupported field still in CSV header")
		}
	}
	g := entity(t, s, "GPU", "0")
	if _, ok := g.Fields["fp64_active"]; ok || g.Fields["sm_active"].N == 0 || len(r.rows) == 0 {
		t.Fatalf("fields %v, %d rows", g.Fields, len(r.rows))
	}
	if hasCode(s, "fp64_required") || hasCode(s, "fp64_unused") || hasCode(s, "no_samples") {
		t.Fatalf("findings %+v", s.Findings)
	}
	if len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "fp64_active") {
		t.Fatalf("notes %q", s.Notes)
	}
}
