// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// edit-base starts no helper while a prepare, tools update or edit of this runner's runs, without a ready
// base, as root, or with both VM slots in use; each refusal is the approved one.
func TestMacOSVMEditBaseRefusals(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	helper, args := fakeHelper(t, 0)
	m.helper = helper
	admit := func(k admission.Kind) admission.Entry {
		t.Helper()
		e, err := m.ledger.Admit(admission.Request{Kind: k, GuestMemory: 4 * gib, DiskReserve: gib, OwnerPID: os.Getpid(), OwnerStart: 3})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	refused := func(want macvm.EditRefusal) {
		t.Helper()
		if err := m.editBase(context.Background(), &bytes.Buffer{}, "runner-a"); err != want {
			t.Fatalf("edit-base: %v, want %q", err, want)
		}
	}
	for _, k := range []admission.Kind{admission.KindPrepare, admission.KindToolsUpdate, kindEditBase} {
		e := admit(k)
		refused(macvm.ErrEditBusy)
		if err := m.ledger.Release(e.ID, admission.Destroyed{Stopped: true}); err != nil {
			t.Fatal(err)
		}
	}
	admit(admission.KindRun)
	admit(admission.KindRun)
	refused(macvm.ErrEditSlots)
	prev := processEUID
	processEUID = func() int { return 0 }
	refused(macvm.ErrEditNoDesktop)
	processEUID = prev
	_ = os.Remove(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile))
	refused(macvm.ErrEditNoBase)
	if _, err := os.Stat(args); !os.IsNotExist(err) {
		t.Fatalf("a refused edit ran the helper: %q", calls(t, args))
	}
}

// A foreground job's helper lines reach the person, and its last error line, a discard, is the command's
// error.
func TestMacOSVMEditBaseAnswersTheHelpersLastLine(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	log := filepath.Join(t.TempDir(), editBaseLog)
	_ = os.WriteFile(log, []byte("wardyn-macvm: an earlier edit's error\n"), 0o600)
	logf, err := os.OpenFile(log, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()
	var out bytes.Buffer
	const discard = "The edited base was discarded: Remote Login is on. Runs keep using the base from 28 Oct."
	cmd := exec.Command("/bin/sh", "-c", `printf '%s\n' "$1"; printf 'wardyn-macvm: %s\n' "$2" >&2; exit 1`, "sh", macvm.EditStartLine, discard)
	cmd.Stdout, cmd.Stderr = io.MultiWriter(logf, &out), logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	err = m.awaitJob(context.Background(), helperJob{log: log, descr: "edit"}, cmd, exited)
	if err == nil || err.Error() != discard {
		t.Fatalf("err = %v", err)
	}
	if strings.TrimSpace(out.String()) != macvm.EditStartLine {
		t.Fatalf("out = %q", out.String())
	}
}

// A ready base's caps carry its last edit and the recheck, from the helper's edit record; a base a newer
// prepare made does not inherit an older base's record.
func TestMacOSVMCapsCarryTheLastEdit(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	base := m.layout.BaseDir(0)
	man, err := macvm.ReadManifest(base)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	b, _ := json.Marshal(macvm.EditRecord{BaseCreatedAt: man.CreatedAt, EditedAt: at, Recheck: macvm.EditRecheckPassed})
	if err := os.WriteFile(base+".edit.json", b, 0o600); err != nil {
		t.Fatal(err)
	}
	_, got := m.base()
	if got.EditedAt == nil || !got.EditedAt.Equal(at) || got.EditRecheck != runnerwire.MacVMEditRecheckPassed {
		t.Fatalf("base %+v", got)
	}
	b, _ = json.Marshal(macvm.EditRecord{BaseCreatedAt: man.CreatedAt.Add(-time.Hour), EditedAt: at, Recheck: macvm.EditRecheckPassed})
	_ = os.WriteFile(base+".edit.json", b, 0o600)
	if _, got := m.base(); got.EditedAt != nil || got.EditRecheck != "" {
		t.Fatalf("another base's record read as this one's: %+v", got)
	}
	for _, pair := range [][2]string{
		{macvm.EditRecheckPassed, runnerwire.MacVMEditRecheckPassed}, {macvm.EditRecheckDesktopOff, runnerwire.MacVMEditRecheckDesktopOff},
		{macvm.EditRecheckExtraAccount, runnerwire.MacVMEditRecheckExtraAccount}, {macvm.EditRecheckRemoteLogin, runnerwire.MacVMEditRecheckRemoteLogin},
		{macvm.EditRecheckSelftest, runnerwire.MacVMEditRecheckSelftest},
	} {
		if pair[0] != pair[1] {
			t.Errorf("helper recheck %q is the wire's %q", pair[0], pair[1])
		}
	}
}
