// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/csv"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// knownFields maps every column name tach can write to its field.
func knownFields() map[string]field {
	m := map[string]field{}
	for _, fs := range profiles {
		for _, f := range fs {
			m[f.Name] = f
		}
	}
	for _, f := range append(append([]field{}, deviceFields...), fPCIeTX, fPCIeRX) {
		m[f.Name] = f
	}
	return m
}

// replayCSV rebuilds tach's per-entity series from one of its CSVs, so a real
// capture can be re-summarised and re-analysed with the current code.
func replayCSV(t *testing.T, path string) ([]field, map[string]*series) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	known := knownFields()
	var fields []field
	for _, name := range recs[0][4:] {
		fl, ok := known[name]
		if !ok {
			t.Fatalf("%s: unknown column %q", path, name)
		}
		fields = append(fields, fl)
	}
	data := map[string]*series{}
	for _, r := range recs[1:] {
		key := r[2] + "/" + r[3]
		sr := data[key]
		if sr == nil {
			sr = &series{EntType: r[2], EntID: r[3], Vals: make([][]float64, len(fields))}
			data[key] = sr
		}
		el, _ := strconv.ParseFloat(r[1], 64)
		sr.T = append(sr.T, el)
		for i, c := range r[4:] {
			v := math.NaN()
			if c != "" {
				v, _ = strconv.ParseFloat(c, 64)
			}
			sr.Vals[i] = append(sr.Vals[i], v)
		}
	}
	return fields, data
}

func replaySummary(t *testing.T, name, profile string) *Summary {
	t.Helper()
	fields, data := replayCSV(t, filepath.Join("testdata", "real", name))
	s := &Summary{Profile: profile, DCP: true, Entities: entitySummaries(fields, data)}
	s.analyze()
	return s
}

// TestBusyPhaseFromRealRun replays the g7e.2xlarge (RTX PRO 6000) FP64 matmul
// that exposed #55: ~95 s of first-time `import torch` with the GPU idle,
// then ~30 s of FP64 GEMM with sm_active 0.99 and every pipe at 0.
func TestBusyPhaseFromRealRun(t *testing.T) {
	s := replaySummary(t, "g7e.2xlarge-v2-matmul-fp64.csv", "triage")
	f, ok := s.busyFraction()
	if !ok || f < 0.15 || f > 0.30 {
		t.Fatalf("busy fraction %v (%v)", f, ok)
	}
	got := "," + codes(s) + ","
	for _, want := range []string{"starved", "fp64_unmeasured"} {
		if !strings.Contains(got, ","+want+",") {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, ",fp64_unused,") {
		t.Errorf("fp64_unused on an FP64 code: %s", got)
	}
	if !strings.Contains(s.Findings[0].Text, "idle in") {
		t.Errorf("starved text does not give the idle share: %q", s.Findings[0].Text)
	}
	// The work samples include the ramp into the GEMM, hence 0.90 not 0.99.
	if sm := s.Entities[0].BusyFields["sm_active"].Mean; sm < 0.90 {
		t.Errorf("busy-phase sm_active %v", sm)
	}
}

// TestFindingsOnRealRuns re-analyses captured runs made with the current
// profile and checks the findings each should give.
func TestFindingsOnRealRuns(t *testing.T) {
	cases := []struct {
		csv           string
		want, notWant []string
	}{
		{"g7e.2xlarge-v2-matmul-bf16.csv", []string{"fp64_unused"}, []string{"low_occupancy", "tensor_idle", "starved"}},
		{"g7e.2xlarge-v2-matmul-fp32.csv", []string{"tensor_idle"}, []string{"low_occupancy", "fp64_unmeasured"}},
		{"ceres-rtx5090-matmul-bf16.csv", []string{"fp64_unused"}, []string{"low_occupancy", "tensor_idle"}},
		{"ceres-rtx5090-matmul-fp64.csv", []string{"fp64_unmeasured"}, []string{"fp64_unused", "tensor_idle", "low_occupancy"}},
		{"ceres-rtx5090-idle.csv", []string{"starved"}, []string{"fp64_unmeasured", "low_occupancy"}},
	}
	for _, c := range cases {
		s := replaySummary(t, c.csv, "triage")
		got := "," + codes(s) + ","
		for _, w := range c.want {
			if !strings.Contains(got, ","+w+",") {
				t.Errorf("%s: missing %s in %s", c.csv, w, got)
			}
		}
		for _, w := range c.notWant {
			if strings.Contains(got, ","+w+",") {
				t.Errorf("%s: unexpected %s in %s", c.csv, w, got)
			}
		}
	}
}

// TestStarvedKinds: GROMACS benchMEM on an RTX 5090 has work in nearly every
// sample at sm_active ~0.3 (#63); the g7e FP64 run idles through a cold
// `import torch` first (#55). Both are starved, for different reasons.
func TestStarvedKinds(t *testing.T) {
	g := replaySummary(t, "ceres-rtx5090-gromacs-hpc.csv", "hpc")
	if f, _ := g.busyFraction(); f < 0.9 {
		t.Fatalf("GROMACS busy fraction %v", f)
	}
	if !hasCode(g, "starved") || !strings.Contains(g.Findings[0].Text, "kernels too small") || hasCode(g, "fp64_unmeasured") {
		t.Fatalf("GROMACS: %s / %q", codes(g), g.Findings[0].Text)
	}
	if !hasCode(g, "fp64_unused") {
		t.Fatalf("GROMACS is mixed precision; want fp64_unused, got %s", codes(g))
	}
	d := replaySummary(t, "g7e.2xlarge-v2-matmul-fp64.csv", "triage")
	if !strings.Contains(d.Findings[0].Text, "idle in") || strings.Contains(d.Findings[0].Text, "kernels too small") {
		t.Fatalf("diluted FP64 run: %q", d.Findings[0].Text)
	}
}
