// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// PIN for the finding that the home-template rules were keyed on the
// MANAGED/SHARE split rather than on WHO NAMES THE OBJECT.
//
// k8s_pvc_static is a share by provisioning — an admin creates the claim and
// wardynd only ever Gets it — but its object NAME is minted by Wardyn, exactly
// as a managed claim's is (DriveObjectName's default arm). Reading the template
// rule off DriveKind therefore had it backwards on both halves:
//
//   - `hash` was REFUSED, so the only authorable templates were `sub` and
//     `email_local` and every static allocation's claim name had to carry the
//     member's sign-in subject or address — the exposure the managed half of the
//     same rule exists to refuse, in a name `kubectl get pvc` prints without
//     describing anything.
//   - `email_local` was PERMITTED, so two principals whose addresses share the
//     part before the "@" were folded onto ONE claim name that the driver's
//     static arm had no per-principal evidence to tell apart.
//
// The rules now key on types.DriveObjectNamedByWardyn, which is the same
// expression DriveObjectName itself branches on.

import "testing"

func TestUserDriveTemplateRulesKeyOnWhoNamesTheObject(t *testing.T) {
	// The axis itself, against the function whose branch it is.
	for _, b := range []DriveBackend{DriveBackendDockerVolume, DriveBackendK8sPVC, DriveBackendK8sPVCStatic, DriveBackendHostPath} {
		d := UserDrive{Name: "Corp NAS", Backend: b, HostRoot: "/srv/homes"}
		minted := DriveObjectName(d, "home") == driveObjectPrefix+DriveSlug(d.Name)+"-home"
		if got := DriveObjectNamedByWardyn(b); got != minted {
			t.Errorf("DriveObjectNamedByWardyn(%s) = %v but DriveObjectName mints = %v; the rules and the name have drifted",
				b, got, minted)
		}
	}

	cases := []struct {
		backend DriveBackend
		tmpl    HomeTemplate
		want    bool // want ValidateUserDrive to REFUSE the template
		why     string
	}{
		// The finding's headline: Wardyn mints a static claim's name, so `hash`
		// is exactly as available there as on a managed claim.
		{DriveBackendK8sPVCStatic, HomeTemplateHash, false,
			"Wardyn mints the claim name, so a hash names a claim an admin can pre-provision from the preview endpoint"},
		// And its second half: the collision the managed rule refuses is the
		// same collision here, because it is the same minted name.
		{DriveBackendK8sPVCStatic, HomeTemplateEmailLocal, true,
			"email_local folds two addresses onto ONE minted claim name and the driver's static arm cannot separate them"},
		{DriveBackendK8sPVCStatic, HomeTemplateSub, false,
			"sub stays authorable: an admin must be able to recognise the claims they pre-provision"},

		// host_path is the ONE backend whose object somebody else named.
		{DriveBackendHostPath, HomeTemplateHash, true, "a hash names a directory nobody created and Wardyn never mkdir's"},
		{DriveBackendHostPath, HomeTemplateSub, false, "unchanged"},
		{DriveBackendHostPath, HomeTemplateEmailLocal, false, "unchanged: an admin's own filesystem, which they can see"},

		// Managed backends are untouched by the re-keying.
		{DriveBackendK8sPVC, HomeTemplateHash, false, "unchanged"},
		{DriveBackendK8sPVC, HomeTemplateSub, true, "unchanged"},
		{DriveBackendK8sPVC, HomeTemplateEmailLocal, true, "unchanged"},
		{DriveBackendDockerVolume, HomeTemplateHash, false, "unchanged"},
		{DriveBackendDockerVolume, HomeTemplateSub, true, "unchanged"},
	}
	for _, c := range cases {
		d := UserDrive{Name: "corp-nas", Backend: c.backend, HomeTemplate: c.tmpl, Reclaim: DriveReclaimRetain}
		if c.backend == DriveBackendHostPath {
			d.HostRoot = "/srv/homes"
		}
		if c.backend == DriveBackendK8sPVC {
			d.SizeMiB = 1024
		}
		err := ValidateUserDrive(&d, c.backend.RunnerTarget())
		if got := err != nil; got != c.want {
			t.Errorf("ValidateUserDrive(%s, %s) refused = %v, want %v (%s); err = %v",
				c.backend, c.tmpl, got, c.want, c.why, err)
		}
	}

	// AND THE DEFAULT: an unstated template on a static PVC now lands on `hash`
	// and is accepted, which is the recommended posture — before, the default
	// was refused and the admin had to name an identity template to get a row.
	d := UserDrive{Name: "corp-nas", Backend: DriveBackendK8sPVCStatic, Reclaim: DriveReclaimRetain}
	if err := ValidateUserDrive(&d, "k8s"); err != nil {
		t.Fatalf("a static PVC drive with no home_template stated: %v", err)
	}
	if d.HomeTemplate != HomeTemplateHash {
		t.Errorf("default home_template on a static PVC = %q, want %q", d.HomeTemplate, HomeTemplateHash)
	}
}

// TestStaticPVCClaimNameNeverServesTwoSubjects is the finding's own acceptance
// test: one claim name must never cover two principals.
func TestStaticPVCClaimNameNeverServesTwoSubjects(t *testing.T) {
	d := UserDrive{Name: "corp-nas", Backend: DriveBackendK8sPVCStatic, HomeTemplate: HomeTemplateHash, Reclaim: DriveReclaimRetain}
	if err := ValidateUserDrive(&d, "k8s"); err != nil {
		t.Fatalf("hash on a static PVC must be authorable: %v", err)
	}
	const alice, other = "alice@corp.example", "alice@acquired.example"
	names := map[string]string{}
	for _, subject := range []string{alice, other} {
		home, err := DriveHomeName(d, subject, "")
		if err != nil {
			t.Fatalf("derive a home for %q: %v", subject, err)
		}
		names[subject] = DriveObjectName(d, home)
	}
	if names[alice] == names[other] {
		t.Errorf("two principals mint ONE claim name %q", names[alice])
	}
	if DriveSubjectHash(alice) == DriveSubjectHash(other) {
		t.Fatal("the two probe subjects hash alike; the test cannot tell them apart")
	}

	// The template that DOES fold them is now unauthorable on this backend,
	// which is the only reason the claim above is one-per-principal.
	folding := d
	folding.HomeTemplate = HomeTemplateEmailLocal
	if err := ValidateUserDrive(&folding, "k8s"); err == nil {
		a, _ := DriveHomeName(folding, alice, "")
		b, _ := DriveHomeName(folding, other, "")
		t.Errorf("email_local is authorable on a static PVC and derives %q for both %q and %q — one claim, two people",
			DriveObjectName(folding, a), alice, DriveObjectName(folding, b))
		_ = b
	}
}
