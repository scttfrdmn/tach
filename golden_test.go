// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/golden and testdata/contract from the current code")

// normalize clears what varies from run to run (times, host, paths, sample
// counts, anything derived from wall time) so a summary from the mock can be
// compared byte for byte.
func normalize(s *Summary) {
	s.TachVersion, s.Start, s.Hostname, s.CSV = "", "", "", ""
	s.WallSeconds = 0
	for i := range s.Entities {
		s.Entities[i].Samples = 0
		for k, st := range s.Entities[i].Fields {
			// A mean of a constant still depends on the sample count in its
			// last bits (0.2 summed 5 times / 5 = 0.20000000000000004).
			st.N, st.Mean = 0, roundSig(st.Mean)
			s.Entities[i].Fields[k] = st
		}
		for k, st := range s.Entities[i].BusyFields {
			st.N, st.Mean = 0, roundSig(st.Mean)
			s.Entities[i].BusyFields[k] = st
		}
	}
	if s.PerUnit != nil {
		s.PerUnit.Seconds, s.PerUnit.Dollars = 0, 0
	}
	if s.EnergyMethod == "power-integral" {
		s.EnergyJoules = 0
	}
}

func roundSig(x float64) float64 {
	if x == 0 || math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	p := math.Pow(10, 9-math.Ceil(math.Log10(math.Abs(x))))
	return math.Round(x*p) / p
}

// TestGoldenSummaries runs each mock scenario and compares its normalised
// summary with testdata/golden/<name>.summary.json. Regenerate with
// `go test -run Golden -update` and review the diff: a changed golden is a
// changed output, and removing or renaming a field breaks the contract.
func TestGoldenSummaries(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		args []string
	}{
		{"fp64", nil, []string{"--work", "10", "--unit", "ns", "--price-per-hour", "2"}},
		{"starved-pcie", []string{"MOCK_SCENARIO=starved"}, []string{"--pcie", "--instance", "g7e.4xlarge"}},
		{"ai-fp32", []string{"MOCK_SCENARIO=ai_fp32"}, []string{"--profile", "inference"}},
		{"hpc", nil, []string{"--profile", "hpc"}},
		{"no-profiling", []string{"MOCK_NOPROF=1"}, nil},
		{"no-energy-counter", []string{"MOCK_NOENERGY=1"}, nil},
		{"mig", []string{"MOCK_MIG=1"}, []string{"--entities", "i:0,i:1"}},
		{"fp64-unsupported", []string{"MOCK_UNSUPPORTED=1006"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			args := append(append([]string{"--label", c.name}, c.args...), "--", "sleep", "1.1")
			s := runTach(t, c.env, args...).sum
			normalize(s)
			got, err := json.MarshalIndent(s, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", "golden", c.name+".summary.json")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run `go test -run Golden -update` to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from the golden file; if the change is intended, run `go test -run Golden -update` and review the diff\n--- got\n%s", path, got)
			}
		})
	}
}

// contractPaths lists every JSON path a summary can carry, e.g.
// "entities[].fields{}.p95", from the json tags of Summary and its types.
func contractPaths() []string {
	var out []string
	var walk func(prefix string, t reflect.Type)
	walk = func(prefix string, t reflect.Type) {
		switch t.Kind() {
		case reflect.Pointer:
			walk(prefix, t.Elem())
		case reflect.Slice:
			walk(prefix+"[]", t.Elem())
		case reflect.Map:
			walk(prefix+"{}", t.Elem())
		case reflect.Struct:
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
				if name == "" || name == "-" || !f.IsExported() {
					continue
				}
				p := name
				if prefix != "" {
					p = prefix + "." + name
				}
				out = append(out, p)
				walk(p, f.Type)
			}
		}
	}
	walk("", reflect.TypeOf(Summary{}))
	sort.Strings(out)
	return out
}

// TestSummaryContract holds the summary JSON to its published shape. Other
// tools read these files, so a path that disappears (renamed or removed) is a
// contract break. A new path must be added to the list deliberately.
func TestSummaryContract(t *testing.T) {
	path := filepath.Join("testdata", "contract", "summary-fields.txt")
	got := contractPaths()
	if *update {
		hdr := "# Every JSON path in <csv>.summary.json. Other tools read these files:\n" +
			"# paths may be added, never renamed or removed. See CONTRIBUTING.md.\n"
		if err := os.WriteFile(path, []byte(hdr+strings.Join(got, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			want[l] = true
		}
	}
	have := map[string]bool{}
	for _, p := range got {
		have[p] = true
		if !want[p] {
			t.Errorf("new summary field %q: add it to %s (fields may be added, never renamed)", p, path)
		}
	}
	for p := range want {
		if !have[p] {
			t.Errorf("summary field %q was renamed or removed: that breaks the contract with tools that read summaries", p)
		}
	}
}
