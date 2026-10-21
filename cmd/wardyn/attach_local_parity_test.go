// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/runnerio"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// The CLI keeps its own copy of the local socket's codec (attach_local.go) so it does not build the
// runner. These tests hold that copy to the runner's (internal/runner/runnerio/local_attach.go): the
// frame bytes, the limits, the frame layout both ways, and the control payloads each side reads.

func TestLocalFrameParity_TypesAndLimits(t *testing.T) {
	for _, c := range []struct {
		name     string
		cli, run byte
	}{
		{"hello", localHello, runnerio.LocalHello},
		{"hello ok", localHelloOK, runnerio.LocalHelloOK},
		{"refuse", localRefuse, runnerio.LocalRefuse},
		{"bound", localBound, runnerio.LocalBound},
		{"data", localData, runnerio.LocalData},
		{"resize", localResize, runnerio.LocalResize},
		{"mode", localMode, runnerio.LocalMode},
		{"end", localEnd, runnerio.LocalEnd},
	} {
		if c.cli != c.run {
			t.Errorf("%s frame: CLI %q, runner %q", c.name, c.cli, c.run)
		}
	}
	if localProtocol != runnerio.LocalProtocol || maxLocalFrame != runnerio.MaxLocalFrame {
		t.Errorf("CLI protocol %d max %d, runner protocol %d max %d", localProtocol, maxLocalFrame, runnerio.LocalProtocol, runnerio.MaxLocalFrame)
	}
	for reason := range localCleanEnds {
		if reason != runnerio.LocalReasonExited && reason != runnerio.LocalReasonRunEnded {
			t.Errorf("the CLI treats end reason %q as clean; the runner sends no such reason", reason)
		}
	}
}

func TestLocalFrameParity_EachSideReadsTheOther(t *testing.T) {
	for _, n := range []int{0, 1, maxLocalFrame} {
		payload := bytes.Repeat([]byte{'z'}, n)
		var a, b bytes.Buffer
		if err := writeLocalFrame(&a, localData, payload); err != nil {
			t.Fatal(err)
		}
		if err := runnerio.WriteLocalFrame(&b, runnerio.LocalData, payload); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a.Bytes(), b.Bytes()) {
			t.Fatalf("%d-byte frame: CLI and runner write different bytes", n)
		}
		typ, got, err := runnerio.ReadLocalFrame(bytes.NewReader(a.Bytes()))
		if err != nil || typ != runnerio.LocalData || !bytes.Equal(got, payload) {
			t.Errorf("%d-byte CLI frame read by the runner: %q, %d bytes, %v", n, typ, len(got), err)
		}
		typ, got, err = readLocalFrame(bytes.NewReader(b.Bytes()))
		if err != nil || typ != localData || !bytes.Equal(got, payload) {
			t.Errorf("%d-byte runner frame read by the CLI: %q, %d bytes, %v", n, typ, len(got), err)
		}
	}
	over := make([]byte, 5)
	over[0] = localData
	binary.BigEndian.PutUint32(over[1:], maxLocalFrame+1)
	if _, _, err := readLocalFrame(bytes.NewReader(over)); err == nil {
		t.Error("the CLI read a frame over the limit")
	}
	if _, _, err := runnerio.ReadLocalFrame(bytes.NewReader(over)); err == nil {
		t.Error("the runner read a frame over the limit")
	}
	if writeLocalFrame(&bytes.Buffer{}, localData, make([]byte, maxLocalFrame+1)) == nil ||
		runnerio.WriteLocalFrame(&bytes.Buffer{}, runnerio.LocalData, make([]byte, runnerio.MaxLocalFrame+1)) == nil {
		t.Error("a side wrote a frame over the limit")
	}
}

// runnerioSocket answers one hello on a fresh socket with runnerio's own codec and types, decoding the
// CLI's hello strictly; answer is what it sends back. It returns the socket path and the connection.
func runnerioSocket(t *testing.T, runID string, answer func(net.Conn)) (string, <-chan net.Conn) {
	t.Helper()
	dir, err := os.MkdirTemp("", "wp") // short: a unix socket path is at most 104 bytes on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "attach.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		typ, b, err := runnerio.ReadLocalFrame(c)
		var h runnerio.LocalHelloMsg
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err != nil || typ != runnerio.LocalHello || dec.Decode(&h) != nil || h.V != runnerio.LocalProtocol || h.RunID.String() != runID {
			t.Errorf("the runner could not read the CLI's hello: %q %s (%v)", typ, b, err)
			_ = c.Close()
			return
		}
		answer(c)
		conns <- c
	}()
	return sock, conns
}

