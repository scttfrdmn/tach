// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// row is one parsed `dcgmi dmon` data line.
type row struct {
	EntType string // GPU, GPU-I, GPU-CI
	EntID   string
	Vals    []float64 // NaN where dmon printed N/A or a blank
}

// parseDmonLine parses a dmon data line such as
//
//	GPU 0     0.812    0.401    N/A
//	GPU-I 3   0.120    0.050    0.000
//
// Header lines (#Entity, ID) and anything that is not an entity row return
// ok=false. nfields is the number of fields requested; a row with a different
// value count is rejected rather than misaligned.
func parseDmonLine(line string, nfields int) (row, bool) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return row{}, false
	}
	t := f[0]
	if t != "GPU" && !strings.HasPrefix(t, "GPU-") {
		return row{}, false
	}
	if len(f)-2 != nfields {
		return row{}, false
	}
	r := row{EntType: t, EntID: f[1], Vals: make([]float64, nfields)}
	for i, s := range f[2:] {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			v = math.NaN()
		}
		r.Vals[i] = v
	}
	return r, true
}

func dmonArgs(fields []field, intervalMs, count int, entities string) []string {
	args := []string{"dmon", "-e", fieldIDs(fields), "-d", strconv.Itoa(intervalMs)}
	if count > 0 {
		args = append(args, "-c", strconv.Itoa(count))
	}
	if entities != "" {
		args = append(args, "-i", entities)
	}
	return args
}

// dmonOnce runs a short dmon and returns the parsed rows.
func dmonOnce(bin string, fields []field, entities string, count int) ([]row, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, dmonArgs(fields, 100, count, entities)...).CombinedOutput()
	var rows []row
	for _, line := range strings.Split(string(out), "\n") {
		if r, ok := parseDmonLine(line, len(fields)); ok {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		msg := strings.TrimSpace(string(out))
		if err != nil {
			return nil, fmt.Errorf("dcgmi dmon: %v: %s", err, msg)
		}
		return nil, fmt.Errorf("dcgmi dmon returned no entity rows: %s", msg)
	}
	return rows, nil
}

// probeDCGM reports whether DCP fields return numbers here. It takes a few
// samples because the first one after group creation can be blank.
//
// A failed profiling probe alone does not mean "no DCP": when dcgmi cannot
// reach nv-hostengine every query fails. So it then probes one device field,
// and if that fails too it returns the error, so the caller can say dcgmi is
// unusable instead of recording an empty run as a device-only one.
func probeDCGM(bin, entities string) (dcp bool, err error) {
	rows, perr := dmonOnce(bin, []field{fSMActive}, entities, 3)
	for _, r := range rows {
		if !math.IsNaN(r.Vals[0]) {
			return true, nil
		}
	}
	if perr == nil {
		return false, nil
	}
	if _, err := dmonOnce(bin, deviceFields[:1], entities, 1); err != nil {
		return false, err
	}
	return false, nil
}

// supportedFields returns the fields dmon accepts as a set here, and the
// names of those it does not. DCGM rejects a whole watch set when one field
// is unsupported ("Error setting watches. Result: -6: Feature not supported";
// fp64_active on an L40S), so when the set fails each field is tried alone.
// The error is non-nil only when the set still fails without them.
func supportedFields(bin string, fields []field, entities string) (ok []field, unsupported []string, err error) {
	if _, err := dmonOnce(bin, fields, entities, 1); err == nil {
		return fields, nil, nil
	}
	for _, f := range fields {
		if _, err := dmonOnce(bin, []field{f}, entities, 1); err != nil {
			unsupported = append(unsupported, f.Name)
			continue
		}
		ok = append(ok, f)
	}
	if len(ok) == 0 {
		return nil, unsupported, fmt.Errorf("dcgmi dmon accepts none of the requested fields")
	}
	if _, err := dmonOnce(bin, ok, entities, 1); err != nil {
		return nil, unsupported, fmt.Errorf("dcgmi dmon rejects the requested fields together, though each works alone: %v", err)
	}
	return ok, unsupported, nil
}

