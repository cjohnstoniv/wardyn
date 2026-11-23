// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// `wardyn run forward` with the SSH gateway off: the check, the per-connection ticket and the WebSocket relay.

// forwardOrgFixture is a deployment with the SSH gateway off. check answers the pre-check; mint answers each
// ticket mint (n counts from 1); the forward route echoes every WebSocket frame back.
type forwardOrgFixture struct {
	url     string
	healthz atomic.Int32
	keyGET  atomic.Int32
	mints   atomic.Int32
	checks  atomic.Int32
	dials   atomic.Int32
	check   func(w http.ResponseWriter, r *http.Request)
	dial    func(w http.ResponseWriter, r *http.Request) bool // true: it answered, no WebSocket
	mint    func(n int, w http.ResponseWriter)
	mu      sync.Mutex
	ports   []string
	tickets []string
	echoed  atomic.Int32
}

func newForwardOrgFixture(t *testing.T) *forwardOrgFixture {
	t.Helper()
	f := &forwardOrgFixture{}
	f.check = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	f.mint = func(n int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ticket":"ticket-%d"}`, n)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		f.healthz.Add(1)
		_, _ = w.Write([]byte(gatewayOffHealthz))
	})
	mux.HandleFunc("/api/v1/me/ssh-keys", func(w http.ResponseWriter, _ *http.Request) { f.keyGET.Add(1) })
	mux.HandleFunc("/api/v1/runs/"+testRunID+"/attach/ticket", func(w http.ResponseWriter, _ *http.Request) {
		f.mint(int(f.mints.Add(1)), w)
	})
	mux.HandleFunc("/api/v1/runs/"+testRunID+"/attach/forward", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("check") == "1" {
			f.checks.Add(1)
			f.check(w, r)
			return
		}
		f.dials.Add(1)
		f.mu.Lock()
		f.ports = append(f.ports, q.Get("port"))
		f.tickets = append(f.tickets, q.Get("ticket"))
		f.mu.Unlock()
		if f.dial != nil && f.dial(w, r) {
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		nc := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
		n, _ := io.Copy(nc, nc)
		f.echoed.Add(int32(n)) //nolint:gosec // a test's byte count
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// freePort is a loopback TCP port nothing listens on now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// startForward runs `run forward` against f until the returned cancel, and returns its stderr and its result.
func startForward(t *testing.T, f *forwardOrgFixture, args ...string) (stderr *syncBuffer, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cmd := runForwardCmd(func() *sdk.Client { return &sdk.Client{BaseURL: f.url} })
	stderr = &syncBuffer{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(stderr)
	ctx, cancel := context.WithCancel(context.Background())
	cmd.SetContext(ctx)
	cmd.SetArgs(append([]string{testRunID}, args...))
	cmd.SilenceUsage = true
	ch := make(chan error, 1)
	go func() { ch <- cmd.Execute() }()
	t.Cleanup(cancel)
	return stderr, cancel, ch
}

func waitForText(t *testing.T, b *syncBuffer, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if strings.Contains(b.String(), want) {
			return
		}
	}
	t.Fatalf("stderr %q never held %q", b.String(), want)
}

// echoOnce dials the local port, sends msg and reads it back.
func echoOnce(t *testing.T, port int, msg string) {
	t.Helper()
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != msg {
		t.Fatalf("echo = %q, %v, want %q", got, err, msg)
	}
}

func TestRunForward_GatewayOffSwitchOffIsOneLine(t *testing.T) {
	home := homeWithKey(t, false)
	log := fakeSSH(t, "0")
	f := newForwardOrgFixture(t)
	f.check = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"Port forwarding over the organisation is off on this deployment: its operator has not turned on WARDYN_ATTACH_FORWARD.","reason":"run_forward_off"}`))
	}
	port := freePort(t)
	_, err := runForwardSync(t, f, strconv.Itoa(port)+":4000")
	if err == nil || !strings.HasPrefix(err.Error(), "forward:") || !strings.Contains(err.Error(), "WARDYN_ATTACH_FORWARD") {
		t.Fatalf("err = %v, want a forward: line naming WARDYN_ATTACH_FORWARD", err)
	}
	neverRan(t, log)
	if ln, lerr := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port)); lerr != nil {
		t.Errorf("the local port was left bound: %v", lerr)
	} else {
		ln.Close()
	}
	if f.dials.Load() != 0 || f.mints.Load() != 1 {
		t.Errorf("dials = %d, mints = %d, want 0 and 1", f.dials.Load(), f.mints.Load())
	}
	if f.keyGET.Load() != 0 {
		t.Error("read the account's keys although the gateway is off")
	}
	if _, serr := os.Stat(filepath.Join(home, ".wardyn")); serr == nil {
		t.Error("wrote under ~/.wardyn")
	}
}

// runForwardSync runs the command to its end, for the cases that end on their own.
func runForwardSync(t *testing.T, f *forwardOrgFixture, args ...string) (string, error) {
	t.Helper()
	cmd := runForwardCmd(func() *sdk.Client { return &sdk.Client{BaseURL: f.url} })
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetContext(context.Background())
	cmd.SetArgs(append([]string{testRunID}, args...))
	cmd.SilenceUsage = true
	err := cmd.Execute()
	return errOut.String(), err
}

func TestRunForward_BadRunIDBeforeAnyRequest(t *testing.T) {
	f := newForwardOrgFixture(t)
	cmd := runForwardCmd(func() *sdk.Client { return &sdk.Client{BaseURL: f.url} })
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"not-a-run", "3000"})
	cmd.SilenceUsage = true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "invalid run id") {
		t.Fatalf("err = %v, want parseID's refusal", err)
	}
	if f.healthz.Load() != 0 {
		t.Error("read /healthz for a run id that is not one")
	}
}

func TestRunForward_GatewayOffOlderServer(t *testing.T) {
	const older = "forward: the operator has not turned SSH on for this deployment, so there is no gateway to reach (WARDYN_SSH_LISTEN), and this server offers no forwarding over the organisation"
	for _, tc := range []struct {
		name   string
		status int
		body   string
		older  bool
	}{
		{"plain_text_404", http.StatusNotFound, "404 page not found\n", true},
		{"json_404_without_a_reason", http.StatusNotFound, `{"error":"not found"}`, true},
		{"empty_405", http.StatusMethodNotAllowed, "", true},
		{"404_with_a_reason", http.StatusNotFound, `{"error":"The run was not found.","reason":"run_not_found"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newForwardOrgFixture(t)
			f.check = func(w http.ResponseWriter, _ *http.Request) {
				if strings.HasPrefix(tc.body, "{") {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}
			_, err := runForwardSync(t, f, strconv.Itoa(freePort(t))+":4000")
			if err == nil {
				t.Fatal("want a refusal")
			}
			if tc.older {
				if err.Error() != older {
					t.Errorf("err = %q, want the older-server sentence", err)
				}
				return
			}
			var ae *sdk.APIError
			if err.Error() == older || !strings.HasPrefix(err.Error(), "forward:") || !errors.As(err, &ae) || ae.Reason != "run_not_found" {
				t.Errorf("err = %v, want forward: with the APIError run_not_found", err)
			}
		})
	}
}

