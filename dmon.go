// Copyright 2026 Playground Logic LLC
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
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

// profilingAvailable reports whether DCP fields return numbers here. It takes
// a few samples because the first one after group creation can be blank.
func profilingAvailable(bin, entities string) bool {
	rows, err := dmonOnce(bin, []field{fSMActive}, entities, 3)
	if err != nil {
		return false
	}
	for _, r := range rows {
		if !math.IsNaN(r.Vals[0]) {
			return true
		}
	}
	return false
}

// readEnergy returns the energy counter (mJ) per whole GPU. MIG instance rows
// are ignored: DCGM has no per-instance energy counter.
func readEnergy(bin, entities string) (map[string]float64, error) {
	rows, err := dmonOnce(bin, []field{{ID: energyFieldID, Name: "energy_mj"}}, entities, 1)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, r := range rows {
		if r.EntType == "GPU" && !math.IsNaN(r.Vals[0]) {
			out[r.EntID] = r.Vals[0]
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no whole-GPU energy readings (MIG instances alone have no energy counter)")
	}
	return out, nil
}

// sampler runs a streaming `dcgmi dmon` and delivers timestamped rows.
type sampler struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func startSampler(bin string, fields []field, interval time.Duration, entities string, onRow func(time.Time, row)) (*sampler, error) {
	cmd := exec.Command(bin, dmonArgs(fields, int(interval/time.Millisecond), 0, entities)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start dcgmi dmon: %w", err)
	}
	s := &sampler{cmd: cmd, done: make(chan struct{})}
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

// stop interrupts dmon and waits for the reader to drain.
func (s *sampler) stop() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(interruptSignal)
	}
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.done
	}
	_ = s.cmd.Wait()
}
