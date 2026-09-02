// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// driveFor builds a drive with a FIXED id, so a hash-template case can assert
// determinism across calls rather than merely "it returned something".
func driveFor(t *testing.T, backend DriveBackend, tmpl HomeTemplate) UserDrive {
	t.Helper()
	id, err := uuid.Parse("11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Fatalf("parse fixture uuid: %v", err)
	}
	return UserDrive{ID: id, Name: "Corp NAS", Backend: backend, HomeTemplate: tmpl}
}

// TestDriveHomeName is the home-name table — the derivation every mount, every
// object name and every offboarding command depends on.
//
// The rule under test is not "it produces a string". It is that a home name is
// either DERIVED or REFUSED, never guessed: a fabricated segment lands one
// member in another member's directory, or outside the drive entirely, and both
// are silent.
func TestDriveHomeName(t *testing.T) {
	const sub = "sub-abc"

	t.Run("hash is deterministic, prefixed and short", func(t *testing.T) {
		d := driveFor(t, DriveBackendDockerVolume, HomeTemplateHash)
		first, err := DriveHomeName(d, sub, "")
		if err != nil {
			t.Fatalf("hash home: %v", err)
		}
		again, err := DriveHomeName(d, sub, "")
		if err != nil {
			t.Fatalf("hash home (second call): %v", err)
		}
		if first != again {
			t.Errorf("hash home is not deterministic: %q then %q — a member's drive would move under them", first, again)
		}
		if !strings.HasPrefix(first, "d-") {
			t.Errorf("hash home %q lacks the d- prefix that keeps it a legal DNS label", first)
		}
		if len(first) != 2+driveHomeHashLen {
			t.Errorf("hash home %q is %d chars, want %d", first, len(first), 2+driveHomeHashLen)
		}
		if !driveHomeSegmentRe.MatchString(first) {
			t.Errorf("hash home %q is not a valid segment", first)
		}
	})

	t.Run("hash separates subjects and drives", func(t *testing.T) {
		// The drive id is IN the digest so one member's two drives never
		// collide, and the subject is in it so two members never share one.
		d := driveFor(t, DriveBackendDockerVolume, HomeTemplateHash)
		mine, _ := DriveHomeName(d, sub, "")
		theirs, _ := DriveHomeName(d, "sub-xyz", "")
		if mine == theirs {
			t.Errorf("two subjects hashed to the same home %q — one member would mount another's drive", mine)
		}
		other := d
		other.ID = uuid.New()
		second, _ := DriveHomeName(other, sub, "")
		if mine == second {
			t.Errorf("two drives hashed to the same home %q for one subject", mine)
		}
	})

	// An empty template is the column default and must behave as `hash`, not as
	// an error: a row written before a template existed still has to resolve.
	t.Run("an empty template is hash", func(t *testing.T) {
		d := driveFor(t, DriveBackendDockerVolume, "")
		got, err := DriveHomeName(d, sub, "")
		if err != nil || !strings.HasPrefix(got, "d-") {
			t.Errorf("empty template = %q, %v; want the hash form", got, err)
		}
	})

	for _, tc := range []struct {
		name     string
		tmpl     HomeTemplate
		subject  string
		override string
		want     string
		wantErr  bool
	}{
		{name: "sub template takes the claim", tmpl: HomeTemplateSub, subject: "sub-abc", want: "sub-abc"},
		{name: "claims are lowercased like every other subject", tmpl: HomeTemplateSub, subject: "SUB-ABC", want: "sub-abc"},
		{name: "email_local takes the part before the @", tmpl: HomeTemplateEmailLocal, subject: "Alice.Smith@Corp.Example", want: "alice.smith"},
		// email_local on something that is not an email is the "never a guess"
		// rule: taking the whole string would name a directory nobody granted.
		{name: "email_local refuses a non-email claim", tmpl: HomeTemplateEmailLocal, subject: "sub-abc", wantErr: true},
		// There is no whole-email template to test: an address carries an @,
		// which is not a legal segment character, so such a template could only
		// resolve for a claim that was not an address. email_local above is the
		// corporate-home case it looked like it served.
		{name: "an unknown template is refused, never guessed", tmpl: HomeTemplate("email"), subject: "alice", wantErr: true},
		// A LEADING DOT is the dotfile class the member-mount rules refuse by
		// segment; ".." is the traversal, excluded by the same clause.
		{name: "a leading dot is refused", tmpl: HomeTemplateSub, subject: ".ssh", wantErr: true},
		{name: "a traversal is refused", tmpl: HomeTemplateSub, subject: "..", wantErr: true},
		{name: "a separator is refused", tmpl: HomeTemplateSub, subject: "alice/bob", wantErr: true},
		{name: "a space is refused", tmpl: HomeTemplateSub, subject: "alice smith", wantErr: true},
		{name: "an empty claim is refused", tmpl: HomeTemplateSub, subject: "", wantErr: true},
		{name: "a 63-character claim is the ceiling", tmpl: HomeTemplateSub, subject: strings.Repeat("a", 63), want: strings.Repeat("a", 63)},
		{name: "a 64-character claim is refused", tmpl: HomeTemplateSub, subject: strings.Repeat("a", 64), wantErr: true},
		// The override is an admin stating a fact about a filesystem Wardyn does
		// not own, so it out-votes every template — including hash.
		{name: "an override beats a claim template", tmpl: HomeTemplateSub, subject: "sub-abc", override: "bsmith", want: "bsmith"},
		{name: "an override beats the hash", tmpl: HomeTemplateHash, subject: "sub-abc", override: "bsmith", want: "bsmith"},
		{name: "an override is lowercased too", tmpl: HomeTemplateHash, subject: "sub-abc", override: "BSmith", want: "bsmith"},
		{name: "an invalid override is refused, not ignored", tmpl: HomeTemplateHash, subject: "sub-abc", override: ".ssh", wantErr: true},
		{name: "an unknown template is refused", tmpl: HomeTemplate("uid"), subject: "alice", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := driveFor(t, DriveBackendHostPath, tc.tmpl)
			got, err := DriveHomeName(d, tc.subject, tc.override)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("DriveHomeName(%q, %q) = %q, want an error — a guessed home is another member's directory",
						tc.tmpl, tc.subject, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DriveHomeName(%q, %q): %v", tc.tmpl, tc.subject, err)
			}
			if got != tc.want {
				t.Errorf("DriveHomeName(%q, %q) = %q, want %q", tc.tmpl, tc.subject, got, tc.want)
			}
		})
	}
}

