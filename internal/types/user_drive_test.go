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

// TestDriveHomeNameIsDNS1123OnKubernetes pins the split this derivation grew
// once the same segment had to be a PVC name: a Kubernetes home is a DNS-1123
// subdomain, a Docker home is a volume-name component, and they are NOT the
// same alphabet.
//
// THE MOTIVATING CASE IS THE FIRST ROW. An Entra `sub` is base64url and
// routinely carries `_`, which is legal in a Docker volume name and illegal in
// a DNS-1123 name. Before the split, a `k8s_pvc` + `sub` drive validated at the
// write boundary, was stored, and then failed at BIND time — on somebody's run,
// as an apiserver error naming a claim they never typed. Refusing it here makes
// it a resolve-time refusal an admin sees on the preview, naming the claim they
// have to write a home_override for.
func TestDriveHomeNameIsDNS1123OnKubernetes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		subject  string
		override string
		docker   bool // does the DOCKER rule accept it?
		k8s      bool // does the KUBERNETES rule?
	}{
		// The Entra shape, and the whole reason for the split.
		{name: "an underscore in a sub", subject: "aBc_dEf-123", docker: true, k8s: false},
		{name: "a trailing dash", subject: "alice-", docker: true, k8s: false},
		{name: "a trailing dot", subject: "alice.", docker: true, k8s: false},
		{name: "consecutive dots", subject: "a..b", docker: true, k8s: false},
		// What BOTH accept: the ordinary corporate username, dotted or not.
		{name: "a plain username", subject: "alice", docker: true, k8s: true},
		{name: "a dotted username", subject: "alice.smith", docker: true, k8s: true},
		{name: "an inner dash", subject: "alice-smith", docker: true, k8s: true},
		{name: "63 characters", subject: strings.Repeat("a", 63), docker: true, k8s: true},
		{name: "64 characters", subject: strings.Repeat("a", 64), docker: false, k8s: false},
		// The override is an admin's fact about a filesystem, and it is still
		// not allowed to name something the apiserver will reject: the grant row
		// holds a drive_id, not a backend, so this is the only place that check
		// can run.
		{name: "an underscore in an override", subject: "alice", override: "b_smith", docker: true, k8s: false},
		{name: "a plain override", subject: "alice", override: "bsmith", docker: true, k8s: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// host_path and k8s_pvc_static are the two SHARE backends, one per
			// substrate, so the template is legal on both and the only thing
			// that differs between the two arms is the alphabet.
			for _, arm := range []struct {
				backend DriveBackend
				want    bool
			}{
				{DriveBackendHostPath, tc.docker},
				{DriveBackendK8sPVCStatic, tc.k8s},
			} {
				d := driveFor(t, arm.backend, HomeTemplateSub)
				got, err := DriveHomeName(d, tc.subject, tc.override)
				if arm.want && err != nil {
					t.Errorf("%s: DriveHomeName(%q, %q) = %v, want it accepted", arm.backend, tc.subject, tc.override, err)
				}
				if !arm.want && err == nil {
					t.Errorf("%s: DriveHomeName(%q, %q) = %q, want a refusal — this name cannot hold on that substrate",
						arm.backend, tc.subject, tc.override, got)
				}
			}
		})
	}

	// `hash` derives a safe segment on BOTH rules, always: it is the only
	// template a MANAGED backend may use, so a k8s_pvc drive must never be able
	// to reach the refusal above through it.
	t.Run("hash is safe on both substrates", func(t *testing.T) {
		for _, b := range []DriveBackend{DriveBackendDockerVolume, DriveBackendK8sPVC} {
			home, err := DriveHomeName(driveFor(t, b, HomeTemplateHash), "aBc_dEf-123", "")
			if err != nil {
				t.Fatalf("%s: hash home: %v", b, err)
			}
			if !driveHomeSegmentOK(b, home) {
				t.Errorf("%s: hash home %q is not a legal segment on its own backend", b, home)
			}
		}
	})
}

