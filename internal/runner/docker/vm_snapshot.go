// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/vm/qemu"
)

// A Linux VM snapshot (runner.Snapshotter) is QEMU's backup job copying the run's disk, from the moment of the
// cut, into a new file in the run's disk/ part that no running disk backs onto. The source VM's chain is never
// changed and its memory never saved: the only commands sent are qemu.SnapshotCommands. The copy stays pending
// in this process until the caller commits it (the job ends, a fork's copy is flattened onto the image's base,
// and the closed file moves to StateDir/snapshots/<id>/layer.qcow2) or aborts it. A pending copy never
// outlives this process: a restart cancels what it finds before it deletes the file (SweepSnapshotTargets).

// The snapshot timings, variables so the tests can shorten them.
var (
	// snapshotSelfAbortAfter is how long a cut copy waits for its Commit or Abort before it aborts itself.
	snapshotSelfAbortAfter = 120 * time.Second
	// snapshotCommitWait bounds a Commit from the cut's end: past it the copy is cancelled and deleted.
	snapshotCommitWait = 9 * time.Minute
	// snapshotCancelWait bounds a cancelled copy's wait for its job to end.
	snapshotCancelWait = 5 * time.Second
	// snapshotPoll is how often a copy's job, or a QEMU waited on, is looked at again.
	snapshotPoll = time.Second
	// snapshotFreeMiB is the space free to the runner under a path.
	snapshotFreeMiB = FreeDiskMiB
	// snapshotBeforeAbort runs between an Abort's or a Delete's read of a pending id and its abort; a test seam.
	snapshotBeforeAbort = func(uuid.UUID) {}
	// snapshotUnlinked runs after a copy target an earlier process left is deleted; a test seam.
	snapshotUnlinked = func(string) {}
)

// forkRecordGrace is how long a fork's record counts as a run backing onto its layer before the fork's run
// directory exists: vmBase records the fork just before createVM makes that directory.
const forkRecordGrace = time.Minute

// forkEnded is the content of a fork's record once its run's teardown has begun: no grace, only its run
// directory keeps the layer in use.
const forkEnded = "ended"

// snapshotHeadroom is the free disk a snapshot leaves beyond what its copy and a fork's flatten may take.
const snapshotHeadroom = 1 << 30

// forksDir, beside a layer, holds one empty file per fork booted on it, named by the fork's run id, so
// DeleteSnapshot can see that a run on this runner still backs onto it.
const forksDir = "forks"

// layerMeta, beside a committed layer, is its SnapshotLayer.
const layerMeta = "layer.json"

// snapTable is one driver's snapshots in this process: the pending copies, the ids aborted or deleted while
// pending (tombstones, which refuse a later Commit), the layers committed (so a repeated Commit answers the
// same layer), and the runs with a copy under way (one at a time per run).
type snapTable struct {
	mu        sync.Mutex
	pending   map[uuid.UUID]*pendingSnap
	tombs     map[uuid.UUID]bool
	committed map[uuid.UUID]runner.SnapshotLayer
	runs      map[uuid.UUID]bool
}

// snapTables holds each driver's snapTable for the life of the process.
var snapTables sync.Map

func (d *Driver) snaps() *snapTable {
	t, _ := snapTables.LoadOrStore(d, &snapTable{pending: map[uuid.UUID]*pendingSnap{}, tombs: map[uuid.UUID]bool{},
		committed: map[uuid.UUID]runner.SnapshotLayer{}, runs: map[uuid.UUID]bool{}})
	return t.(*snapTable)
}

type pendingSnap struct {
	id, runID uuid.UUID
	ref, dir  string
	base      string    // the image cache entry the run's disk backs onto
	layer     string    // the layer directory a fork's disk backs onto; "" for a plain run
	parent    uuid.UUID // that layer's snapshot id
	imageID   string
	digest    string
	stamp     string
	cutStart  time.Time
	cutEnd    time.Time
	timer     *time.Timer
	// commit and done are set while a Commit runs: commit cancels it, and done is closed once it is over.
	commit context.CancelFunc
	done   chan struct{}
}

func (p *pendingSnap) target() string { return filepath.Join(p.dir, "disk", qemu.SnapshotTarget(p.id)) }

