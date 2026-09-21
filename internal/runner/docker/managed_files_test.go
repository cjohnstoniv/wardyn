// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// tarEntry is one header this package's archive produced, flattened for
// assertions.
type tarEntry struct {
	name string
	typ  byte
	mode int64
	uid  int
	gid  int
	body string
}

func readTar(t *testing.T, b []byte) []tarEntry {
	t.Helper()
	var out []tarEntry
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read archive body for %s: %v", h.Name, err)
		}
		out = append(out, tarEntry{name: h.Name, typ: h.Typeflag, mode: h.Mode, uid: h.Uid, gid: h.Gid, body: string(body)})
	}
	return out
}

// The archive IS the security boundary on this substrate: every header must
// carry uid/gid 0, and there must be no directory entry at all — the daemon
// applies one to a directory that already exists, which is how a delivery
// re-owned a host bind-mount source to root.
func TestManagedFilesTar(t *testing.T) {
	buf, err := managedFilesTar([]runner.ManagedFile{
		{Path: "/etc/wardyn/agent/settings.json", Mode: 0o644, Content: []byte(`{"a":1}`)},
		{Path: "/etc/wardyn/agent/locked", Mode: 0o444, Content: []byte("x")},
		{Path: "/opt/wardyn/policy/rules", Content: []byte("r")},
	})
	if err != nil {
		t.Fatalf("managedFilesTar: %v", err)
	}
	got := readTar(t, buf.Bytes())

	want := []tarEntry{
		{name: "etc/wardyn/agent/settings.json", typ: tar.TypeReg, mode: 0o644, body: `{"a":1}`},
		{name: "etc/wardyn/agent/locked", typ: tar.TypeReg, mode: 0o444, body: "x"},
		{name: "opt/wardyn/policy/rules", typ: tar.TypeReg, mode: 0o644, body: "r"},
	}
	if len(got) != len(want) {
		t.Fatalf("archive has %d entries %v, want %d %v — a directory entry re-owns a directory that already exists", len(got), got, len(want), want)
	}
	for i, w := range want {
		g := got[i]
		if g.name != w.name || g.typ != w.typ || g.mode != w.mode || g.body != w.body {
			t.Errorf("entry %d = %+v, want %+v", i, g, w)
		}
		if g.uid != 0 || g.gid != 0 {
			t.Errorf("entry %q has uid/gid %d/%d — every managed-file header MUST be 0/0 or the file lands writable by the thing it is meant to constrain", g.name, g.uid, g.gid)
		}
	}
}

// The same content must produce the same bytes: an archive whose header mtimes
// come from the clock cannot be diffed, and a copy is the one step here with no
// substrate-side verification.
func TestManagedFilesTarIsDeterministic(t *testing.T) {
	files := []runner.ManagedFile{{Path: "/etc/wardyn/a/f", Content: []byte("body")}}
	a, err := managedFilesTar(files)
	if err != nil {
		t.Fatalf("managedFilesTar: %v", err)
	}
	b, err := managedFilesTar(files)
	if err != nil {
		t.Fatalf("managedFilesTar: %v", err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("two archives over identical input differ")
	}
}

func TestManagedFilesTarRefusesAnInvalidSpec(t *testing.T) {
	if _, err := managedFilesTar([]runner.ManagedFile{{Path: "/etc/f", Content: []byte("x")}}); err == nil {
		t.Fatal("managedFilesTar accepted a top-level path; it must run the shared contract check")
	}
}

// managedSpec is testSpec plus two managed files.
func managedSpec() runner.SandboxSpec {
	spec := testSpec()
	spec.ManagedFiles = []runner.ManagedFile{
		{Path: "/etc/wardyn/agent/settings.json", Mode: 0o644, Content: []byte(`{"managed":true}`)},
		{Path: "/etc/wardyn/agent/locked", Mode: 0o444, Content: []byte("locked")},
	}
	return spec
}

// THE ORDERING IS THE CONTRACT. A copy that lands after ContainerStart races
// the agent's own first instruction: the agent can read the absence, or act,
// before the ceiling exists. This is the assertion a "read the file back" test
// cannot make — by the time anything reads it, the copy has happened either
// way.
func TestCreateSandbox_DeliversManagedFilesBeforeStart(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)

	spec := managedSpec()
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	agent := agentContainerName(spec.RunID)
	var copies []fakeCopy
	for _, c := range f.copies {
		if c.id == agent {
			copies = append(copies, c)
		}
	}
	if len(copies) != 1 {
		t.Fatalf("got %d CopyToContainer calls for the agent, want exactly 1 (copies: %+v)", len(copies), f.copies)
	}
	cp := copies[0]
	if cp.afterStart {
		t.Error("managed files were copied AFTER ContainerStart; the agent's main process was already running, so the file it is meant to be bound by arrived late")
	}
	if !slices.Contains(f.startedNames, agent) {
		t.Error("the agent container was never started")
	}
	if cp.dest != "/" {
		t.Errorf("CopyToContainer destination = %q, want %q", cp.dest, "/")
	}
	if cp.copyUIDGID {
		t.Error("CopyUIDGID was set; it makes the daemon chown the extracted tree to the IMAGE's user (uid 1000), which is exactly the agent-writable outcome managed files exist to rule out")
	}

	// And the bytes on the wire carry root ownership.
	for _, e := range readTar(t, cp.archive) {
		if e.uid != 0 || e.gid != 0 {
			t.Errorf("archive entry %q reached the daemon with uid/gid %d/%d, want 0/0", e.name, e.uid, e.gid)
		}
	}
}

