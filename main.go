// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

// tach wraps a command and records GPU utilization and efficiency over time
// through NVIDIA DCGM.
//
//	tach -- python train.py
//	tach --profile hpc --label g6e.xlarge -- gmx mdrun -deffnm bench
//	tach report runs/*.summary.json
//
// It shells out to `dcgmi`, so there is nothing to link: one static binary.
// Output is a per-sample CSV and a <csv>.summary.json with per-field
// mean/p50/p95/max, energy from the DCGM counter delta, heuristic findings,
// and optional time, energy and cost per unit of work.
package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Build identity, overridden at link time (see the Makefile and
// .goreleaser.yaml). version is recorded in every summary as tach_version.
var (
	version = "0.1.0-dev"
	commit  = "none"
	date    = "unknown"
)

var interruptSignal = os.Interrupt

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "report":
			os.Exit(cmdReport(os.Args[2:]))
		case "annotate":
			os.Exit(cmdAnnotate(os.Args[2:]))
		case "version", "--version":
			fmt.Printf("tach %s (commit %s, built %s)\n", version, commit, date)
			return
		}
	}
	os.Exit(cmdRun(os.Args[1:]))
}

func usage() {
	fmt.Fprintf(os.Stderr, `tach %s — GPU utilization and efficiency via DCGM

usage:
  tach [flags] -- <command> [args...]    measure a command
  tach report [--format md|csv] <summary.json>...   compare runs
  tach annotate [--work N] [--unit U] [--price-per-hour P] <summary.json>
  tach version

run flags:
`, version)
}

type series struct {
	EntType, EntID string
	T              []float64
	Vals           [][]float64 // [field][sample]
}

