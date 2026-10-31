// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func labels(runs []*Summary) string {
	var l []string
	for _, r := range runs {
		l = append(l, r.Label)
	}
	return strings.Join(l, ",")
}

func TestSortRuns(t *testing.T) {
	pu := func(sec, dollars float64) *PerUnit { return &PerUnit{Seconds: sec, Dollars: dollars} }
	cases := []struct {
		name string
		runs []*Summary
		want string
	}{
		{"all dollars: ascending dollars", []*Summary{
			{Label: "a", WallSeconds: 1, PerUnit: pu(1, 0.30)},
			{Label: "b", WallSeconds: 2, PerUnit: pu(9, 0.10)},
			{Label: "c", WallSeconds: 3, PerUnit: pu(5, 0.20)},
		}, "b,c,a"},
		{"dollars stable on ties", []*Summary{
			{Label: "a", PerUnit: pu(3, 0.20)},
			{Label: "b", PerUnit: pu(1, 0.10)},
			{Label: "c", PerUnit: pu(2, 0.20)},
			{Label: "d", PerUnit: pu(0, 0.10)},
		}, "b,d,a,c"},
		{"per-unit but a zero dollar: ascending seconds", []*Summary{
			{Label: "a", WallSeconds: 1, PerUnit: pu(30, 0.01)},
			{Label: "b", WallSeconds: 2, PerUnit: pu(10, 0)},
			{Label: "c", WallSeconds: 3, PerUnit: pu(20, 0.50)},
		}, "b,c,a"},
		{"seconds stable on ties", []*Summary{
			{Label: "a", PerUnit: pu(20, 0)},
			{Label: "b", PerUnit: pu(10, 0)},
			{Label: "c", PerUnit: pu(20, 0)},
			{Label: "d", PerUnit: pu(10, 0)},
		}, "b,d,a,c"},
		{"no per-unit: ascending wall time", []*Summary{
			{Label: "a", WallSeconds: 300},
			{Label: "b", WallSeconds: 100},
			{Label: "c", WallSeconds: 200},
		}, "b,c,a"},
		{"mixed per-unit and none falls through to wall time", []*Summary{
			{Label: "a", WallSeconds: 300, PerUnit: pu(1, 0.01)},
			{Label: "b", WallSeconds: 200},
			{Label: "c", WallSeconds: 100, PerUnit: pu(9, 0.90)},
		}, "c,b,a"},
		{"wall time stable on ties", []*Summary{
			{Label: "a", WallSeconds: 50},
			{Label: "b", WallSeconds: 10},
			{Label: "c", WallSeconds: 50},
			{Label: "d", WallSeconds: 10},
		}, "b,d,a,c"},
	}
	for _, c := range cases {
		sortRuns(c.runs)
		if got := labels(c.runs); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestRunName(t *testing.T) {
	cases := []struct {
		s    Summary
		want string
	}{
		{Summary{Label: "baseline", InstanceType: "g6e.2xlarge", CSV: "run.csv"}, "baseline"},
		{Summary{InstanceType: "g6e.2xlarge", CSV: "run.csv"}, "g6e.2xlarge"},
		{Summary{CSV: "run.csv"}, "run.csv"},
		{Summary{}, ""},
	}
	for _, c := range cases {
		if got := runName(&c.s); got != c.want {
			t.Errorf("runName(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestReportRowsHeader(t *testing.T) {
	hdr := func(unit string) []string {
		return []string{"run", "instance", "wall s", "s/" + unit, "$/" + unit, "J/" + unit,
			"SM active", "tensor", "FP64", "DRAM", "peak mem GiB", "verdict"}
	}
	cases := []struct {
		name string
		runs []*Summary
		want []string
	}{
		{"no unit", []*Summary{{}, {}}, hdr("unit")},
		{"first non-empty unit", []*Summary{{}, {Unit: "frame"}}, hdr("frame")},
		{"no runs", nil, hdr("unit")},
	}
	for _, c := range cases {
		rows := reportRows(c.runs)
		if len(rows) != len(c.runs)+1 {
			t.Fatalf("%s: %d rows, want %d", c.name, len(rows), len(c.runs)+1)
		}
		if !reflect.DeepEqual(rows[0], c.want) {
			t.Errorf("%s: header %q, want %q", c.name, rows[0], c.want)
		}
	}
}

func TestReportRows(t *testing.T) {
	full := summaryWith("triage", map[string]float64{
		"sm_active": 0.856, "tensor_active": 0.1, "fp64_active": 0.004, "dram_active": 0.333,
		"fb_used_mib": 6144, "fb_total_mib": 46068,
	})
	full.Label = "l40s"
	full.InstanceType = "g6e.2xlarge"
	full.WallSeconds = 123.45
	full.Unit = "step"
	full.PerUnit = &PerUnit{Seconds: 1.23456, Dollars: 0.000123456, Joules: 250.5}
	full.Verdict = "tensor cores idle"

	// Only SM active present; no per-unit, no memory.
	sparse := summaryWith("triage", map[string]float64{"sm_active": 0.1})
	sparse.CSV = "b.csv"
	sparse.WallSeconds = 10
	sparse.Verdict = "underutilized"

	// Per-unit with no price and no energy: dollars and joules blank.
	secsOnly := &Summary{CSV: "c.csv", WallSeconds: 7.25, PerUnit: &PerUnit{Seconds: 0.5}}

	rows := reportRows([]*Summary{full, sparse, secsOnly})
	want := [][]string{
		{"run", "instance", "wall s", "s/step", "$/step", "J/step",
			"SM active", "tensor", "FP64", "DRAM", "peak mem GiB", "verdict"},
		{"l40s", "g6e.2xlarge", "123.5", "1.235", "0.0001235", "250.5",
			"0.86", "0.10", "0.00", "0.33", "6.0 / 45.0", "tensor cores idle"},
		{"b.csv", "", "10.0", "", "", "",
			"0.10", "", "", "", "", "underutilized"},
		{"c.csv", "", "7.2", "0.5", "", "",
			"", "", "", "", "", ""},
	}
	if !reflect.DeepEqual(rows, want) {
		for i := range want {
			if i < len(rows) && !reflect.DeepEqual(rows[i], want[i]) {
				t.Errorf("row %d:\n got %q\nwant %q", i, rows[i], want[i])
			}
		}
		if len(rows) != len(want) {
			t.Errorf("%d rows, want %d", len(rows), len(want))
		}
	}
	for i, r := range rows {
		if len(r) != len(want[0]) {
			t.Errorf("row %d has %d cells, want %d", i, len(r), len(want[0]))
		}
	}
}

func TestNumMoney(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{0, ""},
		{1, "1"},
		{36, "36"},
		{0.02, "0.02"},
		{1.23456, "1.235"},
		{12345.6, "1.235e+04"},
		{0.000123456, "0.0001235"},
		{-2.5, "-2.5"},
	}
	for _, c := range cases {
		if got := num(c.v); got != c.want {
			t.Errorf("num(%v) = %q, want %q", c.v, got, c.want)
		}
		if got := money(c.v); got != c.want {
			t.Errorf("money(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestWriteMarkdown(t *testing.T) {
	rows := [][]string{
		{"run", "verdict"},
		{"a|b", "busy"},
		{"", "x || y"},
	}
	var b bytes.Buffer
	writeMarkdown(&b, rows)
	want := "| run | verdict |\n" +
		"| --- | --- |\n" +
		"| a\\|b | busy |\n" +
		"|  | x \\|\\| y |\n"
	if got := b.String(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteMarkdownHeaderOnly(t *testing.T) {
	var b bytes.Buffer
	writeMarkdown(&b, [][]string{{"run"}})
	if got, want := b.String(), "| run |\n| --- |\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestWriteFindings(t *testing.T) {
	runs := []*Summary{
		{Label: "clean"},
		{InstanceType: "g6e.2xlarge", Findings: []Finding{
			{Code: "starved", Text: "SM active averaged 0.10."},
			{Code: "no_dcp", Text: "Device-level metrics only."},
		}},
	}
	var b bytes.Buffer
	writeFindings(&b, runs)
	want := "\nFindings (heuristic; the CSVs are the ground truth):\n" +
		"\nclean\n" +
		"- none\n" +
		"\ng6e.2xlarge\n" +
		"- SM active averaged 0.10.\n" +
		"- Device-level metrics only.\n"
	if got := b.String(); got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestSummaryRoundTrip(t *testing.T) {
	s := &Summary{
		TachVersion:  "0.1.0",
		Label:        "l40s",
		Profile:      "triage",
		Command:      []string{"python", "train.py"},
		ExitCode:     0,
		Start:        "2026-10-30T12:00:00Z",
		WallSeconds:  3600,
		IntervalMs:   1000,
		DCP:          true,
		InstanceType: "g6e.2xlarge",
		Hostname:     "ip-10-0-0-1",
		CSV:          "run.csv",
		Entities: []EntitySummary{
			{Type: "GPU", ID: "0", Samples: 3600, Fields: map[string]stat{
				"sm_active":   {N: 3600, Missing: 2, Mean: 0.8, P50: 0.81, P95: 0.95, Min: 0.1, Max: 0.99},
				"fb_used_mib": {N: 3600, Mean: 6000, P50: 6000, P95: 6100, Min: 500, Max: 6144},
			}},
			{Type: "GPU-I", ID: "1", Samples: 10, Fields: map[string]stat{}},
		},
		EnergyJoules:    7200,
		EnergyMethod:    "counter",
		EnergyNote:      "from total energy counter",
		PCIeLineRateGBs: 31.5,
		Work:            100,
		Unit:            "step",
		PricePerHour:    2,
		PerUnit:         &PerUnit{Seconds: 36, Joules: 72, Dollars: 0.02},
		Findings:        []Finding{{Code: "tensor_idle", Text: "Tensor pipe idle."}},
		Verdict:         "tensor cores idle",
		Notes:           []string{"note one"},
	}
	p := filepath.Join(t.TempDir(), "run.summary.json")
	if err := writeSummary(p, s); err != nil {
		t.Fatal(err)
	}
	got, err := readSummary(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, s)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(b, []byte("}\n")) {
		t.Errorf("summary file should end with a newline")
	}
}

func TestReadSummaryErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.summary.json")
	if err := os.WriteFile(bad, []byte(`{"wall_seconds": `), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := readSummary(bad)
	if err == nil {
		t.Fatalf("malformed JSON: want error, got %+v", s)
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("error %q should mention the path %q", err, bad)
	}
	if s != nil {
		t.Errorf("want nil summary on error, got %+v", s)
	}

	if _, err := readSummary(filepath.Join(dir, "missing.json")); err == nil {
		t.Errorf("missing file: want error")
	}
}

func TestComputePerUnitEdges(t *testing.T) {
	cases := []struct {
		name string
		s    Summary
		want *PerUnit
	}{
		{"zero work", Summary{WallSeconds: 100, Work: 0, PricePerHour: 2, EnergyJoules: 10, EnergyMethod: "counter"}, nil},
		{"negative work", Summary{WallSeconds: 100, Work: -1, PricePerHour: 2}, nil},
		{"energy method none", Summary{WallSeconds: 100, Work: 10, EnergyJoules: 500, EnergyMethod: "none"},
			&PerUnit{Seconds: 10}},
		{"no energy", Summary{WallSeconds: 100, Work: 10, EnergyMethod: "power-integral"},
			&PerUnit{Seconds: 10}},
		{"power integral counts", Summary{WallSeconds: 100, Work: 10, EnergyJoules: 500, EnergyMethod: "power-integral"},
			&PerUnit{Seconds: 10, Joules: 50}},
		{"no price", Summary{WallSeconds: 100, Work: 4, EnergyJoules: 400, EnergyMethod: "counter"},
			&PerUnit{Seconds: 25, Joules: 100}},
	}
	for _, c := range cases {
		s := c.s
		s.PerUnit = &PerUnit{Seconds: 999, Joules: 999, Dollars: 999} // stale value must be replaced
		s.computePerUnit()
		if !reflect.DeepEqual(s.PerUnit, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, s.PerUnit, c.want)
		}
	}
}
