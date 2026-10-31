---
name: Hardware validation
about: A measurement on real GPUs that confirms or corrects tach
title: ""
labels: "type: validation"
assignees: ""
---

## Question

<!-- What this run establishes, e.g. "fields 1009/1010 are bytes/s including headers". -->

## Setup

- Instance type / GPU:
- DCGM version (`dcgmi --version`) / driver:
- tach version and flags:
- Workload and its known rate (e.g. pinned cudaMemcpy at N GB/s, GROMACS ns/day):

## Expected

## Measured

<!-- Repeat the run; report the range, not one draw. Raw output goes under
testdata/real/<instance>-<what>.txt. No AWS account numbers. -->

## Outcome

- [ ] Matches; no change
- [ ] Code or README changed (link the commit/PR, with a test against the capture)