func cmdRun(argv []string) int {
	fl := flag.NewFlagSet("tach", flag.ExitOnError)
	fl.Usage = func() { usage(); fl.PrintDefaults() }
	profile := fl.String("profile", "triage", "metric profile: triage | hpc | inference")
	interval := fl.Duration("interval", time.Second, "sample interval (minimum 100ms; DCGM's native rate is 1s, and faster rates on Ada multiplex into blanks)")
	csvPath := fl.String("csv", "", "per-sample CSV path (default tach-<unix>.csv)")
	label := fl.String("label", "", "run label recorded in the summary")
	dcgmiBin := fl.String("dcgmi", "dcgmi", "path to dcgmi")
	entities := fl.String("entities", "", "dcgmi -i entity list, e.g. 0,1 or i:0,i:1 for MIG instances (default all GPUs)")
	pcie := fl.Bool("pcie", false, "also sample PCIe TX/RX (DCGM fields 1009/1010, bytes/s); forces interval >= 1s")
	linerate := fl.Float64("pcie-linerate", 0, "PCIe line rate per direction in GB/s (0 = infer from instance type; Gen5 x16 = 63)")
	work := fl.Float64("work", 0, "units of work the run completed (for per-unit time, energy, cost)")
	unit := fl.String("unit", "", "name of the work unit, e.g. ns, samples, tokens")
	price := fl.Float64("price-per-hour", 0, "instance price in $/hour (for cost per unit)")
	instance := fl.String("instance", "", "instance type to record (default: EC2 metadata when reachable)")
	noIMDS := fl.Bool("no-imds", false, "do not query EC2 instance metadata")
	_ = fl.Parse(argv)

	cmdArgs := fl.Args()
	if len(cmdArgs) == 0 {
		fl.Usage()
		return 2
	}
	fields, err := profileFields(*profile)
	if err != nil {
		fatalf("%v", err)
	}
	if *interval < 100*time.Millisecond {
		fatalf("--interval must be at least 100ms")
	}
	if *pcie {
		fields = append(fields, fPCIeTX, fPCIeRX)
		if *interval < time.Second {
			warnf("--pcie raises the interval to 1s: PCIe counters are a separate metric group and multiplex at faster rates")
			*interval = time.Second
		}
	}
	fields = append(fields, deviceFields...)

	if _, err := exec.LookPath(*dcgmiBin); err != nil {
		fatalf("cannot find %q — is DCGM installed? (%v)", *dcgmiBin, err)
	}

	sum := &Summary{
		TachVersion:  version,
		Label:        *label,
		Profile:      strings.ToLower(*profile),
		Command:      cmdArgs,
		IntervalMs:   int(*interval / time.Millisecond),
		DCP:          true,
		Work:         *work,
		Unit:         *unit,
		PricePerHour: *price,
	}
	sum.Hostname, _ = os.Hostname()
	sum.InstanceType = *instance
	if sum.InstanceType == "" && !*noIMDS {
		sum.InstanceType = imdsInstanceType()
	}
	if *pcie {
		sum.PCIeLineRateGBs = *linerate
		if sum.PCIeLineRateGBs == 0 {
			sum.PCIeLineRateGBs = inferLineRate(sum.InstanceType)
		}
	}

	if !profilingAvailable(*dcgmiBin, *entities) {
		warnf("DCGM profiling fields are unavailable (needs a datacenter GPU and nv-hostengine running as root).")
		warnf("Recording device-level metrics only: utilization, memory, power, clocks, energy.")
		fields = dropProf(fields)
		sum.DCP = false
	}

	if *csvPath == "" {
		*csvPath = fmt.Sprintf("tach-%d.csv", time.Now().Unix())
	}
	sum.CSV = *csvPath
	cf, err := os.Create(*csvPath)
	if err != nil {
		fatalf("create %s: %v", *csvPath, err)
	}
	defer func() { _ = cf.Close() }()
	cw := csv.NewWriter(cf)
	header := []string{"timestamp", "elapsed_s", "entity_type", "entity_id"}
	for _, f := range fields {
		header = append(header, f.Name)
	}
	_ = cw.Write(header)
	cw.Flush()

	energyStart, energyErr := readEnergy(*dcgmiBin, *entities)

	var mu sync.Mutex
	data := map[string]*series{}
	var t0 time.Time
	onRow := func(ts time.Time, r row) {
		mu.Lock()
		defer mu.Unlock()
		el := ts.Sub(t0).Seconds()
		key := r.EntType + "/" + r.EntID
		sr := data[key]
		if sr == nil {
			sr = &series{EntType: r.EntType, EntID: r.EntID, Vals: make([][]float64, len(fields))}
			data[key] = sr
		}
		sr.T = append(sr.T, el)
		rec := []string{ts.UTC().Format(time.RFC3339Nano), strconv.FormatFloat(el, 'f', 3, 64), r.EntType, r.EntID}
		for i, v := range r.Vals {
			sr.Vals[i] = append(sr.Vals[i], v)
			if math.IsNaN(v) {
				rec = append(rec, "")
			} else {
				rec = append(rec, strconv.FormatFloat(v, 'g', -1, 64))
			}
		}
		_ = cw.Write(rec)
		cw.Flush()
	}

	t0 = time.Now()
	sum.Start = t0.UTC().Format(time.RFC3339)
	smp, err := startSampler(*dcgmiBin, fields, *interval, *entities, onRow)
	if err != nil {
		fatalf("%v", err)
	}

	child := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	exitCode := 0
	if err := child.Start(); err != nil {
		smp.stop()
		fatalf("start %s: %v", cmdArgs[0], err)
	}
	go func() {
		for s := range sigs {
			_ = child.Process.Signal(s)
		}
	}()
	if err := child.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = 1
		}
	}
	wall := time.Since(t0).Seconds()
	signal.Stop(sigs)
	close(sigs)

	// Let one more sample land so short runs still have a closing row.
	time.Sleep(minDur(*interval, 1500*time.Millisecond))
	smp.stop()

	sum.ExitCode = exitCode
	sum.WallSeconds = wall

	mu.Lock()
	sum.Entities = entitySummaries(fields, data)
	mu.Unlock()

	energyEnd, err2 := readEnergy(*dcgmiBin, *entities)
	switch {
	case energyErr == nil && err2 == nil:
		j := 0.0
		for id, end := range energyEnd {
			if start, ok := energyStart[id]; ok && end >= start {
				j += (end - start) / 1000
			}
		}
		sum.EnergyJoules, sum.EnergyMethod = j, "counter"
		sum.EnergyNote = "whole-GPU energy counter delta; includes any other work on these GPUs"
	default:
		j, ok := powerIntegral(fields, data)
		if ok {
			sum.EnergyJoules, sum.EnergyMethod = j, "power-integral"
			sum.EnergyNote = "energy counter unavailable; integrated sampled power"
		} else {
			sum.EnergyMethod = "none"
		}
	}

	sum.computePerUnit()
	sum.analyze()
	sumPath := *csvPath + ".summary.json"
	if err := writeSummary(sumPath, sum); err != nil {
		warnf("write summary: %v", err)
	}
	printHuman(os.Stderr, sum, sumPath)
	return exitCode
}