func (d *Driver) layerDir(id uuid.UUID) string {
	return filepath.Join(d.vmConfig().StateDir, "snapshots", id.String())
}

// SnapshotSandbox cuts a snapshot of the Linux VM ref and returns at the cut's end, with the copy running.
func (d *Driver) SnapshotSandbox(ctx context.Context, ref string, req runner.SnapshotRequest) (runner.SnapshotCut, error) {
	v, err := d.vmLookup(ctx, ref)
	if err != nil {
		return runner.SnapshotCut{}, err
	}
	if d.vmConfig() == nil || v == nil || v.mac || v.win != nil || req.ID == uuid.Nil {
		return runner.SnapshotCut{}, runner.ErrSnapshotUnsupported
	}
	t := d.snaps()
	t.mu.Lock()
	_, done := t.committed[req.ID]
	switch {
	case t.runs[v.runID]:
		t.mu.Unlock()
		return runner.SnapshotCut{}, runner.ErrSnapshotBusy
	case done || t.pending[req.ID] != nil || t.tombs[req.ID]:
		t.mu.Unlock()
		return runner.SnapshotCut{}, fmt.Errorf("docker: snapshot id %s is already used on this runner", req.ID)
	}
	t.runs[v.runID] = true
	t.mu.Unlock()
	p, err := d.cutSnapshot(ctx, ref, v, req.ID)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		delete(t.runs, v.runID)
		return runner.SnapshotCut{}, err
	}
	t.pending[p.id] = p
	p.timer = time.AfterFunc(snapshotSelfAbortAfter, func() { d.abortPending(p.id) })
	return runner.SnapshotCut{ID: p.id, CutStart: p.cutStart, CutEnd: p.cutEnd}, nil
}

// cutSnapshot makes the empty target, freezes the guest's filesystems, starts the copy and thaws them.
func (d *Driver) cutSnapshot(ctx context.Context, ref string, v *vmRun, id uuid.UUID) (*pendingSnap, error) {
	vc := d.vmConfig()
	if err := d.clearLeftovers(ctx, v.runID, false); err != nil {
		return nil, err
	}
	base, layer, err := d.vmDisks(ctx, ref)
	if err != nil {
		return nil, err
	}
	p := &pendingSnap{id: id, runID: v.runID, ref: ref, dir: v.dir, base: base, layer: layer, imageID: "sha256:" + filepath.Base(base)}
	if layer != "" {
		if p.parent, err = uuid.Parse(filepath.Base(layer)); err != nil {
			return nil, fmt.Errorf("docker: the VM's layer mount %s names no snapshot", layer)
		}
	}
	if p.stamp, err = baseStamp(filepath.Join(base, qemu.RootFS)); err != nil {
		return nil, err
	}
	p.digest = d.imageDigest(ctx, p.imageID)
	need, err := snapshotNeeds(filepath.Join(v.dir, "disk", qemu.Overlay), layer)
	if err != nil {
		return nil, err
	}
	if free := snapshotFreeMiB(vc.StateDir) << 20; free < need {
		return nil, fmt.Errorf("%w: it needs %d MiB, and %d MiB is free", runner.ErrSnapshotNoSpace, need>>20, free>>20)
	}
	p.cutStart = time.Now()
	if err := d.runOneShot(ctx, v.runID, "snap", qemu.SnapshotTargetArgs(id, layer != ""), p.mounts()); err != nil {
		return nil, err
	}
	// Before the backup is asked for, no job can write to the target.
	fail := func(err error) (*pendingSnap, error) {
		_ = os.Remove(p.target())
		return nil, err
	}
	q, err := d.vmQMP(ctx, v.dir)
	if err != nil {
		return fail(err)
	}
	defer q.Close()
	dev, err := q.DiskDevice(ctx)
	if err != nil {
		return fail(err)
	}
	thaw := d.freezeGuest(ctx, v.runID, v.dir)
	err = q.StartBackup(ctx, dev, id)
	thaw()
	if err != nil {
		// A backup that timed out on this side may have started: the target goes only once its job is seen ended.
		// QEMU's QMP socket takes one client at a time, so this one closes before dropCopy dials its own.
		_ = q.Close()
		d.dropCopy(v.dir, id)
		return nil, err
	}
	p.cutEnd = time.Now()
	return p, nil
}