// TestDriveObjectName pins what the runner asks the substrate for. Every MINTED
// name carries the drive's slug — an operator reading `docker volume ls` or
// `kubectl get pvc` has no other way to tell two drives apart — and a share's
// path carries none, because the directory under the root is not Wardyn's to
// name.
func TestDriveObjectName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		backend  DriveBackend
		hostRoot string
		drive    string
		home     string
		want     string
	}{
		{name: "a volume carries the drive slug", backend: DriveBackendDockerVolume, drive: "Corp NAS", home: "d-abc", want: "wardyn-drive-corp-nas-d-abc"},
		{name: "and a pvc is named the same way", backend: DriveBackendK8sPVC, drive: "Corp NAS", home: "d-abc", want: "wardyn-drive-corp-nas-d-abc"},
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

// TestDriveObjectNameSeparatesTwoDrivesOnOneHome pins the invariant the volume
// arm used to break: a name WARDYN MINTS identifies the drive as well as the
// person.
//
// It reads as though the home alone were enough, because a `hash` home folds
// the drive id and one member's two drives are already two homes. A
// home_override does not fold it: the admin writes "Bob's directory is bsmith"
// on the GRANT, and user_drive_grants is UNIQUE on (subject_type, subject), so
// re-pointing that one grant IS how a member moves between drives. With no slug
// both sides of the re-point named one volume — Bob mounting the old drive's
// contents under the new drive's name, size and writable posture — and an
// offboarding command naming `wardyn-drive-bsmith` could not say which drive it
// was reclaiming.
func TestDriveObjectNameSeparatesTwoDrivesOnOneHome(t *testing.T) {
	// What a user-tier home_override derives on ANY drive, unchanged by the
	// template, which is why the F034 template rule does not cover this.
	const home = "bsmith"

	for _, backend := range []DriveBackend{DriveBackendDockerVolume, DriveBackendK8sPVC} {
		t.Run(string(backend), func(t *testing.T) {
			a := UserDrive{ID: uuid.New(), Name: "Eng scratch", Backend: backend}
			b := UserDrive{ID: uuid.New(), Name: "Design scratch", Backend: backend}
			gotA, gotB := DriveObjectName(a, home), DriveObjectName(b, home)
			if gotA == gotB {
				t.Errorf("both drives mint %q — re-pointing one grant would hand the member the OTHER "+
					"drive's data under this drive's size and writable posture", gotA)
			}
			// The slug has to be the DRIVE's, not merely something distinct:
			// an operator reads these names to reclaim by.
			if !strings.Contains(gotA, "eng-scratch") || !strings.Contains(gotB, "design-scratch") {
				t.Errorf("names = %q / %q, want each to carry its own drive's slug", gotA, gotB)
			}
			// …and the one glob that finds every Wardyn-created object still
			// finds both.
			if !strings.HasPrefix(gotA, driveObjectPrefix) || !strings.HasPrefix(gotB, driveObjectPrefix) {
				t.Errorf("names = %q / %q, want both under %q", gotA, gotB, driveObjectPrefix)
			}
		})
	}

	// The SHARE arm must NOT gain a slug: the root already scopes it and the
	// directory under it was named by whoever owns the tree. Two shares on one
	// root and one home ARE one directory, and that is the admin pointing two
	// rows at one tree — not Wardyn minting a collision.
	t.Run("a share is scoped by its root, not by a slug", func(t *testing.T) {
		a := UserDrive{ID: uuid.New(), Name: "Eng scratch", Backend: DriveBackendHostPath, HostRoot: "/srv/eng"}
		b := UserDrive{ID: uuid.New(), Name: "Design scratch", Backend: DriveBackendHostPath, HostRoot: "/srv/design"}
		if got := DriveObjectName(a, home); got != "/srv/eng/bsmith" {
			t.Errorf("host path = %q, want the root joined to the home with no slug", got)
		}
		if DriveObjectName(a, home) == DriveObjectName(b, home) {
			t.Errorf("two roots gave one path %q", DriveObjectName(a, home))
		}
	})
}

// TestDriveSubjectHash pins the fingerprint the runner stamps on a managed
// object: it separates two principals whose HOME NAME is the same (the whole
// reason it exists, since an object name carries only the home), it leaks
// neither the claim nor its shape, and an absent subject produces NO
// fingerprint rather than one every identity-less caller would share.
func TestDriveSubjectHash(t *testing.T) {
	alice := DriveSubjectHash("alice@corp.example")
	if len(alice) != driveHomeHashLen {
		t.Errorf("hash length = %d, want %d", len(alice), driveHomeHashLen)
	}
	// The case the whole label exists for: DriveHomeName folds both of these to
	// "alice" under email_local, so the home cannot tell them apart and this
	// must.
	if other := DriveSubjectHash("alice@acquired.example"); other == alice {
		t.Error("two principals sharing an email local part hashed the same — the label could not tell them apart")
	}
	// A LABEL IS PUBLIC: `docker volume inspect` echoes it to anyone who can
	// reach the daemon, so no part of the claim may survive into it.
	if strings.Contains(alice, "alice") || strings.Contains(alice, "@") {
		t.Errorf("hash %q carries the claim — a Docker label is echoed verbatim by `docker volume inspect`", alice)
	}
	// Folded exactly as DriveHomeName folds a subject, so the label and the home
	// are decided by one reading of one claim.
	if DriveSubjectHash("  ALICE@CORP.EXAMPLE ") != alice {
		t.Error("the subject is not trimmed and lowercased the way DriveHomeName folds it")
	}
	// EMPTY MEANS NO LABEL, not the digest of "": that value would be one
	// fingerprint shared by every caller with no identity — the single answer
	// that could make two principals look like one.
	if got := DriveSubjectHash("   "); got != "" {
		t.Errorf("DriveSubjectHash(blank) = %q, want \"\"", got)
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
	// The enum's VALUES, not just the mapping: the console keys its enforcement
	// note off these exact strings (screens/drives/display.tsx), and blanking a
	// constant would leave every row above still passing.
	for name, got := range map[string]StorageEnforcement{
		"filesystem": StorageEnforcementFilesystem,
		"request":    StorageEnforcementRequest,
		"external":   StorageEnforcementExternal,
		"none":       StorageEnforcementNone,
	} {
		if string(got) != name {
			t.Errorf("StorageEnforcement constant = %q, want %q", got, name)
		}
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
		// THE MIRROR OF "a share cannot be hashed", and the security half of the
		// pair. A MANAGED object is named by the HOME alone, so two people whose
		// addresses share the part before the "@" — the ordinary merged-tenant
		// shape — would be allocated ONE volume or PVC, with write access to each
		// other's files whenever the drive is writable. `hash` puts the subject
		// in the digest and `sub` is unique by definition, so both stay legal.
		{name: "a managed volume cannot be templated on the email local part",
			drive: ok(func(d *UserDrive) { d.HomeTemplate = HomeTemplateEmailLocal }), target: "docker", wantErr: true},
		{name: "a managed pvc cannot be templated on the email local part",
			drive: ok(func(d *UserDrive) {
				d.Backend, d.HomeTemplate, d.SizeMiB = DriveBackendK8sPVC, HomeTemplateEmailLocal, 10240
			}), target: "k8s", wantErr: true},
		// `sub` was accepted here until 2026-09-03. It answered the COLLISION
		// question (a sub is unique) but not the EXPOSURE one: the home segment is
		// concatenated into the object name, which `docker volume ls` shows without
		// the inspect a label needs — so the subject was refused in the less exposed
		// place (DriveSubjectHash's label rule) and permitted in the more exposed one.
		{name: "a managed volume cannot be templated on sub — the object name shows it",
			drive: ok(func(d *UserDrive) { d.HomeTemplate = HomeTemplateSub }), target: "docker", wantErr: true},
		{name: "a managed pvc cannot be templated on sub either",
			drive: ok(func(d *UserDrive) {
				d.Backend, d.HomeTemplate, d.SizeMiB = DriveBackendK8sPVC, HomeTemplateSub, 10240
			}), target: "k8s", wantErr: true},
		{name: "a managed volume on the hash template is still accepted (the not-everything-is-refused control)",
			drive: ok(func(d *UserDrive) { d.HomeTemplate = HomeTemplateHash }), target: "docker"},
		{name: "a share may still be templated on sub",
			drive: ok(func(d *UserDrive) {
				d.Backend, d.HomeTemplate, d.HostRoot = DriveBackendHostPath, HomeTemplateSub, "/srv/homes"
			}), target: "docker"},
		// …and the refusal is scoped to MANAGED. A share's directories are named
		// by whoever owns the share, and `email_local` is the corporate shape the
		// template exists for.
		{name: "a share may still be templated on the email local part",
			drive: ok(func(d *UserDrive) {
				d.Backend, d.HomeTemplate, d.HostRoot = DriveBackendHostPath, HomeTemplateEmailLocal, "/srv/homes"
			}), target: "docker"},
		{name: "a managed drive on the hash is accepted",
			drive:  ok(func(d *UserDrive) { d.HomeTemplate = HomeTemplateHash }),
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

// TestManagedBackendTakesOnlyTheHashTemplate is the write boundary's half of
// the ONE guarantee a managed drive rests on: the object name Wardyn mints for
// a person is that person's alone.
//
// TWO REASONS, and the second is why `sub` is refused alongside `email_local`.
//
// COLLISION: a claim template is not injective over principals. `email_local`
// drops the domain, so alice@corp.example and alice@partner.example — a
// guest/B2B/contractor tenant, the exact deployment shape user drives target —
// derive one home and therefore ONE Docker volume and ONE PVC. On a MANAGED
// backend Wardyn is CREATING that object, so accepting such a template is
// Wardyn mounting one member's storage into another member's sandbox,
// read-write when the allocation is writable.
//
// EXPOSURE: the object NAME carries the template's output, and `docker volume
// ls` / `kubectl get pvc` show it to anyone who can reach the daemon or the
// namespace WITHOUT inspecting anything. `sub` is injective, so it answers the
// collision question — but it writes the sign-in subject into that name, which
// is the very thing types.DriveSubjectHash refuses to put in a mere LABEL. A
// rule that forbade the subject in the less exposed place and permitted it in
// the more exposed one was answering only half the question.
//
// On a SHARE neither reason applies: the directories exist under names Wardyn
// did not choose, so a claim template is the only thing that can name one, and
// the same-local-part hazard belongs to whoever minted them.
//
// The test proves both halves: the refusal, and the collision that makes the
// refusal load-bearing rather than tidy.
func TestManagedBackendTakesOnlyTheHashTemplate(t *testing.T) {
	managed := []DriveBackend{DriveBackendDockerVolume, DriveBackendK8sPVC}

	t.Run("the write boundary refuses EVERY claim template on a managed backend", func(t *testing.T) {
		for _, b := range managed {
			for _, tmpl := range []HomeTemplate{HomeTemplateEmailLocal, HomeTemplateSub} {
				d := UserDrive{Name: "Corp NAS", Backend: b, HomeTemplate: tmpl, SizeMiB: 10240}
				err := ValidateUserDrive(&d, b.RunnerTarget())
				if err == nil {
					t.Errorf("ValidateUserDrive(%s, %s) = nil, want a refusal — Wardyn mints this object, "+
						"and a claim-derived name is both a collision and an exposure", b, tmpl)
					continue
				}
				// The admin has to be able to act on it: the message names the
				// template they picked and the one that works.
				if !strings.Contains(err.Error(), string(tmpl)) || !strings.Contains(err.Error(), string(HomeTemplateHash)) {
					t.Errorf("ValidateUserDrive(%s, %s) = %v, want a message naming both %q and %q",
						b, tmpl, err, tmpl, HomeTemplateHash)
				}
			}
			// SCOPED: the refusal is about the CLAIM templates, not about a
			// managed drive having a template at all.
			ok := UserDrive{Name: "Corp NAS", Backend: b, HomeTemplate: HomeTemplateHash, SizeMiB: 10240}
			if err := ValidateUserDrive(&ok, b.RunnerTarget()); err != nil {
				t.Errorf("ValidateUserDrive(%s, hash) = %v, want it accepted", b, err)
			}
		}
	})

	// A share is the opposite case and must not be caught by the new rule: its
	// directories exist already under names Wardyn did not choose, so a claim
	// template is the ONLY thing that can name one.
	t.Run("a share still takes both claim templates", func(t *testing.T) {
		for _, b := range []DriveBackend{DriveBackendHostPath, DriveBackendK8sPVCStatic} {
			for _, tmpl := range []HomeTemplate{HomeTemplateSub, HomeTemplateEmailLocal} {
				d := UserDrive{Name: "Corp NAS", Backend: b, HomeTemplate: tmpl}
				if b == DriveBackendHostPath {
					d.HostRoot = "/srv/homes"
				}
				if err := ValidateUserDrive(&d, b.RunnerTarget()); err != nil {
					t.Errorf("ValidateUserDrive(%s, %s) = %v, want it accepted — only a claim can name a "+
						"directory Wardyn did not create", b, tmpl, err)
				}
			}
		}
	})

	// WHY the refusal exists, stated as the collision it now makes
	// unauthorable. Built by hand rather than through ValidateUserDrive,
	// because after the fix this row cannot be written.
	t.Run("the collision the refusal prevents", func(t *testing.T) {
		d := UserDrive{ID: uuid.New(), Name: "Corp NAS", Backend: DriveBackendDockerVolume, HomeTemplate: HomeTemplateEmailLocal}
		one, err := DriveHomeName(d, "alice@corp.example", "")
		if err != nil {
			t.Fatalf("home for alice@corp.example: %v", err)
		}
		two, err := DriveHomeName(d, "alice@partner.example", "")
		if err != nil {
			t.Fatalf("home for alice@partner.example: %v", err)
		}
		if DriveObjectName(d, one) != DriveObjectName(d, two) {
			t.Fatalf("two email domains derived %q and %q — if they no longer collide the write-boundary "+
				"refusal has lost its reason and this test needs rewriting, not deleting",
				DriveObjectName(d, one), DriveObjectName(d, two))
		}
		// …and the template the rule leaves standing does separate them, which
		// is why `hash` is the whole remedy and not a workaround.
		h := d
		h.HomeTemplate = HomeTemplateHash
		hOne, _ := DriveHomeName(h, "alice@corp.example", "")
		hTwo, _ := DriveHomeName(h, "alice@partner.example", "")
		if DriveObjectName(h, hOne) == DriveObjectName(h, hTwo) {
			t.Errorf("hash gave both principals %q; the digest covers the subject, so it must not",
				DriveObjectName(h, hOne))
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
