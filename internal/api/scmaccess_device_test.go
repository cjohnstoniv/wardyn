// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A device context carries no OIDC human, which the launch gate would read as
// the admin token and admit. A run on a per-person Azure DevOps row that a
// laptop submits with no person is refused at launch, never admitted to fail at
// dispatch with no credential stored under anyone.
func TestGitCredentialRefusal_DeviceCallerNeedingAPersonRowIsRefused(t *testing.T) {
	f := newADOSignInFixture(t)
	f.st.site = adoSite(f.row0())
	device := context.WithValue(context.Background(), deviceCtxKey{}, types.Device{ID: uuid.New(), Name: "laptop"})

	var gc *gitCredentialRefusalError
	err := f.srv.gitCredentialRefusalForLauncher(device, "", adoTestRepo)
	if !errors.As(err, &gc) || gc.Sentence != gitCredentialNoPersonRefusal || gc.Org != adoOrgDisplay(f.row0()) {
		t.Fatalf("device with no person: gate = %v, want the no-person refusal naming %q", err, adoOrgDisplay(f.row0()))
	}
	if !errors.Is(err, errGitCredentialRefused) {
		t.Fatalf("device with no person: %v does not match errGitCredentialRefused, so launchers would not map it", err)
	}
	if err := f.srv.gitCredentialRefusalForLauncher(context.Background(), "", adoTestRepo); err != nil {
		t.Fatalf("admin-token caller: gate = %v, want admitted", err)
	}
	if err := f.srv.gitCredentialRefusalForLauncher(device, f.subject, adoTestRepo); errors.As(err, &gc) && gc.Sentence == gitCredentialNoPersonRefusal {
		t.Fatalf("device carrying %q: gate = %v, want the person graded", f.subject, err)
	}
	if err := f.srv.gitCredentialRefusalForLauncher(device, "", "https://github.com/acme/app"); err != nil {
		t.Fatalf("device, repo on no per-person row: gate = %v, want admitted", err)
	}
	f.st.siteErr = errors.New("store down")
	if err := f.srv.gitCredentialRefusalForLauncher(device, "", adoTestRepo); !errors.As(err, &gc) || gc.Sentence != gitCredentialNoPersonRefusal {
		t.Fatalf("device, site config unreadable: gate = %v, want the no-person refusal", err)
	}
}

// A device's launch that queued keeps its refusal when promoted: the start replays the fold as the device, so a
// run on a per-person row still ends with the no-person sentence instead of starting without the credential.
func TestGitCredentialRefusal_QueuedDeviceLaunchKeepsItsRefusal(t *testing.T) {
	f := newADOSignInFixture(t)
	f.st.site = adoSite(f.row0())
	device := context.WithValue(context.Background(), deviceCtxKey{}, types.Device{ID: uuid.New(), Name: "laptop"})
	raw, err := json.Marshal(captureQueueAuthority(device))
	if err != nil {
		t.Fatal(err)
	}
	var auth queueAuthority
	if err := json.Unmarshal(raw, &auth); err != nil {
		t.Fatal(err)
	}
	var gc *gitCredentialRefusalError
	err = f.srv.gitCredentialRefusalForLauncher(auth.context(context.Background()), "", adoTestRepo)
	if !errors.As(err, &gc) || gc.Sentence != gitCredentialNoPersonRefusal {
		t.Fatalf("promoted device run: gate = %v, want the no-person refusal", err)
	}
}
