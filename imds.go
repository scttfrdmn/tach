// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// imdsBase is the IMDS endpoint root; overridable by tests.
var imdsBase = "http://169.254.169.254/latest"

// imdsTimeout is the total budget for the token and instance-type requests.
const imdsTimeout = 400 * time.Millisecond

// imdsTypeLimit bounds the instance-type body. A body that reaches it is not
// an instance type, so it is rejected rather than truncated.
const imdsTypeLimit = 256

// imdsInstanceType asks EC2 instance metadata (IMDSv2) for the instance type.
// It returns "" quickly off EC2 or when metadata is blocked, and "" for a
// response that does not look like an instance type.
func imdsInstanceType() string {
	ctx, cancel := context.WithTimeout(context.Background(), imdsTimeout)
	defer cancel()
	tr := &http.Transport{Proxy: nil}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}

	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, imdsBase+"/api/token", nil)
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	resp, err := c.Do(req)
	if err != nil {
		return ""
	}
	tok, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, imdsBase+"/meta-data/instance-type", nil)
	req.Header.Set("X-aws-ec2-metadata-token", string(tok))
	resp, err = c.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, imdsTypeLimit))
	if err != nil || len(b) >= imdsTypeLimit {
		return ""
	}
	t := strings.TrimSpace(string(b))
	if !validInstanceType(t) {
		return ""
	}
	return t
}

// validInstanceType reports whether s is non-empty and only [a-z0-9.-], the
// alphabet of EC2 instance type names (g6e.xlarge, p5.48xlarge, u-6tb1.metal).
func validInstanceType(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '-' {
			return false
		}
	}
	return true
}
