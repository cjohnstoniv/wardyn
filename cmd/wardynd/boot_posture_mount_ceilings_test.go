// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// capturedCeilingWarns runs parseMountCeilings with the two ceiling values and
// returns the WARN lines it logged.
func capturedCeilingWarns(t *testing.T, memberRoots, driveRoots string) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	empty := ""
	f := &bootFlags{
		memberRoots:         &memberRoots,
		memberRootsMap:      &empty,
		memberWritableRoots: &empty,
		memberWritableDeny:  &empty,
		userDriveHostRoots:  &driveRoots,
	}
	if _, _, err := parseMountCeilings(f); err != nil {
		t.Fatalf("parseMountCeilings(%q, %q): %v", memberRoots, driveRoots, err)
	}
	return buf.String()
}

// The two ceiling parsers warn about their permitted-but-odd roots for
// OPPOSITE reasons, and internal/runner/user_drive_mount.go keeps them in
// separate sentences on purpose: the daemon's own $HOME is far too WIDE, while
// "/" and a root under a denied bind prefix are DEAD — they match NOTHING and
// refuse every drive. parseMountCeilings then prefixed EVERY line with
// "dangerously wide", which re-merged exactly what the library split and told
// an operator to go looking for the drive it wrongly allowed instead of for
// the drive it silently refused. internal/runner/user_drive_mount_test.go
// asserts the "NOTHING" wording believing that is what reaches the log; this
// pins the boot line itself, which is what an operator actually reads.
func TestParseMountCeilings_DeadRootIsNotAnnouncedAsWide(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drive string
		want  string
	}{
		{"slash-is-dead", "/", `bounds only the literal path`},
		{"denied-prefix-is-dead", "/dev/shm", `matches NOTHING`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := capturedCeilingWarns(t, "", tc.drive)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("boot log = %q, want it to carry %q", out, tc.want)
			}
			if strings.Contains(out, "wide") {
				t.Errorf("boot log announces a DEAD ceiling as wide:\n  %s\n"+
					"A root that matches nothing refuses every drive; calling it wide sends the operator hunting the opposite problem.", strings.TrimSpace(out))
			}
			if !strings.Contains(out, "WARDYN_USER_DRIVE_HOST_ROOTS") {
				t.Errorf("boot log does not name the variable that is set wrong:\n  %s", strings.TrimSpace(out))
			}
		})
	}
}

// The control: a genuinely WIDE ceiling must still read as one. $HOME on the
// drive list is the case internal/runner/user_drive_mount.go calls "far too
// WIDE", and its sentence has to survive the same change.
func TestParseMountCeilings_WideRootStillReadsAsWide(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	out := capturedCeilingWarns(t, home, home)
	if !strings.Contains(out, "this daemon's own home directory") {
		t.Errorf("drive $HOME ceiling did not carry its wide-open sentence:\n  %s", strings.TrimSpace(out))
	}
	if !strings.Contains(out, "bounded only by the credential dotfile deny-list") {
		t.Errorf("member $HOME ceiling did not carry its wide-open sentence:\n  %s", strings.TrimSpace(out))
	}
	if !strings.Contains(out, "WARDYN_MEMBER_WORKSPACE_ROOTS") || !strings.Contains(out, "WARDYN_USER_DRIVE_HOST_ROOTS") {
		t.Errorf("boot log does not name both variables:\n  %s", strings.TrimSpace(out))
	}
}

// res2-07 (the boot line): the overlap between the two ceilings reaches the
// operator's log, under its own prefix.
//
// Each parser vets only its own list, so a deployment whose member roots and
// drive host roots name ONE tree booted clean: every drive check passed, every
// member check passed, and a member could onboard the share as a workspace and
// bind every person's home through a surface that consults no drive allocation.
// parseMountCeilings is where both lists exist at once.
func TestParseMountCeilings_OverlappingCeilingsAreWarnedAbout(t *testing.T) {
	out := capturedCeilingWarns(t, "/srv/shares", "/srv/shares")
	if !strings.Contains(out, "mount ceilings overlap") {
		t.Errorf("boot log does not warn that the two ceilings name one tree:\n  %s", strings.TrimSpace(out))
	}
	for _, name := range []string{"WARDYN_MEMBER_WORKSPACE_ROOTS", "WARDYN_USER_DRIVE_HOST_ROOTS"} {
		if !strings.Contains(out, name) {
			t.Errorf("boot log does not name %s, so the operator cannot tell which of the two to change:\n  %s", name, strings.TrimSpace(out))
		}
	}
	// The control: two separate trees are an ordinary posture and say nothing.
	if clean := capturedCeilingWarns(t, "/home/projects", "/srv/shares"); strings.Contains(clean, "overlap") {
		t.Errorf("two separate ceilings warned about overlapping:\n  %s", strings.TrimSpace(clean))
	}
}