func TestRunForward_GatewayOffForwardsThroughTheOrganisation(t *testing.T) {
	f := newForwardOrgFixture(t)
	port := freePort(t)
	stderr, cancel, done := startForward(t, f, strconv.Itoa(port)+":4000")
	waitForText(t, stderr, "through the organisation (Ctrl-C to stop)")
	echoOnce(t, port, "hello")
	echoOnce(t, port, "again")
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Ctrl-C ended with %v, want nil", err)
	}
	if n := f.mints.Load(); n != 3 {
		t.Errorf("mints = %d, want 3 (the check and two connections)", n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ports) != 2 {
		t.Fatalf("dials = %d, want 2", len(f.ports))
	}
	for i, p := range f.ports {
		if p != "4000" || f.tickets[i] == "" {
			t.Errorf("dial %d: port %q ticket %q, want port 4000 and a ticket", i, p, f.tickets[i])
		}
	}
	if f.tickets[0] == f.tickets[1] {
		t.Error("two connections shared a ticket")
	}
}

func TestRunForward_MintRefusedClosesThatConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		mint func(w http.ResponseWriter)
		want string
	}{
		{"cap_reached", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"This run already has 8 terminal sessions open.","reason":"attach_session_cap_reached"}`))
		}, "forward: 4000: This run already has 8 terminal sessions open."},
		{"empty_ticket", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ticket":""}`))
		}, "forward: 4000: could not get an attach ticket from the organisation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newForwardOrgFixture(t)
			good := f.mint
			// the first mint (the check's) and the third succeed; the second, the first connection's, is refused
			f.mint = func(n int, w http.ResponseWriter) {
				if n == 2 {
					tc.mint(w)
					return
				}
				good(n, w)
			}
			port := freePort(t)
			stderr, cancel, done := startForward(t, f, strconv.Itoa(port)+":4000")
			waitForText(t, stderr, "(Ctrl-C to stop)")
			conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := conn.Read(make([]byte, 1)); err == nil {
				t.Error("the refused connection stayed open")
			}
			conn.Close()
			waitForText(t, stderr, tc.want)
			if f.dials.Load() != 0 {
				t.Errorf("dials = %d after a refused mint, want 0", f.dials.Load())
			}
			echoOnce(t, port, "later")
			cancel()
			<-done
		})
	}
}

