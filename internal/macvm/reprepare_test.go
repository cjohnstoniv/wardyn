// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

type stamp struct {
	ino   uint64
	mtime time.Time
	size  int64
}

// stamps records the inode, mtime and size of dir and of every file in it.
func stamps(t *testing.T, dir string) map[string]stamp {
	t.Helper()
	out := map[string]stamp{}
	add := func(name string) {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = stamp{fi.Sys().(*syscall.Stat_t).Ino, fi.ModTime(), fi.Size()}
	}
	add(".")
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range des {
		add(de.Name())
	}
	return out
}

func under(path, dir string) bool { return strings.HasPrefix(path, dir+string(filepath.Separator)) }

// A re-prepare builds the new base in <base>.update: no file under the live
// base is created, replaced or written until the exchange, and every path the
// engine, the first boot and stage 2 are given is in the staging dir.
func TestReprepareNeverWritesUnderTheLiveBase(t *testing.T) {
	f, _, _ := stage2Prep(t)
	before, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	base := f.opts.Layout.BaseDir(0)
	staging := base + ".update"
	want := stamps(t, base)
	idBefore, _ := os.ReadFile(filepath.Join(base, macvm.MachineIDFile))
	var checked []string
	check := func(at string) {
		checked = append(checked, at)
		if got := stamps(t, base); !maps.Equal(got, want) {
			t.Errorf("at %s the live base changed:\n got %v\nwant %v", at, got, want)
		}
	}
	f.eng.InstallFunc = func(_ context.Context, m *macvmtest.Machine, _ string, _ func(float64)) error {
		check("install")
		for _, s := range m.Plan.Storage {
			if !under(s.Path, staging) {
				t.Errorf("the installer was given %s, outside the staging dir", s.Path)
			}
		}
		return nil
	}
	next := f.opts.Stage2
	f.opts.Stage2 = hookStage(func(ctx context.Context, in macvm.Stage2Input) (macvm.ToolsResult, error) {
		check("stage 2")
		if in.Dir != staging || !under(in.Spec.DiskPath, staging) || !under(in.Spec.AuxPath, staging) {
			t.Errorf("stage 2 was given %s, %s and %s", in.Dir, in.Spec.DiskPath, in.Spec.AuxPath)
		}
		return next.Run(ctx, in)
	})
	defer macvm.SetSwapHook(func() { check("swap") })()
	f.opts.Reprepare = true
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(checked) != 3 {
		t.Fatalf("checked at %v, want install, stage 2 and swap", checked)
	}
	if fb := f.fb.Last(); !under(fb.Disk, staging) || !under(fb.Aux, staging) {
		t.Fatalf("the provisioning boot was given %s and %s", fb.Disk, fb.Aux)
	}
	got, err := macvm.ReadyBase(base)
	if err != nil || !got.CreatedAt.Equal(m.CreatedAt) || m.CreatedAt.Equal(before.CreatedAt) || f.eng.Installs() != 2 {
		t.Fatalf("base after the re-prepare: %+v %v (%d installs)", got, err, f.eng.Installs())
	}
	if idAfter, _ := os.ReadFile(filepath.Join(base, macvm.MachineIDFile)); string(idAfter) == string(idBefore) {
		t.Fatal("the re-prepared base kept the old install's machine identifier")
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("the old generation was left in %s: %v", staging, err)
	}
	if _, err := os.Stat(filepath.Join(base, "installing")); !os.IsNotExist(err) {
		t.Fatal("install marker on the swapped-in base")
	}
	// Without a re-prepare request a ready base is returned as it is.
	f.opts.Reprepare = false
	if again, err := macvm.Prepare(context.Background(), f.opts); err != nil || !again.CreatedAt.Equal(m.CreatedAt) || f.eng.Installs() != 2 {
		t.Fatalf("plain prepare of a ready base: %v, %d installs", err, f.eng.Installs())
	}
}

