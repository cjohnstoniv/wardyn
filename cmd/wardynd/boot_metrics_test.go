// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestValidateMetricsListenConfig pins the collision refusal: the
// unauthenticated metrics listener may not share an address with any of the
// four authenticated listeners, in any spelling sameListenAddress treats as
// one origin.
func TestValidateMetricsListenConfig(t *testing.T) {
	const listen, internal, ssh, ui = ":8080", ":8443", ":2222", ":8081"
	for _, tc := range []struct {
		name, metrics, wantErr string
	}{
		{name: "off", metrics: ""},
		{name: "own port", metrics: ":9464"},
		{name: "same as -listen", metrics: ":8080", wantErr: "same address as -listen"},
		{name: "same as -listen, loopback spelling", metrics: "127.0.0.1:8080", wantErr: "same address as -listen"},
		{name: "same as -internal-listen", metrics: ":8443", wantErr: "same address as -internal-listen"},
		{name: "same as -ssh-listen", metrics: ":2222", wantErr: "same address as -ssh-listen"},
		{name: "same as -ui-sandbox-listen", metrics: "0.0.0.0:8081", wantErr: "same address as -ui-sandbox-listen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMetricsListenConfig(tc.metrics, listen, internal, ssh, ui)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.wantErr)
			}
		})
	}
	// An off gateway (empty address) is never a collision.
	if err := validateMetricsListenConfig(":2222", listen, internal, "", ""); err != nil {
		t.Fatalf("metrics on an unused ssh port with ssh off: %v", err)
	}
}

func TestStartMetricsListenerOff(t *testing.T) {
	if err := startMetricsListener(context.Background(), "", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("handler reached with the listener off")
	})); err != nil {
		t.Fatalf("unset listener: %v", err)
	}
}

func TestStartMetricsListenerRefusesBind(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for _, tc := range []struct{ name, addr string }{
		{"occupied", ln.Addr().String()},
		{"missing port", "127.0.0.1"},
		{"invalid port", "127.0.0.1:65536"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := startMetricsListener(t.Context(), tc.addr, http.NotFoundHandler())
			if err == nil || !strings.Contains(err.Error(), "metrics listener") || !strings.Contains(err.Error(), tc.addr) {
				t.Fatalf("bind %q = %v, want synchronous metrics listener error naming the address", tc.addr, err)
			}
			var op *net.OpError
			if !errors.As(err, &op) || op.Op != "listen" {
				t.Fatalf("bind error lost its net.Listen cause: %v", err)
			}
		})
	}
}

func TestStartMetricsListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := startMetricsListener(ctx, addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("scrape carried a credential")
		}
		_, _ = io.WriteString(w, "wardyn_store_up 1\n")
	})); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("metrics listener did not answer on %s: %v", addr, err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if body := string(b); body != "wardyn_store_up 1\n" {
		t.Fatalf("body = %q", body)
	}

	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			break
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatal("metrics listener still accepting after rootCtx was cancelled")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
