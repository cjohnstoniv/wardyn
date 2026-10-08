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
	"path"
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
// ContainerStart; both call sites (CreateSandbox, runAsMainProcess) do
// exactly that.
//
// WHY THERE AND NOWHERE ELSE. The container's filesystem exists the moment
// it's created, and the daemon extracts into it as ROOT regardless of the
// image's USER — the only window to put a file the agent can't modify into a
// sandbox without racing it. After start the equivalent is a one-shot root
// exec, already busy running the main process; before create there's no
// filesystem to write to.
//
// WHY THE OWNERSHIP SURVIVES. Every header goes out with Uid/Gid 0 and
// CopyToContainerOptions.CopyUIDGID stays FALSE — that flag would instead
// make the daemon chown the tree to the IMAGE'S USER (uid 1000 for every
// Wardyn agent image), exactly the agent-writable outcome runner.ManagedFile
// exists to rule out. The one exception is an AgentOwned file (a delivered
// secret), whose header alone carries the agent's uid; see managedFilesTar.
//
// IT NEVER DELIVERS INTO A DIRECTORY THAT ALREADY EXISTS. The daemon creates
// a missing directory root-owned 0755; one already there — shipped by the
// image, or a bind mount — is refused, since the stat carries a mode but no
// owner and can't tell an agent-owned directory from a HOST one. That holds
// for every directory between the delivery's and its anchor
// (managedFileAnchors): a link there would carry the file somewhere else.
//
// NOR INTO AN IMAGE THAT COULD UNDO IT. See checkManagedFileImage.
func (d *Driver) deliverManagedFiles(ctx context.Context, containerID string, files []runner.ManagedFile) error {
	if len(files) == 0 {
		return nil
	}
	agentUID, err := d.checkManagedFileImage(ctx, containerID, files)
	if err != nil {
		return err
	}
	for _, dir := range runner.ManagedFileDirs(files) {
		for p := dir; p != managedFileAnchors[dir] && p != "/"; p = path.Dir(p) {
			_, err := d.cli.ContainerStatPath(ctx, containerID, client.ContainerStatPathOptions{Path: p})
			if err == nil {
				return fmt.Errorf("docker: managed file directory %s already exists in the container (shipped by the image or mounted there); a managed file is delivered only into a directory the delivery creates, never into one it would have to trust or re-own", p)
			}
			if !isNotFound(err) {
				return fmt.Errorf("docker: stat managed file directory %s: %w", p, err)
			}
		}
	}
	archive, err := managedFilesTar(files, agentUID)
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

// managedFileAnchors is, per directory a managed file may be delivered into,
// the image directory its safety rests on: a real directory owned by root and
// writable by nobody else, below which deliverManagedFiles creates every
// directory itself. /run may instead be absent, since the delivery then
// creates it root-owned like the rest; /etc must be there, as it always has.
var managedFileAnchors = map[string]string{
	runner.ManagedFileDir:     "/etc",
	runner.ComponentSecretDir: "/run",
}

// checkManagedFileImage refuses a container whose image would let the agent
// replace a root-owned file in runner.ManagedFileDir, or carry a delivered
// secret out of runner.ComponentSecretDir. Both hold only because the agent
// can neither write the directory's anchor (managedFileAnchors) nor act as its
// owner, and both depend on the image here: the workload runs as the image's
// USER, and the anchor is the image's own. So USER must resolve to a non-root
// uid, and each anchor must be a root-owned directory not writable by group or
// others — a link there is refused, since the delivery would follow it. It
// returns that uid: an AgentOwned file is owned by it. (Kubernetes runs the
// agent as uid 1000 on a read-only mount point regardless of the image.)
//
// Everything is read via the archive API, never by exec: nothing may run in
// the container before the managed files are in place.
func (d *Driver) checkManagedFileImage(ctx context.Context, containerID string, files []runner.ManagedFile) (uint64, error) {
	insp, err := d.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return 0, fmt.Errorf("docker: inspect for managed files: %w", err)
	}
	var user string
	if insp.Container.Config != nil {
		user = insp.Container.Config.User
	}
	uid, err := d.managedFileUID(ctx, containerID, user)
	if err != nil {
		return 0, err
	}
	dirs := runner.ManagedFileDirs(files)
	if uid == 0 {
		return 0, fmt.Errorf("docker: managed files need an image whose USER is a non-root user; this image (USER %q) runs its workload as root, which owns %s and may rename %s aside and replace the file", user, managedFileAnchors[dirs[0]], dirs[0])
	}
	for _, dir := range dirs {
		anchor, ok := managedFileAnchors[dir]
		if !ok {
			return 0, fmt.Errorf("docker: managed file directory %s has no anchor this driver checks", dir)
		}
		ent, _, err := d.firstArchiveEntry(ctx, containerID, anchor, 0)
		if err != nil {
			if anchor != "/etc" && isNotFound(err) {
				continue
			}
			return 0, fmt.Errorf("docker: read %s for managed files: %w", anchor, err)
		}
		if ent.Typeflag != tar.TypeDir || ent.Uid != 0 || ent.Mode&0o022 != 0 {
			what := fmt.Sprintf("owned by uid %d with mode %04o", ent.Uid, ent.Mode&0o7777)
			if ent.Typeflag != tar.TypeDir {
				what = "not a directory"
			}
			return 0, fmt.Errorf("docker: managed files need an image whose %s is a directory owned by root and not writable by group or others; this image's %s is %s, so the workload may rename %s aside and replace the file", anchor, anchor, what, dir)
		}
	}
	return uid, nil
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

// managedFilesTar builds the archive deliverManagedFiles extracts at "/":
// one entry per file, root-owned at its own mode, with numeric uid/gid 0
// rather than a user NAME the daemon would resolve against /etc/passwd.
//
// An AgentOwned file (a delivered secret) is the exception: its header
// carries agentUID, the uid the image's USER runs as, with gid 0 at
// runner.ComponentSecretFileMode (0400) — so on this substrate the agent's
// uid, and no other uid in the sandbox, can read it. Its directory is still
// created root-owned 0755, so the agent cannot add or swap a sibling.
//
// It carries NO directory entries. The daemon applies a directory entry's
// owner/mode to a directory that already exists — an earlier version of this
// archive re-owned a host bind-mount source to root that way — whereas a
// file whose directory is missing gets it created root-owned 0755, ancestors
// untouched.
func managedFilesTar(files []runner.ManagedFile, agentUID uint64) (*bytes.Buffer, error) {
	if err := runner.ValidateManagedFiles(files); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		uid := 0
		if f.AgentOwned {
			uid = int(agentUID)
		}
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     strings.TrimPrefix(f.Path, "/"),
			Mode:     int64(f.FileMode().Perm()),
			Size:     int64(len(f.Content)),
			Uid:      uid,
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

// preflightSpec refuses everything about spec that can be refused for free,
// before the daemon holds a single object for this run — leaving the host
// exactly as it found it, work CreateSandbox's rollback never has to undo.
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

// deliverManagedFilesAndStart is ONE function on purpose: the ordering is
// the contract. A managed file placed after start races the agent's own
// first instruction, and two statements side by side in a long assembly
// sequence are two statements someone reorders. Fail closed on either half —
// a run promised a root-owned ceiling that didn't get one must not start,
// since nothing downstream can tell that apart from an untouched file.
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
// failure bookkeeping differs — the ref is CLAIMED in d.creating and the
// container must be removed, since on this path the container's MAIN
// process IS the agent workload, with no later window to deliver in. A
// started agent without its ceiling is what this field exists to prevent,
// so this fails closed and reaps.
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
