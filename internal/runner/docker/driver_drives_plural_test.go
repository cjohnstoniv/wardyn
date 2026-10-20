// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// secondVolumeDrive is a managed drive beside the legacy one, at its derived path.
func secondVolumeDrive() types.DriveMount {
	d := *dockerVolumeDrive()
	d.DriveID, d.ObjectName, d.Target, d.ReadOnly = uuid.New(), "wardyn-drive-team-alice", runner.DrivesRoot+"/team", false
	return d
}

func createWithDrives(t *testing.T, f *fakeDocker, runID uuid.UUID, drives ...types.DriveMount) ([]mount.Mount, error) {
	t.Helper()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
	spec := testSpec()
	spec.RunID = runID
	spec.Drives = drives
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		return nil, err
	}
	return f.containers[agentContainerName(spec.RunID)].host.Mounts, nil
}

// TestCreateSandbox_MountsEveryDrive: two drives, each at its own path, each a
// volume of its own. The one below DrivesRoot is first seeded through the
// image's own drive directory by a container created and removed, never
// started, with no network and the run's labels; the legacy one needs no seed.
func TestCreateSandbox_MountsEveryDrive(t *testing.T) {
	f := newFakeDocker()
	var seeds []string
	f.onCreate = func(name string) {
		if strings.HasPrefix(name, driveSeedPrefix) {
			seeds = append(seeds, name)
		}
	}
	second := secondVolumeDrive()
	mounts, err := createWithDrives(t, f, testSpec().RunID, *dockerVolumeDrive(), second)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []types.DriveMount{*dockerVolumeDrive(), second} {
		m := findMount(mounts, d.Target)
		if m == nil || m.Type != mount.TypeVolume || m.Source != d.ObjectName || m.ReadOnly != d.ReadOnly {
			t.Errorf("drive at %s mounted as %+v, want volume %s ro=%v", d.Target, m, d.ObjectName, d.ReadOnly)
		}
	}
	if len(seeds) != 1 {
		t.Fatalf("seed containers created = %v, want one, for the drive below DrivesRoot only", seeds)
	}
	seed := f.containers[driveSeedName(testSpec().RunID)]
	if seed == nil || !seed.removed || seed.state != nil && seed.state.Running {
		t.Fatalf("seed container = %+v, want one created and removed, never started", seed)
	}
	if m := findMount(seed.host.Mounts, runner.DriveTarget); m == nil || m.Source != second.ObjectName || m.ReadOnly || len(seed.host.Mounts) != 1 {
		t.Errorf("seed mounts = %+v, want only %s writable at %s", seed.host.Mounts, second.ObjectName, runner.DriveTarget)
	}
	if !seed.cfg.NetworkDisabled || seed.host.NetworkMode != "none" {
		t.Errorf("seed network = disabled %v, mode %q; want no network at all", seed.cfg.NetworkDisabled, seed.host.NetworkMode)
	}
	if seed.cfg.Labels[labelRun] != testSpec().RunID.String() || seed.cfg.Labels[labelManaged] != "true" || seed.cfg.Labels[labelComponent] != componentDriveSeed {
		t.Errorf("seed labels = %v, want the run's managed labels", seed.cfg.Labels)
	}
}

// teamVolumeDrive is a team drive's docker volume at its shared path.
func teamVolumeDrive(readOnly bool) types.DriveMount {
	d := *dockerVolumeDrive()
	d.DriveID, d.ObjectName, d.Target, d.ReadOnly, d.Team = uuid.New(), "wardyn-drive-team-eng", runner.SharedRoot+"/eng", readOnly, true
	return d
}

// seedCopies is every CopyToContainer sent to the run's drive seed.
func seedCopies(f *fakeDocker, runID uuid.UUID) []fakeCopy {
	var out []fakeCopy
	for _, c := range f.copies {
		if c.id == driveSeedName(runID) {
			out = append(out, c)
		}
	}
	return out
}

func wantNoCopyMount(t *testing.T, m *mount.Mount, what string) {
	t.Helper()
	if m == nil || m.VolumeOptions == nil || !m.VolumeOptions.NoCopy {
		t.Errorf("%s = %+v, want a NoCopy volume mount", what, m)
	}
}

