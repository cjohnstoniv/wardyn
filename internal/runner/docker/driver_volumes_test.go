// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// driveRowID is the user_drives row every managed drive in this file belongs
// to — the value wardyn.drive carries, so a test can tell "this drive's
// volume" from "some other drive's volume that took the same name".
var driveRowID = uuid.MustParse("11111111-2222-3333-4444-555555555555")

// driveSubject is the principal every managed drive in this file resolved for.
// The fingerprint the driver stamps is DERIVED from it rather than written as a
// literal, so this file and the resolver cannot disagree about what
// types.DriveSubjectHash produces.
const driveSubject = "alice@corp.example"

// dockerVolumeDrive is a resolved MANAGED user drive, in the shape the control
// plane hands the driver (types.DriveMount — never a runner.Mount).
func dockerVolumeDrive() *types.DriveMount {
	return &types.DriveMount{
		DriveID:     driveRowID,
		Backend:     types.DriveBackendDockerVolume,
		ObjectName:  "wardyn-drive-alice",
		HomeName:    "alice",
		SubjectHash: types.DriveSubjectHash(driveSubject),
		Target:      runner.DriveTarget,
		ReadOnly:    true,
		Enforcement: types.StorageEnforcementNone,
	}
}

// createWithDrive runs CreateSandbox for a spec carrying drive, with the given
// deployment host-root ceiling on the driver's config, and returns the fake
// daemon (so a test can assert volume state) plus the agent's applied mounts.
func createWithDrive(t *testing.T, drive *types.DriveMount, hostRoots []string) (*fakeDocker, []mount.Mount, error) {
	t.Helper()
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", UserDriveHostRoots: hostRoots})

	spec := testSpec()
	spec.Drive = drive
	_, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		return f, nil, err
	}
	agent := f.containers[agentContainerName(spec.RunID)]
	if agent == nil {
		t.Fatalf("agent container not created")
	}
	return f, agent.host.Mounts, nil
}

// findMount returns the applied mount at target, or nil.
func findMount(mounts []mount.Mount, target string) *mount.Mount {
	for i := range mounts {
		if mounts[i].Target == target {
			return &mounts[i]
		}
	}
	return nil
}

// TestDriveVolume_CreatedWithLabelsAndNoOptions is the whole managed-drive
// contract in one place: the volume is created by NAME on the local driver,
// carries the drive/home labels an operator reclaims by, carries NO run label
// (teardown must never reap a member's persistent storage), and — the security
// half — carries NO DriverOpts, which is what keeps a share credential from
// ever reaching `docker volume inspect`.
func TestDriveVolume_CreatedWithLabelsAndNoOptions(t *testing.T) {
	f, mounts, err := createWithDrive(t, dockerVolumeDrive(), nil)
	if err != nil {
		t.Fatalf("CreateSandbox with a docker_volume drive: %v", err)
	}

	opts, ok := f.volumes["wardyn-drive-alice"]
	if !ok {
		t.Fatalf("drive volume was not created; volumes=%v", f.volumes)
	}
	if opts.Driver != "local" {
		t.Errorf("volume Driver = %q, want %q", opts.Driver, "local")
	}
	if len(opts.DriverOpts) != 0 {
		t.Errorf("volume DriverOpts = %v, want none — driver options are how a CIFS/NFS password leaks into `docker volume inspect`", opts.DriverOpts)
	}
	// The DRIVE ROW's id, never the volume's own name: the name is what the
	// volume already answers to, and only the id groups every principal's
	// object under the drive that allocated them (DESIGN §3.2).
	if opts.Labels[labelDrive] != driveRowID.String() {
		t.Errorf("label %s = %q, want the drive row's id %q — the object name is not a discriminator, it is the volume's own name",
			labelDrive, opts.Labels[labelDrive], driveRowID)
	}
	if opts.Labels[labelDriveHome] != "alice" {
		t.Errorf("label %s = %q, want %q", labelDriveHome, opts.Labels[labelDriveHome], "alice")
	}
	// The PRINCIPAL's fingerprint — the discriminator neither the name nor the
	// drive id can supply, since a home template can fold two people onto one
	// home INSIDE one drive.
	if got, want := opts.Labels[labelDriveSubject], types.DriveSubjectHash(driveSubject); got != want {
		t.Errorf("label %s = %q, want the subject digest %q", labelDriveSubject, got, want)
	}
	// And it is a DIGEST: `docker volume inspect` echoes labels to anyone who can
	// reach the daemon, so the claim itself must never appear.
	for k, v := range opts.Labels {
		if strings.Contains(v, driveSubject) || strings.Contains(v, "@") {
			t.Errorf("label %s = %q carries the subject claim — a label is echoed by `docker volume inspect`", k, v)
		}
	}
	if _, has := opts.Labels[labelRun]; has {
		t.Errorf("drive volume carries a %s label (%v) — teardown reaps by that label and would delete the member's storage", labelRun, opts.Labels)
	}

	m := findMount(mounts, runner.DriveTarget)
	if m == nil {
		t.Fatalf("drive not mounted at %s; mounts=%+v", runner.DriveTarget, mounts)
	}
	if m.Type != mount.TypeVolume {
		t.Errorf("drive mount Type = %q, want %q (a managed drive has no host path)", m.Type, mount.TypeVolume)
	}
	if m.Source != "wardyn-drive-alice" {
		t.Errorf("drive mount Source = %q, want the volume name", m.Source)
	}
	if !m.ReadOnly {
		t.Errorf("drive mount ReadOnly = false, want true (the resolved allocation was read-only)")
	}
}

