// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Summary is the <csv>.summary.json document. Its fields are the contract
// `tach report` and `tach annotate` read; add fields, do not rename them.
type Summary struct {
	TachVersion  string   `json:"tach_version"`
	Label        string   `json:"label,omitempty"`
	Profile      string   `json:"profile"`
	Command      []string `json:"command"`
	ExitCode     int      `json:"exit_code"`
	Start        string   `json:"start"`
	WallSeconds  float64  `json:"wall_seconds"`
	IntervalMs   int      `json:"interval_ms"`
	DCP          bool     `json:"dcp"`
	InstanceType string   `json:"instance_type,omitempty"`
	Hostname     string   `json:"hostname,omitempty"`
	CSV          string   `json:"csv"`

	Entities []EntitySummary `json:"entities"`

	EnergyJoules float64 `json:"energy_joules"`
	EnergyMethod string  `json:"energy_method"` // counter | power-integral | none
	EnergyNote   string  `json:"energy_note,omitempty"`

	PCIeLineRateGBs float64 `json:"pcie_linerate_gbs,omitempty"`

	// Optional, set by --work/--unit/--price-per-hour or `tach annotate`.
	Work         float64   `json:"work,omitempty"`
	Unit         string    `json:"unit,omitempty"`
	PricePerHour float64   `json:"price_per_hour,omitempty"`
	PerUnit      *PerUnit  `json:"per_unit,omitempty"`
	Findings     []Finding `json:"findings"`
	Verdict      string    `json:"verdict"`
	Notes        []string  `json:"notes,omitempty"`
}

type EntitySummary struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Samples int             `json:"samples"`
	Fields  map[string]stat `json:"fields"`
}

type PerUnit struct {
	Seconds float64 `json:"seconds"`
	Joules  float64 `json:"joules,omitempty"`
	Dollars float64 `json:"dollars,omitempty"`
}

// Finding is one heuristic observation. Code is stable for tooling; Text is
// for people.
type Finding struct {
	Code string `json:"code"`
	Text string `json:"text"`
}

func (s *Summary) computePerUnit() {
	s.PerUnit = nil
	if s.Work <= 0 {
		return
	}
	pu := &PerUnit{Seconds: s.WallSeconds / s.Work}
	if s.EnergyMethod != "none" && s.EnergyJoules > 0 {
		pu.Joules = s.EnergyJoules / s.Work
	}
	if s.PricePerHour > 0 {
		pu.Dollars = s.PricePerHour * s.WallSeconds / 3600 / s.Work
	}
	s.PerUnit = pu
}

