// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/version"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"golang.org/x/sys/unix"
)

type onAC struct{}

func (onAC) OnAC(context.Context) (bool, error) { return true, nil }

// lockedBuf is a bytes.Buffer two goroutines may share.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// runnerdArgs is the argv runnerd's startJob builds for a tools update
// (cmd/wardyn-runnerd/macos_vm.go), from the exported names.
func runnerdArgs(root, entry, ledger string) []string {
	return []string{macvm.UpdateToolsCommand, "--" + macvm.FlagRoot, root, "--" + macvm.FlagCPUs, "2", "--" + macvm.FlagMemoryMiB, "4096",
		"--" + macvm.FlagRunnerID, "r1", "--" + macvm.FlagLedgerEntry, entry, "--" + macvm.FlagLedgerDir, ledger}
}

// update-tools takes runnerd's flags, claims the entry runnerd passed,
// boots the base's staging copy on the net_fd runnerd sends to the job's
// ctl.sock, reaches the guest agent's tools mode on 1026, records the guest's
// tools-port dial, and swaps the updated base in.
func TestUpdateToolsTakesRunnerdsFlagsAndNetFD(t *testing.T) {
	root := t.TempDir()
	ledger := makeLedger(t, root)
	l, err := admission.Open(admission.Config{Dir: ledger, UID: os.Getuid(), RunnerID: "r1", Host: host{}, Fstat: rootFstat})
	if err != nil {
		t.Fatal(err)
	}
	e, err := l.Admit(admission.Request{Kind: admission.KindToolsUpdate, GuestMemory: 4 << 30, DiskReserve: 1 << 30, OwnerPID: os.Getpid(), OwnerStart: 7})
	if err != nil {
		t.Fatal(err)
	}
	layout := macvm.Layout{Root: filepath.Join(root, "vm")}
	base := layout.BaseDir(0)
	_ = os.MkdirAll(base, 0o700)
	for n, b := range map[string]string{macvm.DiskFile: "d", macvm.AuxFile: "a", macvm.MachineIDFile: "base-id"} {
		_ = os.WriteFile(filepath.Join(base, n), []byte(b), 0o600)
	}
	_ = os.WriteFile(filepath.Join(base, macvm.ManifestFile),
		[]byte(`{"format":1,"state":"ready","hardware_model":"aHc=","min_cpus":2,"min_memory_mib":4096,"desktop":"control","helper_version":"old"}`), 0o600)

	eng := &macvmtest.Engine{}
	d, _, _ := testDeps(eng)
	var out, errb lockedBuf
	d.stdout, d.stderr, d.powerSource = &out, &errb, onAC{}
	sockRoot := macvmtest.SockRoot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, append(runnerdArgs(layout.Root, e.ID, ledger), "--"+macvm.FlagSockRoot, sockRoot), d)
	}()

	// runnerd's side: the net_fd to the job's ctl.sock.
	var cc *macvm.ControlClient
	for deadline := time.Now().Add(10 * time.Second); cc == nil; time.Sleep(10 * time.Millisecond) {
		if cc, err = macvm.DialControl(ctx, macvm.JobControlSock(sockRoot, e.ID)); err != nil && time.Now().After(deadline) {
			t.Fatalf("the job's ctl.sock never answered: %v (%s)", err, errb.String())
		}
	}
	defer cc.Close()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	ours, theirs := os.NewFile(uintptr(fds[0]), "attach"), os.NewFile(uintptr(fds[1]), "relay")
	defer theirs.Close()
	if err := cc.SendNetFD(ctx, ours); err != nil {
		t.Fatal(err)
	}
	_ = ours.Close()

	// The guest agent: tools mode on 1026, its control session on 1024.
	var m *macvmtest.Machine
	for deadline := time.Now().Add(10 * time.Second); m == nil || !m.Listening(macvm.ToolsPort); time.Sleep(10 * time.Millisecond) {
		if ms := eng.Machines(); len(ms) > 0 {
			m = ms[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no stage-2 VM listened on the tools port (%s)", errb.String())
		}
	}
	var mu sync.Mutex
	var execs [][]string
	tools := http.NewServeMux()
	tools.HandleFunc("POST /v1/tools/exec", func(w http.ResponseWriter, r *http.Request) {
		var x guestagent.ToolsExec
		_ = json.NewDecoder(r.Body).Decode(&x)
		mu.Lock()
		execs = append(execs, x.Argv)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(guestagent.ToolsExecResult{Output: "No new software available.", ExitCode: 0})
	})
	control := http.NewServeMux()
	control.HandleFunc("GET /v1/dial/tools", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(guestagent.ToolsDial{Class: guestagent.DialConnected, Dials: 1})
	})
	go func() {
		_ = guestagent.ServeVsock(ctx, func(context.Context) (net.Conn, error) { return m.DialGuest(macvm.ToolsPort) }, tools)
	}()
	go func() {
		_ = guestagent.ServeVsock(ctx, func(context.Context) (net.Conn, error) { return m.DialGuest(macvm.AgentPort) }, control)
	}()
	if code := <-done; code != 0 {
		t.Fatalf("update-tools exit %d: %s", code, errb.String())
	}
	es, _ := l.Counted()
	if len(es) != 1 || es[0].HelperPID != os.Getpid() {
		t.Fatalf("the entry runnerd passed was not claimed: %+v", es)
	}
	mu.Lock()
	ranSU := slices.ContainsFunc(execs, func(a []string) bool { return strings.HasSuffix(a[0], "softwareupdate") })
	mu.Unlock()
	if !ranSU {
		t.Fatalf("stage 2 ran no tools step over 1026: %v", execs)
	}
	man, err := macvm.ReadyBase(base)
	if err != nil || man.HelperVersion != version.String() || man.Desktop != "control" {
		t.Fatalf("base after the update: %+v %v", man, err)
	}
	if !strings.Contains(out.String(), `"tools_dial":"connected"`) {
		t.Fatalf("the tools-port dial is not in the job's log:\n%s", out.String())
	}
	if _, err := os.Lstat(filepath.Dir(macvm.JobControlSock(sockRoot, e.ID))); !os.IsNotExist(err) {
		t.Fatalf("the job's socket dir outlived it: %v", err)
	}
}