// Nothing is copied when nothing was asked for: the field is additive and a
// run without it must produce the same daemon traffic it always has.
func TestCreateSandbox_NoManagedFilesNoCopy(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	if _, err := d.CreateSandbox(context.Background(), testSpec()); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if len(f.copies) != 0 {
		t.Errorf("a spec with no managed files produced %d CopyToContainer calls: %+v", len(f.copies), f.copies)
	}
}

// A delivery that fails must fail the RUN. A sandbox that starts without the
// ceiling it was promised is the one outcome worse than no ceiling: the
// control plane records it as delivered.
func TestCreateSandbox_FailsClosedWhenDeliveryFails(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.failCopyToContainer = true
	d := newTestDriver(f)

	spec := managedSpec()
	if _, err := d.CreateSandbox(context.Background(), spec); err == nil {
		t.Fatal("CreateSandbox must fail when managed-file delivery fails")
	}
	agent := agentContainerName(spec.RunID)
	if slices.Contains(f.startedNames, agent) {
		t.Errorf("the agent was STARTED after a failed managed-file delivery (started: %v)", f.startedNames)
	}
	if _, ok := f.networks[internalNetName(spec.RunID)]; ok {
		t.Error("the per-run network survived a failed managed-file delivery; the rollback must be complete")
	}
}

// A managed file's directory that already exists — shipped by the image, or
// mounted there — is refused, not delivered into: nothing the daemon reports
// says who owns it, and a mounted one is a host directory.
func TestCreateSandbox_RefusesAManagedFileDirectoryThatAlreadyExists(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.existingPaths = map[string]bool{"/etc/wardyn/agent": true}
	d := newTestDriver(f)

	spec := managedSpec()
	_, err := d.CreateSandbox(context.Background(), spec)
	if err == nil {
		t.Fatal("CreateSandbox delivered into a managed-file directory that already existed")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("refusal = %v, want it to say the directory already exists", err)
	}
	if len(f.copies) != 0 {
		t.Errorf("the refusal still copied %d archives into the container", len(f.copies))
	}
	if slices.Contains(f.startedNames, agentContainerName(spec.RunID)) {
		t.Error("the agent was STARTED after its managed files were refused")
	}
	if _, ok := f.networks[internalNetName(spec.RunID)]; ok {
		t.Error("the per-run network survived the refusal; the rollback must be complete")
	}
}

// An unusable spec is refused before anything exists on the daemon, not
// half-applied and rolled back.
func TestCreateSandbox_RefusesAnInvalidManagedFileBeforeCreatingAnything(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)

	spec := testSpec()
	spec.ManagedFiles = []runner.ManagedFile{{Path: "/etc/wardyn/agent/settings.json", Mode: 0o666, Content: []byte("{}")}}
	_, err := d.CreateSandbox(context.Background(), spec)
	if err == nil {
		t.Fatal("CreateSandbox accepted a world-writable managed file")
	}
	if !strings.Contains(err.Error(), "writable") {
		t.Errorf("refusal = %v, want it to name the writable mode", err)
	}
	if len(f.networks) != 0 || len(f.containers) != 0 {
		t.Errorf("the refusal created objects on the daemon: %d networks, %d containers", len(f.networks), len(f.containers))
	}
}

// The exec-less (krun/libkrun) path creates its container in runAsMainProcess,
// where the workload IS the main process — so the delivery has to happen there
// too, and it is the path most likely to be forgotten.
func TestExecLessPath_DeliversManagedFilesBeforeStart(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.info = infoWithRuntimes("krun") // CC3 via krun: the exec-less path
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})

	spec := managedSpec()
	spec.ConfinementClass = types.CC3
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if len(f.copies) != 0 {
		t.Fatalf("the exec-less path has no container until Exec; %d copies happened at create", len(f.copies))
	}
	if _, err := d.Exec(context.Background(), sb.Ref, []string{"agent-run"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(f.copies) != 1 {
		t.Fatalf("got %d CopyToContainer calls, want 1 (the deferred create must deliver too)", len(f.copies))
	}
	if f.copies[0].afterStart {
		t.Error("the exec-less path copied managed files AFTER start; its main process IS the agent workload, so there is no later window at all")
	}
}