// TestDriveVolume_WritableDriveMountsWritable: ReadOnly comes from the resolved
// mount, so an allocation an admin made writable is bound writable.
func TestDriveVolume_WritableDriveMountsWritable(t *testing.T) {
	drive := dockerVolumeDrive()
	drive.ReadOnly = false
	_, mounts, err := createWithDrive(t, drive, nil)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	m := findMount(mounts, runner.DriveTarget)
	if m == nil {
		t.Fatalf("drive not mounted; mounts=%+v", mounts)
	}
	if m.ReadOnly {
		t.Errorf("drive mount ReadOnly = true, want false (the allocation is writable)")
	}
}

// TestEnsureDriveVolume_IdempotentByName: the SECOND run for the same person
// must not create anything. Idempotence is the contract (a drive is created on
// first use and lived in thereafter), not merely the end state.
func TestEnsureDriveVolume_IdempotentByName(t *testing.T) {
	f := newFakeDocker()
	drive := dockerVolumeDrive()

	if err := ensureDriveVolume(context.Background(), f, drive); err != nil {
		t.Fatalf("first ensureDriveVolume: %v", err)
	}
	if f.volumeCreates != 1 {
		t.Fatalf("first call made %d VolumeCreate calls, want 1", f.volumeCreates)
	}
	if err := ensureDriveVolume(context.Background(), f, drive); err != nil {
		t.Fatalf("second ensureDriveVolume: %v", err)
	}
	if f.volumeCreates != 1 {
		t.Errorf("second call made another VolumeCreate (total %d) — an existing drive must be inspected, not re-created", f.volumeCreates)
	}
}

