// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// UserDriveLockedDir is the directory UserDriveFixture.Lock makes at the root
// of the allowed drive, holding UserDriveLockedFile.
const (
	UserDriveLockedDir  = "locked"
	UserDriveLockedFile = "secret"
)

// userDriveProbeFile is what the probe writes at the drive's root, and what
// UserDriveFixture.ReadBack reads from outside the sandbox.
const userDriveProbeFile = "wardyn-conformance-drive-probe"

// UserDriveFixture is a substrate's half of the UserDrives case: the drives it
// is held to, built by the driver-specific test file, because what the
// "ceiling" is differs by substrate (docker: WARDYN_USER_DRIVE_HOST_ROOTS over
// a host path; kubernetes: only a claim in the run's namespace).
type UserDriveFixture struct {
	// Mount is a drive inside the ceiling. The case binds it and requires the
	// agent (uid 1000) to write and read it back.
	Mount types.DriveMount
	// Lock makes UserDriveLockedDir at the drive's root, holding
	// UserDriveLockedFile, mode 0700 and owned by an identity other than uid
	// 1000. Called AFTER the sandbox holding Mount exists: on kubernetes that
	// create is what provisions the storage.
	Lock func(t *testing.T)
	// ReadBack, when non-nil, returns a file at the drive's root as seen from
	// OUTSIDE any sandbox (the host directory a docker share binds). It is
	// what proves the bind landed in the allowed directory and not on some
	// other storage that happens to be writable.
	ReadBack func(t *testing.T, name string) string
	// Refused are drives outside the ceiling; CreateSandbox must refuse each.
	Refused []RefusedDrive
}

// RefusedDrive is one drive the substrate must refuse.
type RefusedDrive struct {
	Name  string
	Mount types.DriveMount
	// WantErr is a substring the refusal must carry, so a failure for an
	// unrelated reason (an image pull, a missing network) cannot pass as the
	// drive refusal.
	WantErr string
	// Allowing, when non-nil, is the same substrate configured so that THIS
	// drive is inside its ceiling. The case mounts it there first and
	// requires the agent to write it: the positive control that makes the
	// refusal attributable to the ceiling rather than to anything else about
	// the fixture (a missing directory, an unmountable path).
	Allowing runner.Runner
}

// userDriveProbeScript reports the drive's verdicts as key=value lines (see
// managedFileProbeScript for why one exec). locked=PRESENT is what makes the
// three refusals after it mean something: a directory that never arrived
// refuses everything too.
func userDriveProbeScript(token string) string {
	return fmt.Sprintf(`D=%[1]s
echo uid=$(id -u)
if (printf '%%s' %[2]s > $D/%[3]s) 2>/dev/null; then echo w_drive=WROTE; else echo w_drive=REFUSED; fi
echo r_drive=$(cat $D/%[3]s 2>/dev/null)
if [ -d $D/%[4]s ]; then echo locked=PRESENT; else echo locked=ABSENT; fi
if (ls $D/%[4]s) >/dev/null 2>&1; then echo r_locked_dir=READ; else echo r_locked_dir=REFUSED; fi
if (cat $D/%[4]s/%[5]s) >/dev/null 2>&1; then echo r_locked_file=READ; else echo r_locked_file=REFUSED; fi
if (: > $D/%[4]s/wardyn-intruder) 2>/dev/null; then echo w_locked=WROTE; else echo w_locked=REFUSED; fi
`, runner.DriveTarget, token, userDriveProbeFile, UserDriveLockedDir, UserDriveLockedFile)
}