// mounts are the one-shot steps' mounts: the base, the run's disk part, and a fork's layer, read-only.
func (p *pendingSnap) mounts() []mount.Mount {
	m := []mount.Mount{{Type: mount.TypeBind, Source: p.base, Target: qemu.BaseDir, ReadOnly: true},
		{Type: mount.TypeBind, Source: filepath.Join(p.dir, "disk"), Target: qemu.DiskDir}}
	if p.layer != "" {
		m = append(m, mount.Mount{Type: mount.TypeBind, Source: p.layer, Target: qemu.SnapDir, ReadOnly: true})
	}
	return m
}

// snapshotNeeds is the free disk a snapshot takes at most: the overlay's allocated size (the copy), a fork's
// parent layer (the flatten copies its clusters in), and the headroom.
func snapshotNeeds(overlay, layerDir string) (int64, error) {
	fi, err := os.Stat(overlay)
	if err != nil {
		return 0, fmt.Errorf("docker: the VM's disk: %w", err)
	}
	need := fi.Size()
	if st, ok := statT(fi); ok {
		need = st.Blocks * 512
	}
	if layerDir != "" {
		lfi, err := os.Stat(filepath.Join(layerDir, qemu.SnapLayer))
		if err != nil {
			return 0, fmt.Errorf("docker: the VM's snapshot layer: %w", err)
		}
		need += lfi.Size()
	}
	return need + snapshotHeadroom, nil
}

// vmDisks are the sources the QEMU container ref mounts at the base and, for a fork, the layer.
func (d *Driver) vmDisks(ctx context.Context, ref string) (base, layer string, err error) {
	res, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		return "", "", fmt.Errorf("docker: inspect the VM: %w", err)
	}
	for _, m := range res.Container.Mounts {
		switch m.Destination {
		case qemu.BaseDir:
			base = m.Source
		case qemu.SnapDir:
			layer = m.Source
		}
	}
	if base == "" {
		return "", "", errors.New("docker: the VM mounts no image base")
	}
	return base, layer, nil
}

// baseStamp is the base file's inode, size, mtime and ctime. A conversion of the same image again writes a
// new file, so its stamp differs; the device number is left out, as a remount can change it.
func baseStamp(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", fmt.Errorf("docker: the VM image's base: %w", err)
	}
	return fmt.Sprintf("ino=%d size=%d mtime=%d.%09d ctime=%d.%09d", st.Ino, st.Size, st.Mtim.Sec, st.Mtim.Nsec, st.Ctim.Sec, st.Ctim.Nsec), nil
}

// imageDigest is the repository digest of the image id, or "" when it has none or several.
func (d *Driver) imageDigest(ctx context.Context, imageID string) string {
	res, err := d.cli.ImageInspect(ctx, imageID)
	if err != nil {
		return ""
	}
	digest := ""
	for _, rd := range res.RepoDigests {
		_, dg, ok := strings.Cut(rd, "@")
		if !ok || (digest != "" && dg != digest) {
			return ""
		}
		digest = dg
	}
	return digest
}

