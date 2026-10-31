// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"math"
	"strings"
	"testing"
)

func TestParseDmonLine(t *testing.T) {
	cases := []struct {
		line  string
		n     int
		ok    bool
		typ   string
		id    string
		nanAt int
	}{
		{"GPU 0     0.812    0.401    N/A", 3, true, "GPU", "0", 2},
		{"GPU-I 3   0.1 0.2 0.3", 3, true, "GPU-I", "3", -1},
		{"#Entity   SMACT  SMOCC", 2, false, "", "", -1},
		{"ID", 2, false, "", "", -1},
		{"GPU 0  0.1", 2, false, "", "", -1}, // short row is rejected, not misaligned
		{"Error: unable to connect", 2, false, "", "", -1},
	}
	for _, c := range cases {
		r, ok := parseDmonLine(c.line, c.n)
		if ok != c.ok {
			t.Fatalf("%q: ok=%v want %v", c.line, ok, c.ok)
		}
		if !ok {
			continue
		}
		if r.EntType != c.typ || r.EntID != c.id {
			t.Fatalf("%q: got %s %s", c.line, r.EntType, r.EntID)
		}
		for i, v := range r.Vals {
			if (i == c.nanAt) != math.IsNaN(v) {
				t.Fatalf("%q: val %d = %v", c.line, i, v)
			}
		}
	}
}

func TestSummarize(t *testing.T) {
	st, ok := summarize([]float64{1, 2, math.NaN(), 3, 4})
	if !ok || st.N != 4 || st.Missing != 1 {
		t.Fatalf("bad counts: %+v", st)
	}
	if st.Mean != 2.5 || st.Min != 1 || st.Max != 4 || st.P50 != 2.5 {
		t.Fatalf("bad stats: %+v", st)
	}
	if math.Abs(st.P95-3.85) > 1e-9 {
		t.Fatalf("p95 = %v", st.P95)
	}
	if _, ok := summarize([]float64{math.NaN()}); ok {
		t.Fatal("all-missing should not be ok")
	}
}

func TestTrapezoid(t *testing.T) {
	// 100 W for 10 s = 1000 J.
	got := trapezoid([]float64{0, 5, 10}, []float64{100, 100, 100})
	if got != 1000 {
		t.Fatalf("got %v", got)
	}
}

func summaryWith(profile string, fields map[string]float64) *Summary {
	e := EntitySummary{Type: "GPU", ID: "0", Samples: 10, Fields: map[string]stat{}}
	for k, v := range fields {
		e.Fields[k] = stat{N: 10, Mean: v, P50: v, P95: v, Min: v, Max: v}
	}
	return &Summary{Profile: profile, DCP: true, Entities: []EntitySummary{e}}
}

func codes(s *Summary) string {
	var c []string
	for _, f := range s.Findings {
		c = append(c, f.Code)
	}
	return strings.Join(c, ",")
}