func TestLocalFrameParity_HelloAndEnd(t *testing.T) {
	f := newLocalFixture(t, grantLocal)
	sock, conns := runnerioSocket(t, f.org.runID, func(c net.Conn) {
		_ = runnerio.WriteLocalJSON(c, runnerio.LocalHelloOK, runnerio.LocalHelloOKMsg{RunnerID: f.org.runnerID, Nonce: "N9"})
	})
	f.pointer["socket"] = sock
	f.writePointer(t)
	lc, why := openLocal(context.Background(), &sdk.Client{BaseURL: f.srv.URL}, f.org.runID)
	if lc == nil || lc.nonce != "N9" {
		t.Fatalf("openLocal = %v, %q; want the runner's nonce", lc, why)
	}
	defer func() { _ = lc.conn.Close() }()
	c := <-conns
	go func() {
		_ = runnerio.WriteLocalJSON(c, runnerio.LocalBound, runnerio.LocalBoundMsg{Cols: 80, Rows: 24})
		_ = runnerio.WriteLocalFrame(c, runnerio.LocalData, []byte("screen"))
		_ = runnerio.WriteLocalJSON(c, runnerio.LocalEnd, runnerio.LocalReasonMsg{Reason: runnerio.LocalReasonExited})
	}()
	var shown bytes.Buffer
	err := localOutput(lc, func(b []byte) error { shown.Write(b); return nil })
	var end *localEnded
	if !errors.As(err, &end) || end.reason != runnerio.LocalReasonExited || !localCleanEnds[end.reason] || shown.String() != "screen" {
		t.Errorf("localOutput = %v, shown %q; want the screen, then the runner's clean end", err, shown.String())
	}
}

func TestLocalFrameParity_Refusal(t *testing.T) {
	f := newLocalFixture(t, grantLocal)
	sock, _ := runnerioSocket(t, f.org.runID, func(c net.Conn) {
		_ = runnerio.WriteLocalJSON(c, runnerio.LocalRefuse, runnerio.LocalReasonMsg{Reason: runnerio.LocalReasonBusy})
	})
	f.pointer["socket"] = sock
	f.writePointer(t)
	lc, why := openLocal(context.Background(), &sdk.Client{BaseURL: f.srv.URL}, f.org.runID)
	if lc != nil || why != "the runner refused: "+runnerio.LocalReasonBusy {
		t.Errorf("openLocal = %v, %q; want the runner's refusal with its reason", lc, why)
	}
}

// The CLI writes a resize inline in localPipe, beside a WebSocket write; its keys are pinned in the
// source and held to the runner's LocalSizeMsg.
func TestLocalFrameParity_Resize(t *testing.T) {
	src, err := os.ReadFile("attach_local.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `json.Marshal(map[string]uint16{"cols": cols, "rows": rows})`) ||
		!strings.Contains(string(src), "lc.send(localResize, b)") {
		t.Fatal("attach_local.go no longer writes the resize frame as {cols, rows}; re-check it against runnerio.LocalSizeMsg")
	}
	var keys []string
	st := reflect.TypeFor[runnerio.LocalSizeMsg]()
	for i := range st.NumField() {
		keys = append(keys, st.Field(i).Tag.Get("json"))
	}
	if !reflect.DeepEqual(keys, []string{"cols", "rows"}) {
		t.Errorf("runnerio.LocalSizeMsg keys %v, the CLI sends cols and rows", keys)
	}
	var m runnerio.LocalSizeMsg
	if err := json.Unmarshal([]byte(`{"cols":80,"rows":24}`), &m); err != nil || m != (runnerio.LocalSizeMsg{Cols: 80, Rows: 24}) {
		t.Errorf("the runner reads the CLI's resize as %+v (%v)", m, err)
	}
}