func readSummary(path string) (*Summary, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Summary
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

func writeSummary(path string, s *Summary) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// gpuMean averages a field's mean over the measured entities. Whole GPUs are
// used when present; otherwise MIG instances.
func (s *Summary) gpuMean(name string) (float64, bool) {
	return s.aggregate(name, func(st stat) float64 { return st.Mean })
}

func (s *Summary) gpuP95(name string) (float64, bool) {
	return s.aggregate(name, func(st stat) float64 { return st.P95 })
}

func (s *Summary) aggregate(name string, pick func(stat) float64) (float64, bool) {
	ents := s.primaryEntities()
	sum, n := 0.0, 0
	for _, e := range ents {
		if st, ok := e.Fields[name]; ok && st.N > 0 {
			sum += pick(st)
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

func (s *Summary) primaryEntities() []EntitySummary {
	var gpus, other []EntitySummary
	for _, e := range s.Entities {
		if e.Type == "GPU" {
			gpus = append(gpus, e)
		} else {
			other = append(other, e)
		}
	}
	if len(gpus) > 0 {
		return gpus
	}
	return other
}

// peakMemory returns the largest per-GPU peak framebuffer use and that GPU's
// total, in MiB.
func (s *Summary) peakMemory() (used, total float64, ok bool) {
	for _, e := range s.primaryEntities() {
		u, ok1 := e.Fields["fb_used_mib"]
		t, ok2 := e.Fields["fb_total_mib"]
		if ok1 && ok2 && u.N > 0 && t.N > 0 && u.Max >= used {
			used, total, ok = u.Max, t.Max, true
		}
	}
	return
}

// Thresholds for the heuristic findings. They are deliberately coarse; the
// CSV is the ground truth.
const (
	thStarved      = 0.30 // sm_active mean below this: GPU mostly idle
	thBusy         = 0.50
	thLowOcc       = 0.25
	thMemBoundDRAM = 0.50
	thPipeLow      = 0.30
	thFP64Used     = 0.05 // fp64_active mean at or above this: FP64 matters
	thFP64Unused   = 0.01
	thTensorIdle   = 0.05
	thHeadroom     = 0.25 // peak memory below this fraction of installed
	thPCIeHigh     = 0.60 // p95 of line rate
)

func (s *Summary) analyze() {
	var fs []Finding
	add := func(code, format string, a ...any) {
		fs = append(fs, Finding{code, fmt.Sprintf(format, a...)})
	}
	sm, okSM := s.gpuMean("sm_active")
	occ, okOcc := s.gpuMean("sm_occupancy")
	dram, okDRAM := s.gpuMean("dram_active")
	tensor, okT := s.gpuMean("tensor_active")
	fp64, okF64 := s.gpuMean("fp64_active")

	if len(s.Entities) == 0 {
		add("no_samples", "No samples were recorded: dcgmi dmon returned no data during the run, so there is nothing to analyse. See notes for dcgmi's message.")
	}
	if !s.DCP {
		add("no_dcp", "Device-level metrics only. Run nv-hostengine as root on a datacenter GPU for SM, pipe and DRAM activity.")
	}

	if okSM && sm < thStarved {
		add("starved", "SM active averaged %.2f: the GPU was idle most of the run. Check data loading, host-side loops and host-device copies before changing hardware.", sm)
		if tx, ok := s.pcieUtil(); ok && tx >= thPCIeHigh {
			add("pcie_bound", "PCIe reached %.0f%% of line rate (p95) while SMs were idle: likely input-bound or host round-trips.", tx*100)
		}
	}
	if okSM && okOcc && sm >= thBusy && occ < thLowOcc {
		add("low_occupancy", "SMs active (%.2f) but occupancy low (%.2f): small kernels or small batches. Larger batches, fused kernels, or several jobs per GPU (MPS/MIG).", sm, occ)
	}
	if okDRAM && dram >= thMemBoundDRAM && s.maxPipe() < thPipeLow {
		add("memory_bound", "DRAM active %.2f with compute pipes below %.2f: memory-bandwidth-bound. Choose GPUs on GB/s, not FLOPs.", dram, thPipeLow)
	}
	if okF64 {
		switch {
		case fp64 >= thFP64Used:
			add("fp64_required", "FP64 pipe active %.2f: FP64 is on the critical path. Needs A100, H100/H200 or B200 (1:2 FP64); L4, L40S, RTX PRO and B300 run FP64 at about 1:64.", fp64)
		case fp64 < thFP64Unused:
			add("fp64_unused", "FP64 pipe active %.3f: the code does not need an FP64-class GPU.", fp64)
		}
	}
	// Low tensor activity only matters for AI work; an FP64 code with idle
	// tensor cores is expected, not a finding.
	fp64Bound := okF64 && fp64 >= thFP64Used
	if okT && okSM && sm >= thStarved && tensor < thTensorIdle && s.Profile != "hpc" && !fp64Bound {
		add("tensor_idle", "Tensor pipe active %.3f while SMs were busy: for AI work this usually means FP32. Try BF16 autocast or TF32.", tensor)
	}
	if used, total, ok := s.peakMemory(); ok && total > 0 && used/total < thHeadroom {
		add("memory_headroom", "Peak memory %.1f GiB of %.1f GiB (%.0f%%): a smaller GPU or a MIG slice would fit.", used/1024, total/1024, 100*used/total)
	}
	s.Findings = fs
	s.Verdict = s.verdict()
}

func (s *Summary) maxPipe() float64 {
	m := 0.0
	for _, n := range []string{"tensor_active", "fp64_active", "fp32_active", "fp16_active"} {
		if v, ok := s.gpuMean(n); ok && v > m {
			m = v
		}
	}
	return m
}

// pcieUtil returns the larger of TX/RX p95 as a fraction of line rate.
func (s *Summary) pcieUtil() (float64, bool) {
	if s.PCIeLineRateGBs <= 0 {
		return 0, false
	}
	best, ok := 0.0, false
	for _, n := range []string{"pcie_tx_bps", "pcie_rx_bps"} {
		if v, got := s.gpuP95(n); got {
			ok = true
			best = math.Max(best, v/(s.PCIeLineRateGBs*1e9))
		}
	}
	return best, ok
}

func (s *Summary) verdict() string {
	order := []string{"no_samples", "starved", "memory_bound", "low_occupancy", "tensor_idle", "fp64_required", "memory_headroom", "no_dcp"}
	have := map[string]bool{}
	for _, f := range s.Findings {
		have[f.Code] = true
	}
	labels := map[string]string{
		"no_samples":      "no data",
		"starved":         "underutilized",
		"memory_bound":    "memory-bound",
		"low_occupancy":   "occupancy-limited",
		"tensor_idle":     "tensor cores idle",
		"fp64_required":   "FP64-bound hardware choice",
		"memory_headroom": "oversized memory",
		"no_dcp":          "device-level only",
	}
	var parts []string
	for _, c := range order {
		if have[c] {
			parts = append(parts, labels[c])
		}
	}
	if len(parts) == 0 {
		if sm, ok := s.gpuMean("sm_active"); ok {
			return fmt.Sprintf("busy (SM active %.2f); no bottleneck flagged", sm)
		}
		return "no verdict"
	}
	return strings.Join(parts, "; ")
}

// entitySummaries builds per-entity stats from collected samples.
func entitySummaries(fields []field, data map[string]*series) []EntitySummary {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	// Whole GPUs first, then MIG instances, each by numeric ID.
	rank := map[string]int{"GPU": 0, "GPU-I": 1, "GPU-CI": 2}
	sort.Slice(keys, func(i, j int) bool {
		a, b := data[keys[i]], data[keys[j]]
		if rank[a.EntType] != rank[b.EntType] {
			return rank[a.EntType] < rank[b.EntType]
		}
		ai, errA := strconv.Atoi(a.EntID)
		bi, errB := strconv.Atoi(b.EntID)
		if errA == nil && errB == nil && ai != bi {
			return ai < bi
		}
		return keys[i] < keys[j]
	})
	var out []EntitySummary
	for _, k := range keys {
		sr := data[k]
		es := EntitySummary{Type: sr.EntType, ID: sr.EntID, Samples: len(sr.T), Fields: map[string]stat{}}
		for i, f := range fields {
			if st, ok := summarize(sr.Vals[i]); ok {
				es.Fields[f.Name] = st
			} else if st.Missing > 0 {
				es.Fields[f.Name] = st
			}
		}
		out = append(out, es)
	}
	return out
}
