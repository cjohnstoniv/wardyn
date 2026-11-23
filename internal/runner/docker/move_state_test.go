// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func skipWithoutMoveDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker move tests")
	}
}

// moveSpec is a busybox sandbox spec; withProxy adds the proxy config a create takes and an import refuses.
func moveSpec(withProxy bool) runner.SandboxSpec {
	s := runner.SandboxSpec{RunID: uuid.New(), Image: "busybox:latest", ConfinementClass: types.CC1,
		Resources: runner.Resources{CPUMillis: 500, MemoryMiB: 128}}
	if withProxy {
		s.ProxyConfig = runner.ProxyConfig{RunToken: "test", ControlPlaneURL: "http://127.0.0.1:0"}
	}
	return s
}

// readStream reads a movepack stream of set to its end, answering each entry and each file's content.
func readStream(t *testing.T, r io.Reader, set runner.CarriedSet) (map[string]movepack.Header, map[string]string, error) {
	t.Helper()
	rd, err := movepack.NewReader(r, movepack.Options{Set: set})
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	heads, files := map[string]movepack.Header{}, map[string]string{}
	for {
		h, content, err := rd.Next()
		if err == io.EOF {
			return heads, files, nil
		}
		if err != nil {
			return heads, files, err
		}
		heads[h.Path] = h
		if content != nil {
			b, _ := io.ReadAll(content)
			files[h.Path] = string(b)
		}
	}
}

// TestMoveExport_NeverFollowsLinks: a stopped sandbox's export carries a symlink as its text, never its
// target's bytes, and a hardlink as a file with its own content; a root that is itself a symlink refuses.
func TestMoveExport_NeverFollowsLinks(t *testing.T) {
	skipWithoutMoveDocker(t)
	d := newNetworkTestDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	sb, err := d.CreateSandbox(ctx, moveSpec(true))
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })
	execInSandbox(ctx, t, d, sb.Ref, []string{"sh", "-c",
		"mkdir -p /work/d && echo hi > /work/d/f && ln /work/d/f /work/hard && ln -s /etc/passwd /work/link && ln -s /work /worklink"})
	if _, err := d.cli.ContainerStop(ctx, sb.Ref, client.ContainerStopOptions{}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	set := runner.CarriedSet{Roots: []string{"/work"}}
	out, err := d.ExportState(ctx, sb.Ref, set)
	if err != nil {
		t.Fatalf("ExportState: %v", err)
	}
	heads, files, err := readStream(t, out.Stream, set)
	_ = out.Stream.Close()
	if err != nil {
		t.Fatalf("reading the export: %v", err)
	}
	if h := heads["work/link"]; h.Type != movepack.TypeSymlink || h.Link != "/etc/passwd" {
		t.Errorf("work/link = %+v; want a symlink carrying its text", h)
	}
	for p, body := range files {
		if strings.Contains(body, "root:") {
			t.Errorf("%s carries /etc/passwd's bytes: the export followed a link", p)
		}
	}
	if files["work/d/f"] != "hi\n" || files["work/hard"] != "hi\n" || heads["work/hard"].Type != movepack.TypeFile {
		t.Errorf("files = %q; want work/d/f and the hardlink work/hard each carried as a file holding \"hi\\n\"", files)
	}
	if !out.SpoolDrained {
		t.Error("SpoolDrained = false; the driver keeps no spool")
	}

	linked := runner.CarriedSet{Roots: []string{"/worklink"}}
	out, err = d.ExportState(ctx, sb.Ref, linked)
	if err != nil {
		t.Fatalf("ExportState of a symlink root: %v", err)
	}
	_, _, err = readStream(t, out.Stream, linked)
	_ = out.Stream.Close()
	if !errors.Is(err, movepack.ErrArchiveRefused) {
		t.Errorf("export of a root that is a symlink = %v; want ErrArchiveRefused", err)
	}
}