func (d *Driver) vmQMP(ctx context.Context, dir string) (*qemu.QMP, error) {
	conn, err := d.dialUnix(ctx, filepath.Join(dir, "ctl", qemu.QMPSocket))
	if err != nil {
		return nil, fmt.Errorf("docker: the VM's QMP: %w", err)
	}
	q, err := qemu.NewQMP(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return q, nil
}

// freezeGuest freezes the guest's filesystems through qemu-ga and returns the thaw. A guest that cannot freeze
// still gets its copy, crash-consistent as a power cut would leave the disk.
func (d *Driver) freezeGuest(ctx context.Context, runID uuid.UUID, dir string) func() {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := d.dialUnix(ctx, filepath.Join(dir, "ctl", qemu.QGASocket))
	if err != nil {
		cancel()
		return func() {}
	}
	g, err := qemu.NewQGA(ctx, conn)
	if err != nil {
		_ = conn.Close()
		cancel()
		return func() {}
	}
	if err := g.Execute(ctx, "guest-fsfreeze-freeze", nil, nil); err != nil {
		slog.Warn("docker: the VM's filesystems were not frozen for its snapshot", slog.String("err", err.Error()))
	}
	return func() {
		defer cancel()
		// The thaw gets its own bound: a guest left frozen hangs every write in it. A failed thaw is tried once
		// more on a new connection; the snapshot answers either way, and the log names the run.
		tctx, tcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err := g.Execute(tctx, "guest-fsfreeze-thaw", nil, nil)
		tcancel()
		_ = g.Close()
		if err == nil {
			return
		}
		slog.Warn("docker: the VM's filesystems did not thaw after its snapshot; trying once more",
			slog.String("run_id", runID.String()), slog.String("err", err.Error()))
		if err := d.thawGuest(dir); err != nil {
			slog.Error("docker: the VM's filesystems may still be frozen after its snapshot",
				slog.String("run_id", runID.String()), slog.String("err", err.Error()))
		}
	}
}

// thawGuest thaws the guest's filesystems over a new qemu-ga connection, bounded at 10 seconds.
func (d *Driver) thawGuest(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := d.dialUnix(ctx, filepath.Join(dir, "ctl", qemu.QGASocket))
	if err != nil {
		return err
	}
	g, err := qemu.NewQGA(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer g.Close()
	return g.Execute(ctx, "guest-fsfreeze-thaw", nil, nil)
}

// CommitSnapshot finishes the pending copy id and keeps it as the snapshot's layer.
func (d *Driver) CommitSnapshot(ctx context.Context, ref string, id uuid.UUID) (runner.SnapshotLayer, error) {
	t := d.snaps()
	t.mu.Lock()
	if l, ok := t.committed[id]; ok {
		t.mu.Unlock()
		return l, nil
	}
	p := t.pending[id]
	if p == nil || t.tombs[id] || p.ref != ref {
		t.mu.Unlock()
		return runner.SnapshotLayer{}, runner.ErrSnapshotNotPending
	}
	if p.done != nil { // a repeat while the first Commit runs: its answer is this one's
		done := p.done
		t.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return runner.SnapshotLayer{}, ctx.Err()
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if l, ok := t.committed[id]; ok {
			return l, nil
		}
		return runner.SnapshotLayer{}, runner.ErrSnapshotNotPending
	}
	p.timer.Stop()
	// The Commit runs to its own bound, not the caller's: a call lost on the wire is asked again and waits.
	cctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), p.cutEnd.Add(snapshotCommitWait))
	p.commit, p.done = cancel, make(chan struct{})
	t.mu.Unlock()
	defer cancel()
	return d.commit(cctx, p)
}

func (d *Driver) commit(ctx context.Context, p *pendingSnap) (runner.SnapshotLayer, error) {
	defer close(p.done)
	err := d.finishCopy(ctx, p)
	if err == nil && p.parent != uuid.Nil {
		err = d.runOneShot(ctx, p.runID, "flatten", qemu.FlattenArgs(p.id), p.mounts())
	}
	var l runner.SnapshotLayer
	if err == nil {
		l, err = p.layerInfo()
	}
	t := d.snaps()
	t.mu.Lock()
	switch {
	case err != nil:
	case t.tombs[p.id] || ctx.Err() != nil:
		err = errors.New("aborted while it was being committed")
	default:
		// Under the table's lock, so a delete either tombstoned the id first or finds the layer.
		err = d.keepLayer(p.target(), l)
	}
	delete(t.pending, p.id)
	if err == nil {
		t.committed[p.id] = l
		delete(t.runs, p.runID)
		t.mu.Unlock()
		return l, nil
	}
	t.tombs[p.id] = true
	t.mu.Unlock()
	d.dropCopy(p.dir, p.id)
	t.mu.Lock()
	delete(t.runs, p.runID)
	t.mu.Unlock()
	return runner.SnapshotLayer{}, fmt.Errorf("%w: %v", runner.ErrSnapshotNotPending, err)
}

// finishCopy waits for the copy's job to end without error, then dismisses it and closes its node. It asks
// QEMU afresh each time, so the VM's QMP socket stays free between asks.
func (d *Driver) finishCopy(ctx context.Context, p *pendingSnap) error {
	for {
		q, err := d.vmQMP(ctx, p.dir)
		if err != nil {
			return err
		}
		j, err := q.SnapshotJob(ctx, p.id)
		switch {
		case err != nil:
		case !j.Found:
			err = errors.New("docker: the snapshot's copy job is gone")
		case j.Ended() && j.Error != "":
			err = fmt.Errorf("docker: the snapshot's copy failed: %.200s", j.Error)
		case j.Ended():
			err = q.CloseSnapshot(ctx, p.id)
			_ = q.Close()
			return err
		}
		_ = q.Close()
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(snapshotPoll):
		}
	}
}