// readEnergy returns the energy counter (mJ) per whole GPU.
//
// DCGM has no per-instance energy counter: GPU-I rows repeat the whole GPU's
// value on every instance, so they are never summed. When only MIG instances
// were requested, the counter is read from their parent GPUs instead, once
// per GPU (viaParents reports this).
func readEnergy(bin, entities string) (mj map[string]float64, viaParents bool, err error) {
	rows, err := dmonOnce(bin, []field{{ID: energyFieldID, Name: "energy_mj"}}, entities, 1)
	if err != nil {
		return nil, false, err
	}
	out := map[string]float64{}
	var instances []string
	for _, r := range rows {
		switch {
		case r.EntType == "GPU" && !math.IsNaN(r.Vals[0]):
			out[r.EntID] = r.Vals[0]
		case r.EntType == "GPU-I":
			instances = append(instances, r.EntID)
		}
	}
	if len(out) > 0 {
		return out, false, nil
	}
	if len(instances) == 0 {
		return nil, false, fmt.Errorf("no whole-GPU energy readings")
	}
	parents, err := migParents(bin)
	if err != nil {
		return nil, false, fmt.Errorf("MIG instances only, and their parent GPUs are unknown: %w", err)
	}
	seen := map[string]bool{}
	var gpus []string
	for _, id := range instances {
		if g, ok := parents[id]; ok && !seen[g] {
			seen[g] = true
			gpus = append(gpus, g)
		}
	}
	if len(gpus) == 0 {
		return nil, false, fmt.Errorf("no parent GPU found for MIG instances %s", strings.Join(instances, ","))
	}
	out, _, err = readEnergy(bin, strings.Join(gpus, ","))
	return out, true, err
}

// pcieLink is a negotiated PCIe link: generation and lane count.
type pcieLink struct{ Gen, Width int }

func (l pcieLink) String() string { return fmt.Sprintf("gen%d x%d", l.Gen, l.Width) }

// pcieLaneGBs is the per-lane, per-direction rate after encoding, in GB/s:
// the usual x16 figures (4, 8, 15.75, 31.5, 63, 121) over 16, so Gen5 x16 is
// 63 and Gen4 x8 is 15.75 exactly.
var pcieLaneGBs = map[int]float64{1: 0.25, 2: 0.5, 3: 0.984375, 4: 1.96875, 5: 3.9375, 6: 7.5625}

// GBs is the link's line rate per direction, or 0 for an unknown generation.
func (l pcieLink) GBs() float64 { return pcieLaneGBs[l.Gen] * float64(l.Width) }

// readPCIeLink returns the slowest negotiated link among the whole GPUs
// measured, from DCGM's max link gen (235) and current width (238). Like the
// energy counter, the link is a whole-GPU property: with MIG instances only,
// it is read from their parent GPUs.
func readPCIeLink(bin, entities string) (pcieLink, error) {
	rows, err := dmonOnce(bin, []field{fPCIeMaxGen, fPCIeWidth}, entities, 1)
	if err != nil {
		return pcieLink{}, err
	}
	var instances []string
	for _, r := range rows {
		if r.EntType == "GPU-I" {
			instances = append(instances, r.EntID)
		}
	}
	if l, sawGPU := slowestLink(rows); sawGPU || len(instances) == 0 {
		return l, linkErr(l)
	}
	parents, err := migParents(bin)
	if err != nil {
		return pcieLink{}, fmt.Errorf("MIG instances only, and their parent GPUs are unknown: %w", err)
	}
	seen := map[string]bool{}
	var gpus []string
	for _, id := range instances {
		if g, ok := parents[id]; ok && !seen[g] {
			seen[g] = true
			gpus = append(gpus, g)
		}
	}
	if len(gpus) == 0 {
		return pcieLink{}, fmt.Errorf("no parent GPU found for MIG instances %s", strings.Join(instances, ","))
	}
	if rows, err = dmonOnce(bin, []field{fPCIeMaxGen, fPCIeWidth}, strings.Join(gpus, ","), 1); err != nil {
		return pcieLink{}, err
	}
	l, _ := slowestLink(rows)
	return l, linkErr(l)
}

