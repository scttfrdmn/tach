// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"sort"
	"strings"
)

// field is one DCGM field tach samples. Prof marks DCGM profiling ("DCP")
// fields, which need a supported datacenter GPU and nv-hostengine running
// with privilege.
type field struct {
	ID   int
	Name string // CSV column and summary key
	Unit string
	Prof bool
}

// DCGM field IDs (dcgm_fields.h).
var (
	fSMActive    = field{1002, "sm_active", "ratio", true}
	fSMOccupancy = field{1003, "sm_occupancy", "ratio", true}
	fTensor      = field{1004, "tensor_active", "ratio", true}
	fDRAM        = field{1005, "dram_active", "ratio", true}
	fFP64        = field{1006, "fp64_active", "ratio", true}
	fFP32        = field{1007, "fp32_active", "ratio", true}
	fFP16        = field{1008, "fp16_active", "ratio", true}
	fIMMA        = field{1013, "tensor_imma_active", "ratio", true}
	fHMMA        = field{1014, "tensor_hmma_active", "ratio", true}
	fPCIeTX      = field{1009, "pcie_tx_bps", "B/s", true}
	fPCIeRX      = field{1010, "pcie_rx_bps", "B/s", true}

	// The negotiated PCIe link, read once per --pcie run for the line rate
	// (#47). Not 237, the current gen: it idles down (Gen1 on an idle RTX PRO
	// 6000) while the width holds, so max gen with current width is the link.
	fPCIeMaxGen = field{235, "pcie_max_link_gen", "", false}
	fPCIeWidth  = field{238, "pcie_link_width", "lanes", false}
)

// deviceFields are appended to every profile. They are NVML-backed and work
// without the profiling module.
var deviceFields = []field{
	{155, "power_w", "W", false},
	{100, "sm_clock_mhz", "MHz", false},
	{252, "fb_used_mib", "MiB", false},
	{250, "fb_total_mib", "MiB", false},
	{203, "gpu_util_pct", "%", false},
	{150, "gpu_temp_c", "C", false},
}

// energyFieldID is DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION: millijoules since
// driver load, monotonic. A start/stop delta gives integrated energy.
const energyFieldID = 156

// profiles group DCP fields by the question they answer.
//
//	triage    — the right-sizing questions in one run: is the GPU busy, is it
//	            using tensor cores, is FP64 on the critical path, is it
//	            memory-bound. The default.
//	hpc       — compute vs memory for FP-heavy simulation codes.
//	inference — are the tensor cores being fed.
var profiles = map[string][]field{
	// imma and hmma split tensor activity by precision. What neither claims
	// is FP64 on tensor cores (DMMA): on an H100, cuBLAS DGEMM shows up only
	// there, not in fp64_active (#11). GPUs without them drop them (#43).
	"triage":    {fSMActive, fSMOccupancy, fTensor, fDRAM, fFP64, fFP32, fIMMA, fHMMA},
	"hpc":       {fSMActive, fSMOccupancy, fDRAM, fFP64, fFP32, fTensor, fIMMA, fHMMA},
	"inference": {fSMActive, fDRAM, fTensor, fFP16},
}

func profileFields(name string) ([]field, error) {
	p, ok := profiles[strings.ToLower(name)]
	if !ok {
		names := make([]string, 0, len(profiles))
		for k := range profiles {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("unknown profile %q (choose one of: %s)", name, strings.Join(names, ", "))
	}
	return append([]field(nil), p...), nil
}

func dropProf(fs []field) []field {
	var out []field
	for _, f := range fs {
		if !f.Prof {
			out = append(out, f)
		}
	}
	return out
}

func fieldIDs(fs []field) string {
	ids := make([]string, len(fs))
	for i, f := range fs {
		ids[i] = fmt.Sprint(f.ID)
	}
	return strings.Join(ids, ",")
}