// layerInfo is the closed target's size, digest and provenance.
func (p *pendingSnap) layerInfo() (runner.SnapshotLayer, error) {
	f, err := os.Open(p.target())
	if err != nil {
		return runner.SnapshotLayer{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return runner.SnapshotLayer{}, err
	}
	return runner.SnapshotLayer{ID: p.id, Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil)), BaseStamp: p.stamp, ImageID: p.imageID, ImageDigest: p.digest}, nil
}

// keepLayer moves the closed target, which nothing backs onto yet, into StateDir/snapshots/<id> beside its
// metadata: 0400 and the VM user's, in a directory that appears whole or not at all.
func (d *Driver) keepLayer(target string, l runner.SnapshotLayer) error {
	vc := d.vmConfig()
	root := filepath.Join(vc.StateDir, "snapshots")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(root, l.ID.String()+".tmp-")
	if err != nil {
		return err
	}
	layer, meta := filepath.Join(tmp, qemu.SnapLayer), filepath.Join(tmp, layerMeta)
	b, _ := json.Marshal(l)
	err = os.Rename(target, layer)
	if err == nil {
		err = os.Chmod(layer, 0o400)
	}
	if err == nil {
		err = os.WriteFile(meta, b, 0o400)
	}
	if err == nil {
		err = vc.chown(tmp, layer, meta)
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(root, l.ID.String()))
	}
	if err != nil {
		_ = os.RemoveAll(tmp)
	}
	return err
}

// AbortSnapshot cancels and deletes the pending copy id. A Commit that won the race has kept the layer, which
// the abort then deletes, as the caller no longer wants it.
func (d *Driver) AbortSnapshot(ctx context.Context, ref string, id uuid.UUID) error {
	t := d.snaps()
	t.mu.Lock()
	p := t.pending[id]
	t.mu.Unlock()
	if p != nil {
		if p.ref != ref {
			return runner.ErrSnapshotNotPending
		}
		snapshotBeforeAbort(id)
		if d.abortPending(id) {
			return nil
		}
	}
	return d.dropLayer(id, false)
}

// abortPending tombstones the pending copy id, then cancels it: a running Commit is stopped and waited for,
// which drops the copy; otherwise the copy is dropped here (dropCopy). It reports false when id is not pending,
// a Commit having kept it or an abort having dropped it already.
func (d *Driver) abortPending(id uuid.UUID) bool {
	t := d.snaps()
	t.mu.Lock()
	p := t.pending[id]
	if p == nil {
		t.mu.Unlock()
		return false
	}
	t.tombs[id] = true
	if p.done != nil {
		cancel, done := p.commit, p.done
		t.mu.Unlock()
		cancel()
		<-done
		return true
	}
	delete(t.pending, id)
	p.timer.Stop()
	t.mu.Unlock()
	d.dropCopy(p.dir, p.id)
	t.mu.Lock()
	delete(t.runs, p.runID)
	t.mu.Unlock()
	return true
}

// dropCopy cancels the copy id (job-cancel, a bounded wait for the job, then its node closed) and deletes its
// target only once the job is seen ended. A target deleted while its job may still run is written to by QEMU
// after it is gone, and its space is held until that QEMU exits, out of every sweep's sight. A target whose
// job did not end in the bound is left for clearLeftovers (the run's next snapshot and its teardown) and the
// start sweep, which delete it only after the job has ended or its QEMU has exited. The VM is never stopped.
func (d *Driver) dropCopy(dir string, id uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), snapshotCancelWait)
	defer cancel()
	if d.cancelCopy(ctx, dir, id) {
		_ = os.Remove(filepath.Join(dir, "disk", qemu.SnapshotTarget(id)))
	}
}

