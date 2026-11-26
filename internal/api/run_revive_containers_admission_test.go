// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Image admission at revive for a run's extra containers: each is re-checked at the digest it runs, under the
// policy as it is now, as the run's own image is.

const (
	extraReviveDigest = "sha256:" + "ee00000000000000000000000000000000000000000000000000000000000000"
	extraRevivePinned = "registry.example.com/team/db@" + extraReviveDigest
)

// extrasReviveStore is a revive fixture's store that keeps extras beside the run.
type extrasReviveStore struct {
	*reviveStore
	extras []types.RunExtraContainer
	err    error
}

func (s *extrasReviveStore) RunContainers(context.Context, uuid.UUID) ([]types.RunExtraContainer, error) {
	return s.extras, s.err
}

// extrasReviveFixture is reviveAdmissionFixture's pinned, enforced run that also keeps one extra at
// extraRevivePinned, admitted by its create.
func extrasReviveFixture(t *testing.T, mode string, a ImageAdmitter) *reviveFixture {
	t.Helper()
	f := reviveAdmissionFixture(t, mode, pinnedVerdict(admitPinned), a)
	f.srv.cfg.Store = &extrasReviveStore{reviveStore: f.rs, extras: []types.RunExtraContainer{{Name: "db", Image: extraRevivePinned,
		CPUMillis: 250, MemoryMiB: 256, ImageAdmission: pinnedVerdict(extraRevivePinned)}}}
	return f
}

// TestReviveImageAdmission_ExtraContainersAreRechecked: an extra whose digest still passes revives, asked by its
// pin; one whose signer is gone refuses the revive with the check's reason, naming the extra, and a denied row;
// in audit mode nothing about the extra is asked.
func TestReviveImageAdmission_ExtraContainersAreRechecked(t *testing.T) {
	t.Run("admitted", func(t *testing.T) {
		a := &fakeAdmitter{}
		f := extrasReviveFixture(t, types.ImageAdmissionEnforce, a)
		if code, body := f.reviveAs(t, true); code != http.StatusOK {
			t.Fatalf("revive = %d %s, want 200", code, body)
		}
		if !slices.Equal(a.admits, []string{admitPinned, extraRevivePinned}) {
			t.Errorf("Admit refs = %q, want the run's pin then the extra's", a.admits)
		}
	})
	t.Run("signer gone", func(t *testing.T) {
		a := &fakeAdmitter{admit: func(req imageadmit.Requirements) (imageadmit.Verdict, error) {
			if req.Ref == extraRevivePinned {
				return imageadmit.Verdict{Reason: imageadmit.SignatureMissing, Digest: extraReviveDigest, Signers: []string{"other-ci"}}, nil
			}
			return imageadmit.Verdict{Digest: admitDigest, Signer: admitSigner}, nil
		}}
		f := extrasReviveFixture(t, types.ImageAdmissionEnforce, a)
		code, body := f.reviveAs(t, true)
		if code != http.StatusForbidden || !strings.Contains(body, reasonImageSignatureMissing) || !strings.Contains(body, "containers[0].image") ||
			!strings.Contains(body, "Start a new run to use an allowed image") {
			t.Fatalf("revive = %d %s; want 403 %s naming containers[0].image, with the revive sentence", code, body, reasonImageSignatureMissing)
		}
		f.assertReviveRefused(t, reasonImageSignatureMissing)
	})
	t.Run("extras unreadable", func(t *testing.T) {
		a := &fakeAdmitter{}
		f := extrasReviveFixture(t, types.ImageAdmissionEnforce, a)
		f.srv.cfg.Store.(*extrasReviveStore).err = errors.New("permission denied for table run_containers")
		code, body := f.reviveAs(t, true)
		if code != http.StatusServiceUnavailable || !strings.Contains(body, reasonReviveOwnerAuthorityUnreadable) {
			t.Fatalf("revive = %d %s, want 503 %s", code, body, reasonReviveOwnerAuthorityUnreadable)
		}
		f.assertReviveRefused(t, reasonReviveOwnerAuthorityUnreadable)
	})
	t.Run("audit", func(t *testing.T) {
		a := &fakeAdmitter{}
		f := extrasReviveFixture(t, types.ImageAdmissionAudit, a)
		if code, body := f.reviveAs(t, true); code != http.StatusOK {
			t.Fatalf("revive = %d %s, want 200", code, body)
		}
		if r, n := a.calls(); r != 0 || n != 0 {
			t.Errorf("audit mode asked %d Require / %d Admit; want none", r, n)
		}
	})
}
