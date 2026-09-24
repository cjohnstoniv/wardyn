// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A device context carries no OIDC human, which the launch gate would read as
// the admin token and admit. A run on a per-person Azure DevOps row that a
// laptop submits with no person must be refused at create, never admitted to
// fail at dispatch with no credential stored under anyone.
func TestGitCredentialGate_DeviceWithNoPersonIsRefused(t *testing.T) {
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

	// The admin token (no device, no person) keeps its old answer.
	if err := f.srv.gitCredentialRefusalForLauncher(context.Background(), "", adoTestRepo); err != nil {
		t.Fatalf("admin-token caller: gate = %v, want admitted", err)
	}
	// A device that carries the person is graded as that person.
	if err := f.srv.gitCredentialRefusalForLauncher(device, f.subject, adoTestRepo); errors.As(err, &gc) &&
		gc.Sentence == gitCredentialNoPersonRefusal {
		t.Fatalf("device carrying %q: gate = %v, want the person graded", f.subject, err)
	}
	// A repository no per-person row admits needs no person.
	if err := f.srv.gitCredentialRefusalForLauncher(device, "", "https://github.com/acme/app"); err != nil {
		t.Fatalf("device, repo on no per-person row: gate = %v, want admitted", err)
	}
	// An unreadable site config fails closed for a device.
	f.st.siteErr = errors.New("store down")
	if err := f.srv.gitCredentialRefusalForLauncher(device, "", adoTestRepo); !errors.As(err, &gc) ||
		gc.Sentence != gitCredentialNoPersonRefusal {
		t.Fatalf("device, site config unreadable: gate = %v, want the no-person refusal", err)
	}
}