// cancelCopy cancels the copy id and reports whether its job has ended. Every failure answers false.
func (d *Driver) cancelCopy(ctx context.Context, dir string, id uuid.UUID) bool {
	q, err := d.vmQMP(ctx, dir)
	if err != nil {
		return false
	}
	defer q.Close()
	_ = q.CancelSnapshotJob(ctx, id) // a job already ended refuses the cancel
	for {
		j, err := q.SnapshotJob(ctx, id)
		if err != nil {
			return false
		}
		if j.Ended() {
			_ = q.CloseSnapshot(ctx, id)
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(snapshotPoll):
		}
	}
}

// DeleteSnapshot deletes the layer id, or aborts it while it is pending. A Commit that won the race has kept
// the layer, which is then deleted as any other.
func (d *Driver) DeleteSnapshot(ctx context.Context, id uuid.UUID) error {
	t := d.snaps()
	t.mu.Lock()
	pending := t.pending[id] != nil
	t.mu.Unlock()
	if pending {
		snapshotBeforeAbort(id)
		if d.abortPending(id) {
			return nil
		}
	}
	if d.vmConfig() == nil {
		return runner.ErrSnapshotUnsupported
	}
	return d.dropLayer(id, true)
}

// dropLayer deletes the layer id under the table's lock, and tombstones id so no later Commit answers it. With
// anyLayer false (an abort), only a layer this process committed is deleted. It answers success only once the
// layer's directory is gone, and refuses while a fork on this runner backs onto it.
func (d *Driver) dropLayer(id uuid.UUID, anyLayer bool) error {
	t := d.snaps()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tombs[id] = true
	if _, ok := t.committed[id]; !ok && !anyLayer {
		return nil
	}
	if d.vmConfig() == nil {
		return runner.ErrSnapshotUnsupported
	}
	if used, err := d.layerInUse(id); err != nil || used {
		return errors.Join(runner.ErrSnapshotInUse, err)
	}
	if err := os.RemoveAll(d.layerDir(id)); err != nil {
		return err
	}
	delete(t.committed, id)
	return nil
}

// layerInUse reports whether a fork of the layer id still has its run directory on this runner, its overlay
// in it. An unreadable record answers an error, which refuses the delete.
func (d *Driver) layerInUse(id uuid.UUID) (bool, error) {
	forks, err := os.ReadDir(filepath.Join(d.layerDir(id), forksDir))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, f := range forks {
		runID, err := uuid.Parse(f.Name())
		if err != nil {
			continue
		}
		if _, err := os.Stat(d.vmConfig().runDir(runID)); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		b, err := os.ReadFile(filepath.Join(d.layerDir(id), forksDir, f.Name()))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if string(b) == forkEnded {
			continue
		}
		if fi, err := f.Info(); err == nil && time.Since(fi.ModTime()) < forkRecordGrace {
			return true, nil // recorded just now; its run directory comes next
		}
	}
	return false, nil
}