// TestEnsureDriveVolume_AdoptsOnlyWardynsOwnShape is the credential guardrail's
// second half. The first half is "Wardyn never CREATES a volume with driver
// options"; this is "Wardyn never MOUNTS one it did not create". The two are
// separable, and only asserting the first left the gap: `docker volume create
// --opt type=cifs --opt o=…,password=… wardyn-drive-alice` is a supported
// operator gesture, and an inspect-hit arm that returned nil on any name match
// would bind that share — password and all — into whichever member resolves to
// that object name.
//
// A matching volume (local driver, no options) is still REUSED, which is the
// ordinary second-run path and must not become a refusal.
func TestEnsureDriveVolume_AdoptsOnlyWardynsOwnShape(t *testing.T) {
	seed := func(opts client.VolumeCreateOptions) *fakeDocker {
		f := newFakeDocker()
		f.volumes["wardyn-drive-alice"] = opts
		return f
	}

	t.Run("foreign-driver", func(t *testing.T) {
		f := seed(client.VolumeCreateOptions{Name: "wardyn-drive-alice", Driver: "some-csi-plugin"})
		err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive())
		if err == nil {
			t.Fatal("a same-named volume on a FOREIGN driver must be refused, not adopted as a managed drive")
		}
		if !strings.Contains(err.Error(), "some-csi-plugin") {
			t.Errorf("the refusal should name the driver it found, got: %v", err)
		}
	})

	t.Run("options-bearing", func(t *testing.T) {
		// The dangerous one: the local driver, but carrying share options. The
		// password is deliberately NOT asserted on — the point is that Wardyn
		// refuses before it can ever mount it.
		f := seed(client.VolumeCreateOptions{
			Name:       "wardyn-drive-alice",
			Driver:     "local",
			DriverOpts: map[string]string{"type": "cifs", "device": "//nas.corp/share", "o": "username=svc,password=hunter2"},
		})
		if err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive()); err == nil {
			t.Fatal("a same-named volume carrying DRIVER OPTIONS must be refused — adopting it would mount an operator's share on a credential Wardyn never chose")
		}
	})

	// A DIFFERENT DRIVE's volume, Wardyn-shaped in every other way. Object names
	// are per-PRINCIPAL (DriveObjectName), so two drives whose home templates
	// collide resolve to one name — and adopting on the name alone would hand
	// this member the other drive's storage, plus a place to write into it when
	// the allocation is writable.
	t.Run("another-drives-id", func(t *testing.T) {
		other := uuid.MustParse("99999999-8888-7777-6666-555555555555")
		f := seed(client.VolumeCreateOptions{
			Name:   "wardyn-drive-alice",
			Driver: "local",
			Labels: map[string]string{labelManaged: "true", labelDrive: other.String(), labelDriveHome: "alice"},
		})
		err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive())
		if err == nil {
			t.Fatal("a volume labelled with ANOTHER drive's id must be refused, not adopted")
		}
		for _, want := range []string{other.String(), driveRowID.String()} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal should name both ids so an admin can tell which drives collided, missing %q in: %v", want, err)
			}
		}
	})

	// ANOTHER PRINCIPAL's volume, on the SAME drive — the case the id check
	// above cannot see, because the id matches. One `email_local` drive derives
	// one home for alice@corp.example and alice@acquired.example, so both
	// allocations resolve to `wardyn-drive-alice` labelled with this same drive.
	// The write boundary and the resolver both refuse that pair now; this is the
	// third layer, at the object, for a volume created before either did.
	t.Run("another-principals-subject", func(t *testing.T) {
		f := seed(client.VolumeCreateOptions{
			Name:   "wardyn-drive-alice",
			Driver: "local",
			Labels: map[string]string{
				labelManaged: "true", labelDrive: driveRowID.String(), labelDriveHome: "alice",
				labelDriveSubject: types.DriveSubjectHash("alice@acquired.example"),
			},
		})
		err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive())
		if err == nil {
			t.Fatal("a volume allocated to ANOTHER principal was adopted — that is one person's files handed to another")
		}
		if !strings.Contains(err.Error(), labelDriveSubject) {
			t.Errorf("the refusal should name the label that disagreed, got: %v", err)
		}
		// The digest is a person's fingerprint and must not be copied into an
		// error string an operator will paste into a ticket.
		for _, digest := range []string{types.DriveSubjectHash("alice@acquired.example"), types.DriveSubjectHash(driveSubject)} {
			if strings.Contains(err.Error(), digest) {
				t.Errorf("the refusal reproduces a subject digest, which is a fingerprint of a person: %v", err)
			}
		}
	})

	t.Run("wardyn-shaped-is-reused", func(t *testing.T) {
		f := seed(client.VolumeCreateOptions{
			Name:   "wardyn-drive-alice",
			Driver: "local",
			Labels: map[string]string{
				labelManaged: "true", labelDrive: driveRowID.String(), labelDriveHome: "alice",
				labelDriveSubject: types.DriveSubjectHash(driveSubject),
			},
		})
		if err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive()); err != nil {
			t.Fatalf("a Wardyn-shaped volume must be REUSED, not refused: %v", err)
		}
		if f.volumeCreates != 0 {
			t.Errorf("reuse made %d VolumeCreate calls, want 0", f.volumeCreates)
		}
	})

	// A volume with the drive's labels but NO subject label is still adopted —
	// the same restore path the label-less arm below covers, and every volume
	// created before wardyn.subject existed. Refusing here would make this
	// release an outage for every deployment that already has a managed drive.
	t.Run("subject-label-less-is-adopted", func(t *testing.T) {
		f := seed(client.VolumeCreateOptions{
			Name:   "wardyn-drive-alice",
			Driver: "local",
			Labels: map[string]string{labelManaged: "true", labelDrive: driveRowID.String(), labelDriveHome: "alice"},
		})
		if err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive()); err != nil {
			t.Fatalf("a volume predating the subject label must still be adopted: %v", err)
		}
	})

	// A LABEL-LESS volume is still adopted, and that is the documented restore
	// path: `docker volume create` + copy the data back leaves no labels unless
	// the operator passes them, and every volume created before wardyn.drive
	// carried an id has none. Refusing here would turn a restore-from-backup
	// into an outage; the refusal above is for a label that names a DIFFERENT
	// drive, which is a collision, not an absence.
	t.Run("label-less-is-adopted", func(t *testing.T) {
		f := seed(client.VolumeCreateOptions{Name: "wardyn-drive-alice", Driver: "local"})
		if err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive()); err != nil {
			t.Fatalf("a label-less Wardyn-shaped volume must be adopted (the hand-restore path): %v", err)
		}
	})
}

