// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// A run directory is named by the run id's first 12 hex digits, and every
// open checks run.json against that name.
func TestRunDirIsTheRunKeyAndRunJSONIsCheckedOnOpen(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	if got := filepath.Base(l.RunDir(testRunID)); got != "6f1c2a3b4d5e" {
		t.Fatalf("run dir name %q, want 6f1c2a3b4d5e", got)
	}
	if got := filepath.Base(l.RunDir("6f1c2a3b4d5e")); got != "6f1c2a3b4d5e" {
		t.Fatalf("a key is not its own run dir: %q", got)
	}
	cfg := macvm.RunConfig{RunID: testRunID, BaseDir: "/b"}
	if err := macvm.WriteRunConfig(l.RunDir(testRunID), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := macvm.ReadRunConfig(l.RunDir(testRunID)); err != nil {
		t.Fatalf("its own dir: %v", err)
	}
	other := filepath.Join(l.RunsDir(), "aaaaaaaaaaaa")
	if err := macvm.WriteRunConfig(other, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := macvm.ReadRunConfig(other); err == nil || !strings.Contains(err.Error(), "whose directory is 6f1c2a3b4d5e") {
		t.Fatalf("a run.json in another run's dir: %v", err)
	}
	r := startRun(t, &macvmtest.Engine{}, func(_ *macvm.RunConfig, o *macvm.RunOptions) { o.RunDir = other })
	if err := <-r.done; err == nil || !strings.Contains(err.Error(), "whose directory is") {
		t.Fatalf("run --dir of a mismatched dir: %v", err)
	}
}

// A run.json that carries tools mode is refused before anything is built.
func TestRunJSONCarryingToolsModeIsRefused(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	dir := l.RunDir(testRunID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"run_id":"` + testRunID + `","base_dir":"/b","tools_mode":true}`
	if err := os.WriteFile(filepath.Join(dir, macvm.RunConfigFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := macvm.ReadRunConfig(dir); !errors.Is(err, macvm.ErrToolsModeRun) {
		t.Fatalf("got %v, want ErrToolsModeRun", err)
	}
	eng := &macvmtest.Engine{}
	if err := macvm.Run(context.Background(), macvm.RunOptions{Engine: eng, RunDir: dir}); !errors.Is(err, macvm.ErrToolsModeRun) || len(eng.Machines()) != 0 {
		t.Fatalf("run: %v, machines %d", err, len(eng.Machines()))
	}
}

// A run's sockets live in its socket directory, not the run dir, which the
// helper removes when it ends; status names the vsock listeners, a run's
// only (1024 here: no desktop shim).
func TestRunSocketsLiveInTheSocketDir(t *testing.T) {
	r := startRun(t, &macvmtest.Engine{}, nil)
	if filepath.Base(r.sock) != "6f1c2a3b4d5e" || filepath.Base(filepath.Dir(r.sock)) != "u"+strconv.Itoa(os.Getuid()) {
		t.Fatalf("socket dir %s", r.sock)
	}
	if fi, err := os.Lstat(r.sock); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode: %v %v", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(r.dir, macvm.ControlSock)); !os.IsNotExist(err) {
		t.Fatalf("ctl.sock in the run dir: %v", err)
	}
	r.attach(t)
	st := waitState(t, r.cc, macvm.StateRunning)
	if !slices.Equal(st.Listeners, []uint32{macvm.AgentPort}) {
		t.Fatalf("listeners %v", st.Listeners)
	}
	if err := r.cc.Stop(context.Background(), true, 0); err != nil {
		t.Fatal(err)
	}
	if err := <-r.done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(r.sock); !os.IsNotExist(err) {
		t.Fatalf("the socket dir outlived its helper: %v", err)
	}
}

// A run's VM that listens on the tools port fails closed before Init: no
// agent.sock exists for runnerd to send Init on, and status says why and
// shows the listener.
func TestRunListeningOnTheToolsPortFailsBeforeInit(t *testing.T) {
	defer macvm.SetRunExtraPorts(macvm.ToolsPort)()
	eng := &macvmtest.Engine{}
	r := startRun(t, eng, nil)
	evc, err := macvm.DialControl(context.Background(), filepath.Join(r.sock, macvm.ControlSock))
	if err != nil {
		t.Fatal(err)
	}
	defer evc.Close()
	evs, err := evc.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.attach(t)
	select {
	case err = <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run went on with a VM listening on the tools port")
	}
	if !errors.Is(err, macvm.ErrToolsModeRun) {
		t.Fatalf("run: %v, want ErrToolsModeRun", err)
	}
	if _, serr := os.Lstat(filepath.Join(r.sock, macvm.AgentSock)); !os.IsNotExist(serr) {
		t.Fatalf("agent.sock was served: %v", serr)
	}
	var detail string
	for ev := range evs {
		if ev.State == macvm.StateError {
			detail = ev.Detail
		}
	}
	if !strings.Contains(detail, "This macOS VM started in tools mode") || !strings.Contains(detail, "1026") {
		t.Fatalf("error event %q", detail)
	}
	if m := eng.Machines()[0]; m.State() != macvm.StateStopped {
		t.Fatalf("the VM is %s", m.State())
	}
}

// The helper sweeps the socket dirs of its login's gone runs when a run
// starts, and keeps one whose run dir or helper is still there.
func TestRunStartSweepsGoneRunsSocketDirs(t *testing.T) {
	root := macvmtest.SockRoot(t)
	sc := macvm.SockConfig(root)
	sc.Fstat = macvmtest.SockFstat
	gone, live, answering := "111111111111", "222222222222", "333333333333"
	for _, k := range []string{gone, live, answering} {
		if _, err := sc.MakeRunDir(k); err != nil {
			t.Fatal(err)
		}
	}
	l, err := net.Listen("unix", filepath.Join(sc.UserDir(), answering, macvm.ControlSock))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	own := macvm.Layout{Root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(own.RunsDir(), live), 0o700); err != nil {
		t.Fatal(err)
	}
	r := startRun(t, &macvmtest.Engine{}, func(c *macvm.RunConfig, o *macvm.RunOptions) {
		c.SockRoot = root
		o.RunDir = own.RunDir(c.RunID)
		if err := macvm.WriteRunConfig(o.RunDir, *c); err != nil {
			t.Fatal(err)
		}
	})
	r.cancel()
	<-r.done
	if _, err := os.Lstat(filepath.Join(sc.UserDir(), gone)); !os.IsNotExist(err) {
		t.Fatalf("a gone run's socket dir was kept: %v", err)
	}
	for _, k := range []string{live, answering} {
		if _, err := os.Lstat(filepath.Join(sc.UserDir(), k)); err != nil {
			t.Fatalf("socket dir %s went: %v", k, err)
		}
	}
}
