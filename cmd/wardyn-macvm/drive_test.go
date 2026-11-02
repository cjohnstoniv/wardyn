// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// driveOpenBuffer is stdout, written by the helper while the test polls it.
type driveOpenBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *driveOpenBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *driveOpenBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// drive-open needs its entry, its drive and its label; none of the refusals boots a VM.
func TestDriveOpenRequiredFlags(t *testing.T) {
	eng := &macvmtest.Engine{}
	d, _, errb := testDeps(eng)
	full := []string{"--" + macvm.FlagRoot, t.TempDir(), "--" + macvm.FlagCPUs, "2", "--" + macvm.FlagMemoryMiB, "4096", "--" + macvm.FlagRunnerID, "r1",
		"--" + macvm.FlagLedgerEntry, "0123456789abcdef", "--" + macvm.FlagDrive, "wardyn-drive-a", "--" + macvm.FlagDriveLabel, "Wardyn-a"}
	for i := 2; i < len(full); i += 2 {
		args := append(append([]string{macvm.DriveOpenCommand}, full[:i]...), full[i+2:]...)
		if code := run(context.Background(), args, d); code != 2 {
			t.Errorf("without %s: exit %d: %s", full[i], code, errb)
		}
	}
	if len(eng.Machines()) != 0 {
		t.Fatal("a refused drive-open built a VM")
	}
}

// The command as the CLI drives it: it claims the entry, prints one ready line, and stops when its
// caller's stdin pipe ends, exiting 0.
func TestDriveOpenCommandReadyLineAndStdinStop(t *testing.T) {
	root := t.TempDir()
	ledger := makeLedger(t, root)
	l, err := admission.Open(admission.Config{Dir: ledger, UID: os.Getuid(), RunnerID: "r1", Host: host{}, Fstat: rootFstat})
	if err != nil {
		t.Fatal(err)
	}
	e, err := l.Admit(admission.Request{Kind: "drive_open", GuestMemory: 4 << 30, DiskReserve: 1 << 30, OwnerPID: os.Getpid(), OwnerStart: 7})
	if err != nil {
		t.Fatal(err)
	}
	layout := macvm.Layout{Root: filepath.Join(root, "vm")}
	base := layout.BaseDir(0)
	_ = os.MkdirAll(base, 0o700)
	for n, b := range map[string]string{macvm.DiskFile: "d", macvm.AuxFile: "a", macvm.MachineIDFile: "base-id", macvm.PairKeyFile: "wardyn-test-pair-key-0123456789!"} {
		_ = os.WriteFile(filepath.Join(base, n), []byte(b), 0o600)
	}
	_ = os.WriteFile(filepath.Join(base, macvm.ManifestFile), []byte(`{"format":2,"state":"ready","hardware_model":"aHc="}`), 0o600)
	const object = "wardyn-drive-a"
	img, _ := layout.DriveImage(object)
	_ = os.MkdirAll(filepath.Dir(img), 0o700)
	_ = os.WriteFile(img, []byte("x"), 0o600)

	eng := &macvmtest.Engine{}
	d, _, errb := testDeps(eng)
	var out driveOpenBuffer
	d.stdout = &out
	stdinR, stdinW := io.Pipe()
	d.stdin = stdinR
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sockRoot := macvmtest.SockRoot(t)
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{macvm.DriveOpenCommand, "--" + macvm.FlagRoot, layout.Root, "--" + macvm.FlagCPUs, "2", "--" + macvm.FlagMemoryMiB, "4096",
			"--" + macvm.FlagRunnerID, "r1", "--" + macvm.FlagLedgerEntry, e.ID, "--" + macvm.FlagLedgerDir, ledger, "--" + macvm.FlagSockRoot, sockRoot,
			"--" + macvm.FlagDrive, object, "--" + macvm.FlagDriveLabel, "Wardyn-a"}, d)
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ready", func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(guestagent.Ready{}) })
	mux.HandleFunc("POST /v1/init", func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(guestagent.InitResult{}) })
	served := false
	deadline := time.Now().Add(20 * time.Second)
	for out.String() == "" {
		if time.Now().After(deadline) {
			t.Fatalf("no ready line; stderr: %s", errb)
		}
		if ms := eng.Machines(); !served && len(ms) > 0 && ms[0].Listening(macvm.AgentPort) {
			served = true
			m := ms[0]
			go func() {
				_ = guestagent.ServeVsock(ctx, func(context.Context) (net.Conn, error) { return m.DialAgent(macvm.AgentPort) }, mux)
			}()
		}
		time.Sleep(5 * time.Millisecond)
	}
	var ready macvm.DriveOpenReady
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &ready); err != nil || ready.Target != macvm.DriveOpenTarget || ready.AgentSock == "" {
		t.Fatalf("ready line %q: %v", out.String(), err)
	}
	if got, err := l.Counted(); err != nil || len(got) != 1 || got[0].HelperPID != os.Getpid() {
		t.Fatalf("the entry was not claimed: %+v %v", got, err)
	}
	_ = stdinW.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errb)
		}
	case <-ctx.Done():
		t.Fatal("stdin's end did not stop the VM")
	}
	if eng.Machines()[0].State() != macvm.StateStopped {
		t.Fatalf("the VM is %s", eng.Machines()[0].State())
	}
}
