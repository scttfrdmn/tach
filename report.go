// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// cmdReport compares runs: one row per summary, sorted by cost per unit when
// every run has one, else time per unit, else wall time. This is the table a
// right-sizing clinic hands back.
func cmdReport(argv []string) int {
	fl := flag.NewFlagSet("tach report", flag.ExitOnError)
	format := fl.String("format", "md", "output format: md | csv")
	_ = fl.Parse(argv)
	if fl.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: tach report [--format md|csv] <summary.json>...")
		return 2
	}
	var runs []*Summary
	for _, p := range fl.Args() {
		s, err := readSummary(p)
		if err != nil {
			fatalf("%v", err)
		}
		if s.Findings == nil {
			s.analyze()
		}
		runs = append(runs, s)
	}
	sortRuns(runs)
	rows := reportRows(runs)
	switch *format {
	case "csv":
		w := csv.NewWriter(os.Stdout)
		_ = w.WriteAll(rows)
	case "md":
		writeMarkdown(os.Stdout, rows)
		writeFindings(os.Stdout, runs)
	default:
		fatalf("unknown format %q", *format)
	}
	return 0
}

func sortRuns(runs []*Summary) {
	all := func(f func(*Summary) bool) bool {
		for _, r := range runs {
			if !f(r) {
				return false
			}
		}
		return true
	}
	switch {
	case all(func(s *Summary) bool { return s.PerUnit != nil && s.PerUnit.Dollars > 0 }):
		sort.SliceStable(runs, func(i, j int) bool { return runs[i].PerUnit.Dollars < runs[j].PerUnit.Dollars })
	case all(func(s *Summary) bool { return s.PerUnit != nil }):
		sort.SliceStable(runs, func(i, j int) bool { return runs[i].PerUnit.Seconds < runs[j].PerUnit.Seconds })
	default:
		sort.SliceStable(runs, func(i, j int) bool { return runs[i].WallSeconds < runs[j].WallSeconds })
	}
}

func runName(s *Summary) string {
	switch {
	case s.Label != "":
		return s.Label
	case s.InstanceType != "":
		return s.InstanceType
	}
	return s.CSV
}

func reportRows(runs []*Summary) [][]string {
	unit := ""
	for _, r := range runs {
		if r.Unit != "" {
			unit = r.Unit
			break
		}
	}
	per := "unit"
	if unit != "" {
		per = unit
	}
	hdr := []string{"run", "instance", "wall s", "s/" + per, "$/" + per, "J/" + per,
		"SM active", "tensor", "FP64", "DRAM", "peak mem GiB", "verdict"}
	rows := [][]string{hdr}
	for _, s := range runs {
		r := []string{runName(s), s.InstanceType, fmt.Sprintf("%.1f", s.WallSeconds)}
		if s.PerUnit != nil {
			r = append(r, num(s.PerUnit.Seconds), money(s.PerUnit.Dollars), num(s.PerUnit.Joules))
		} else {
			r = append(r, "", "", "")
		}
		for _, n := range []string{"sm_active", "tensor_active", "fp64_active", "dram_active"} {
			if v, ok := s.gpuMean(n); ok {
				r = append(r, fmt.Sprintf("%.2f", v))
			} else {
				r = append(r, "")
			}
		}
		if used, total, ok := s.peakMemory(); ok {
			r = append(r, fmt.Sprintf("%.1f / %.1f", used/1024, total/1024))
		} else {
			r = append(r, "")
		}
		r = append(r, s.Verdict)
		rows = append(rows, r)
	}
	return rows
}

func num(v float64) string {
	if v == 0 {
		return ""
	}
	return fmt.Sprintf("%.4g", v)
}

func money(v float64) string {
	if v == 0 {
		return ""
	}
	return fmt.Sprintf("%.4g", v)
}

func writeMarkdown(w io.Writer, rows [][]string) {
	esc := func(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
	for i, r := range rows {
		cells := make([]string, len(r))
		for j, c := range r {
			cells[j] = esc(c)
		}
		fmt.Fprintf(w, "| %s |\n", strings.Join(cells, " | "))
		if i == 0 {
			seps := make([]string, len(r))
			for j := range seps {
				seps[j] = "---"
			}
			fmt.Fprintf(w, "| %s |\n", strings.Join(seps, " | "))
		}
	}
}

func writeFindings(w io.Writer, runs []*Summary) {
	fmt.Fprintln(w, "\nFindings (heuristic; the CSVs are the ground truth):")
	for _, s := range runs {
		fmt.Fprintf(w, "\n%s\n", runName(s))
		if len(s.Findings) == 0 {
			fmt.Fprintln(w, "- none")
		}
		for _, f := range s.Findings {
			fmt.Fprintf(w, "- %s\n", f.Text)
		}
	}
}