// TestCreateSandbox_ReadOnlyTeamDriveCopiesNothing: a read-only team allocation
// creates no seed and mounts the volume read-only with NoCopy, so nothing the
// run's image holds reaches the group's volume.
func TestCreateSandbox_ReadOnlyTeamDriveCopiesNothing(t *testing.T) {
	f := newFakeDocker()
	drive := teamVolumeDrive(true)
	mounts, err := createWithDrives(t, f, testSpec().RunID, drive)
	if err != nil {
		t.Fatal(err)
	}
	if c := f.containers[driveSeedName(testSpec().RunID)]; c != nil {
		t.Errorf("a read-only team drive created a seed %+v, want none", c.host.Mounts)
	}
	m := findMount(mounts, drive.Target)
	wantNoCopyMount(t, m, "read-only team mount")
	if m != nil && !m.ReadOnly {
		t.Errorf("read-only team mount is writable")
	}
}

// TestCreateSandbox_WritableTeamDriveCopiesNothing: a writable team allocation
// seeds with the volume mounted NoCopy at Wardyn's own seed path, never at the
// image's drive directory, gives the empty volume's top directory to the agent
// (uid/gid 1000, 0755) with one directory entry and no file, and mounts it
// writable with NoCopy.
func TestCreateSandbox_WritableTeamDriveCopiesNothing(t *testing.T) {
	f := newFakeDocker()
	f.imageUsers = map[string]string{"busybox:latest": "1000"}
	drive := teamVolumeDrive(false)
	run := testSpec().RunID
	mounts, err := createWithDrives(t, f, run, drive)
	if err != nil {
		t.Fatal(err)
	}
	seed := f.containers[driveSeedName(run)]
	if seed == nil || !seed.removed || slices.Contains(f.startedNames, driveSeedName(run)) {
		t.Fatalf("seed = %+v, want one created and removed, never started", seed)
	}
	if len(seed.host.Mounts) != 1 || seed.host.Mounts[0].Target != teamDriveSeedTarget || seed.host.Mounts[0].Source != drive.ObjectName {
		t.Fatalf("seed mounts = %+v, want only %s at %s", seed.host.Mounts, drive.ObjectName, teamDriveSeedTarget)
	}
	wantNoCopyMount(t, &seed.host.Mounts[0], "seed mount")
	m := findMount(mounts, drive.Target)
	wantNoCopyMount(t, m, "writable team mount")
	if m != nil && m.ReadOnly {
		t.Errorf("writable team mount is read-only")
	}
	copies := seedCopies(f, run)
	if len(copies) != 1 || copies[0].dest != "/" || copies[0].copyUIDGID {
		t.Fatalf("seed copies = %+v, want one archive at / with CopyUIDGID false", copies)
	}
	tr := tar.NewReader(bytes.NewReader(copies[0].archive))
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "wardyn-drive-seed/" || hdr.Typeflag != tar.TypeDir || hdr.Uid != 1000 || hdr.Gid != 1000 || hdr.Mode != 0o755 {
		t.Fatalf("seed archive entry = %+v (%v), want the seed directory owned 1000:1000 0755", hdr, err)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Errorf("seed archive holds more than the directory entry (%v)", err)
	}
}

// TestCreateSandbox_TeamDriveHoldingFilesIsLeftAlone: a team volume that
// already holds anything gets no ownership change and no copy.
func TestCreateSandbox_TeamDriveHoldingFilesIsLeftAlone(t *testing.T) {
	f := newFakeDocker()
	f.imageUsers = map[string]string{"busybox:latest": "1000"}
	f.teamSeedVolume = []*tar.Header{
		{Name: "wardyn-drive-seed/", Typeflag: tar.TypeDir, Mode: 0o750},
		{Name: "wardyn-drive-seed/notes", Typeflag: tar.TypeReg, Mode: 0o644},
	}
	run := testSpec().RunID
	mounts, err := createWithDrives(t, f, run, teamVolumeDrive(false))
	if err != nil {
		t.Fatal(err)
	}
	if c := seedCopies(f, run); len(c) != 0 {
		t.Errorf("seed copies = %+v, want none into a volume that holds files", c)
	}
	wantNoCopyMount(t, findMount(mounts, runner.SharedRoot+"/eng"), "team mount")
}

