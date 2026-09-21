// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// managedFileEpoch is the mtime every archive entry carries. Fixed, so the
// same managed files always produce byte-identical bytes — an archive whose
// content depends on the clock cannot be diffed in a test.
var managedFileEpoch = time.Unix(0, 0).UTC()

// deliverManagedFiles copies spec.ManagedFiles into containerID as a
// ROOT-OWNED archive. It must be called BETWEEN ContainerCreate and
// ContainerStart, and both call sites (CreateSandbox for the exec-capable
// runtimes, runAsMainProcess for the exec-less ones) do exactly that.
//
// WHY THERE AND NOWHERE ELSE. The container's filesystem exists the moment it
// is created, and the daemon extracts into it as ROOT regardless of the
// image's USER — so this is the only window in which Wardyn can put a file the
// agent cannot modify into a sandbox without racing the agent itself. After
// start, the equivalent is a one-shot root exec, which the main process is
// already running against; before create there is no filesystem to write to.
//
// WHY THE OWNERSHIP SURVIVES. Every header goes out with Uid/Gid 0 and
// CopyToContainerOptions.CopyUIDGID stays FALSE. That flag does not mean "use
// the archive's ids" — it makes the daemon chown the extracted tree to the
// IMAGE'S USER (uid 1000 for every Wardyn agent image), which is precisely the
// agent-writable outcome runner.ManagedFile exists to rule out. Left false,
// the daemon honours the header ids and the file lands root:root.
func (d *Driver) deliverManagedFiles(ctx context.Context, containerID string, files []runner.ManagedFile) error {
	if len(files) == 0 {
		return nil
	}
	archive, err := managedFilesTar(files)
	if err != nil {
		return fmt.Errorf("docker: build managed-file archive: %w", err)
	}
	if _, err := d.cli.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{
		DestinationPath: "/",
		Content:         archive,
		// CopyUIDGID false — see the doc comment. This is the security-critical
		// line in this file.
		CopyUIDGID: false,
	}); err != nil {
		return fmt.Errorf("docker: deliver managed files: %w", err)
	}
	return nil
}

// managedFilesTar builds the archive deliverManagedFiles extracts at "/":
// every parent directory root-owned 0755, every file root-owned at its own
// mode, all with numeric uid/gid 0 rather than a user NAME the daemon would
// have to resolve against the image's /etc/passwd.
//
// The directory chain deliberately STOPS SHORT of the top level: for
// /etc/wardyn/agent/settings.json it emits etc/wardyn/ and etc/wardyn/agent/,
// never etc/. Extraction applies a directory header's ownership to a directory
// that already exists, so emitting the top level would re-own the image's own
// /etc (or /home, or /usr) on the way past — a change nothing asked for, on a
// tree the image already got right. runner.ValidateManagedFiles' two-deep rule
// is what guarantees there is always at least one directory below it to own.
func managedFilesTar(files []runner.ManagedFile) (*bytes.Buffer, error) {
	if err := runner.ValidateManagedFiles(files); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	done := map[string]bool{}
	for _, f := range files {
		for _, dir := range managedFileDirChain(f.Path) {
			if done[dir] {
				continue
			}
			done[dir] = true
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     strings.TrimPrefix(dir, "/") + "/",
				Mode:     int64(runner.ManagedFileDirMode.Perm()),
				Uid:      0,
				Gid:      0,
				ModTime:  managedFileEpoch,
			}); err != nil {
				return nil, err
			}
		}
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     strings.TrimPrefix(f.Path, "/"),
			Mode:     int64(f.FileMode().Perm()),
			Size:     int64(len(f.Content)),
			Uid:      0,
			Gid:      0,
			ModTime:  managedFileEpoch,
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.Content); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

// managedFileDirChain lists the directories on p that the archive owns,
// outermost first and excluding the top level: /a/b/c/d yields /a/b and /a/b/c.
// A path shorter than that cannot occur — ValidateManagedFiles refuses it.
func managedFileDirChain(p string) []string {
	segs := strings.Split(strings.Trim(path.Dir(p), "/"), "/")
	dirs := make([]string, 0, len(segs))
	for i := 2; i <= len(segs); i++ {
		dirs = append(dirs, "/"+strings.Join(segs[:i], "/"))
	}
	return dirs
}

// preflightSpec refuses everything about spec that can be refused for free —
// before the daemon holds a single object for this run. A refusal here leaves
// the host exactly as it found it, which is worth more than the one branch it
// costs CreateSandbox: the rollback path is the hardest thing in that function
// to get right, and this is work it never has to undo.
func (d *Driver) preflightSpec(spec runner.SandboxSpec) error {
	if d.cfg.ProxyImage == "" {
		return errProxyImageUnset
	}
	// An unusable managed-file path, or a mode the agent could write, must
	// refuse the run outright rather than surface at the copy — by then there
	// is a network and a sidecar for the rollback to find.
	if err := runner.ValidateManagedFiles(spec.ManagedFiles); err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	return nil
}

// deliverManagedFilesAndStart is ONE function on purpose: the ordering is the
// contract. A managed file placed after the start races the agent's own first
// instruction, and two statements side by side in a 200-line assembly sequence
// are two statements someone reorders. Fail closed on either half — a run
// promised a root-owned ceiling that did not get one must not start, because
// nothing downstream can tell that apart from a file the agent has not touched
// yet.
func (d *Driver) deliverManagedFilesAndStart(ctx context.Context, containerID string, files []runner.ManagedFile) error {
	if err := d.deliverManagedFiles(ctx, containerID, files); err != nil {
		return err
	}
	if _, err := d.cli.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("docker: start agent: %w", err)
	}
	return nil
}

// deliverManagedFilesOrReap is deliverManagedFilesAndStart's exec-less twin,
// minus the start: runAsMainProcess still has its own start to do, and the
// bookkeeping a failure needs here is different — the ref is CLAIMED in
// d.creating and the container must be removed, because on this path the
// container's MAIN process IS the agent workload and there is no later window
// to deliver in at all. A started agent without its ceiling is the outcome the
// whole field exists to prevent, so this fails closed and reaps.
func (d *Driver) deliverManagedFilesOrReap(ctx context.Context, ref, containerID string, files []runner.ManagedFile) error {
	mfErr := d.deliverManagedFiles(ctx, containerID, files)
	if mfErr == nil {
		return nil
	}
	d.mu.Lock()
	delete(d.creating, ref)
	d.mu.Unlock()
	// Not ctx: whatever failed the delivery may have cancelled it, and the
	// removal must still happen (the same reasoning the teardown race below
	// runAsMainProcess's start spells out).
	rmCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if _, rerr := d.cli.ContainerRemove(rmCtx, containerID, client.ContainerRemoveOptions{Force: true}); rerr != nil && !isNotFound(rerr) {
		return fmt.Errorf("docker: managed-file delivery failed and the container could not be removed: %w (delivery error: %v)", rerr, mfErr)
	}
	return mfErr
}
