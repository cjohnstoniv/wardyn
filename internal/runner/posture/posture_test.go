// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package posture

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The MDM's file is trusted only when its owner wrote it and nobody else can;
// otherwise the runner's own probe answers, marked os_probe.
func TestPosture_MDMFileWhenTrustedElseTheProbe(t *testing.T) {
	uid := uint32(os.Getuid())
	path := filepath.Join(t.TempDir(), "posture.json")
	write := func(body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	if p := read(path, uid); p.Evidence != types.PostureEvidenceOSProbe || p.MDMManaged || p.OS == "" {
		t.Fatalf("no file: %+v, want the probe", p)
	}
	write(`{"mdm_managed":true,"disk_encrypted":true}`, 0o644)
	if p := read(path, uid); p.Evidence != types.PostureEvidenceMDMFile || !p.MDMManaged || !p.DiskEncrypted {
		t.Fatalf("trusted file: %+v", p)
	}
	for name, ignored := range map[string]func() types.RunnerPosture{
		"another owner":  func() types.RunnerPosture { return read(path, uid+1) },
		"group-writable": func() types.RunnerPosture { write(`{"mdm_managed":true}`, 0o664); return read(path, uid) },
		"unknown field": func() types.RunnerPosture {
			write(`{"mdm_managed":true,"attested":true}`, 0o644)
			return read(path, uid)
		},
	} {
		if p := ignored(); p.Evidence != types.PostureEvidenceOSProbe || p.MDMManaged {
			t.Errorf("%s: %+v, want the file ignored", name, p)
		}
	}
}

// The posture is sent at READY and again only when it changes.
func TestPosture_ReporterSendsAtReadyAndOnChange(t *testing.T) {
	got := make(chan types.RunnerPosture, 4)
	a, b := runnerwire.Loopback()
	org := runnerwire.NewPeer(a, runnerwire.PeerConfig{Org: true, OnEventChecked: func(ev runnerwire.Event) error {
		var p types.RunnerPosture
		if ev.Kind == runnerwire.EventPosture && runnerwire.Unmarshal(ev.Data, &p) == nil {
			got <- p
		}
		return nil
	}})
	run := runnerwire.NewPeer(b, runnerwire.PeerConfig{})
	defer org.Close()
	defer run.Close()
	go org.Run(t.Context()) //nolint:errcheck // the test owns shutdown
	go run.Run(t.Context()) //nolint:errcheck // the test owns shutdown

	latest := types.RunnerPosture{OS: "linux", Evidence: types.PostureEvidenceOSProbe}
	reads := make(chan types.RunnerPosture, 8)
	r := &Reporter{read: func() types.RunnerPosture {
		select {
		case latest = <-reads:
		default:
		}
		return latest
	}}
	if err := r.Send(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if p := <-got; p.OS != "linux" {
		t.Fatalf("READY posture = %+v", p)
	}
	go r.Watch(run, 5*time.Millisecond)
	changed := types.RunnerPosture{OS: "linux", DiskEncrypted: true, Evidence: types.PostureEvidenceOSProbe}
	reads <- changed
	select {
	case p := <-got:
		if p != changed {
			t.Fatalf("changed posture = %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a changed posture was not reported")
	}
	select {
	case p := <-got:
		t.Fatalf("an unchanged posture was reported again: %+v", p)
	case <-time.After(50 * time.Millisecond):
	}
}

// An unusable MDM file is logged once per reason, however many reads find it.
func TestPosture_UnusableFileIsLoggedOncePerReason(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	uid := uint32(os.Getuid())
	path := filepath.Join(t.TempDir(), "posture.json")
	if err := os.WriteFile(path, []byte(`{"mdm_managed":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		read(path, uid+1) // another owner: ignored each time
	}
	if got := strings.Count(buf.String(), "the MDM posture file is ignored"); got != 1 {
		t.Fatalf("logged %d times for one reason:\n%s", got, buf.String())
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	read(path, uid) // a different reason (group-writable) is logged again
	if got := strings.Count(buf.String(), "the MDM posture file is ignored"); got != 2 {
		t.Fatalf("a changed reason logged %d times in all:\n%s", got, buf.String())
	}
}
