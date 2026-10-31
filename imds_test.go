// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// The token is truncated to 4096 bytes; an instance type that reaches the
// 256-byte limit is rejected, not truncated.
func TestIMDSOversizeBodies(t *testing.T) {
	longToken := strings.Repeat("t", 5000)
	for _, n := range []int{255, 256, 300} {
		t.Run(strconv.Itoa(n)+"B", func(t *testing.T) {
			typ := strings.Repeat("x", n)
			fakeIMDS(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == imdsTokenPath {
					_, _ = w.Write([]byte(longToken))
					return
				}
				if got := r.Header.Get("X-aws-ec2-metadata-token"); got != longToken[:4096] {
					t.Errorf("token header length = %d, want 4096", len(got))
				}
				_, _ = w.Write([]byte(typ))
			})
			want := ""
			if n < 256 {
				want = typ
			}
			if got := imdsInstanceType(); got != want {
				t.Fatalf("%d-byte body: imdsInstanceType() length = %d, want %d", n, len(got), len(want))
			}
		})
	}
}

func TestIMDSInstanceTypeRejectsGarbage(t *testing.T) {
	cases := map[string]string{
		"g6e.xlarge":                "g6e.xlarge",
		"  p5.48xlarge\r\n":         "p5.48xlarge",
		"u-6tb1.metal":              "u-6tb1.metal",
		"":                          "",
		"   \n":                     "",
		"G6E.XLARGE":                "",
		"g6e xlarge":                "",
		"g6e.xlarge\nx-injected: 1": "",
		"<html>error</html>":        "",
		"g6e.xlarge;rm":             "",
		"g6e.xlarge\x00":            "",
		"g6e.éxlarge":               "",
		"{\"type\":\"g6e.xlarge\"}": "",
	}
	for body, want := range cases {
		fakeIMDS(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == imdsTokenPath {
				_, _ = w.Write([]byte("tok"))
				return
			}
			_, _ = w.Write([]byte(body))
		})
		if got := imdsInstanceType(); got != want {
			t.Errorf("body %q: imdsInstanceType() = %q, want %q", body, got, want)
		}
	}
}

// The 400 ms is a budget for both requests together: a fast token followed by
// a stalled GET must not get a fresh 400 ms of its own.
func TestIMDSTotalTimeout(t *testing.T) {
	done := make(chan struct{})
	fakeIMDS(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == imdsTokenPath {
			// Fast, but not free: spend most of the budget here.
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte("tok"))
			return
		}
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		case <-done:
		}
	})
	t.Cleanup(func() { close(done) })

	start := time.Now()
	got := imdsInstanceType()
	elapsed := time.Since(start)
	if got != "" {
		t.Fatalf("imdsInstanceType() = %q, want \"\"", got)
	}
	if elapsed >= 600*time.Millisecond {
		t.Fatalf("imdsInstanceType() took %v, want < 600ms", elapsed)
	}
}

// The lookup closes its keep-alive connection before returning rather than
// leaving it idle in a transport nobody will use again.
func TestIMDSClosesIdleConnection(t *testing.T) {
	var opened, closed atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == imdsTokenPath {
			_, _ = w.Write([]byte("tok"))
			return
		}
		_, _ = w.Write([]byte("g6e.xlarge"))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			opened.Add(1)
		case http.StateClosed:
			closed.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	setIMDSBase(t, srv.URL+"/latest")

	if got := imdsInstanceType(); got != "g6e.xlarge" {
		t.Fatalf("imdsInstanceType() = %q, want g6e.xlarge", got)
	}
	// The server sees the client's close asynchronously.
	deadline := time.Now().Add(2 * time.Second)
	for closed.Load() < opened.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if o, c := opened.Load(), closed.Load(); o == 0 || c != o {
		t.Fatalf("connections opened %d, closed %d: want all closed", o, c)
	}
}