// ListSnapshots is every committed layer under StateDir/snapshots.
func (d *Driver) ListSnapshots(ctx context.Context) ([]runner.SnapshotLayer, error) {
	vc := d.vmConfig()
	if vc == nil {
		return nil, runner.ErrSnapshotUnsupported
	}
	root := filepath.Join(vc.StateDir, "snapshots")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []runner.SnapshotLayer
	for _, e := range entries {
		id, err := uuid.Parse(e.Name())
		if err != nil || id.String() != e.Name() {
			continue
		}
		l, err := readLayerMeta(filepath.Join(root, e.Name()), id)
		if err != nil {
			slog.Warn("docker: a snapshot directory with no readable record is left out of the list", slog.String("snapshot_id", id.String()), slog.String("err", err.Error()))
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// readLayerMeta is the SnapshotLayer kept in dir for the layer id.
func readLayerMeta(dir string, id uuid.UUID) (runner.SnapshotLayer, error) {
	var l runner.SnapshotLayer
	b, err := os.ReadFile(filepath.Join(dir, layerMeta))
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return l, err
	}
	if l.ID != id {
		return l, fmt.Errorf("the record names snapshot %s", l.ID)
	}
	return l, nil
}

// vmBase is the base a Linux VM run's overlay backs onto: the image's, converted on first use, or for a fork
// the cache entry its snapshot was cut on, never converted again. A fork whose image now resolves to another
// id, whose cache entry is gone, whose base file is another, or whose spec disagrees with the layer's own
// record is refused runner.ErrForkBaseChanged.
func (d *Driver) vmBase(ctx context.Context, spec runner.SandboxSpec, guestID string) (string, error) {
	src := spec.Snapshot
	if src == nil {
		return d.vmImage(ctx, spec, guestID)
	}
	id := strings.TrimPrefix(guestID, "sha256:")
	if id == "" || strings.ContainsAny(id, "/.") || id != strings.TrimPrefix(src.ImageID, "sha256:") {
		return "", fmt.Errorf("%w: the run's image is %s now, and the snapshot was taken on %s", runner.ErrForkBaseChanged, guestID, src.ImageID)
	}
	dir := d.vmConfig().imageDir(id)
	stamp, err := baseStamp(filepath.Join(dir, qemu.RootFS))
	if err != nil {
		return "", fmt.Errorf("%w: the image's converted base is gone from this runner", runner.ErrForkBaseChanged)
	}
	if stamp != src.BaseStamp {
		return "", fmt.Errorf("%w: the image's base was converted again since the snapshot", runner.ErrForkBaseChanged)
	}
	// Under the table's lock, so a delete either removed the layer first or finds the fork's record.
	t := d.snaps()
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := os.Stat(filepath.Join(d.layerDir(src.ID), qemu.SnapLayer)); err != nil {
		return "", fmt.Errorf("docker: the snapshot's layer is not on this runner: %w", err)
	}
	l, err := readLayerMeta(d.layerDir(src.ID), src.ID)
	if err != nil {
		return "", fmt.Errorf("docker: the snapshot's record: %w", err)
	}
	if l.ImageID != src.ImageID || l.BaseStamp != src.BaseStamp {
		return "", fmt.Errorf("%w: the fork names another image or base than its snapshot's record", runner.ErrForkBaseChanged)
	}
	// The fork is recorded beside the layer just before its run directory is made (forkRecordGrace).
	forks := filepath.Join(d.layerDir(src.ID), forksDir)
	if err := os.MkdirAll(forks, 0o700); err != nil {
		return "", fmt.Errorf("docker: record the fork: %w", err)
	}
	if err := os.WriteFile(filepath.Join(forks, spec.RunID.String()), nil, 0o600); err != nil {
		return "", fmt.Errorf("docker: record the fork: %w", err)
	}
	return dir, nil
}

// vmOverlayArgs is the overlay step's command: the run's overlay on the base, or a fork's on its layer.
func vmOverlayArgs(spec runner.SandboxSpec) []string {
	if spec.Snapshot == nil {
		return qemu.OverlayArgs()
	}
	return qemu.ForkOverlayArgs()
}

// vmDiskMounts are the overlay step's mounts: the base, the run's disk part and a fork's layer.
func (d *Driver) vmDiskMounts(spec runner.SandboxSpec, base, disk string) []mount.Mount {
	return append([]mount.Mount{{Type: mount.TypeBind, Source: base, Target: qemu.BaseDir, ReadOnly: true},
		{Type: mount.TypeBind, Source: disk, Target: qemu.DiskDir}}, d.vmSnapMounts(spec)...)
}

// vmSnapMounts is a fork's layer, read-only at qemu.SnapDir; nothing for any other run.
func (d *Driver) vmSnapMounts(spec runner.SandboxSpec) []mount.Mount {
	if spec.Snapshot == nil {
		return nil
	}
	return []mount.Mount{{Type: mount.TypeBind, Source: d.layerDir(spec.Snapshot.ID), Target: qemu.SnapDir, ReadOnly: true}}
}

// cancelRunSnapshots aborts every pending copy of runID and clears any copy an earlier runner process left in
// its run directory, each bounded by snapshotCancelWait. teardownVM calls it first, ignoring every failure,
// since the run directory goes next. The teardown removes the QEMU container before teardownVM runs, so the
// job-cancel normally finds that QEMU gone, and its removal is what ended the job.
func (d *Driver) cancelRunSnapshots(ctx context.Context, runID uuid.UUID) {
	t := d.snaps()
	t.mu.Lock()
	var ids []uuid.UUID
	for id, p := range t.pending {
		if p.runID == runID {
			ids = append(ids, id)
		}
	}
	t.mu.Unlock()
	for _, id := range ids {
		d.abortPending(id)
	}
	_ = d.clearLeftovers(ctx, runID, false)
	// A fork's record is marked ended, not removed: teardownVM removes the run directory after this, and a
	// removal that fails leaves the overlay there, so layerInUse keeps honouring the record while that directory
	// exists, and stops the moment it is gone.
	if vc := d.vmConfig(); vc != nil {
		recs, _ := filepath.Glob(filepath.Join(vc.StateDir, "snapshots", "*", forksDir, runID.String()))
		t.mu.Lock()
		for _, r := range recs {
			_ = os.WriteFile(r, []byte(forkEnded), 0o600)
		}
		t.mu.Unlock()
	}
}

// clearLeftovers clears every copy target in runID's disk part that this process does not hold pending. With
// wait false, a copy whose job does not end within the bound is left in place and answers ErrSnapshotBusy.
func (d *Driver) clearLeftovers(ctx context.Context, runID uuid.UUID, wait bool) error {
	vc := d.vmConfig()
	if vc == nil {
		return nil
	}
	dir := vc.runDir(runID)
	files, _ := filepath.Glob(filepath.Join(dir, "disk", "snap-*.qcow2"))
	var errs []error
	for _, f := range files {
		if !d.heldPending(f) {
			errs = append(errs, d.clearLeftover(ctx, runID, dir, f, wait))
		}
	}
	return errors.Join(errs...)
}

// heldPending reports whether the copy target file is one this process holds pending.
func (d *Driver) heldPending(file string) bool {
	id, err := uuid.Parse(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "snap-"), ".qcow2"))
	if err != nil {
		return false
	}
	t := d.snaps()
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pending[id] != nil
}

