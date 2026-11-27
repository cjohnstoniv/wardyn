// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent/guestagenttest"
	"github.com/cjohnstoniv/wardyn/internal/vm/qemu"
)

// snapQMP is a scripted QMP for the snapshot tests: it records every command and plays the copy jobs. A job
// reads "running" for runFor asks after it starts, then "concluded"; a cancel ends it at once unless stuck;
// silent accepts connections and never greets.
type snapQMP struct {
	mu        sync.Mutex
	cmds      []string
	jobs      map[string]string
	asks      map[string]int
	runFor    int
	jobErr    string
	stuck     bool
	silent    bool
	conns     []net.Conn
	beforeAns func(cmd string, args map[string]any) // outside the lock
}

func newSnapQMP() *snapQMP { return &snapQMP{jobs: map[string]string{}, asks: map[string]int{}} }

func (q *snapQMP) commands() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.cmds)
}

func (q *snapQMP) set(f func(q *snapQMP)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	f(q)
}

func (q *snapQMP) serve(t *testing.T, r *vmRig, path string) {
	ln := r.listen(t, path)
	if ln == nil {
		return
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go q.session(conn)
		}
	}()
}

func (q *snapQMP) session(conn net.Conn) {
	q.mu.Lock()
	silent := q.silent
	q.conns = append(q.conns, conn)
	q.mu.Unlock()
	defer conn.Close()
	if silent {
		_, _ = bufio.NewReader(conn).ReadByte() // until the client gives up
		return
	}
	_, _ = conn.Write([]byte(`{"QMP": {"version": {}, "capabilities": []}}` + "\n"))
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var req struct {
			Execute   string         `json:"execute"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(sc.Bytes(), &req)
		q.mu.Lock()
		q.cmds = append(q.cmds, req.Execute)
		hook := q.beforeAns
		q.mu.Unlock()
		if hook != nil {
			hook(req.Execute, req.Arguments)
		}
		q.mu.Lock()
		ret := `{}`
		id, _ := req.Arguments["id"].(string)
		switch req.Execute {
		case "query-kvm":
			ret = `{"enabled": true, "present": true}`
		case "query-block":
			ret = `[{"device": "virtio0", "inserted": {"file": "/vm/disk/overlay.qcow2", "node-name": "#block1"}}]`
		case "blockdev-backup":
			job, _ := req.Arguments["job-id"].(string)
			q.jobs[job] = "running"
		case "query-jobs":
			var out []map[string]string
			for job, st := range q.jobs {
				if st == "running" {
					if q.asks[job]++; q.asks[job] > q.runFor {
						st, q.jobs[job] = "concluded", "concluded"
					}
				}
				j := map[string]string{"id": job, "status": st}
				if st == "concluded" && q.jobErr != "" {
					j["error"] = q.jobErr
				}
				out = append(out, j)
			}
			b, _ := json.Marshal(out)
			ret = string(b)
		case "job-cancel":
			if !q.stuck {
				q.jobs[id] = "concluded"
			}
		case "job-dismiss":
			delete(q.jobs, id)
		}
		q.mu.Unlock()
		_, _ = conn.Write([]byte(`{"return": ` + ret + "}\n"))
	}
}

// snapRig is a VM rig whose QEMU answers QMP as snapQMP does and whose prep steps write what qemu-img would:
// each target or overlay records its backing ("backing=<path>"), and a flatten rewrites it. Every VM's guest
// agent keeps its record of Init's files in one directory, as a fork's copied disk carries its source's.
type snapRig struct {
	*vmRig
	qmp   *snapQMP
	disk  string
	pmu   sync.Mutex
	preps [][]string
}

func newSnapRig(t *testing.T) *snapRig {
	t.Helper()
	r := &snapRig{vmRig: newVMRig(t), qmp: newSnapQMP(), disk: t.TempDir()}
	r.f.onStart = func(c *createdContainer) { r.startSnap(t, c) }
	return r
}

func flagAfter(argv []string, flag string) string {
	if i := slices.Index(argv, flag); i >= 0 && i+1 < len(argv) {
		return argv[i+1]
	}
	return ""
}

func (r *snapRig) startSnap(t *testing.T, c *createdContainer) {
	switch {
	case strings.HasPrefix(c.name, "wardyn-vmprep-") && (strings.HasSuffix(c.name, "-snap") || strings.HasSuffix(c.name, "-flatten") || strings.HasSuffix(c.name, "-overlay")):
		argv := []string(c.cfg.Cmd)
		r.pmu.Lock()
		r.preps = append(r.preps, argv)
		r.pmu.Unlock()
		out := filepath.Join(mountSource(c, qemu.DiskDir), filepath.Base(argv[len(argv)-1]))
		if err := os.WriteFile(out, []byte("backing="+flagAfter(argv, "-b")), 0o600); err != nil {
			t.Errorf("prep %s: %v", c.name, err)
		}
	case strings.HasPrefix(c.name, agentNamePrefix) && c.cfg.Labels[labelMachineKind] == "linux_vm":
		ctl := mountSource(c, qemu.CtlDir)
		r.serveGuest(t, filepath.Join(ctl, qemu.AgentSocket))
		r.serveLines(t, filepath.Join(ctl, qemu.QGASocket), false)
		r.qmp.serve(t, r.vmRig, filepath.Join(ctl, qemu.QMPSocket))
	default:
		r.start(t, c)
	}
}

func (r *snapRig) serveGuest(t *testing.T, path string) {
	srv := &guestagent.Server{Version: "test", Home: t.TempDir(), OutDir: r.disk, MarkerDir: t.TempDir(), SignInStore: func(bool) error { return nil },
		Mount: func(guestagent.Mount) error { return nil }}
	h, err := srv.Handler()
	if err != nil {
		t.Fatal(err)
	}
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	hs := &http.Server{Handler: h, Protocols: &p, ReadHeaderTimeout: 5 * time.Second}
	if ln := r.listen(t, path); ln != nil {
		go func() { _ = hs.Serve(guestagenttest.Listener(ln)) }()
	}
}

func (r *snapRig) prepArgv() [][]string {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	return slices.Clone(r.preps)
}

// boot creates a Linux VM run, a fork when src is set.
func (r *snapRig) boot(t *testing.T, src *runner.SnapshotSource) (runner.SandboxSpec, runner.Sandbox) {
	t.Helper()
	spec := vmSpec()
	spec.RunID = uuid.New()
	spec.Snapshot = src
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return spec, sb
}

func (r *snapRig) target(runID, id uuid.UUID) string {
	return filepath.Join(r.vc.runDir(runID), "disk", qemu.SnapshotTarget(id))
}

// snapshot cuts and commits a snapshot of ref.
func (r *snapRig) snapshot(t *testing.T, ref string) runner.SnapshotLayer {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := r.d.SnapshotSandbox(ctx, ref, runner.SnapshotRequest{ID: id}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	l, err := r.d.CommitSnapshot(ctx, ref, id)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return l
}

func sourceOf(l runner.SnapshotLayer) *runner.SnapshotSource {
	return &runner.SnapshotSource{ID: l.ID, ImageID: l.ImageID, BaseStamp: l.BaseStamp}
}

// fastSnapshots shortens the snapshot timings for one test.
func fastSnapshots(t *testing.T, selfAbort, commitWait, cancelWait time.Duration) {
	t.Helper()
	sa, cw, cn, poll := snapshotSelfAbortAfter, snapshotCommitWait, snapshotCancelWait, snapshotPoll
	snapshotSelfAbortAfter, snapshotCommitWait, snapshotCancelWait, snapshotPoll = selfAbort, commitWait, cancelWait, 20*time.Millisecond
	t.Cleanup(func() {
		snapshotSelfAbortAfter, snapshotCommitWait, snapshotCancelWait, snapshotPoll = sa, cw, cn, poll
	})
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestSnapshot_NeverSavesMemoryOrChangesALiveChain: a snapshot, a fork's snapshot (flattened), an abort and a
// delete send only the snapshot commands: nothing that writes guest memory or changes a live disk's chain, and
// no one-shot rebase or commit of a run's overlay.
func TestSnapshot_NeverSavesMemoryOrChangesALiveChain(t *testing.T) {
	r := newSnapRig(t)
	ctx := context.Background()
	counts := func() (int, int) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.qmp.commands()), len(r.qga)
	}
	_, sb := r.boot(t, nil)
	q0, g0 := counts()
	l := r.snapshot(t, sb.Ref)
	q1, g1 := counts()
	_, fork := r.boot(t, sourceOf(l)) // the fork's own boot is left out of what is checked
	q2, g2 := counts()
	r.snapshot(t, fork.Ref)
	aborted := uuid.New()
	if _, err := r.d.SnapshotSandbox(ctx, sb.Ref, runner.SnapshotRequest{ID: aborted}); err != nil {
		t.Fatal(err)
	}
	if err := r.d.AbortSnapshot(ctx, sb.Ref, aborted); err != nil {
		t.Fatal(err)
	}
	if err := r.d.DeleteSnapshot(ctx, uuid.New()); err != nil {
		t.Fatalf("delete of an id the runner holds nothing for = %v", err)
	}
	allowed := append(slices.Clone(qemu.SnapshotCommands), "qmp_capabilities", "guest-sync")
	banned := []string{"migrate", "savevm", "snapshot-save", "dump-guest-memory", "human-monitor-command",
		"blockdev-snapshot-sync", "block-commit", "block-stream", "change-backing-file", "blockdev-reopen", "x-blockdev-reopen"}
	cmds := r.qmp.commands()
	r.mu.Lock()
	snapCmds := slices.Concat(cmds[q0:q1], cmds[q2:], r.qga[g0:g1], r.qga[g2:])
	r.mu.Unlock()
	for _, c := range snapCmds {
		if slices.Contains(banned, c) {
			t.Errorf("the snapshot code sent %s", c)
		}
		if !slices.Contains(allowed, c) {
			t.Errorf("the snapshot code sent %s, which is not a snapshot command", c)
		}
	}
	for _, want := range []string{"guest-fsfreeze-freeze", "guest-fsfreeze-thaw", "blockdev-add", "blockdev-backup", "job-cancel", "job-dismiss", "blockdev-del"} {
		if !slices.Contains(snapCmds, want) {
			t.Errorf("no %s among %q", want, snapCmds)
		}
	}
	for _, argv := range r.prepArgv() {
		if slices.Contains(argv, "rebase") || slices.Contains(argv, "commit") {
			if slices.Contains(argv, qemu.DiskDir+"/"+qemu.Overlay) || slices.Contains(argv, "-u") {
				t.Errorf("one-shot %q rewrites a run's overlay", argv)
			}
		}
	}
	if !slices.ContainsFunc(r.prepArgv(), func(a []string) bool { return slices.Contains(a, "rebase") }) {
		t.Error("the fork's snapshot was not flattened")
	}
}

// TestSnapshot_LayerStaysOnRunner: no snapshot frame carries bytes, and a committed layer is 0400 and, when the
// runner runs as root (the only case it changes owners), the VM user's.
func TestSnapshot_LayerStaysOnRunner(t *testing.T) {
	var walk func(rt reflect.Type, path string, seen map[reflect.Type]bool)
	walk = func(rt reflect.Type, path string, seen map[reflect.Type]bool) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array || rt.Kind() == reflect.Map {
			if rt.Kind() == reflect.Slice && rt.Elem().Kind() == reflect.Uint8 {
				t.Errorf("%s carries bytes", path)
				return
			}
			rt = rt.Elem()
		}
		if rt.Kind() == reflect.Interface {
			t.Errorf("%s can carry anything", path)
		}
		if rt.Kind() != reflect.Struct || seen[rt] || rt == reflect.TypeFor[time.Time]() {
			return
		}
		seen[rt] = true
		for i := range rt.NumField() {
			walk(rt.Field(i).Type, path+"."+rt.Field(i).Name, seen)
		}
	}
	for _, rt := range []reflect.Type{reflect.TypeFor[runnerwire.SnapshotArgs](), reflect.TypeFor[runnerwire.SnapshotReply](), reflect.TypeFor[runner.SnapshotSource]()} {
		walk(rt, rt.Name(), map[reflect.Type]bool{})
	}

	r := newSnapRig(t)
	_, sb := r.boot(t, nil)
	l := r.snapshot(t, sb.Ref)
	fi, err := os.Stat(filepath.Join(r.d.layerDir(l.ID), qemu.SnapLayer))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o400 {
		t.Fatalf("layer mode %v, want 0400", fi.Mode().Perm())
	}
	if os.Getuid() != 0 {
		t.Log("not root: the layer's owner is not checked, as the runner changes owners only as root")
		return
	}
	if st := fi.Sys().(*syscall.Stat_t); int(st.Uid) != r.vc.UID {
		t.Fatalf("layer owner %d, want the VM user %d", st.Uid, r.vc.UID)
	}
}

// TestSnapshotLayer_RunnerRefusesDeleteWhileARunBacksOnIt: the runner refuses to delete a layer while a fork on
// it still has its disk there, also after a teardown whose removal of the fork's run directory failed, and
// deletes it once that directory is gone.
func TestSnapshotLayer_RunnerRefusesDeleteWhileARunBacksOnIt(t *testing.T) {
	r := newSnapRig(t)
	ctx := context.Background()
	_, sb := r.boot(t, nil)
	l := r.snapshot(t, sb.Ref)
	forkSpec, fork := r.boot(t, sourceOf(l))
	if err := r.d.DeleteSnapshot(ctx, l.ID); !errors.Is(err, runner.ErrSnapshotInUse) {
		t.Fatalf("delete while a fork backs onto the layer = %v, want snapshot_in_use", err)
	}
	if !exists(filepath.Join(r.d.layerDir(l.ID), qemu.SnapLayer)) {
		t.Fatal("a refused delete removed the layer")
	}
	if got := runnerwire.SnapshotRefusal(r.d.DeleteSnapshot(ctx, l.ID)); got != "snapshot_in_use" {
		t.Fatalf("wire reason = %q", got)
	}
	// A folder the teardown cannot empty makes its removal of the run directory fail; the overlay stays.
	forkDir := r.vc.runDir(forkSpec.RunID)
	stuck := filepath.Join(forkDir, "stuck")
	asRoot := os.Getuid() == 0
	if !asRoot {
		if err := os.MkdirAll(stuck, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stuck, "f"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(stuck, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })
	}
	if err := r.d.KillSandbox(ctx, fork.Ref); err != nil {
		t.Fatal(err)
	}
	if asRoot {
		t.Log("root: a run directory's failed removal cannot be forced by permissions, so that case is not checked")
	} else {
		if !exists(forkDir) {
			t.Fatal("the run directory's removal did not fail as planted")
		}
		if err := r.d.DeleteSnapshot(ctx, l.ID); !errors.Is(err, runner.ErrSnapshotInUse) {
			t.Fatalf("delete while the ended fork's run directory is still there = %v, want snapshot_in_use", err)
		}
		if err := os.Chmod(stuck, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(forkDir); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.d.DeleteSnapshot(ctx, l.ID); err != nil {
		t.Fatalf("delete once the fork's disk is gone = %v", err)
	}
	if exists(r.d.layerDir(l.ID)) {
		t.Fatal("the deleted layer is still there")
	}
	if ls, err := r.d.ListSnapshots(ctx); err != nil || len(ls) != 0 {
		t.Fatalf("list after the delete = %+v, %v", ls, err)
	}
}

// chainOf follows the recorded backing references from a host path, mapping the containers' paths back.
func (r *snapRig) chainOf(t *testing.T, path string, layers map[string]string) []string {
	t.Helper()
	var chain []string
	for path != "" && len(chain) < 8 {
		chain = append(chain, filepath.Base(path))
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		next, ok := strings.CutPrefix(string(b), "backing=")
		switch {
		case !ok:
			path = ""
		case strings.HasPrefix(next, qemu.BaseDir+"/"):
			path = filepath.Join(r.vc.imageDir(vmTestImage), strings.TrimPrefix(next, qemu.BaseDir+"/"))
		case strings.HasPrefix(next, qemu.SnapDir+"/"):
			path = filepath.Join(layers[path], strings.TrimPrefix(next, qemu.SnapDir+"/"))
		default:
			t.Fatalf("%s backs onto %q", path, next)
		}
	}
	return chain
}

// TestSnapshot_LayerBacksOnlyOnBase: a plain snapshot's layer and a fork's snapshot's layer each back onto
// the image's base and nothing else: the fork's copy, cut onto its parent layer, is flattened before it is kept.
// The chain is read from the backing each step names; the live-vm ticket reads it with qemu-img.
func TestSnapshot_LayerBacksOnlyOnBase(t *testing.T) {
	r := newSnapRig(t)
	_, sb := r.boot(t, nil)
	plain := r.snapshot(t, sb.Ref)
	forkSpec, fork := r.boot(t, sourceOf(plain))
	forked := r.snapshot(t, fork.Ref)
	for _, l := range []runner.SnapshotLayer{plain, forked} {
		layer := filepath.Join(r.d.layerDir(l.ID), qemu.SnapLayer)
		if got := r.chainOf(t, layer, nil); !slices.Equal(got, []string{qemu.SnapLayer, qemu.RootFS}) {
			t.Errorf("layer %s chain = %q, want the layer and %s only", l.ID, got, qemu.RootFS)
		}
	}
	// The fork's own overlay backs onto the plain layer, which backs onto the base.
	overlay := filepath.Join(r.vc.runDir(forkSpec.RunID), "disk", qemu.Overlay)
	got := r.chainOf(t, overlay, map[string]string{overlay: r.d.layerDir(plain.ID)})
	if !slices.Equal(got, []string{qemu.Overlay, qemu.SnapLayer, qemu.RootFS}) {
		t.Errorf("the fork's overlay chain = %q", got)
	}
}

// TestSnapshotLayer_ForkRefusesAnotherBase: a fork whose image now resolves to another id, whose cache entry
// is gone, or whose base was converted again is refused fork_base_changed, and nothing is converted.
func TestSnapshotLayer_ForkRefusesAnotherBase(t *testing.T) {
	r := newSnapRig(t)
	ctx := context.Background()
	_, sb := r.boot(t, nil)
	l := r.snapshot(t, sb.Ref)
	exports := r.f.exports
	refuse := func(what string, prep func(), undo func()) {
		t.Helper()
		prep()
		defer undo()
		spec := vmSpec()
		spec.RunID = uuid.New()
		spec.Snapshot = sourceOf(l)
		_, err := r.d.CreateSandbox(ctx, spec)
		if !errors.Is(err, runner.ErrForkBaseChanged) || runnerwire.SnapshotRefusal(err) != "fork_base_changed" {
			t.Errorf("%s: create = %v, want fork_base_changed", what, err)
		}
		if r.f.exports != exports {
			t.Errorf("%s: the image was converted again for a fork", what)
		}
		if exists(r.vc.runDir(spec.RunID)) {
			t.Errorf("%s: a refused fork left its run directory", what)
		}
	}
	refuse("another image id", func() {
		r.f.mu.Lock()
		r.f.imageIDs = map[string]string{vmTestImage: "sha256:another"}
		r.f.mu.Unlock()
	}, func() {
		r.f.mu.Lock()
		r.f.imageIDs = nil
		r.f.mu.Unlock()
	})
	base := filepath.Join(r.vc.imageDir(vmTestImage), qemu.RootFS)
	aside := base + ".aside"
	refuse("a removed cache entry", func() { _ = os.Rename(base, aside) }, func() { _ = os.Rename(aside, base) })
	refuse("a re-converted base", func() {
		_ = os.Rename(base, aside)
		_ = os.WriteFile(base, []byte(qemu.RootFS), 0o600) // a new file: another inode and times
	}, func() {})
	if _, err := os.Stat(base); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotLayer_SourceEndCancelsJob: the source's teardown asks for the running copy's cancel before the VM's
// followers stop and before the run directory goes; a QMP that never answers delays the teardown by the cancel
// bound at most. In production the teardown removes the QEMU container first, so the cancel normally finds
// that QEMU gone; the fake keeps its socket open, and this test pins the call order only.
func TestSnapshotLayer_SourceEndCancelsJob(t *testing.T) {
	if snapshotCancelWait != 5*time.Second {
		t.Fatalf("the cancel bound is %s, want 5s", snapshotCancelWait)
	}
	r := newSnapRig(t)
	ctx := context.Background()
	spec, sb := r.boot(t, nil)
	id := uuid.New()
	r.qmp.set(func(q *snapQMP) { q.runFor = 1 << 30 })
	if _, err := r.d.SnapshotSandbox(ctx, sb.Ref, runner.SnapshotRequest{ID: id}); err != nil {
		t.Fatal(err)
	}
	v, _ := r.d.vms.Load(sb.Ref)
	var dirThere, followersLive, seen bool
	r.qmp.set(func(q *snapQMP) {
		q.beforeAns = func(cmd string, _ map[string]any) {
			if cmd == "job-cancel" && !seen {
				seen, dirThere, followersLive = true, exists(r.vc.runDir(spec.RunID)), v.(*vmRun).ctx.Err() == nil
			}
		}
	})
	if err := r.d.KillSandbox(ctx, sb.Ref); err != nil {
		t.Fatal(err)
	}
	if !seen || !dirThere || !followersLive {
		t.Fatalf("job-cancel sent %v, with the run directory there %v and the VM's followers live %v", seen, dirThere, followersLive)
	}
	if exists(r.vc.runDir(spec.RunID)) {
		t.Fatal("the run directory outlived the teardown")
	}
	if _, err := r.d.CommitSnapshot(ctx, sb.Ref, id); !errors.Is(err, runner.ErrSnapshotNotPending) {
		t.Fatalf("commit after the source ended = %v", err)
	}

	// A QMP that never answers: the teardown waits for it no longer than the bound.
	spec2, sb2 := r.boot(t, nil)
	id2 := uuid.New()
	if _, err := r.d.SnapshotSandbox(ctx, sb2.Ref, runner.SnapshotRequest{ID: id2}); err != nil {
		t.Fatal(err)
	}
	r.qmp.set(func(q *snapQMP) { q.silent = true; q.beforeAns = nil })
	start := time.Now()
	if err := r.d.KillSandbox(ctx, sb2.Ref); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > snapshotCancelWait+2*time.Second {
		t.Fatalf("teardown took %s behind a silent QMP", took)
	}
	if exists(r.vc.runDir(spec2.RunID)) {
		t.Fatal("the run directory outlived the teardown")
	}
}

// TestSnapshot_FreeDiskCountsParentLayer: a fork's snapshot needs room for its parent layer too, as the
// flatten copies it in.
func TestSnapshot_FreeDiskCountsParentLayer(t *testing.T) {
	r := newSnapRig(t)
	ctx := context.Background()
	free := int64(1 << 20) // MiB
	old := snapshotFreeMiB
	snapshotFreeMiB = func(string) int64 { return free }
	t.Cleanup(func() { snapshotFreeMiB = old })
	_, sb := r.boot(t, nil)
	free = snapshotHeadroom>>20 + 1 // the plain run's overlay holds a few bytes: the headroom and a MiB cover it
	l := r.snapshot(t, sb.Ref)
	layer := filepath.Join(r.d.layerDir(l.ID), qemu.SnapLayer)
	if err := os.Chmod(layer, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(layer, 3<<20); err != nil {
		t.Fatal(err)
	}
	_, fork := r.boot(t, sourceOf(l))
	_, err := r.d.SnapshotSandbox(ctx, fork.Ref, runner.SnapshotRequest{ID: uuid.New()})
	if !errors.Is(err, runner.ErrSnapshotNoSpace) || runnerwire.SnapshotRefusal(err) != "snapshot_no_space" {
		t.Fatalf("fork snapshot with room for its copy but not its parent = %v, want snapshot_no_space", err)
	}
	free += 3
	if _, err := r.d.SnapshotSandbox(ctx, fork.Ref, runner.SnapshotRequest{ID: uuid.New()}); err != nil {
		t.Fatalf("fork snapshot with room for its parent too = %v", err)
	}
}

// TestSnapshotLayer_NoCommitNoRename: no layer is kept without a Commit for its id, and an
// aborted, timed-out or deleted id is never committed later.
func TestSnapshotLayer_NoCommitNoRename(t *testing.T) {
	fastSnapshots(t, 300*time.Millisecond, 9*time.Minute, 2*time.Second)
	r := newSnapRig(t)
	ctx := context.Background()
	spec, sb := r.boot(t, nil)
	cut := func() uuid.UUID {
		t.Helper()
		id := uuid.New()
		c, err := r.d.SnapshotSandbox(ctx, sb.Ref, runner.SnapshotRequest{ID: id})
		if err != nil || c.ID != id || c.CutEnd.Before(c.CutStart) {
			t.Fatalf("cut = %+v, %v", c, err)
		}
		return id
	}
	noLayer := func(what string, id uuid.UUID) {
		t.Helper()
		if exists(r.target(spec.RunID, id)) || exists(r.d.layerDir(id)) {
			t.Errorf("%s: a file of %s is left", what, id)
		}
		if ls, _ := r.d.ListSnapshots(ctx); slices.ContainsFunc(ls, func(l runner.SnapshotLayer) bool { return l.ID == id }) {
			t.Errorf("%s: list names %s", what, id)
		}
		if _, err := r.d.CommitSnapshot(ctx, sb.Ref, id); !errors.Is(err, runner.ErrSnapshotNotPending) || runnerwire.SnapshotRefusal(err) != "snapshot_not_pending" {
			t.Errorf("%s: a later commit = %v, want snapshot_not_pending", what, err)
		}
		if exists(r.d.layerDir(id)) {
			t.Errorf("%s: a later commit kept a layer", what)
		}
	}

	// No Commit within the bound: the runner aborts by itself.
	r.qmp.set(func(q *snapQMP) { q.runFor = 1 << 30 })
	id := cut()
	if !exists(r.target(spec.RunID, id)) {
		t.Fatal("no copy target after the cut")
	}
	waitFor(t, func() bool { return !exists(r.target(spec.RunID, id)) })
	noLayer("no commit", id)

	// An Abort while the copy runs cancels the job before the target goes. From here on the self-abort is far
	// beyond each case's run time, so only the case above can abort by itself.
	fastSnapshots(t, time.Hour, 9*time.Minute, 2*time.Second)
	id = cut()
	var targetAtCancel bool
	r.qmp.set(func(q *snapQMP) {
		q.beforeAns = func(cmd string, _ map[string]any) {
			if cmd == "job-cancel" {
				targetAtCancel = exists(r.target(spec.RunID, id))
			}
		}
	})
	if err := r.d.AbortSnapshot(ctx, sb.Ref, id); err != nil {
		t.Fatal(err)
	}
	if !targetAtCancel {
		t.Error("the abort deleted the target before it cancelled the job")
	}
	r.qmp.set(func(q *snapQMP) { q.beforeAns = nil })
	noLayer("abort", id)

	// A Commit keeps the layer only once the job has concluded, and answers its bytes, digest and stamp.
	r.qmp.set(func(q *snapQMP) { q.runFor = 3 })
	id = cut()
	var keptEarly bool
	r.qmp.set(func(q *snapQMP) {
		q.beforeAns = func(cmd string, _ map[string]any) {
			if cmd == "query-jobs" && exists(r.d.layerDir(id)) {
				keptEarly = true
			}
		}
	})
	want, _ := os.ReadFile(r.target(spec.RunID, id))
	l, err := r.d.CommitSnapshot(ctx, sb.Ref, id)
	r.qmp.set(func(q *snapQMP) { q.beforeAns = nil })
	sum := sha256.Sum256(want)
	stamp, _ := baseStamp(filepath.Join(r.vc.imageDir(vmTestImage), qemu.RootFS))
	if err != nil || keptEarly || l.ID != id || l.Bytes != int64(len(want)) || l.SHA256 != hex.EncodeToString(sum[:]) || l.BaseStamp != stamp || l.BaseStamp == "" {
		t.Fatalf("commit = %+v, %v (kept before the job concluded %v)", l, err, keptEarly)
	}
	if again, err := r.d.CommitSnapshot(ctx, sb.Ref, id); err != nil || again != l {
		t.Fatalf("a repeated commit = %+v, %v, want the same layer", again, err)
	}
	if ls, err := r.d.ListSnapshots(ctx); err != nil || !slices.Contains(ls, l) {
		t.Fatalf("list = %+v, %v", ls, err)
	}

	// A Commit still copying at the commit bound is cancelled, deleted and tombstoned.
	fastSnapshots(t, time.Hour, 400*time.Millisecond, 2*time.Second)
	r.qmp.set(func(q *snapQMP) { q.runFor = 1 << 30 })
	id = cut()
	if _, err := r.d.CommitSnapshot(ctx, sb.Ref, id); !errors.Is(err, runner.ErrSnapshotNotPending) {
		t.Fatalf("commit past the bound = %v, want snapshot_not_pending", err)
	}
	noLayer("commit past its bound", id)

	// A delete of a pending id, with no Commit running: success only once the target is gone.
	fastSnapshots(t, time.Hour, time.Minute, 2*time.Second)
	id = cut()
	if err := r.d.DeleteSnapshot(ctx, id); err != nil || exists(r.target(spec.RunID, id)) {
		t.Fatalf("delete of a pending id = %v, target left %v", err, exists(r.target(spec.RunID, id)))
	}
	noLayer("delete while pending", id)

	// A delete while its Commit runs: the Commit is stopped and answers snapshot_not_pending.
	id = cut()
	commitErr := make(chan error, 1)
	go func() {
		_, err := r.d.CommitSnapshot(ctx, sb.Ref, id)
		commitErr <- err
	}()
	waitFor(t, func() bool {
		r.qmp.mu.Lock()
		defer r.qmp.mu.Unlock()
		return r.qmp.asks[qemu.SnapshotNode(id)] > 1
	})
	if err := r.d.DeleteSnapshot(ctx, id); err != nil || exists(r.target(spec.RunID, id)) {
		t.Fatalf("delete during the commit = %v, target left %v", err, exists(r.target(spec.RunID, id)))
	}
	if err := <-commitErr; !errors.Is(err, runner.ErrSnapshotNotPending) {
		t.Fatalf("the commit a delete stopped = %v", err)
	}
	noLayer("delete while committing", id)

	// A restarted runnerd holds nothing pending: it deletes the copy it finds and refuses its Commit.
	id = cut()
	d2 := newTestDriver(r.f)
	if err := d2.EnableVM(r.vc); err != nil {
		t.Fatal(err)
	}
	d2.SweepSnapshotTargets(ctx)
	if exists(r.target(spec.RunID, id)) {
		t.Fatal("the restarted runner left the earlier process's copy")
	}
	if _, err := d2.CommitSnapshot(ctx, sb.Ref, id); !errors.Is(err, runner.ErrSnapshotNotPending) {
		t.Fatalf("commit on the restarted runner = %v", err)
	}
}

// TestSnapshotLayer_LeftoverJobCancelledBeforeUnlink: a copy an earlier runner process left in a live VM is
// cancelled and its node closed before its file goes; a QMP that never answers, or a job that does not end,
// leaves the file until that QEMU exits, and the QEMU is never stopped for it. The same runs before the next
// snapshot of the run; at its teardown the QEMU is already gone, so the file is cleared as a leftover with no
// wait, before the run directory goes.
func TestSnapshotLayer_LeftoverJobCancelledBeforeUnlink(t *testing.T) {
	fastSnapshots(t, time.Minute, time.Minute, 300*time.Millisecond)
	r := newSnapRig(t)
	ctx := context.Background()
	leftover := func(runID uuid.UUID) (uuid.UUID, string) {
		t.Helper()
		id := uuid.New()
		f := r.target(runID, id)
		if err := os.WriteFile(f, []byte("backing="+qemu.BaseDir+"/"+qemu.RootFS), 0o600); err != nil {
			t.Fatal(err)
		}
		r.qmp.set(func(q *snapQMP) { q.jobs[qemu.SnapshotNode(id)], q.runFor = "running", 1<<30 })
		return id, f
	}
	running := func(name string) bool {
		r.f.mu.Lock()
		defer r.f.mu.Unlock()
		c := r.f.containers[name]
		return c != nil && !c.removed && c.state != nil && c.state.Running
	}
	exit := func(name string) {
		r.f.mu.Lock()
		r.f.containers[name].state = &container.State{Status: "exited"}
		r.f.mu.Unlock()
	}

	// The job ends at the cancel: job-cancel, then its node closed, then the unlink.
	spec, sb := r.boot(t, nil)
	_, f := leftover(spec.RunID)
	var order []string
	r.qmp.set(func(q *snapQMP) {
		q.beforeAns = func(cmd string, _ map[string]any) {
			if cmd == "job-cancel" || cmd == "blockdev-del" {
				order = append(order, cmd+":"+map[bool]string{true: "file", false: "gone"}[exists(f)])
			}
		}
	})
	r.d.SweepSnapshotTargets(ctx)
	r.qmp.set(func(q *snapQMP) { q.beforeAns = nil })
	if !slices.Equal(order, []string{"job-cancel:file", "blockdev-del:file"}) || exists(f) {
		t.Fatalf("sweep order %q, file left %v", order, exists(f))
	}

	// A silent QMP, then a job that will not end: the file stays until the QEMU exits, which nobody forces.
	for _, how := range []string{"silent", "stuck"} {
		spec, sb := r.boot(t, nil)
		_, f := leftover(spec.RunID)
		r.qmp.set(func(q *snapQMP) { q.silent, q.stuck = how == "silent", how == "stuck" })
		done := make(chan struct{})
		go func() { r.d.SweepSnapshotTargets(ctx); close(done) }()
		time.Sleep(3 * snapshotCancelWait)
		if !exists(f) || !running(sb.Ref) {
			t.Fatalf("%s: file left %v, QEMU running %v, before the QEMU exited", how, exists(f), running(sb.Ref))
		}
		exit(sb.Ref)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the sweep did not finish after the QEMU exited", how)
		}
		if exists(f) {
			t.Fatalf("%s: the file outlived its QEMU", how)
		}
		r.qmp.set(func(q *snapQMP) { q.silent, q.stuck = false, false })
	}

	// Before the next snapshot of the run, and at its teardown.
	old, f := leftover(spec.RunID)
	r.qmp.set(func(q *snapQMP) { q.runFor = 0 })
	if _, err := r.d.SnapshotSandbox(ctx, sb.Ref, runner.SnapshotRequest{ID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	if exists(f) {
		t.Fatal("the next snapshot left the earlier copy")
	}
	r.qmp.mu.Lock()
	_, stillThere := r.qmp.jobs[qemu.SnapshotNode(old)]
	r.qmp.mu.Unlock()
	if stillThere {
		t.Fatal("the earlier copy's job was not cancelled and dismissed")
	}
	spec3, sb3 := r.boot(t, nil)
	_, f3 := leftover(spec3.RunID)
	var cleared, dirThere bool
	old3 := snapshotUnlinked
	snapshotUnlinked = func(f string) {
		if f == f3 {
			cleared, dirThere = true, exists(r.vc.runDir(spec3.RunID))
		}
	}
	t.Cleanup(func() { snapshotUnlinked = old3 })
	start := time.Now()
	if err := r.d.KillSandbox(ctx, sb3.Ref); err != nil {
		t.Fatal(err)
	}
	if !cleared || !dirThere {
		t.Fatalf("teardown cleared the copy as a leftover %v, with the run directory still there %v", cleared, dirThere)
	}
	if took := time.Since(start); took > snapshotCancelWait+2*time.Second {
		t.Fatalf("teardown took %s", took)
	}
}

// TestSnapshotLayer_DeleteOrAbortAfterCommitWinsDropsTheLayer: a Delete or an Abort that read the id as pending,
// then lost the race to its Commit, deletes the layer the Commit kept: it answers success only once the layer's
// directory is gone, and no later Commit answers the layer.
func TestSnapshotLayer_DeleteOrAbortAfterCommitWinsDropsTheLayer(t *testing.T) {
	r := newSnapRig(t)
	ctx := context.Background()
	_, sb := r.boot(t, nil)
	r.qmp.set(func(q *snapQMP) { q.runFor = 0 })
	old := snapshotBeforeAbort
	t.Cleanup(func() { snapshotBeforeAbort = old })
	for _, how := range []string{"delete", "abort"} {
		id := uuid.New()
		if _, err := r.d.SnapshotSandbox(ctx, sb.Ref, runner.SnapshotRequest{ID: id}); err != nil {
			t.Fatal(err)
		}
		var won runner.SnapshotLayer
		var wonErr error
		ran := false
		// The Commit runs to its end between the call's read of the pending id and its abort.
		snapshotBeforeAbort = func(got uuid.UUID) {
			if got == id && !ran {
				ran = true
				won, wonErr = r.d.CommitSnapshot(ctx, sb.Ref, id)
			}
		}
		var err error
		if how == "delete" {
			err = r.d.DeleteSnapshot(ctx, id)
		} else {
			err = r.d.AbortSnapshot(ctx, sb.Ref, id)
		}
		snapshotBeforeAbort = old
		if !ran || wonErr != nil || won.ID != id {
			t.Fatalf("%s: the racing commit ran %v and answered %+v, %v", how, ran, won, wonErr)
		}
		if err != nil {
			t.Fatalf("%s after the commit won = %v", how, err)
		}
		if exists(r.d.layerDir(id)) {
			t.Fatalf("%s: the layer the commit kept is still there", how)
		}
		if l, err := r.d.CommitSnapshot(ctx, sb.Ref, id); !errors.Is(err, runner.ErrSnapshotNotPending) {
			t.Fatalf("%s: a later commit = %+v, %v, want snapshot_not_pending", how, l, err)
		}
		if ls, _ := r.d.ListSnapshots(ctx); slices.ContainsFunc(ls, func(l runner.SnapshotLayer) bool { return l.ID == id }) {
			t.Fatalf("%s: list still names %s", how, id)
		}
	}
}