func TestAnalyze(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		fields  map[string]float64
		want    []string
		notWant []string
	}{
		{"fp64 code", "triage",
			map[string]float64{"sm_active": 0.85, "sm_occupancy": 0.4, "tensor_active": 0.02, "dram_active": 0.3, "fp64_active": 0.2, "fb_used_mib": 6000, "fb_total_mib": 46068},
			[]string{"fp64_required", "memory_headroom"}, []string{"starved", "tensor_idle"}},
		{"fp32 ai", "triage",
			map[string]float64{"sm_active": 0.8, "sm_occupancy": 0.45, "tensor_active": 0.01, "dram_active": 0.35, "fp64_active": 0, "fp32_active": 0.6, "tensor_hmma_active": 0.01, "tensor_imma_active": 0},
			[]string{"tensor_idle", "fp64_unused"}, []string{"fp64_required"}},
		{"hpc profile has no tensor field", "hpc",
			map[string]float64{"sm_active": 0.8, "sm_occupancy": 0.45, "dram_active": 0.7, "fp64_active": 0.0, "fp32_active": 0.1},
			[]string{"memory_bound"}, []string{"tensor_idle"}},
		{"starved", "triage",
			map[string]float64{"sm_active": 0.1, "dram_active": 0.04},
			[]string{"starved"}, nil},
		{"low occupancy, no pipe saturated", "triage",
			map[string]float64{"sm_active": 0.7, "sm_occupancy": 0.1, "tensor_active": 0.3, "fp64_active": 0, "fp32_active": 0.2, "tensor_hmma_active": 0.3, "tensor_imma_active": 0},
			[]string{"low_occupancy"}, []string{"tensor_idle"}},
		// Measured on hardware (#11, testdata/real/), matmul n=8192, DCGM 4.7.0.
		{"H100 FP64 DGEMM: FP64 on tensor cores", "triage",
			map[string]float64{"sm_active": 0.923, "sm_occupancy": 0.114, "tensor_active": 0.91, "dram_active": 0.132, "fp64_active": 0, "fp32_active": 0, "tensor_hmma_active": 0, "tensor_imma_active": 0},
			[]string{"fp64_required"}, []string{"fp64_unused", "fp64_unmeasured", "tensor_idle", "low_occupancy"}},
		{"H100 FP64 DGEMM, hpc profile", "hpc",
			map[string]float64{"sm_active": 0.923, "sm_occupancy": 0.114, "tensor_active": 0.91, "dram_active": 0.132, "fp64_active": 0, "fp32_active": 0, "tensor_hmma_active": 0, "tensor_imma_active": 0},
			[]string{"fp64_required"}, []string{"fp64_unused"}},
		{"H100 BF16 GEMM", "triage",
			map[string]float64{"sm_active": 0.919, "sm_occupancy": 0.135, "tensor_active": 0.893, "dram_active": 0.244, "fp64_active": 0, "fp32_active": 0, "tensor_hmma_active": 0.893, "tensor_imma_active": 0},
			[]string{"fp64_unused"}, []string{"fp64_required", "tensor_idle", "low_occupancy"}},
		{"H100 FP32 GEMM, TF32 off", "triage",
			map[string]float64{"sm_active": 0.908, "sm_occupancy": 0.114, "tensor_active": 0, "dram_active": 0.06, "fp64_active": 0, "fp32_active": 0.775, "tensor_hmma_active": 0, "tensor_imma_active": 0},
			[]string{"tensor_idle", "fp64_unused"}, []string{"low_occupancy", "fp64_required"}},
		{"RTX 5090 BF16 GEMM at peak: tensor_active only 0.477", "triage",
			map[string]float64{"sm_active": 0.977, "sm_occupancy": 0.0607, "tensor_active": 0.477, "dram_active": 0.1, "fp64_active": 0, "fp32_active": 0, "tensor_hmma_active": 0.477, "tensor_imma_active": 0},
			[]string{"fp64_unused"}, []string{"low_occupancy", "tensor_idle"}},
		{"RTX 5090 FP64 DGEMM", "triage",
			map[string]float64{"sm_active": 0.984, "sm_occupancy": 0.0614, "tensor_active": 0, "fp64_active": 0, "fp32_active": 0, "tensor_hmma_active": 0, "tensor_imma_active": 0},
			[]string{"fp64_unmeasured"}, []string{"fp64_unused", "low_occupancy", "tensor_idle"}},
		{"RTX PRO 6000 FP64 DGEMM: no pipe sees it", "triage",
			map[string]float64{"sm_active": 0.9588, "sm_occupancy": 0.06, "tensor_active": 0, "dram_active": 0.007, "fp64_active": 0, "fp32_active": 0, "tensor_hmma_active": 0, "tensor_imma_active": 0},
			[]string{"fp64_unmeasured"}, []string{"fp64_unused", "tensor_idle", "low_occupancy"}},
		{"RTX PRO 6000 BF16 GEMM", "triage",
			map[string]float64{"sm_active": 0.9496, "sm_occupancy": 0.118, "tensor_active": 0.9311, "dram_active": 0.18, "fp64_active": 0, "tensor_hmma_active": 0.9311, "tensor_imma_active": 0},
			[]string{"fp64_unused"}, []string{"low_occupancy", "tensor_idle", "fp64_unmeasured"}},
		{"inference profile: no fp32_active, FP32 work is not 'unattributed'", "inference",
			map[string]float64{"sm_active": 0.81, "tensor_active": 0.01, "dram_active": 0.35, "fp16_active": 0},
			[]string{"tensor_idle"}, []string{"fp64_unmeasured"}},
		{"L40S FP64 DGEMM: fp64_active unsupported", "triage",
			map[string]float64{"sm_active": 0.9505, "sm_occupancy": 0.079, "tensor_active": 0, "dram_active": 0.0115, "fp32_active": 0},
			[]string{"fp64_unmeasured"}, []string{"fp64_unused", "tensor_idle", "low_occupancy"}},
		{"L40S BF16 GEMM: busy tensor cores, FP64 not measurable", "triage",
			map[string]float64{"sm_active": 0.9413, "sm_occupancy": 0.078, "tensor_active": 0.9195, "dram_active": 0.27},
			nil, []string{"fp64_unused", "fp64_unmeasured", "fp64_required", "low_occupancy", "tensor_idle"}},
		{"tensor busy without the imma/hmma split: FP64 could hide there", "triage",
			map[string]float64{"sm_active": 0.9, "sm_occupancy": 0.1, "tensor_active": 0.9, "fp64_active": 0},
			nil, []string{"fp64_unused", "fp64_required"}},
	}
	for _, c := range cases {
		s := summaryWith(c.profile, c.fields)
		s.analyze()
		got := "," + codes(s) + ","
		for _, w := range c.want {
			if !strings.Contains(got, ","+w+",") {
				t.Errorf("%s: missing %s in %s", c.name, w, got)
			}
		}
		for _, w := range c.notWant {
			if strings.Contains(got, ","+w+",") {
				t.Errorf("%s: unexpected %s in %s", c.name, w, got)
			}
		}
		if s.Verdict == "" {
			t.Errorf("%s: empty verdict", c.name)
		}
	}
}