// A clone that holds the base lock shared while a re-prepare finishes keeps
// seeing the whole old generation: the exchange waits for it. The next clone
// gets the new generation.
func TestCloneHoldingBaseLockSeesOldGenerationThroughReprepare(t *testing.T) {
	f := newPrep(t)
	before, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	base := f.opts.Layout.BaseDir(0)
	for n, c := range map[string]string{macvm.DiskFile: "gen1-disk", macvm.AuxFile: "gen1-aux"} {
		if err := os.WriteFile(filepath.Join(base, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id1, _ := os.ReadFile(filepath.Join(base, macvm.MachineIDFile))
	f.eng.InstallFunc = func(_ context.Context, m *macvmtest.Machine, _ string, _ func(float64)) error {
		return os.WriteFile(m.Plan.Storage[0].Path, []byte("gen2-disk"), 0o600)
	}
	unlock, err := macvm.LockBase(base, false)
	if err != nil {
		t.Fatal(err)
	}
	f.opts.Reprepare = true
	res := make(chan error, 1)
	go func() { _, err := macvm.Prepare(context.Background(), f.opts); res <- err }()
	// Wait until the new base is ready in the staging dir, then give the
	// exchange time to (wrongly) happen.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := macvm.ReadyBase(base + ".update"); err == nil {
			break
		}
		select {
		case err := <-res:
			unlock()
			t.Fatalf("the re-prepare finished (%v) while a clone held the base lock", err)
		default:
		}
		if time.Now().After(deadline) {
			unlock()
			t.Fatal("the staging base never became ready")
		}
	}
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-res:
		unlock()
		t.Fatalf("the exchange did not wait for the clone's shared lock (%v)", err)
	default:
	}
	m, _ := macvm.ReadManifest(base)
	id, _ := os.ReadFile(filepath.Join(base, macvm.MachineIDFile))
	if !m.CreatedAt.Equal(before.CreatedAt) || string(id) != string(id1) {
		t.Errorf("the live base changed under a clone's lock: manifest %v, id %q", m.CreatedAt, id)
	}
	run1 := filepath.Join(t.TempDir(), "run1")
	if _, err := macvm.CloneRun(f.eng, base, run1, macvm.IdentityFresh); err != nil {
		t.Error(err)
	}
	unlock()
	if err := <-res; err != nil {
		t.Fatal(err)
	}
	for n, want := range map[string]string{macvm.DiskFile: "gen1-disk", macvm.AuxFile: "gen1-aux"} {
		if b, _ := os.ReadFile(filepath.Join(run1, n)); string(b) != want {
			t.Errorf("the clone under the lock read %s = %q, want %q", n, b, want)
		}
	}
	run2 := filepath.Join(t.TempDir(), "run2")
	if _, err := macvm.CloneRun(f.eng, base, run2, macvm.IdentityFresh); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(run2, macvm.DiskFile)); string(b) != "gen2-disk" {
		t.Fatalf("the clone after the exchange read %q", b)
	}
	if id, _ := os.ReadFile(filepath.Join(base, macvm.MachineIDFile)); string(id) == string(id1) {
		t.Fatal("the base kept the old machine identifier after the exchange")
	}
}

// A re-prepare that fails leaves the live base as it was; one that is killed
// leaves its half-built staging dir, which the next update sweeps.
func TestReprepareCutShortKeepsLiveBaseAndIsSwept(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	before, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	base := f.opts.Layout.BaseDir(0)
	staging := base + ".update"
	want := stamps(t, base)
	crashed := filepath.Join(t.TempDir(), "crashed")
	st := &failingStage2{next: f.opts.Stage2}
	f.opts.Stage2 = hookStage(func(ctx context.Context, in macvm.Stage2Input) (macvm.ToolsResult, error) {
		if st.calls.Load() == 0 {
			// What the staging dir holds when the helper is killed here.
			if err := os.CopyFS(crashed, os.DirFS(in.Dir)); err != nil {
				t.Error(err)
			}
		}
		return st.Run(ctx, in)
	})
	f.opts.Reprepare = true
	if _, err := macvm.Prepare(context.Background(), f.opts); err == nil {
		t.Fatal("the cut-short re-prepare succeeded")
	}
	if got := stamps(t, base); !maps.Equal(got, want) {
		t.Fatalf("a failed re-prepare changed the live base:\n got %v\nwant %v", got, want)
	}
	if m, err := macvm.ReadyBase(base); err != nil || !m.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("live base after a failed re-prepare: %+v %v", m, err)
	}
	// The helper is killed instead: the half-built staging dir stays.
	if err := os.Rename(crashed, staging); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staging, "installing")); err != nil {
		t.Fatalf("the crashed staging dir has no install marker: %v", err)
	}
	// The next update sweeps it before it builds its own staging copy.
	o := updateOpts(f, tf, newGuest(), "")
	o.Force = true
	o.Stage = hookStage(func(_ context.Context, in macvm.Stage2Input) (macvm.ToolsResult, error) {
		if _, err := os.Stat(filepath.Join(in.Dir, "installing")); !os.IsNotExist(err) {
			t.Errorf("the update built on the crashed staging dir: %v", err)
		}
		return noneOnly, nil
	})
	if _, changed, err := macvm.UpdateTools(context.Background(), o); err != nil || !changed {
		t.Fatalf("update after a crashed re-prepare: changed %v err %v", changed, err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("the crashed staging dir outlived the next update: %v", err)
	}
	// The re-prepare asked for again finishes.
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil || m.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("re-prepare after the sweep: %+v %v", m, err)
	}
}

