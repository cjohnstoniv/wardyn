// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
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
//
// IT NEVER DELIVERS INTO A DIRECTORY THAT ALREADY EXISTS. The daemon creates a
// missing directory root-owned 0755; one that is already there — shipped by
// the image, or a bind mount, which the daemon mounts for the copy — is
// refused. The stat carries a mode but no owner, so an existing directory
// cannot be told apart from one the agent owns (which makes the file
// replaceable whatever its own mode), and a bind-mounted one is a HOST
// directory the delivery would write into.
func (d *Driver) deliverManagedFiles(ctx context.Context, containerID string, files []runner.ManagedFile) error {
	if len(files) == 0 {
		return nil
	}
	for _, dir := range runner.ManagedFileDirs(files) {
		_, err := d.cli.ContainerStatPath(ctx, containerID, client.ContainerStatPathOptions{Path: dir})
		if err == nil {
			return fmt.Errorf("docker: managed file directory %s already exists in the container (shipped by the image or mounted there); a managed file is delivered only into a directory the delivery creates, never into one it would have to trust or re-own", dir)
		}
		if !isNotFound(err) {
			return fmt.Errorf("docker: stat managed file directory %s: %w", dir, err)
		}
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

// managedFilesTar builds the archive deliverManagedFiles extracts at "/": one
// entry per file, root-owned at its own mode, with numeric uid/gid 0 rather
// than a user NAME the daemon would have to resolve against the image's
// /etc/passwd.
//
// It carries NO directory entries. The daemon applies a directory entry's
// owner and mode to a directory that already exists — an earlier version of
// this archive re-owned a host bind-mount source to root that way — whereas a
// file whose directory is missing gets that directory created root-owned 0755
// and every existing ancestor left exactly as it was.
func managedFilesTar(files []runner.ManagedFile) (*bytes.Buffer, error) {
	if err := runner.ValidateManagedFiles(files); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
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
