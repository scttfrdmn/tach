// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"strings"
	"time"
)

// imdsInstanceType asks EC2 instance metadata (IMDSv2) for the instance type.
// It returns "" quickly off EC2 or when metadata is blocked.
func imdsInstanceType() string {
	const base = "http://169.254.169.254/latest"
	c := &http.Client{Timeout: 400 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	req, _ := http.NewRequest(http.MethodPut, base+"/api/token", nil)
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
	req, _ = http.NewRequest(http.MethodGet, base+"/meta-data/instance-type", nil)
	req.Header.Set("X-aws-ec2-metadata-token", string(tok))
	resp, err = c.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return strings.TrimSpace(string(b))
}