// A re-prepare takes the locks as a tools update does: the update lock for the
// whole run, then the base lock exclusively only around the exchange.
func TestReprepareLockOrderMatchesToolsUpdate(t *testing.T) {
	flows := map[string]func(*prepFixture, *toolsFixture, macvm.Stage2) error{
		"tools update": func(f *prepFixture, tf *toolsFixture, s macvm.Stage2) error {
			o := updateOpts(f, tf, newGuest(), "")
			o.Force, o.Stage = true, s
			_, _, err := macvm.UpdateTools(context.Background(), o)
			return err
		},
		"re-prepare": func(f *prepFixture, _ *toolsFixture, s macvm.Stage2) error {
			f.opts.Reprepare, f.opts.Stage2 = true, s
			_, err := macvm.Prepare(context.Background(), f.opts)
			return err
		},
	}
	for name, run := range flows {
		t.Run(name, func(t *testing.T) {
			f, tf, _ := stage2Prep(t)
			if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
				t.Fatal(err)
			}
			base := f.opts.Layout.BaseDir(0)
			var seen []string
			observe := func(at string) {
				state := "free"
				if !macvm.BaseLockFree(base, false) {
					state = "exclusive"
				} else if !macvm.BaseLockFree(base, true) {
					state = "shared"
				}
				seen = append(seen, at+": update held "+boolWord(!macvm.UpdateLockFree(base, false))+", base "+state)
			}
			defer macvm.SetSwapHook(func() { observe("swap") })()
			stage := hookStage(func(context.Context, macvm.Stage2Input) (macvm.ToolsResult, error) {
				observe("stage 2")
				return noneOnly, nil
			})
			if err := run(f, tf, stage); err != nil {
				t.Fatal(err)
			}
			observe("after")
			want := []string{
				"stage 2: update held yes, base free",
				"swap: update held yes, base exclusive",
				"after: update held no, base free",
			}
			if strings.Join(seen, "\n") != strings.Join(want, "\n") {
				t.Fatalf("locks seen:\n%s\nwant:\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// A complete base in a format this helper does not run is replaced through the
// staging dir: a failed install leaves it exactly as it was (and still not
// runnable), and a later prepare swaps the new base in and deletes the old.
func TestPrepareKeepsACompleteOldFormatBaseUntilTheSwap(t *testing.T) {
	f, _, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	base := f.opts.Layout.BaseDir(0)
	old, err := macvm.ReadManifest(base)
	if err != nil {
		t.Fatal(err)
	}
	old.Format = macvm.BaseFormat - 1
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, macvm.ManifestFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	want := stamps(t, base)
	for _, name := range []string{macvm.DiskFile, macvm.AuxFile, macvm.MachineIDFile, macvm.ManifestFile, macvm.PairKeyFile, macvm.ToolsRecordFile} {
		if _, ok := want[name]; !ok {
			t.Fatalf("the old base has no %s", name)
		}
	}
	f.eng.InstallFunc = func(context.Context, *macvmtest.Machine, string, func(float64)) error {
		return errors.New("restore failed")
	}
	if _, err := macvm.Prepare(context.Background(), f.opts); err == nil {
		t.Fatal("prepare with a failing restore succeeded")
	}
	if got := stamps(t, base); !maps.Equal(got, want) {
		t.Fatalf("a failed replacement changed the old base:\n got %v\nwant %v", got, want)
	}
	if _, err := macvm.ReadyBase(base); err == nil {
		t.Fatal("the old-format base became runnable")
	}
	f.eng.InstallFunc = nil
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil || m.Format != macvm.BaseFormat {
		t.Fatalf("replacement prepare: %+v %v", m, err)
	}
	if got, err := macvm.ReadyBase(base); err != nil || !got.CreatedAt.Equal(m.CreatedAt) {
		t.Fatalf("base after the replacement: %+v %v", got, err)
	}
	if _, err := os.Stat(base + ".update"); !os.IsNotExist(err) {
		t.Fatalf("the old generation outlived the replacement: %v", err)
	}
}

// A staging dir that cannot be deleted after a swap is logged with its path and
// error and left for the next update to sweep; the re-prepare itself succeeded.
func TestStagingDeleteFailureIsLogged(t *testing.T) {
	f, _, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	base := f.opts.Layout.BaseDir(0)
	staging := base + ".update"
	// A dir this user cannot empty goes with the old generation into staging.
	stuck := filepath.Join(base, "stuck")
	if err := os.MkdirAll(filepath.Join(stuck, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(staging, "stuck"), 0o700) })
	f.opts.Reprepare = true
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatalf("re-prepare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staging, "stuck")); err != nil {
		t.Fatalf("the undeletable old generation is gone: %v", err)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, `"level":"WARN"`) || !strings.Contains(logs, "staging dir was not deleted") ||
		!strings.Contains(logs, staging) || !strings.Contains(logs, "permission denied") {
		t.Fatalf("no warning with the path and the error:\n%s", logs)
	}
}

// The admission entry of a re-prepare and of a tools update is released only
// after the old generation it reserves disk for is deleted.
func TestAdmissionReleasedAfterOldGenerationIsDeleted(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	staging := f.opts.Layout.BaseDir(0) + ".update"
	released := 0
	admit := func(context.Context) (func(), error) {
		return func() {
			released++
			if _, err := os.Stat(staging); !os.IsNotExist(err) {
				t.Errorf("admission released while the old generation is still at %s: %v", staging, err)
			}
		}, nil
	}
	f.opts.Reprepare, f.opts.Admit = true, admit
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	o := updateOpts(f, tf, newGuest(), "")
	o.Force, o.Admit = true, admit
	if _, changed, err := macvm.UpdateTools(context.Background(), o); err != nil || !changed {
		t.Fatalf("update: changed %v err %v", changed, err)
	}
	if released != 2 {
		t.Fatalf("admission released %d times, want 2", released)
	}
}