// slowestLink returns the slowest valid link among the whole-GPU rows, and
// whether there were any whole-GPU rows. N/A (NaN) and out-of-range values
// are skipped.
func slowestLink(rows []row) (best pcieLink, sawGPU bool) {
	for _, r := range rows {
		if r.EntType != "GPU" {
			continue
		}
		sawGPU = true
		gen, width := r.Vals[0], r.Vals[1]
		if !(gen == math.Trunc(gen) && width == math.Trunc(width) && width >= 1 && width <= 32) {
			continue
		}
		l := pcieLink{int(gen), int(width)}
		if l.GBs() > 0 && (best.Gen == 0 || l.GBs() < best.GBs()) {
			best = l
		}
	}
	return best, sawGPU
}

func linkErr(l pcieLink) error {
	if l.Gen == 0 {
		return fmt.Errorf("DCGM reports no usable PCIe link generation and width")
	}
	return nil
}

// discoveryInstance matches a GPU instance line of `dcgmi discovery -c`:
//
//	| -> I 0/3          | GPU Instance (EntityID: 0)          |
var discoveryInstance = regexp.MustCompile(`->\s*I\s+(\d+)/\d+\s*\|\s*GPU Instance \(EntityID:\s*(\d+)\)`)

// migParents maps each MIG GPU instance entity ID to its parent GPU index.
func migParents(bin string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "discovery", "-c").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("dcgmi discovery -c: %v: %s", err, strings.TrimSpace(string(out)))
	}
	m := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		if g := discoveryInstance.FindStringSubmatch(l); g != nil {
			m[g[2]] = g[1]
		}
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("dcgmi discovery -c listed no GPU instances")
	}
	return m, nil
}

// sampler runs a streaming `dcgmi dmon` and delivers timestamped rows.
type sampler struct {
	cmd    *exec.Cmd
	done   chan struct{}
	stderr tailBuffer
}

// tailBuffer keeps the last few KiB written to it: enough of dmon's stderr to
// explain a failure, bounded however long the run.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

const tailMax = 4096

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > tailMax {
		t.b = append([]byte(nil), t.b[len(t.b)-tailMax:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

func startSampler(bin string, fields []field, interval time.Duration, entities string, onRow func(time.Time, row)) (*sampler, error) {
	cmd := exec.Command(bin, dmonArgs(fields, int(interval/time.Millisecond), 0, entities)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s := &sampler{cmd: cmd, done: make(chan struct{})}
	cmd.Stderr = &s.stderr
	// Its own process group, so a Ctrl-C meant for the wrapped command does
	// not stop sampling while that command shuts down. tach stops it in
	// stop(); if tach dies first, dmon exits on its next write to the pipe.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start dcgmi dmon: %w", err)
	}
	go func() {
		defer close(s.done)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			if r, ok := parseDmonLine(sc.Text(), len(fields)); ok {
				onRow(time.Now(), r)
			}
		}
	}()
	return s, nil
}

// stop interrupts dmon's process group and waits for the reader to drain.
// The group, not just the pid: a dcgmi wrapper script (or anything dmon
// spawns) gets the signal too, instead of waiting out the kill timeout.
func (s *sampler) stop() {
	if s.cmd.Process != nil {
		if sig, ok := interruptSignal.(syscall.Signal); ok {
			_ = syscall.Kill(-s.cmd.Process.Pid, sig)
		} else {
			_ = s.cmd.Process.Signal(interruptSignal)
		}
	}
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
	}
	_ = s.cmd.Wait()
}