// testUserDrives is conformance case 8. A driver advertising
// Capabilities.UserDrives must:
//
//   - bind a drive inside its ceiling at runner.DriveTarget, writable by the
//     agent's uid 1000, on the storage the fixture named (ReadBack);
//   - keep ordinary permissions beneath it: a 0700 directory another identity
//     owns stays closed to the agent (no DAC bypass, no re-owning mount);
//   - refuse, at CreateSandbox, every drive outside that ceiling.
//
// The refusals are the case. A fixture whose refused drive would be refused
// for some other reason proves nothing about the ceiling, which is what
// RefusedDrive.Allowing and WantErr are for.
func testUserDrives(t *testing.T, r runner.Runner, opts Options) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.UserDrives {
		t.Skipf("driver %q does not advertise UserDrives; nothing to hold it to", r.Name())
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skipf("driver %q declares no confinement classes; a drive is not testable without a sandbox substrate", r.Name())
	}
	fx := opts.UserDrives
	if fx == nil {
		t.Skipf("driver %q advertises UserDrives but the test supplied no Options.UserDrives fixture", r.Name())
	}

	image := opts.image()
	if opts.AgentUserImage != "" {
		image = opts.AgentUserImage
	}
	withDrive := func(d types.DriveMount) func(*runner.SandboxSpec) {
		return func(spec *runner.SandboxSpec) {
			spec.Image = image
			spec.Drive = &d
		}
	}

	// ── 1. inside the ceiling: bound, writable, DAC intact ─────────────────
	sb := createStrongestSandboxWith(t, ctx, r, caps, opts, "UserDrives", withDrive(fx.Mount))
	fx.Lock(t)
	token := "wardyn-drive-" + uuid.NewString()
	got := ExecKeyValues(t, ctx, r, sb.Ref, userDriveProbeScript(token))
	if got["uid"] != "1000" {
		t.Fatalf("the drive probe ran as uid %q, want 1000 — as any other identity its refusals are not the agent's", got["uid"])
	}
	if got["w_drive"] != "WROTE" || got["r_drive"] != token {
		t.Fatalf("the agent could not write and read back its own drive at %s (w_drive=%s, r_drive=%q) — a drive the agent cannot use is not a drive",
			runner.DriveTarget, got["w_drive"], got["r_drive"])
	}
	if fx.ReadBack != nil {
		if host := fx.ReadBack(t, userDriveProbeFile); host != token {
			t.Errorf("outside the sandbox the drive's %s holds %q, want %q — the bind did not land on the directory the drive names", userDriveProbeFile, host, token)
		}
	}
	if got["locked"] != "PRESENT" {
		t.Fatalf("%s/%s is not visible inside the sandbox, so the refusals below would prove nothing — the fixture's Lock did not reach the bound storage", runner.DriveTarget, UserDriveLockedDir)
	}
	for _, c := range []struct{ key, want, what string }{
		{"r_locked_dir", "REFUSED", "listing"},
		{"r_locked_file", "REFUSED", "reading " + UserDriveLockedFile + " in"},
		{"w_locked", "REFUSED", "creating a file in"},
	} {
		if got[c.key] != c.want {
			t.Errorf("%s the 0700 directory %s/%s that uid 1000 does not own = %s, want %s — the drive's bind or the agent's identity bypasses ordinary permissions",
				c.what, runner.DriveTarget, UserDriveLockedDir, got[c.key], c.want)
		}
	}

	// ── 2. outside the ceiling: refused ────────────────────────────────────
	for _, rd := range fx.Refused {
		t.Run(rd.Name, func(t *testing.T) {
			if rd.Allowing != nil {
				acaps, err := rd.Allowing.Capabilities(ctx)
				if err != nil {
					t.Fatalf("Capabilities (allowing runner): %v", err)
				}
				asb := createStrongestSandboxWith(t, ctx, rd.Allowing, acaps, opts, "UserDrives positive control", withDrive(rd.Mount))
				ctl := ExecKeyValues(t, ctx, rd.Allowing, asb.Ref, userDriveProbeScript(token))
				if ctl["w_drive"] != "WROTE" || ctl["r_drive"] != token {
					t.Fatalf("positive control: with its ceiling widened to hold this drive, the agent still could not write it (w_drive=%s) — "+
						"the refusal below would not be the ceiling's", ctl["w_drive"])
				}
			}
			spec := minimalSpec(image)
			spec.ConfinementClass = caps.ConfinementClasses[len(caps.ConfinementClasses)-1]
			withDrive(rd.Mount)(&spec)
			sb, err := r.CreateSandbox(ctx, spec)
			if err == nil {
				stopCtx, stop := context.WithTimeout(context.Background(), conformanceCleanupTimeout)
				defer stop()
				_ = r.StopSandbox(stopCtx, sb.Ref)
				t.Fatalf("CreateSandbox mounted drive %q (backend %s, object %q), which is outside this substrate's ceiling", rd.Name, rd.Mount.Backend, rd.Mount.ObjectName)
			}
			if !strings.Contains(err.Error(), rd.WantErr) {
				t.Errorf("CreateSandbox refused drive %q, but not as a drive refusal: %v (want it to contain %q)", rd.Name, err, rd.WantErr)
			}
			t.Logf("refused as required: %v", err)
		})
	}
}
