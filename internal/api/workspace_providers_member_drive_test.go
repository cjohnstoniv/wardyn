// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// namedRunner is the fake runner under a substrate's name, the one fact the
// target-aware member-drive refusals read.
type namedRunner struct {
	*fakeRunner
	name string
}

func (n namedRunner) Name() string { return n.name }

func memberDriveBlock(ceilingMiB int, m types.MemberDrivePolicy) string {
	return fmt.Sprintf(`{"storage":{"user_drive":{"max_size_mib":%d,"member_create":{"enabled":%t,"max_per_person":%d,"size_mib":%d,"storage_class":%q}}}}`,
		ceilingMiB, m.Enabled, m.MaxPerPerson, m.SizeMiB, m.StorageClass)
}

// TestMemberDrivePolicyValidation: the member_create refusals at the console
// door, per substrate. The size and storage-class rules read the runner, so the
// same block saves on one target and is refused on the other.
func TestMemberDrivePolicyValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  string
		ceiling int
		m       types.MemberDrivePolicy
		want    string // "" = saved; else a substring of the 400 body
	}{
		{"off by default saves with nothing else", "docker", 0, types.MemberDrivePolicy{}, ""},
		{"on with a count saves on docker", "docker", 0, types.MemberDrivePolicy{Enabled: true, MaxPerPerson: 3}, ""},
		{"count 1 saves", "docker", 0, types.MemberDrivePolicy{Enabled: true, MaxPerPerson: 1}, ""},
		{"count 20 saves", "docker", 0, types.MemberDrivePolicy{Enabled: true, MaxPerPerson: 20}, ""},
		{"on without a count", "docker", 0, types.MemberDrivePolicy{Enabled: true}, "max_per_person"},
		{"count 21", "docker", 0, types.MemberDrivePolicy{Enabled: true, MaxPerPerson: 21}, "max_per_person"},
		{"docker with a size is a promise nothing keeps", "docker", 0, types.MemberDrivePolicy{Enabled: true, MaxPerPerson: 2, SizeMiB: 512}, "a promise nothing keeps"},
		{"docker with a storage class", "docker", 0, types.MemberDrivePolicy{Enabled: true, MaxPerPerson: 2, StorageClass: "fast"}, "no storage class"},
		{"k8s with a size and a class saves", "k8s", 0, types.MemberDrivePolicy{Enabled: true, MaxPerPerson: 2, SizeMiB: 512, StorageClass: "fast"}, ""},
		{"k8s on without a size", "k8s", 0, types.MemberDrivePolicy{Enabled: true, MaxPerPerson: 2}, "size_mib"},
		{"k8s off without a size saves", "k8s", 0, types.MemberDrivePolicy{}, ""},
		{"k8s size above the drive ceiling", "k8s", 1024, types.MemberDrivePolicy{Enabled: true, MaxPerPerson: 2, SizeMiB: 2048}, "above storage.user_drive.max_size_mib"},
		{"k8s size at the drive ceiling saves", "k8s", 1024, types.MemberDrivePolicy{Enabled: true, MaxPerPerson: 2, SizeMiB: 1024}, ""},
		{"a negative size", "k8s", 0, types.MemberDrivePolicy{Enabled: true, MaxPerPerson: 2, SizeMiB: -5}, "not a size in MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := baseTestConfig(h, &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}})
			cfg.Runner = namedRunner{fakeRunner: &fakeRunner{}, name: tc.target}
			w := do(t, New(cfg), http.MethodPut, "/api/v1/workspace-providers", adminToken, memberDriveBlock(tc.ceiling, tc.m))
			if tc.want == "" {
				if w.Code != http.StatusOK {
					t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("PUT = %d %s, want 400 naming %q", w.Code, w.Body.String(), tc.want)
			}
		})
	}
}

// TestMemberDrivePolicyKeepsOnTheWire: the policy round-trips through the
// strict decoder and back out of GET, so a console that mirrors the tags by
// hand reads what the admin saved.
func TestMemberDrivePolicyKeepsOnTheWire(t *testing.T) {
	fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
	h := newHarness(t)
	cfg := baseTestConfig(h, fake)
	cfg.Runner = namedRunner{fakeRunner: &fakeRunner{}, name: "k8s"}
	srv := New(cfg)
	in := memberDriveBlock(0, types.MemberDrivePolicy{Enabled: true, MaxPerPerson: 4, SizeMiB: 256, StorageClass: "fast"})
	if w := do(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken, in); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
	}
	got := fake.putSeen.WorkspaceProviders.Storage.UserDrive.MemberCreate
	if got == nil || *got != (types.MemberDrivePolicy{Enabled: true, MaxPerPerson: 4, SizeMiB: 256, StorageClass: "fast"}) {
		t.Fatalf("stored member_create = %+v", got)
	}
}

// The whole-document site-config door applies the same target-aware refusal as
// the workspace-providers door, so a Docker size cannot be saved through it.
func TestMemberDrivePolicyValidation_SiteConfigDoor(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}})
	cfg.Runner = namedRunner{fakeRunner: &fakeRunner{}, name: "docker"}
	body := `{"workspace_providers":` + memberDriveBlock(0, types.MemberDrivePolicy{Enabled: true, MaxPerPerson: 2, SizeMiB: 512}) + `}`
	w := do(t, New(cfg), http.MethodPut, "/api/v1/site-config", adminToken, body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "a promise nothing keeps") {
		t.Fatalf("site-config with a docker member-drive size = %d %s, want 400 naming the refusal", w.Code, w.Body.String())
	}
}
