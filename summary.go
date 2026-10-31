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

	// Requested fields DCGM does not support on this GPU (e.g. fp64_active
	// on an L40S). They are absent from the CSV and entity stats.
	UnsupportedFields []string `json:"unsupported_fields,omitempty"`

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

	// BusyFraction is the share of samples with sm_active at or above
	// thStarved; BusyFields are the field stats over just those samples.
	// Pipe and precision findings are judged on the busy samples, so a job
	// that spends most of its time loading or staging is still read for
	// what it does on the GPU (#55). Absent without sm_active.
	BusyFraction *float64        `json:"busy_fraction,omitempty"`
	BusyFields   map[string]stat `json:"busy_fields,omitempty"`
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

// workMean averages a field over the busy samples of the measured entities,
// falling back to the whole run when no entity has busy samples.
func (s *Summary) workMean(name string) (float64, bool) {
	ents := s.primaryEntities()
	sum, n := 0.0, 0
	for _, e := range ents {
		if st, ok := e.BusyFields[name]; ok && st.N > 0 {
			sum += st.Mean
			n++
		}
	}
	if n == 0 {
		return s.gpuMean(name)
	}
	return sum / float64(n), true
}

// busyFraction averages the entities' busy fractions.
func (s *Summary) busyFraction() (float64, bool) {
	sum, n := 0.0, 0
	for _, e := range s.primaryEntities() {
		if e.BusyFraction != nil {
			sum += *e.BusyFraction
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
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
	// A pipe this busy is saturated: not small kernels. 0.40, not 0.50: an
	// RTX 5090 BF16 GEMM at ~218 TFLOPS (its peak) reads tensor_active 0.477
	// (#29), and RTX PRO 6000 / L40S / H100 GEMMs read 0.65-0.97 (#11).
	thPipeBusy     = 0.40
	thUnattributed = 0.10 // busy SMs, every measured pipe below this: work no counter sees
	thHeadroom     = 0.25 // peak memory below this fraction of installed
	thPCIeHigh     = 0.60 // p95 of line rate
)

func (s *Summary) analyze() {
	var fs []Finding
	add := func(code, format string, a ...any) {
		fs = append(fs, Finding{code, fmt.Sprintf(format, a...)})
	}
	// starved judges the whole run; everything else judges the busy samples.
	runSM, okSM := s.gpuMean("sm_active")
	sm, _ := s.workMean("sm_active")
	occ, okOcc := s.workMean("sm_occupancy")
	dram, okDRAM := s.workMean("dram_active")
	tensor, okT := s.workMean("tensor_active")
	fp64Pipe, okF64 := s.workMean("fp64_active")
	fp64Tensor, okF64T := s.fp64Tensor()
	fp64 := fp64Pipe + fp64Tensor
	pipe := s.maxPipe()
	// Busy SMs that no measured pipe accounts for: on an RTX PRO 6000 an FP64
	// DGEMM reads 0 on every pipe at sm_active 0.96 (#11).
	// fp32_active must be measured: without it ordinary FP32 work (e.g. under
	// the inference profile) would look unattributed.
	_, okF32 := s.workMean("fp32_active")
	unattributed := s.DCP && okSM && sm >= thBusy && okF32 && pipe < thUnattributed

	if len(s.Entities) == 0 {
		add("no_samples", "No samples were recorded: dcgmi dmon returned no data during the run, so there is nothing to analyse. See notes for dcgmi's message.")
	}
	if !s.DCP {
		add("no_dcp", "Device-level metrics only. Run nv-hostengine as root on a datacenter GPU for SM, pipe and DRAM activity.")
	}

	if okSM && runSM < thStarved {
		busy := ""
		if f, ok := s.busyFraction(); ok && f > 0 {
			busy = fmt.Sprintf(" It was busy in %.0f%% of samples; the other findings describe those.", 100*f)
		}
		add("starved", "SM active averaged %.2f: the GPU was idle most of the run. Check data loading, host-side loops and host-device copies before changing hardware.%s", runSM, busy)
		if tx, ok := s.pcieUtil(); ok && tx >= thPCIeHigh {
			add("pcie_bound", "PCIe reached %.0f%% of line rate (p95) while SMs were idle: likely input-bound or host round-trips.", tx*100)
		}
	}
	// Large-tile GEMM kernels run few warps per SM by design; occupancy is
	// only a problem when no pipe is saturated (#11: occupancy 0.06-0.15 at
	// full BF16/FP32/FP64 throughput on all three GPUs measured).
	if okSM && okOcc && sm >= thBusy && occ < thLowOcc && pipe < thPipeBusy && !unattributed {
		add("low_occupancy", "SMs active (%.2f) but occupancy low (%.2f) and no compute pipe saturated: small kernels or small batches. Larger batches, fused kernels, or several jobs per GPU (MPS/MIG).", sm, occ)
	}
	if okDRAM && dram >= thMemBoundDRAM && pipe < thPipeLow {
		add("memory_bound", "DRAM active %.2f with compute pipes below %.2f: memory-bandwidth-bound. Choose GPUs on GB/s, not FLOPs.", dram, thPipeLow)
	}
	fp64Required := okF64 && fp64 >= thFP64Used
	switch {
	case !s.wantsFP64():
	case fp64Required:
		add("fp64_required", "FP64 active %.2f (FP64 pipe %.2f, FP64 on tensor cores %.2f): FP64 is on the critical path. Needs A100, H100/H200 or B200 (1:2 FP64); L4, L40S, RTX PRO and B300 run FP64 at about 1:64.", fp64, fp64Pipe, fp64Tensor)
	case unattributed:
		add("fp64_unmeasured", "SMs active %.2f but no measured pipe accounts for it (all below %.2f): this GPU's counters do not show FP64 work, so FP64 need cannot be judged here. Check the code's precision before choosing hardware.", sm, thUnattributed)
	case okF64 && fp64 < thFP64Unused && (okF64T || !okT || tensor < thTensorIdle):
		// Without the imma/hmma split, busy tensor cores could be FP64 (DMMA).
		add("fp64_unused", "FP64 active %.3f: the code does not need an FP64-class GPU.", fp64)
	}
	// Low tensor activity only matters for AI work; an FP64 code with idle
	// tensor cores is expected, not a finding.
	if okT && okSM && sm >= thStarved && tensor < thTensorIdle && s.Profile != "hpc" && !fp64Required && !unattributed {
		add("tensor_idle", "Tensor pipe active %.3f while SMs were busy: for AI work this usually means FP32. Try BF16 autocast or TF32.", tensor)
	}
	if used, total, ok := s.peakMemory(); ok && total > 0 && used/total < thHeadroom {
		add("memory_headroom", "Peak memory %.1f GiB of %.1f GiB (%.0f%%): a smaller GPU or a MIG slice would fit.", used/1024, total/1024, 100*used/total)
	}
	s.Findings = fs
	s.Verdict = s.verdict()
}

// fp64Tensor estimates FP64 work on tensor cores (DMMA): tensor activity that
// neither integer (imma) nor half/bf16/tf32 (hmma) matrix work claims. On an
// H100, FP64 DGEMM reads tensor_active 0.97 with hmma and imma both 0, and
// BF16/TF32 read hmma = tensor_active (#11). ok is false without the split.
func (s *Summary) fp64Tensor() (float64, bool) {
	t, ok1 := s.workMean("tensor_active")
	h, ok2 := s.workMean("tensor_hmma_active")
	i, ok3 := s.workMean("tensor_imma_active")
	if !ok1 || !ok2 || !ok3 {
		return 0, false
	}
	return math.Max(0, t-h-i), true
}

func (s *Summary) maxPipe() float64 {
	m := 0.0
	for _, n := range []string{"tensor_active", "fp64_active", "fp32_active", "fp16_active"} {
		if v, ok := s.workMean(n); ok && v > m {
			m = v
		}
	}
	return m
}

// wantsFP64 reports whether this run asked about FP64: profiling was on and
// the profile includes fp64_active (whether or not the GPU supports it).
func (s *Summary) wantsFP64() bool {
	if !s.DCP {
		return false
	}
	for _, f := range profiles[s.Profile] {
		if f.Name == "fp64_active" {
			return true
		}
	}
	return false
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
	order := []string{"no_samples", "starved", "memory_bound", "low_occupancy", "tensor_idle", "fp64_required", "fp64_unmeasured", "memory_headroom", "no_dcp"}
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
		"fp64_unmeasured": "FP64 need unknown on this GPU",
		"memory_headroom": "oversized memory",
		"no_dcp":          "device-level only",
	}
	// pcie_bound only fires on a starved run, and says why it is starved.
	if have["pcie_bound"] {
		labels["starved"] = "underutilized (PCIe-bound)"
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

// busyStats returns the share of samples whose sm_active (field index smi) is
// at or above thStarved, and per-field stats over those samples only. Samples
// with sm_active missing count toward neither side.
func busyStats(fields []field, sr *series, smi int) (*float64, map[string]stat) {
	var busy []int
	measured := 0
	for j, v := range sr.Vals[smi] {
		if math.IsNaN(v) {
			continue
		}
		measured++
		if v >= thStarved {
			busy = append(busy, j)
		}
	}
	if measured == 0 {
		return nil, nil
	}
	frac := float64(len(busy)) / float64(measured)
	if len(busy) == 0 {
		return &frac, nil
	}
	out := map[string]stat{}
	for i, f := range fields {
		vals := make([]float64, 0, len(busy))
		for _, j := range busy {
			if j < len(sr.Vals[i]) {
				vals = append(vals, sr.Vals[i][j])
			}
		}
		if st, ok := summarize(vals); ok {
			out[f.Name] = st
		}
	}
	return &frac, out
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
		smi := -1
		for i, f := range fields {
			if f.Name == "sm_active" {
				smi = i
			}
			if st, ok := summarize(sr.Vals[i]); ok {
				es.Fields[f.Name] = st
			} else if st.Missing > 0 {
				es.Fields[f.Name] = st
			}
		}
		if smi >= 0 {
			es.BusyFraction, es.BusyFields = busyStats(fields, sr, smi)
		}
		out = append(out, es)
	}
	return out
}