// driveLabelState is one label's three states on an existing volume: not
// written at all (a restore, or a volume older than the label), written with
// this drive's/principal's value, and written with somebody else's. Absent and
// Other are the two that read alike from a distance and mean opposite things —
// absence is adopted, disagreement is refused — which is the whole reason the
// fold below enumerates them rather than sampling.
type driveLabelState int

const (
	driveLabelAbsent driveLabelState = iota
	driveLabelSame
	driveLabelOther
)

func (s driveLabelState) String() string { return [...]string{"absent", "same", "other"}[s] }

// TestEnsureDriveVolume_AdoptionRulesComposeOnBothArms closes the two
// hand-written tables above — TestEnsureDriveVolume_AdoptsOnlyWardynsOwnShape
// (the inspect hit) and TestEnsureDriveVolume_CreateThenVerify (the create that
// lost the race) — over every combination of the four things driveVolumeAdoptable
// looks at, through both doors.
//
// Two properties neither named table can state on its own:
//
//   - THE RULES ARE A CONJUNCTION. Each named case varies one dimension against
//     an otherwise-clean volume, so nothing pins what happens when two disagree
//     at once — and "refuse if the driver is foreign OR an option is set OR a
//     label names somebody else" is a rule a refactor can turn into an
//     any-two-of-three without failing a single case above.
//   - THE TWO DOORS ASK THE SAME QUESTION. The create arm is a SECOND entry
//     into the same decision, reached only on a first run that lost a race, and
//     the named table for it covers three shapes out of nine. A door that
//     diverged would diverge exactly where nobody looks: once, on a first run,
//     under contention.
//
// The verb count is asserted per cell for the same reason: an adoption is a
// reuse, so the inspect arm must never create, and the race arm must create
// exactly once and never twice.
func TestEnsureDriveVolume_AdoptionRulesComposeOnBothArms(t *testing.T) {
	const name = "wardyn-drive-alice"
	otherDrive := uuid.MustParse("99999999-8888-7777-6666-555555555555")
	label := func(s driveLabelState, same, other string) (string, bool) {
		switch s {
		case driveLabelSame:
			return same, true
		case driveLabelOther:
			return other, true
		}
		return "", false
	}
	for _, raced := range []bool{false, true} {
		for _, driver := range []string{"local", "some-csi-plugin"} {
			for _, opts := range []map[string]string{nil, {"type": "cifs", "o": "username=svc,password=hunter2"}} {
				for _, drive := range []driveLabelState{driveLabelAbsent, driveLabelSame, driveLabelOther} {
					for _, subject := range []driveLabelState{driveLabelAbsent, driveLabelSame, driveLabelOther} {
						// Wardyn's own shape, and nobody else's label: the local
						// driver, no options, and no label naming another drive or
						// another principal. An ABSENT label is not a disagreement.
						wantAdopt := driver == "local" && len(opts) == 0 &&
							drive != driveLabelOther && subject != driveLabelOther
						labels := map[string]string{labelManaged: "true", labelDriveHome: "alice"}
						if v, ok := label(drive, driveRowID.String(), otherDrive.String()); ok {
							labels[labelDrive] = v
						}
						if v, ok := label(subject, types.DriveSubjectHash(driveSubject), types.DriveSubjectHash("alice@acquired.example")); ok {
							labels[labelDriveSubject] = v
						}
						arm := map[bool]string{false: "inspect-hit", true: "create-race"}[raced]
						optName := map[bool]string{false: "none", true: "cifs"}[len(opts) > 0]
						t.Run(arm+"/driver="+driver+"/opts="+optName+"/drive="+drive.String()+"/subject="+subject.String(), func(t *testing.T) {
							f := newFakeDocker()
							f.volumes[name] = client.VolumeCreateOptions{Name: name, Driver: driver, Labels: labels, DriverOpts: opts}
							// The race, modelled exactly: the volume EXISTS and the
							// first inspect misses it, so the create is attempted and
							// comes back against somebody else's volume.
							f.volumeInspectMissesExisting = raced

							err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive())
							if (err == nil) != wantAdopt {
								t.Errorf("ensureDriveVolume = %v, want adopt=%v", err, wantAdopt)
							}
							wantCreates := map[bool]int{false: 0, true: 1}[raced]
							if f.volumeCreates != wantCreates {
								t.Errorf("VolumeCreate calls = %d, want %d — an existing volume is inspected, never re-created", f.volumeCreates, wantCreates)
							}
						})
					}
				}
			}
		}
	}
}