// TestMoveImport_WritesOnlyBeneathRoots: an import changes the new container's filesystem only beneath the
// carried roots and at the booted marker, as the agent's uid, with special bits cleared and the symlink
// written as a link; a stream with an entry outside its roots is refused before anything is written, and
// the sandbox it created is removed.
func TestMoveImport_WritesOnlyBeneathRoots(t *testing.T) {
	skipWithoutMoveDocker(t)
	d := newNetworkTestDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	set := runner.CarriedSet{Roots: []string{"/work"}}
	var stream bytes.Buffer
	w, err := movepack.NewWriter(&stream, set)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct {
		h    movepack.Header
		body string
	}{
		{movepack.Header{Path: "work", Type: movepack.TypeDir, Mode: 0o755}, ""},
		{movepack.Header{Path: "work/a", Type: movepack.TypeFile, Mode: 0o4755, Size: 4}, "data"},
		{movepack.Header{Path: "work/l", Type: movepack.TypeSymlink, Mode: 0o777, Link: "/etc"}, ""},
	} {
		if err := w.Add(e.h, strings.NewReader(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}

	sb, m, err := d.ImportSandbox(ctx, moveSpec(false), set, &stream)
	if err != nil {
		t.Fatalf("ImportSandbox: %v", err)
	}
	t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })
	if m.Entries != 3 {
		t.Errorf("manifest = %+v; want the three entries read", m)
	}
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	diff, err := cli.ContainerDiff(ctx, sb.Ref, client.ContainerDiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	allowed := []string{"/work", "/home", "/home/agent", "/home/agent/.wardyn", bootedMarker}
	for _, c := range diff.Changes {
		ok := strings.HasPrefix(c.Path, "/work/")
		for _, a := range allowed {
			ok = ok || c.Path == a
		}
		if !ok {
			t.Errorf("the import changed %s, outside the carried roots", c.Path)
		}
	}
	res, err := cli.CopyFromContainer(ctx, sb.Ref, client.CopyFromContainerOptions{SourcePath: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Content.Close()
	tr := tar.NewReader(res.Content)
	for {
		th, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if th.Uid != runner.AgentUID {
			t.Errorf("%s is owned by uid %d; want the agent's", th.Name, th.Uid)
		}
		switch th.Name {
		case "work/a":
			if th.Mode&0o7000 != 0 {
				t.Errorf("work/a mode %04o keeps a special bit", th.Mode)
			}
		case "work/l":
			if th.Typeflag != tar.TypeSymlink || th.Linkname != "/etc" {
				t.Errorf("work/l = type %q link %q; want the symlink itself", th.Typeflag, th.Linkname)
			}
		}
	}

	// A stream naming a path outside its roots: refused, and the sandbox the import created is gone.
	var hostile bytes.Buffer
	tw := tar.NewWriter(&hostile)
	_ = tw.WriteHeader(&tar.Header{Name: "etc/evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, Format: tar.FormatPAX})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	spec := moveSpec(false)
	if _, _, err := d.ImportSandbox(ctx, spec, set, &hostile); !errors.Is(err, movepack.ErrArchiveRefused) || !errors.Is(err, runner.ErrMoveFiles) {
		t.Fatalf("import of an entry outside the roots = %v; want ErrArchiveRefused at the files stage", err)
	}
	if _, err := cli.ContainerInspect(ctx, agentContainerName(spec.RunID), client.ContainerInspectOptions{}); !isNotFound(err) {
		t.Errorf("the refused import's container: inspect = %v; want it removed", err)
	}
}

// TestMoveImport_CreatedReadsPresent: the imported sandbox is created and not started, and Status reads it
// RunStarting, never RunStopped with no exit code, so the runner's observer reports the moved run present.
func TestMoveImport_CreatedReadsPresent(t *testing.T) {
	skipWithoutMoveDocker(t)
	d := newNetworkTestDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	set := runner.CarriedSet{Roots: []string{"/work"}}
	var stream bytes.Buffer
	w, err := movepack.NewWriter(&stream, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add(movepack.Header{Path: "work", Type: movepack.TypeDir, Mode: 0o755}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	sb, _, err := d.ImportSandbox(ctx, moveSpec(false), set, &stream)
	if err != nil {
		t.Fatalf("ImportSandbox: %v", err)
	}
	t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })
	st, err := d.Status(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != types.RunStarting || st.ExitCode != nil {
		t.Errorf("Status = %+v; want RunStarting with no exit code", st)
	}
	if _, _, err := d.ImportSandbox(ctx, moveSpec(true), set, bytes.NewReader(nil)); !errors.Is(err, runner.ErrMoveCreate) {
		t.Errorf("import of a spec with proxy config = %v; want ErrMoveCreate", err)
	}
}