// TestCreateSandbox_TeamDriveSeedPathNotADirectoryRefused: an image with a
// link at the seed path would leave the volume root-owned; the run is refused.
func TestCreateSandbox_TeamDriveSeedPathNotADirectoryRefused(t *testing.T) {
	f := newFakeDocker()
	f.imageUsers = map[string]string{"busybox:latest": "1000"}
	f.teamSeedVolume = []*tar.Header{{Name: "wardyn-drive-seed", Typeflag: tar.TypeSymlink, Linkname: "/etc"}}
	if _, err := createWithDrives(t, f, testSpec().RunID, teamVolumeDrive(false)); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("CreateSandbox = %v, want the seed refusal", err)
	}
}

// TestCreateSandbox_PersonalDriveSeedUnchanged: a person's managed drive is
// still seeded through the image's drive directory with Docker's default copy,
// and nothing is extracted into its seed.
func TestCreateSandbox_PersonalDriveSeedUnchanged(t *testing.T) {
	f := newFakeDocker()
	drive := secondVolumeDrive()
	run := testSpec().RunID
	mounts, err := createWithDrives(t, f, run, drive)
	if err != nil {
		t.Fatal(err)
	}
	seed := f.containers[driveSeedName(run)]
	if seed == nil || len(seed.host.Mounts) != 1 || seed.host.Mounts[0].Target != runner.DriveTarget || seed.host.Mounts[0].VolumeOptions != nil {
		t.Fatalf("personal seed = %+v, want the volume at %s with the default copy", seed, runner.DriveTarget)
	}
	if c := seedCopies(f, run); len(c) != 0 {
		t.Errorf("personal seed copies = %+v, want none", c)
	}
	if m := findMount(mounts, drive.Target); m == nil || m.VolumeOptions != nil {
		t.Errorf("personal mount = %+v, want the default copy", m)
	}
}

// TestCreateSandbox_WritableTeamDriveRefusesAnotherAgentUID: an image whose
// agent runs as any uid but runner.AgentUID, root included, is refused, by name, before any
// ownership change, whether the volume is empty or holds files; a read-only
// allocation of the same image is not, since it never writes.
func TestCreateSandbox_WritableTeamDriveRefusesAnotherAgentUID(t *testing.T) {
	for _, held := range []bool{false, true} {
		f := newFakeDocker()
		f.imageUsers = map[string]string{"busybox:latest": "1001"}
		if held {
			f.teamSeedVolume = []*tar.Header{
				{Name: "wardyn-drive-seed/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 1000, Gid: 1000},
				{Name: "wardyn-drive-seed/notes", Typeflag: tar.TypeReg, Mode: 0o644},
			}
		}
		run := testSpec().RunID
		drive := teamVolumeDrive(false)
		drive.DriveName = "Eng share"
		_, err := createWithDrives(t, f, run, drive)
		const want = `docker: seed user drive volume "wardyn-drive-team-eng": team drive drive "Eng share", directory "alice" is writable for this run, ` +
			`but this image runs the agent as uid 1001 (USER "1001"), and a team drive's folder belongs to uid 1000, the user Wardyn images run the agent as. ` +
			`Use an image whose agent runs as uid 1000, or ask an admin for a read-only allocation`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("held=%v: CreateSandbox = %v, want the refusal %q", held, err, want)
		}
		if c := seedCopies(f, run); len(c) != 0 {
			t.Errorf("held=%v: seed copies = %+v, want no ownership change", held, c)
		}
	}
	for _, user := range []string{"", "0", "root"} {
		f := newFakeDocker()
		f.imageUsers = map[string]string{"busybox:latest": user}
		f.passwd = "root:x:0:0::/root:/bin/sh\n"
		want := fmt.Sprintf("this image runs the agent as uid 0 (USER %q)", user)
		if _, err := createWithDrives(t, f, testSpec().RunID, teamVolumeDrive(false)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("USER %q: CreateSandbox = %v, want the refusal naming uid 0", user, err)
		}
	}
	f := newFakeDocker()
	f.imageUsers = map[string]string{"busybox:latest": "1001"}
	if _, err := createWithDrives(t, f, testSpec().RunID, teamVolumeDrive(true)); err != nil {
		t.Errorf("read-only team drive with USER 1001 = %v, want it mounted", err)
	}
}

