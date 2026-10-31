// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	imdsTokenPath = "/latest/api/token"
	imdsTypePath  = "/latest/meta-data/instance-type"
)

// fakeIMDS starts a test server running h and points imdsBase at it for the
// duration of the test. Tests using it must not call t.Parallel.
func fakeIMDS(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	setIMDSBase(t, srv.URL+"/latest")
	return srv
}

func setIMDSBase(t *testing.T, base string) {
	t.Helper()
	old := imdsBase
	imdsBase = base
	t.Cleanup(func() { imdsBase = old })
}

func TestIMDSInstanceTypeHappyPath(t *testing.T) {
	const token = "AQAEAFake-Token=="
	var gets atomic.Int32
	fakeIMDS(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case imdsTokenPath:
			if r.Method != http.MethodPut {
				t.Errorf("token request method = %s, want PUT", r.Method)
			}
			if got := r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds"); got != "60" {
				t.Errorf("token TTL header = %q, want \"60\"", got)
			}
			_, _ = w.Write([]byte(token))
		case imdsTypePath:
			gets.Add(1)
			if r.Method != http.MethodGet {
				t.Errorf("instance-type request method = %s, want GET", r.Method)
			}
			if got := r.Header.Get("X-aws-ec2-metadata-token"); got != token {
				t.Errorf("token header = %q, want %q", got, token)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("g6e.xlarge\n"))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if got := imdsInstanceType(); got != "g6e.xlarge" {
		t.Fatalf("imdsInstanceType() = %q, want %q", got, "g6e.xlarge")
	}
	if n := gets.Load(); n != 1 {
		t.Fatalf("instance-type requests = %d, want 1", n)
	}
}

func TestIMDSTokenRefused(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var gets atomic.Int32
			fakeIMDS(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == imdsTokenPath {
					w.WriteHeader(code)
					_, _ = w.Write([]byte("nope"))
					return
				}
				gets.Add(1)
				_, _ = w.Write([]byte("g6e.xlarge"))
			})
			if got := imdsInstanceType(); got != "" {
				t.Fatalf("imdsInstanceType() = %q, want \"\"", got)
			}
			if n := gets.Load(); n != 0 {
				t.Fatalf("instance-type requested %d times after token %d, want 0", n, code)
			}
		})
	}
}

func TestIMDSInstanceTypeUnauthorized(t *testing.T) {
	fakeIMDS(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == imdsTokenPath {
			_, _ = w.Write([]byte("tok"))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("g6e.xlarge"))
	})
	if got := imdsInstanceType(); got != "" {
		t.Fatalf("imdsInstanceType() = %q, want \"\"", got)
	}
}

func TestIMDSSlowServer(t *testing.T) {
	for _, slow := range []string{imdsTokenPath, imdsTypePath} {
		t.Run(strings.TrimPrefix(slow, "/latest/"), func(t *testing.T) {
			done := make(chan struct{})
			fakeIMDS(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == slow {
					select {
					case <-time.After(2 * time.Second):
					case <-r.Context().Done():
					case <-done:
					}
					return
				}
				_, _ = w.Write([]byte("tok"))
			})
			// Registered after srv.Close, so it runs first and unblocks
			// any handler still sleeping.
			t.Cleanup(func() { close(done) })

			start := time.Now()
			got := imdsInstanceType()
			elapsed := time.Since(start)
			if got != "" {
				t.Fatalf("imdsInstanceType() = %q, want \"\"", got)
			}
			if elapsed >= 900*time.Millisecond {
				t.Fatalf("imdsInstanceType() took %v, want < 900ms", elapsed)
			}
		})
	}
}

func TestIMDSConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	setIMDSBase(t, url+"/latest")
	if got := imdsInstanceType(); got != "" {
		t.Fatalf("imdsInstanceType() = %q, want \"\"", got)
	}
}

func TestIMDSOversizeBodiesTruncated(t *testing.T) {
	longToken := strings.Repeat("t", 5000)
	longType := strings.Repeat("x", 300)
	fakeIMDS(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == imdsTokenPath {
			_, _ = w.Write([]byte(longToken))
			return
		}
		if got := r.Header.Get("X-aws-ec2-metadata-token"); got != longToken[:4096] {
			t.Errorf("token header length = %d, want 4096", len(got))
		}
		_, _ = w.Write([]byte(longType))
	})
	if got, want := imdsInstanceType(), longType[:256]; got != want {
		t.Fatalf("imdsInstanceType() length = %d, want %d", len(got), len(want))
	}
}