// TestEnsureDriveVolume_CreateThenVerify is the FIRST-RUN RACE, and it is the
// one window every check above had.
//
// Two colliding drives start their first run at the same moment. Both inspect,
// both see 404, both create. Docker's VolumeCreate against a name that already
// resolves SUCCEEDS and hands back the EXISTING volume — its driver, its options
// and its labels — applying none of the ones it was given. So the loser's create
// looked identical to the winner's, and every refusal lived in the inspect arm it
// legitimately never reached: it adopted, once, silently, and only on the first
// run (every later run inspects and refuses, which is what made this so easy to
// miss).
//
// The fake models the race exactly: the volume EXISTS, and inspect answers
// not-found for it.
func TestEnsureDriveVolume_CreateThenVerify(t *testing.T) {
	seedRaced := func(labels map[string]string, driver string, opts map[string]string) *fakeDocker {
		f := newFakeDocker()
		f.volumes["wardyn-drive-alice"] = client.VolumeCreateOptions{
			Name: "wardyn-drive-alice", Driver: driver, Labels: labels, DriverOpts: opts,
		}
		f.volumeInspectMissesExisting = true
		return f
	}

	t.Run("another-drives-volume", func(t *testing.T) {
		other := uuid.MustParse("99999999-8888-7777-6666-555555555555")
		f := seedRaced(map[string]string{labelManaged: "true", labelDrive: other.String(), labelDriveHome: "alice"}, "local", nil)
		err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive())
		if err == nil {
			t.Fatal("a create that lost the race adopted ANOTHER drive's volume — the create's result must run the same checks the inspect does")
		}
		if !strings.Contains(err.Error(), other.String()) {
			t.Errorf("the refusal should name the drive it found, got: %v", err)
		}
	})

	t.Run("another-principals-volume", func(t *testing.T) {
		f := seedRaced(map[string]string{
			labelManaged: "true", labelDrive: driveRowID.String(), labelDriveHome: "alice",
			labelDriveSubject: types.DriveSubjectHash("alice@acquired.example"),
		}, "local", nil)
		if err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive()); err == nil {
			t.Fatal("a create that lost the race adopted ANOTHER principal's volume")
		}
	})

	// The credential guardrail through the same window: an operator's precreated
	// `--opt type=cifs` volume must not become somebody's drive because a create
	// arrived a moment after it.
	t.Run("options-bearing", func(t *testing.T) {
		f := seedRaced(nil, "local", map[string]string{"type": "cifs", "o": "username=svc,password=hunter2"})
		if err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive()); err == nil {
			t.Fatal("a create that lost the race adopted an OPTIONS-BEARING volume — the share credential guardrail has a first-run hole")
		}
	})

	// The positive control: the create that WON applies its own options, so the
	// verify passes and the ordinary first run is untouched.
	t.Run("the-winner-is-unaffected", func(t *testing.T) {
		f := newFakeDocker()
		if err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive()); err != nil {
			t.Fatalf("an uncontended first run was refused by its own create: %v", err)
		}
		if f.volumeCreates != 1 {
			t.Errorf("made %d VolumeCreate calls, want 1", f.volumeCreates)
		}
	})
}

// TestCreateSandbox_HostPathDriveAllocatesNothing: only docker_volume allocates.
// A host_path drive is a bind of a tree the operator already mounted, and Wardyn
// never mkdirs on a share — asserted through the real dispatch path rather than
// against a backend guard inside ensureDriveVolume, which its single caller
// (driveMount's docker_volume arm) makes unreachable.
func TestCreateSandbox_HostPathDriveAllocatesNothing(t *testing.T) {
	root, home := driveHostRoot(t)
	f, mounts, err := createWithDrive(t, hostPathDrive(home), []string{root})
	if err != nil {
		t.Fatalf("CreateSandbox with an in-root host_path drive: %v", err)
	}
	if f.volumeCreates != 0 {
		t.Errorf("a host_path drive made %d VolumeCreate calls, want 0", f.volumeCreates)
	}
	// (the bind's source/mode are asserted by TestCreateSandbox_HostPathDriveApplied)
	if m := findMount(mounts, runner.DriveTarget); m != nil && m.Type != mount.TypeBind {
		t.Errorf("host_path drive mount Type = %q, want %q — a share is bound, never allocated", m.Type, mount.TypeBind)
	}
}