// TestCreateSandbox_WritableTeamDriveAcceptsTheAgentByName: USER agent, uid
// runner.AgentUID in the image's /etc/passwd, is the agent.
func TestCreateSandbox_WritableTeamDriveAcceptsTheAgentByName(t *testing.T) {
	f := newFakeDocker()
	f.imageUsers = map[string]string{"busybox:latest": "agent"}
	f.passwd = "root:x:0:0::/root:/bin/sh\nagent:x:1000:1000::/home/agent:/bin/sh\n"
	if _, err := createWithDrives(t, f, testSpec().RunID, teamVolumeDrive(false)); err != nil {
		t.Fatalf("CreateSandbox = %v, want USER agent (uid 1000) accepted", err)
	}
}

// TestCreateSandbox_ConcurrentTeamSeedsAreIdempotent: four runs seeding one
// empty team volume at once, all held until each has read it empty, all
// succeed, each through its own seed, and each sends the same one entry: the
// directory as runner.AgentUID:runner.AgentUID 0755 and nothing in it.
func TestCreateSandbox_ConcurrentTeamSeedsAreIdempotent(t *testing.T) {
	f := newFakeDocker()
	f.imageUsers = map[string]string{"busybox:latest": "1000"}
	const n = 4
	var arrived sync.WaitGroup
	arrived.Add(n)
	f.onCopyFrom = func(_, path string) {
		if path == teamDriveSeedTarget {
			arrived.Done()
			arrived.Wait()
		}
	}
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
	drive := teamVolumeDrive(false)
	runs := make([]uuid.UUID, n)
	for i := range runs {
		runs[i] = uuid.New()
	}
	errs := make([]error, len(runs))
	var wg sync.WaitGroup
	for i, run := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			spec := testSpec()
			spec.RunID = run
			spec.Drives = []types.DriveMount{drive}
			_, errs[i] = d.CreateSandbox(context.Background(), spec)
		}()
	}
	wg.Wait()
	var first *tar.Header
	for i, run := range runs {
		if errs[i] != nil {
			t.Errorf("run %d: %v", i, errs[i])
			continue
		}
		copies := seedCopies(f, run)
		if len(copies) != 1 {
			t.Errorf("run %d: seed copies = %d, want its own one", i, len(copies))
			continue
		}
		tr := tar.NewReader(bytes.NewReader(copies[0].archive))
		hdr, err := tr.Next()
		if err != nil || hdr.Name != "wardyn-drive-seed/" || hdr.Typeflag != tar.TypeDir || hdr.Uid != runner.AgentUID || hdr.Gid != runner.AgentUID || hdr.Mode != 0o755 {
			t.Errorf("run %d: seed entry = %+v (%v), want the directory as %d:%d 0755", i, hdr, err, runner.AgentUID, runner.AgentUID)
		}
		if _, err := tr.Next(); err != io.EOF {
			t.Errorf("run %d: seed archive holds more than the directory (%v)", i, err)
		}
		hdr.ModTime, hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}, time.Time{}
		if first == nil {
			first = hdr
		} else if !reflect.DeepEqual(hdr, first) {
			t.Errorf("run %d: seed entry %+v differs from the first run's %+v (apart from its time)", i, hdr, first)
		}
	}
}

// TestCreateSandbox_ConcurrentRunsSeedOneVolume: two runs attaching the same
// drive at once each seed under their own name, so neither refuses the other.
func TestCreateSandbox_ConcurrentRunsSeedOneVolume(t *testing.T) {
	f := newFakeDocker()
	var mu sync.Mutex
	seen := map[string]int{}
	f.onCreate = func(name string) {
		if strings.HasPrefix(name, driveSeedPrefix) {
			mu.Lock()
			seen[name]++
			mu.Unlock()
		}
	}
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
	drive := secondVolumeDrive()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			spec := testSpec()
			spec.RunID = uuid.New()
			spec.Drives = []types.DriveMount{drive}
			_, errs[i] = d.CreateSandbox(context.Background(), spec)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("run %d: %v", i, err)
		}
	}
	if len(seen) != 2 {
		t.Errorf("seed names = %v, want one per run", seen)
	}
}