// clearLeftover deletes one copy target only once no QEMU can write to it: its run's QEMU is not running, or
// its job was cancelled and has ended. Otherwise, with wait, it waits for that QEMU to exit (the run's own
// end); the QEMU is the person's running VM and is never stopped for this.
func (d *Driver) clearLeftover(ctx context.Context, runID uuid.UUID, dir, file string, wait bool) error {
	name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "snap-"), ".qcow2")
	id, perr := uuid.Parse(name)
	if d.qemuRunning(ctx, runID) {
		cctx, cancel := context.WithTimeout(ctx, snapshotCancelWait)
		ended := perr == nil && d.cancelCopy(cctx, dir, id)
		cancel()
		if !ended {
			if !wait {
				return runner.ErrSnapshotBusy
			}
			for d.qemuRunning(ctx, runID) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(snapshotPoll):
				}
			}
		}
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	snapshotUnlinked(file)
	return nil
}

// qemuRunning reports whether runID's QEMU container may be running. An engine that cannot say answers true,
// so nothing it may write to is deleted.
func (d *Driver) qemuRunning(ctx context.Context, runID uuid.UUID) bool {
	res, err := d.cli.ContainerInspect(ctx, agentContainerName(runID), client.ContainerInspectOptions{})
	if isNotFound(err) {
		return false
	}
	return err != nil || res.Container.State == nil || res.Container.State.Running
}

// SweepSnapshotTargets runs once at wardyn-runnerd's start: a pending copy never outlives the process that cut
// it, so every copy target left in a run directory is cancelled and deleted (clearLeftover, waiting for a QEMU
// whose job will not end), and every half-kept layer directory is removed. It returns once all are gone.
func (d *Driver) SweepSnapshotTargets(ctx context.Context) {
	vc := d.vmConfig()
	if vc == nil {
		return
	}
	half, _ := filepath.Glob(filepath.Join(vc.StateDir, "snapshots", "*.tmp-*"))
	for _, h := range half {
		_ = os.RemoveAll(h)
	}
	files, _ := filepath.Glob(filepath.Join(vc.StateDir, "runs", "*", "disk", "snap-*.qcow2"))
	var wg sync.WaitGroup
	for _, f := range files {
		dir := filepath.Dir(filepath.Dir(f))
		runID, err := uuid.Parse(filepath.Base(dir))
		if err != nil || d.heldPending(f) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.clearLeftover(ctx, runID, dir, f, true); err != nil {
				slog.Warn("docker: a snapshot copy left by an earlier runner process", slog.String("file", f), slog.String("err", err.Error()))
			}
		}()
	}
	wg.Wait()
}
