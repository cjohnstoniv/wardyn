// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
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
//
// NOR INTO AN IMAGE THAT COULD UNDO IT. See checkManagedFileImage.
func (d *Driver) deliverManagedFiles(ctx context.Context, containerID string, files []runner.ManagedFile) error {
	if len(files) == 0 {
		return nil
	}
	if err := d.checkManagedFileImage(ctx, containerID); err != nil {
		return err
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

// checkManagedFileImage refuses a container whose image would let the agent
// replace a root-owned file in runner.ManagedFileDir. That directory holds
// only because the agent can neither write /etc nor act as its owner, and on
// this substrate both depend on the image: the workload runs as the image's
// USER, and /etc is the image's own. So the USER must resolve to a non-root
// uid, and /etc must be a directory owned by root and not writable by group or
// others. (Kubernetes runs the agent as uid 1000 on a read-only mount point
// whatever the image says.)
//
// Everything is read from the created container through the archive API,
// never by exec: nothing may run in it before the managed files are in place.
func (d *Driver) checkManagedFileImage(ctx context.Context, containerID string) error {
	insp, err := d.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("docker: inspect for managed files: %w", err)
	}
	var user string
	if insp.Container.Config != nil {
		user = insp.Container.Config.User
	}
	uid, err := d.managedFileUID(ctx, containerID, user)
	if err != nil {
		return err
	}
	if uid == 0 {
		return fmt.Errorf("docker: managed files need an image whose USER is a non-root user; this image (USER %q) runs its workload as root, which owns /etc and may rename %s aside and replace the file", user, runner.ManagedFileDir)
	}
	etc, _, err := d.firstArchiveEntry(ctx, containerID, "/etc", 0)
	if err != nil {
		return fmt.Errorf("docker: read /etc for managed files: %w", err)
	}
	if etc.Typeflag != tar.TypeDir || etc.Uid != 0 || etc.Mode&0o022 != 0 {
		what := fmt.Sprintf("owned by uid %d with mode %04o", etc.Uid, etc.Mode&0o7777)
		if etc.Typeflag != tar.TypeDir {
			what = "not a directory"
		}
		return fmt.Errorf("docker: managed files need an image whose /etc is a directory owned by root and not writable by group or others; this image's /etc is %s, so the workload may rename %s aside and replace the file", what, runner.ManagedFileDir)
	}
	return nil
}

// managedFileUID is the uid a workload runs as under USER user: numeric as
// written, or the name's uid in the container's own /etc/passwd — the file
// the runtime resolves it against at start. No USER at all is root.
func (d *Driver) managedFileUID(ctx context.Context, containerID, user string) (uint64, error) {
	name, _, _ := strings.Cut(user, ":")
	if name == "" {
		return 0, nil
	}
	if uid, err := strconv.ParseUint(name, 10, 32); err == nil {
		return uid, nil
	}
	_, passwd, err := d.firstArchiveEntry(ctx, containerID, "/etc/passwd", 1<<20)
	if err != nil {
		return 0, fmt.Errorf("docker: managed files need an image whose USER is a non-root user; USER %q is a name, and the image's /etc/passwd could not be read to resolve it: %w", user, err)
	}
	for _, line := range strings.Split(string(passwd), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 3 || f[0] != name {
			continue
		}
		uid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("docker: managed files need an image whose USER is a non-root user; USER %q has uid %q in the image's /etc/passwd, which is not a number", user, f[2])
		}
		return uid, nil
	}
	return 0, fmt.Errorf("docker: managed files need an image whose USER is a non-root user; USER %q is neither a number nor a name in the image's /etc/passwd, so the uid it runs as cannot be checked", user)
}

// firstArchiveEntry returns the header of the first entry of the archive the
// daemon streams for p — p itself — and up to limit bytes of its content.
func (d *Driver) firstArchiveEntry(ctx context.Context, containerID, p string, limit int64) (*tar.Header, []byte, error) {
	res, err := d.cli.CopyFromContainer(ctx, containerID, client.CopyFromContainerOptions{SourcePath: p})
	if err != nil {
		return nil, nil, err
	}
	defer res.Content.Close()
	tr := tar.NewReader(res.Content)
	hdr, err := tr.Next()
	if err != nil {
		return nil, nil, err
	}
	body, err := io.ReadAll(io.LimitReader(tr, limit))
	return hdr, body, err
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
