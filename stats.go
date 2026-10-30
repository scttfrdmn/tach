// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"math"
	"sort"
)

// stat summarizes one field for one entity. Missing samples (N/A, metric
// multiplexing blanks) are excluded and counted separately.
type stat struct {
	N       int     `json:"n"`
	Missing int     `json:"missing"`
	Mean    float64 `json:"mean"`
	P50     float64 `json:"p50"`
	P95     float64 `json:"p95"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
}

func summarize(vals []float64) (stat, bool) {
	var xs []float64
	missing := 0
	for _, v := range vals {
		if math.IsNaN(v) {
			missing++
			continue
		}
		xs = append(xs, v)
	}
	if len(xs) == 0 {
		return stat{Missing: missing}, false
	}
	sort.Float64s(xs)
	sum := 0.0
	for _, v := range xs {
		sum += v
	}
	return stat{
		N:       len(xs),
		Missing: missing,
		Mean:    sum / float64(len(xs)),
		P50:     percentile(xs, 0.50),
		P95:     percentile(xs, 0.95),
		Min:     xs[0],
		Max:     xs[len(xs)-1],
	}, true
}

// percentile uses linear interpolation between closest ranks on sorted xs.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 1 {
		return xs[0]
	}
	pos := p * float64(len(xs)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return xs[lo]
	}
	return xs[lo] + (xs[hi]-xs[lo])*(pos-float64(lo))
}

// trapezoid integrates y over t (seconds), skipping gaps with missing values.
func trapezoid(t, y []float64) float64 {
	total := 0.0
	for i := 1; i < len(t) && i < len(y); i++ {
		if math.IsNaN(y[i]) || math.IsNaN(y[i-1]) {
			continue
		}
		total += (t[i] - t[i-1]) * (y[i] + y[i-1]) / 2
	}
	return total
}