// TestCreateSandbox_DriveSeedFailureRefuses: a drive that could not be seeded
// refuses the sandbox rather than starting it with an unwritable drive.
func TestCreateSandbox_DriveSeedFailureRefuses(t *testing.T) {
	f := newFakeDocker()
	f.failCreateContainer = driveSeedPrefix
	if _, err := createWithDrives(t, f, testSpec().RunID, secondVolumeDrive()); err == nil || !strings.Contains(err.Error(), "seed user drive volume") {
		t.Fatalf("CreateSandbox = %v, want the seed refusal", err)
	}
}

// TestCreateSandbox_DrivesAtOneOrForeignTargetRefused: two drives at one path
// would hide one; a managed drive under the shared root, or below its own
// folder, is not a path the server derives.
func TestCreateSandbox_DrivesAtOneOrForeignTargetRefused(t *testing.T) {
	twin := secondVolumeDrive()
	twin.Target = runner.DriveTarget
	cases := map[string][]types.DriveMount{
		"two at one target":      {*dockerVolumeDrive(), twin},
		"managed under shared":   {withTarget(secondVolumeDrive(), runner.SharedRoot+"/team")},
		"below its own folder":   {withTarget(secondVolumeDrive(), runner.DrivesRoot+"/team/x")},
		"the drives root itself": {withTarget(secondVolumeDrive(), runner.DrivesRoot)},
	}
	for name, drives := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := createWithDrives(t, newFakeDocker(), testSpec().RunID, drives...)
			if err == nil || !strings.Contains(err.Error(), errDriveTargetInvalid.Error()) {
				t.Fatalf("CreateSandbox = %v, want %v", err, errDriveTargetInvalid)
			}
		})
	}
}

// TestTeardown_RemovesALeftDriveSeed: a seed a crash left behind holds its
// drive's volume; the run's teardown removes it by its run-derived name.
func TestTeardown_RemovesALeftDriveSeed(t *testing.T) {
	f := newFakeDocker()
	id := uuid.New()
	f.containers[driveSeedName(id)] = &createdContainer{name: driveSeedName(id)}
	if err := newTestDriver(f).teardown(context.Background(), agentContainerName(id)); err != nil {
		t.Fatal(err)
	}
	if c := f.containers[driveSeedName(id)]; c != nil && !c.removed {
		t.Errorf("teardown left the drive seed %s", driveSeedName(id))
	}
}

// TestSweepOrphanedSandboxes_RemovesLeftoverDriveSeeds: a seed a crash left
// behind (older than minAge, carrying the seed's managed labels) is removed by
// the orphan sweep, whatever its run's state; a young seed, which a create may
// still be using, is left alone, and so is everything else.
func TestSweepOrphanedSandboxes_RemovesLeftoverDriveSeeds(t *testing.T) {
	f := newFakeDocker()
	oldRun, youngRun, liveRun := uuid.New(), uuid.New(), uuid.New()
	seedLabels := func(id uuid.UUID) map[string]string { return wardynLabels(id, componentDriveSeed, nil) }
	for _, id := range []uuid.UUID{oldRun, youngRun} {
		f.containers[driveSeedName(id)] = &createdContainer{name: driveSeedName(id)}
	}
	seedSandbox(f, liveRun)
	old := time.Now().Add(-time.Hour).Unix()
	f.listItems = []container.Summary{
		{ID: driveSeedName(oldRun), Created: old, Labels: seedLabels(oldRun)},
		{ID: driveSeedName(youngRun), Created: time.Now().Unix(), Labels: seedLabels(youngRun)},
		{ID: agentContainerName(liveRun), Created: old, Labels: wardynLabels(liveRun, componentAgent, nil)},
	}
	swept, err := newTestDriver(f).SweepOrphanedSandboxes(context.Background(), 10*time.Minute, func(uuid.UUID) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if swept != 1 || !f.containers[driveSeedName(oldRun)].removed {
		t.Errorf("swept %d; the aged seed removed = %v, want it removed", swept, f.containers[driveSeedName(oldRun)].removed)
	}
	if f.containers[driveSeedName(youngRun)].removed || f.containers[agentContainerName(liveRun)].removed {
		t.Error("the sweep removed a young seed or a live run's agent")
	}
}

func withTarget(d types.DriveMount, target string) types.DriveMount {
	d.Target = target
	return d
}