// TestEnsureDriveVolume_FailsClosed: an unreadable daemon, a refused create and
// an empty object name each abort rather than proceed. The empty-name arm is
// the sharpest: VolumeCreate with no Name allocates an ANONYMOUS volume, which
// would be storage that is neither the member's drive nor reclaimable by name.
func TestEnsureDriveVolume_FailsClosed(t *testing.T) {
	t.Run("inspect-error", func(t *testing.T) {
		f := newFakeDocker()
		f.failVolumeInspect = true
		err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive())
		if err == nil {
			t.Fatal("an unreadable daemon must fail closed, got nil")
		}
		if f.volumeCreates != 0 {
			t.Errorf("created a volume over an unreadable daemon (%d calls)", f.volumeCreates)
		}
	})
	t.Run("create-error", func(t *testing.T) {
		f := newFakeDocker()
		f.failVolumeCreate = true
		if err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive()); err == nil {
			t.Fatal("a refused VolumeCreate must fail closed, got nil")
		}
	})
	t.Run("empty-object-name", func(t *testing.T) {
		f := newFakeDocker()
		drive := dockerVolumeDrive()
		drive.ObjectName = ""
		err := ensureDriveVolume(context.Background(), f, drive)
		if err == nil {
			t.Fatal("an empty object name must fail closed (it would create an anonymous volume), got nil")
		}
		if f.volumeCreates != 0 {
			t.Errorf("made %d VolumeCreate calls for an unnamed drive, want 0", f.volumeCreates)
		}
	})
}

// TestCreateSandbox_DriveVolumeCreateFailureAbortsSandbox: a drive that cannot
// be allocated fails the whole CreateSandbox closed. Coming up WITHOUT the
// drive would hand the member a sandbox whose /home/agent/drive is an ordinary
// container directory — writable, empty, and gone at teardown.
func TestCreateSandbox_DriveVolumeCreateFailureAbortsSandbox(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.failVolumeCreate = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})

	spec := testSpec()
	spec.Drive = dockerVolumeDrive()
	if _, err := d.CreateSandbox(context.Background(), spec); err == nil {
		t.Fatal("CreateSandbox must fail closed when the drive volume cannot be created, got nil")
	}
	if f.containers[agentContainerName(spec.RunID)] != nil {
		t.Error("agent container exists after a failed drive allocation — the refusal must precede ContainerCreate")
	}
}

// TestCreateSandbox_NoDriveTouchesNothing: the overwhelmingly common run (no
// drive) must be byte-identical to before this feature — no volume calls, no
// mount at the reserved target.
func TestCreateSandbox_NoDriveTouchesNothing(t *testing.T) {
	f, mounts, err := createWithDrive(t, nil, nil)
	if err != nil {
		t.Fatalf("CreateSandbox without a drive: %v", err)
	}
	if f.volumeCreates != 0 {
		t.Errorf("a run with no drive made %d VolumeCreate calls, want 0", f.volumeCreates)
	}
	if m := findMount(mounts, runner.DriveTarget); m != nil {
		t.Errorf("a run with no drive got a mount at %s: %+v", runner.DriveTarget, *m)
	}
}

// TestCreateSandbox_K8sBackedDriveIsAnError: a k8s backend on the Docker driver
// is refused, never silently skipped. types.ValidateUserDrive refuses the
// combination at the write boundary, so reaching here means a stored row
// outlived a change of runner target — and a writable drive that silently did
// not mount would let a session's work land in a directory that dies with the
// run.
func TestCreateSandbox_K8sBackedDriveIsAnError(t *testing.T) {
	for _, backend := range []types.DriveBackend{types.DriveBackendK8sPVC, types.DriveBackendK8sPVCStatic} {
		t.Run(string(backend), func(t *testing.T) {
			drive := dockerVolumeDrive()
			drive.Backend = backend
			f, _, err := createWithDrive(t, drive, nil)
			if err == nil {
				t.Fatalf("a %s drive on the docker runner must FAIL CLOSED, got nil", backend)
			}
			if !strings.Contains(err.Error(), string(backend)) {
				t.Errorf("the error should name the backend it cannot mount, got: %v", err)
			}
			if f.containers[agentContainerName(testSpec().RunID)] != nil {
				t.Error("agent container exists after the refusal")
			}
		})
	}
}