func TestRunForward_HalfCloseClosesBoth(t *testing.T) {
	f := newForwardOrgFixture(t)
	port := freePort(t)
	stderr, cancel, done := startForward(t, f, strconv.Itoa(port)+":4000")
	waitForText(t, stderr, "(Ctrl-C to stop)")
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("bye")); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	// The CLI closes the WebSocket and the local connection: the read ends, with whatever echo arrived first.
	if _, err := io.ReadAll(conn); err != nil && !strings.Contains(err.Error(), "reset") {
		t.Errorf("read after the half-close: %v, want the connection closed", err)
	}
	cancel()
	<-done
}

func TestRunForward_LocalPortInUse(t *testing.T) {
	f := newForwardOrgFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	_, err = runForwardSync(t, f, strconv.Itoa(port)+":4000")
	want := "forward: 127.0.0.1:" + strconv.Itoa(port) + " is not free here"
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("err = %v, want it to start %q", err, want)
	}
	if f.dials.Load() != 0 {
		t.Errorf("dials = %d, want 0", f.dials.Load())
	}
}

// dropConn ends the request's connection with no answer, as a transport failure does.
func dropConn(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// TestRunForward_TransportErrorNeverPrintsTheTicket: an error that quotes the request's URL carries the ticket
// query; the ticket was never spent, so it stays off stderr.
func TestRunForward_TransportErrorNeverPrintsTheTicket(t *testing.T) {
	t.Run("pre_check", func(t *testing.T) {
		f := newForwardOrgFixture(t)
		f.check = func(w http.ResponseWriter, _ *http.Request) { dropConn(w) }
		_, err := runForwardSync(t, f, strconv.Itoa(freePort(t))+":4000")
		if err == nil || !strings.HasPrefix(err.Error(), "forward:") {
			t.Fatalf("err = %v, want a forward: refusal", err)
		}
		if strings.Contains(err.Error(), "ticket-") || !strings.Contains(err.Error(), "ticket=REDACTED") {
			t.Errorf("err = %q, want the ticket value redacted", err)
		}
	})
	t.Run("dial", func(t *testing.T) {
		f := newForwardOrgFixture(t)
		f.dial = func(w http.ResponseWriter, _ *http.Request) bool { dropConn(w); return true }
		port := freePort(t)
		stderr, cancel, done := startForward(t, f, strconv.Itoa(port)+":4000")
		waitForText(t, stderr, "(Ctrl-C to stop)")
		conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = conn.Read(make([]byte, 1))
		conn.Close()
		waitForText(t, stderr, "forward: 4000:")
		if out := stderr.String(); strings.Contains(out, "ticket-") {
			t.Errorf("stderr %q holds a ticket value", out)
		}
		cancel()
		<-done
	})
}