// TestDriveObjectName pins what the runner asks the substrate for. The PVC form
// carries the drive's slug and the volume form does not, and an operator
// reading `kubectl get pvc` has no other way to tell two drives apart.
func TestDriveObjectName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		backend  DriveBackend
		hostRoot string
		drive    string
		home     string
		want     string
	}{
		{name: "docker volume", backend: DriveBackendDockerVolume, drive: "Corp NAS", home: "d-abc", want: "wardyn-drive-d-abc"},
		{name: "pvc carries the drive slug", backend: DriveBackendK8sPVC, drive: "Corp NAS", home: "d-abc", want: "wardyn-drive-corp-nas-d-abc"},
		{name: "static pvc is named the same way", backend: DriveBackendK8sPVCStatic, drive: "Corp NAS", home: "alice", want: "wardyn-drive-corp-nas-alice"},
		// Punctuation folds to a single dash and the edges are trimmed, so one
		// drive can never produce two different claim names.
		{name: "a punctuated name folds to one dash", backend: DriveBackendK8sPVC, drive: "  Corp NAS (eng)! ", home: "alice", want: "wardyn-drive-corp-nas-eng-alice"},
		{name: "host path joins the root", backend: DriveBackendHostPath, hostRoot: "/srv/homes", drive: "Corp NAS", home: "alice", want: "/srv/homes/alice"},
		{name: "host path is cleaned", backend: DriveBackendHostPath, hostRoot: "/srv/homes/", drive: "Corp NAS", home: "alice", want: "/srv/homes/alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := UserDrive{ID: uuid.New(), Name: tc.drive, Backend: tc.backend, HostRoot: tc.hostRoot}
			if got := DriveObjectName(d, tc.home); got != tc.want {
				t.Errorf("DriveObjectName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDriveBackendMapping pins the three derivations a backend carries: which
// runner can mount it, whether Wardyn allocates it, and what actually binds its
// bytes. All three are switch statements over one closed enum, and all three
// have a default arm whose answer is the conservative one.
func TestDriveBackendMapping(t *testing.T) {
	for _, tc := range []struct {
		backend DriveBackend
		valid   bool
		target  string
		kind    DriveKind
		enforce StorageEnforcement
	}{
		{DriveBackendDockerVolume, true, "docker", DriveKindManaged, StorageEnforcementNone},
		{DriveBackendHostPath, true, "docker", DriveKindShare, StorageEnforcementExternal},
		{DriveBackendK8sPVC, true, "k8s", DriveKindManaged, StorageEnforcementRequest},
		{DriveBackendK8sPVCStatic, true, "k8s", DriveKindShare, StorageEnforcementExternal},
		// An unknown backend must never read as managed (the control plane
		// would provision storage for a row it does not understand) and never
		// as enforced (an admin would be shown a cap that binds nothing).
		{DriveBackend("nfs_direct"), false, "", DriveKindShare, StorageEnforcementNone},
	} {
		t.Run(string(tc.backend), func(t *testing.T) {
			if got := tc.backend.Valid(); got != tc.valid {
				t.Errorf("Valid() = %v, want %v", got, tc.valid)
			}
			if got := tc.backend.RunnerTarget(); got != tc.target {
				t.Errorf("RunnerTarget() = %q, want %q", got, tc.target)
			}
			if got := tc.backend.Kind(); got != tc.kind {
				t.Errorf("Kind() = %q, want %q", got, tc.kind)
			}
			if got := EnforcementFor(tc.backend); got != tc.enforce {
				t.Errorf("EnforcementFor() = %q, want %q", got, tc.enforce)
			}
		})
	}
	// The enforcement vocabulary is introduced complete: `filesystem` has no v1
	// backend and is named here so a future quota lane reuses the word instead
	// of inventing a fifth one.
	if StorageEnforcementFilesystem == "" {
		t.Error("StorageEnforcementFilesystem is unset")
	}
}

// TestValidateUserDrive is the write-boundary matrix. Two arms are the reason
// the function exists at all: a backend must match the runner this deployment
// dispatches to (otherwise the row is a 422 on somebody's run three days
// later), and a SHARE cannot be hashed (Wardyn does not create directories on
// somebody else's NAS, so a hashed home names one that will never exist).
func TestValidateUserDrive(t *testing.T) {
	ok := func(mut func(*UserDrive)) UserDrive {
		d := UserDrive{Name: "Corp NAS", Backend: DriveBackendDockerVolume}
		mut(&d)
		return d
	}
	for _, tc := range []struct {
		name    string
		drive   UserDrive
		target  string
		wantErr bool
	}{
		{name: "a docker volume on a docker deployment", drive: ok(func(*UserDrive) {}), target: "docker"},
		{name: "a k8s drive on a docker deployment is refused",
			drive: ok(func(d *UserDrive) { d.Backend = DriveBackendK8sPVC; d.HomeTemplate = HomeTemplateHash }), target: "docker", wantErr: true},
		{name: "a docker drive on a k8s deployment is refused", drive: ok(func(*UserDrive) {}), target: "k8s", wantErr: true},
		// A control plane that dispatches nowhere has no business registering
		// storage for it — fail closed rather than accept an unmountable row.
		{name: "a headless deployment accepts no backend", drive: ok(func(*UserDrive) {}), target: "none", wantErr: true},
		{name: "an unknown backend is refused", drive: ok(func(d *UserDrive) { d.Backend = "nfs_direct" }), target: "docker", wantErr: true},
		{name: "a name is required", drive: ok(func(d *UserDrive) { d.Name = "  " }), target: "docker", wantErr: true},
		// The name is folded into a PVC's name, so one that folds to nothing
		// would produce an object name with an empty middle.
		{name: "a name that folds to nothing is refused", drive: ok(func(d *UserDrive) { d.Name = "!!!" }), target: "docker", wantErr: true},
		{name: "an over-long name is refused",
			drive: ok(func(d *UserDrive) { d.Name = strings.Repeat("a", maxUserDriveNameLen+1) }), target: "docker", wantErr: true},
		{name: "a control character in the name is refused",
			drive: ok(func(d *UserDrive) { d.Name = "corp\x00nas" }), target: "docker", wantErr: true},
		{name: "a host_path drive needs its root",
			drive: ok(func(d *UserDrive) { d.Backend = DriveBackendHostPath; d.HomeTemplate = HomeTemplateEmailLocal }), target: "docker", wantErr: true},
		{name: "a host root must be absolute",
			drive: ok(func(d *UserDrive) {
				d.Backend, d.HomeTemplate, d.HostRoot = DriveBackendHostPath, HomeTemplateEmailLocal, "srv/homes"
			}), target: "docker", wantErr: true},
		// Storing the uncleaned form would make two rows that name one tree
		// look different to the operator reviewing the env allowlist.
		{name: "a host root must already be cleaned",
			drive: ok(func(d *UserDrive) {
				d.Backend, d.HomeTemplate, d.HostRoot = DriveBackendHostPath, HomeTemplateEmailLocal, "/srv/homes/../homes"
			}), target: "docker", wantErr: true},
		{name: "a clean absolute host root is accepted",
			drive: ok(func(d *UserDrive) {
				d.Backend, d.HomeTemplate, d.HostRoot = DriveBackendHostPath, HomeTemplateEmailLocal, "/srv/homes"
			}), target: "docker"},
		{name: "only a host_path drive carries a host root",
			drive: ok(func(d *UserDrive) { d.HostRoot = "/srv/homes" }), target: "docker", wantErr: true},
		{name: "only a provisioning pvc carries a storage class",
			drive: ok(func(d *UserDrive) {
				d.Backend, d.HomeTemplate, d.StorageClass = DriveBackendK8sPVCStatic, HomeTemplateSub, "fast"
			}),
			target: "k8s", wantErr: true},
		{name: "a provisioning pvc may carry one",
			drive: ok(func(d *UserDrive) {
				d.Backend, d.StorageClass, d.SizeMiB = DriveBackendK8sPVC, "fast", 10240
			}),
			target: "k8s"},
		// THE ONE BACKEND WHERE SIZE IS NOT A DISPLAY VALUE. A k8s_pvc drive's
		// size becomes resources.requests.storage, and a claim requesting zero
		// bytes is rejected by the apiserver — so the value every other backend
		// reads as "no allocation shown" is, here, a row whose every member's run
		// fails at bind time on the cluster.
		{name: "a provisioning pvc with no size is refused",
			drive:  ok(func(d *UserDrive) { d.Backend = DriveBackendK8sPVC }),
			target: "k8s", wantErr: true},
		// …and the refusal is scoped to that ONE backend: 0 stays legal
		// everywhere else, where it honestly means "no allocation shown".
		{name: "zero is fine on a docker volume", drive: ok(func(*UserDrive) {}), target: "docker"},
		{name: "zero is fine on a static pvc",
			drive:  ok(func(d *UserDrive) { d.Backend, d.HomeTemplate = DriveBackendK8sPVCStatic, HomeTemplateSub }),
			target: "k8s"},
		{name: "zero is fine on a share",
			drive: ok(func(d *UserDrive) {
				d.Backend, d.HomeTemplate, d.HostRoot = DriveBackendHostPath, HomeTemplateSub, "/srv/homes"
			}),
			target: "docker"},
		{name: "a share cannot be hashed",
			drive: ok(func(d *UserDrive) {
				d.Backend, d.HomeTemplate, d.HostRoot = DriveBackendHostPath, HomeTemplateHash, "/srv/homes"
			}),
			target: "docker", wantErr: true},
		{name: "a share with a claim template is accepted",
			drive: ok(func(d *UserDrive) {
				d.Backend, d.HomeTemplate, d.HostRoot = DriveBackendHostPath, HomeTemplateSub, "/srv/homes"
			}),
			target: "docker"},
		{name: "an unknown home template is refused",
			drive: ok(func(d *UserDrive) { d.HomeTemplate = "uid" }), target: "docker", wantErr: true},
		{name: "a negative size is refused", drive: ok(func(d *UserDrive) { d.SizeMiB = -1 }), target: "docker", wantErr: true},
		{name: "an unknown reclaim is refused", drive: ok(func(d *UserDrive) { d.Reclaim = "archive" }), target: "docker", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.drive
			err := ValidateUserDrive(&d, tc.target)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateUserDrive = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}

	// The two defaults the column DEFAULTs mirror: a caller that omits them
	// gets the safe value rather than a validation error, and `retain` is the
	// safe half because the alternative default is data loss.
	t.Run("empty template and reclaim take their defaults", func(t *testing.T) {
		d := UserDrive{Name: "vol", Backend: DriveBackendDockerVolume}
		if err := ValidateUserDrive(&d, "docker"); err != nil {
			t.Fatalf("validate: %v", err)
		}
		if d.HomeTemplate != HomeTemplateHash || d.Reclaim != DriveReclaimRetain {
			t.Errorf("defaults = %q/%q, want %q/%q", d.HomeTemplate, d.Reclaim, HomeTemplateHash, DriveReclaimRetain)
		}
	})
}

// TestValidateUserDriveGrant pins the subject hygiene the two sibling tables
// already apply — a subject normalized one way here and another way there is a
// row that silently never matches — and the one rule specific to this table:
// a home override names ONE person's directory.
func TestValidateUserDriveGrant(t *testing.T) {
	driveID := uuid.New()
	base := func(mut func(*UserDriveGrant)) UserDriveGrant {
		g := UserDriveGrant{SubjectType: CapabilitySubjectUser, Subject: "alice", DriveID: driveID}
		mut(&g)
		return g
	}
	for _, tc := range []struct {
		name    string
		grant   UserDriveGrant
		wantErr bool
	}{
		{name: "a user grant", grant: base(func(*UserDriveGrant) {})},
		{name: "an unknown subject type is refused", grant: base(func(g *UserDriveGrant) { g.SubjectType = "everyone" }), wantErr: true},
		{name: "a drive id is required", grant: base(func(g *UserDriveGrant) { g.DriveID = uuid.Nil }), wantErr: true},
		{name: "a user grant needs a subject", grant: base(func(g *UserDriveGrant) { g.Subject = "  " }), wantErr: true},
		{name: "a control character is refused", grant: base(func(g *UserDriveGrant) { g.Subject = "al\x01ice" }), wantErr: true},
		{name: "a negative size override is refused", grant: base(func(g *UserDriveGrant) { g.SizeMiBOverride = -1 }), wantErr: true},
		{name: "a home override on a group grant is refused",
			grant: base(func(g *UserDriveGrant) {
				g.SubjectType, g.Subject, g.HomeOverride = CapabilitySubjectGroup, "eng", "shared"
			}),
			wantErr: true},
		{name: "an invalid home override is refused", grant: base(func(g *UserDriveGrant) { g.HomeOverride = "../etc" }), wantErr: true},
		{name: "a valid home override is accepted", grant: base(func(g *UserDriveGrant) { g.HomeOverride = "bsmith" })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := tc.grant
			err := ValidateUserDriveGrant(&g)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateUserDriveGrant = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}

	t.Run("subjects and overrides are folded", func(t *testing.T) {
		g := UserDriveGrant{SubjectType: CapabilitySubjectUser, Subject: "  Alice@Corp.Example ",
			DriveID: driveID, HomeOverride: " BSmith "}
		if err := ValidateUserDriveGrant(&g); err != nil {
			t.Fatalf("validate: %v", err)
		}
		if g.Subject != "alice@corp.example" || g.HomeOverride != "bsmith" {
			t.Errorf("folded = %q/%q, want the lowercased trimmed pair", g.Subject, g.HomeOverride)
		}
	})

	// "all" names every signed-in human, and the migration is explicit that its
	// subject is ''. Honouring a caller-supplied one would create a second,
	// unreachable everyone row.
	t.Run("an all grant drops its subject", func(t *testing.T) {
		g := UserDriveGrant{SubjectType: CapabilitySubjectAll, Subject: "everyone", DriveID: driveID}
		if err := ValidateUserDriveGrant(&g); err != nil {
			t.Fatalf("validate: %v", err)
		}
		if g.Subject != "" {
			t.Errorf("all-tier subject = %q, want empty", g.Subject)
		}
	})
}
