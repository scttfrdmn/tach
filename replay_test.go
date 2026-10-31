// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var dmonSection = regexp.MustCompile(`^### dcgmi dmon (?:-i \S+ )?-e ([0-9,]+) `)

// TestParseRealCaptures replays raw `dcgmi dmon` output captured on real
// GPUs (testdata/real/*-dmon*.txt from scripts/hw/capture.sh, and the MIG
// *-mig-*-dcgmi.txt captures). Every
// data row of every section must parse with the field count that section
// requested, and header lines must not.
func TestParseRealCaptures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "real", "*-dmon*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// MIG captures: dmon -i <instances>, with GPU-I rows.
	mig, _ := filepath.Glob(filepath.Join("testdata", "real", "*-mig-*-dcgmi.txt"))
	files = append(files, mig...)
	if len(files) == 0 {
		t.Fatal("no captures under testdata/real: the replay would pass without testing anything")
	}
	total := 0
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		n, rows, blanks := 0, 0, 0
		for sc.Scan() {
			line := sc.Text()
			if m := dmonSection.FindStringSubmatch(line); m != nil {
				n = len(strings.Split(m[1], ","))
				continue
			}
			if n == 0 {
				continue
			}
			r, ok := parseDmonLine(line, n)
			switch {
			case strings.HasPrefix(line, "#Entity"), strings.HasPrefix(line, "ID"):
				if ok {
					t.Errorf("%s: header parsed as a row: %q", path, line)
				}
			case strings.HasPrefix(line, "GPU"):
				if !ok {
					t.Errorf("%s: row did not parse with %d fields: %q", path, n, line)
					continue
				}
				rows++
				for _, v := range r.Vals {
					if math.IsNaN(v) {
						blanks++
					}
				}
			}
		}
		_ = f.Close()
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d rows, %d blank values", filepath.Base(path), rows, blanks)
		total += rows
	}
	if total == 0 {
		t.Fatal("captures contain no dmon data rows")
	}
}