func TestNoDCPIsNeutral(t *testing.T) {
	s := summaryWith("triage", map[string]float64{"power_w": 80, "gpu_util_pct": 100})
	s.DCP = false
	s.analyze()
	if c := codes(s); c != "no_dcp" {
		t.Fatalf("absent profiling fields must not read as idle; got %s", c)
	}
}

func TestPCIeBound(t *testing.T) {
	s := summaryWith("triage", map[string]float64{"sm_active": 0.1, "pcie_tx_bps": 4.1e10})
	s.PCIeLineRateGBs = 63
	s.analyze()
	if !strings.Contains(codes(s), "pcie_bound") {
		t.Fatalf("got %s", codes(s))
	}
	if s.Verdict != "underutilized (PCIe-bound)" {
		t.Fatalf("verdict %q", s.Verdict)
	}
}

func TestPerUnit(t *testing.T) {
	s := &Summary{WallSeconds: 3600, EnergyJoules: 7200, EnergyMethod: "counter", Work: 100, PricePerHour: 2}
	s.computePerUnit()
	if s.PerUnit.Seconds != 36 || s.PerUnit.Joules != 72 || s.PerUnit.Dollars != 0.02 {
		t.Fatalf("%+v", s.PerUnit)
	}
}

func TestPrimaryEntitiesPrefersWholeGPUs(t *testing.T) {
	s := &Summary{Entities: []EntitySummary{
		{Type: "GPU-I", ID: "0", Fields: map[string]stat{"sm_active": {N: 1, Mean: 0.2}}},
		{Type: "GPU", ID: "0", Fields: map[string]stat{"sm_active": {N: 1, Mean: 0.9}}},
	}}
	if v, _ := s.gpuMean("sm_active"); v != 0.9 {
		t.Fatalf("got %v", v)
	}
}

func TestInferLineRate(t *testing.T) {
	if inferLineRate("g6e.2xlarge") != 31.5 || inferLineRate("g7e.4xlarge") != 63 || inferLineRate("") != 63 {
		t.Fatal("line rate inference")
	}
}

// FuzzParseDmonLine checks that no dmon line, however malformed, panics the
// parser or yields a row whose value count differs from the fields requested.
func FuzzParseDmonLine(f *testing.F) {
	for _, s := range []string{
		"GPU 0     0.812    0.401    N/A",
		"GPU-I 3   0.1 0.2 0.3",
		"GPU-CI 1  1e10 -0 +Inf",
		"#Entity   SMACT  SMOCC",
		"ID",
		"",
		"Error: unable to connect",
	} {
		f.Add(s, 3)
	}
	f.Fuzz(func(t *testing.T, line string, n int) {
		if n < 0 || n > 64 {
			return
		}
		r, ok := parseDmonLine(line, n)
		if ok && len(r.Vals) != n {
			t.Fatalf("%q: %d values for %d fields", line, len(r.Vals), n)
		}
	})
}