func powerIntegral(fields []field, data map[string]*series) (float64, bool) {
	pi := -1
	for i, f := range fields {
		if f.Name == "power_w" {
			pi = i
		}
	}
	if pi < 0 {
		return 0, false
	}
	total, ok := 0.0, false
	for _, sr := range data {
		if sr.EntType != "GPU" {
			continue
		}
		total += trapezoid(sr.T, sr.Vals[pi])
		ok = true
	}
	return total, ok
}

// inferLineRate maps EC2 GPU families to their PCIe generation (x16 links).
func inferLineRate(instance string) float64 {
	fam := strings.SplitN(instance, ".", 2)[0]
	switch fam {
	case "g4dn", "g5", "g5g", "g6", "g6e", "g6f", "gr6", "gr6f", "p4d", "p4de":
		return 31.5 // Gen4 x16
	}
	return 63 // Gen5 x16
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func printHuman(w *os.File, s *Summary, path string) {
	_, _ = fmt.Fprintf(w, "\n── tach %s ── %s profile, %.1fs, exit %d", version, s.Profile, s.WallSeconds, s.ExitCode)
	if s.InstanceType != "" {
		_, _ = fmt.Fprintf(w, ", %s", s.InstanceType)
	}
	_, _ = fmt.Fprintln(w)
	for _, e := range s.Entities {
		_, _ = fmt.Fprintf(w, "%s %s (%d samples)\n", e.Type, e.ID, e.Samples)
		for _, name := range orderedNames(e.Fields) {
			st := e.Fields[name]
			if st.N == 0 {
				_, _ = fmt.Fprintf(w, "  %-14s n/a (%d blank)\n", name, st.Missing)
				continue
			}
			_, _ = fmt.Fprintf(w, "  %-14s mean %-10.6g p50 %-10.6g p95 %-10.6g max %.6g\n", name, st.Mean, st.P50, st.P95, st.Max)
		}
	}
	if s.EnergyMethod != "none" {
		_, _ = fmt.Fprintf(w, "energy: %.1f J (%s)\n", s.EnergyJoules, s.EnergyMethod)
	}
	if s.PerUnit != nil {
		unit := s.Unit
		if unit == "" {
			unit = "unit"
		}
		_, _ = fmt.Fprintf(w, "per %s: %.4g s", unit, s.PerUnit.Seconds)
		if s.PerUnit.Joules > 0 {
			_, _ = fmt.Fprintf(w, ", %.4g J", s.PerUnit.Joules)
		}
		if s.PerUnit.Dollars > 0 {
			_, _ = fmt.Fprintf(w, ", $%.4g", s.PerUnit.Dollars)
		}
		_, _ = fmt.Fprintln(w)
	}
	_, _ = fmt.Fprintf(w, "verdict (heuristic): %s\n", s.Verdict)
	for _, f := range s.Findings {
		_, _ = fmt.Fprintf(w, "  - %s\n", f.Text)
	}
	_, _ = fmt.Fprintf(w, "summary: %s\n", path)
}

// orderedNames lists fields in a stable, readable order.
func orderedNames(m map[string]stat) []string {
	order := []string{"sm_active", "sm_occupancy", "tensor_active", "dram_active", "fp64_active", "fp32_active", "fp16_active", "pcie_tx_bps", "pcie_rx_bps", "power_w", "sm_clock_mhz", "fb_used_mib", "fb_total_mib", "gpu_util_pct", "gpu_temp_c"}
	var out []string
	for _, n := range order {
		if _, ok := m[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

func cmdAnnotate(argv []string) int {
	fl := flag.NewFlagSet("tach annotate", flag.ExitOnError)
	work := fl.Float64("work", -1, "units of work completed")
	unit := fl.String("unit", "", "work unit name")
	price := fl.Float64("price-per-hour", -1, "instance price in $/hour")
	label := fl.String("label", "", "run label")
	_ = fl.Parse(argv)
	if fl.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: tach annotate [--work N] [--unit U] [--price-per-hour P] [--label L] <summary.json>...")
		return 2
	}
	for _, p := range fl.Args() {
		s, err := readSummary(p)
		if err != nil {
			fatalf("%v", err)
		}
		if *work >= 0 {
			s.Work = *work
		}
		if *unit != "" {
			s.Unit = *unit
		}
		if *price >= 0 {
			s.PricePerHour = *price
		}
		if *label != "" {
			s.Label = *label
		}
		s.computePerUnit()
		if err := writeSummary(p, s); err != nil {
			fatalf("%v", err)
		}
		fmt.Fprintf(os.Stderr, "annotated %s\n", filepath.Base(p))
	}
	return 0
}

func warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "tach: "+format+"\n", a...)
}

func fatalf(format string, a ...any) {
	warnf(format, a...)
	os.Exit(1)
}
