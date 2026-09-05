// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestManagedTemplateWriteAndResolveAgree is F158's structural half, and it is
// the check the finding says would have caught the change that caused it: the
// managed non-hash rule was WIDENED at the write boundary and the resolver's
// defence-in-depth copy was left matching email_local alone, so a managed drive
// templated on `sub` was refused on write and still mounted — publishing the
// sign-in subject in an object name `docker volume ls` and `kubectl get pvc`
// print without inspecting anything.
//
// The two existing tests pin two SPECIFIC templates (user_drives_resolve_test.go
// 626-673). This pins the SET, table-driven over types.HomeTemplates, so the
// next template added to the enum is covered on both sides the day it is added
// rather than the day someone remembers — which is the exact shape of the
// original divergence.
//
// It asserts AGREEMENT, not a hand-listed expectation: the resolver must refuse
// a managed drive on exactly the template set the write boundary refuses. A copy
// of the rule that happens to agree today is the thing that drifted.
func TestManagedTemplateWriteAndResolveAgree(t *testing.T) {
	managed := []types.DriveBackend{types.DriveBackendDockerVolume, types.DriveBackendK8sPVC}

	for _, backend := range managed {
		for _, tmpl := range types.HomeTemplates {
			name := string(backend) + "/" + string(tmpl)
			t.Run(name, func(t *testing.T) {
				// THE WRITE BOUNDARY's answer, taken from the shared predicate
				// rather than from ValidateUserDrive's whole-row validation —
				// the row also has to satisfy size/reclaim/host-root rules that
				// have nothing to do with this question.
				writeRefuses := types.ManagedBackendRejectsTemplate(backend, tmpl)

				d := driveFixture(func(d *types.UserDrive) {
					d.Backend = backend
					d.HomeTemplate = tmpl
				})
				st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
				runner := "docker"
				if backend == types.DriveBackendK8sPVC {
					runner = "k8s"
				}
				_, err := driveServerOn(st, runner).resolveUserDrive(driveMemberCtx([]string{"eng"}, false))
				resolveRefuses := errors.Is(err, errDriveUnmountable)

				if writeRefuses != resolveRefuses {
					t.Fatalf("write boundary refuses=%v but the resolver refuses=%v (err=%v) — the two sites "+
						"answer 'may a managed drive carry this template' differently, which is exactly how a "+
						"managed `sub` row came to be refused on write and still mount",
						writeRefuses, resolveRefuses, err)
				}
				// And the rule is the one it is meant to be: hash is the only
				// template a managed backend may carry.
				if want := tmpl != types.HomeTemplateHash; writeRefuses != want {
					t.Errorf("managed %s + %s: refused=%v, want %v — hash is the ONLY template a managed backend "+
						"may use", backend, tmpl, writeRefuses, want)
				}
			})
		}
	}

	// The counterweight: a SHARE backend takes every template, so the agreement
	// above is not satisfied by both sites refusing everything.
	t.Run("a share backend takes every template", func(t *testing.T) {
		for _, tmpl := range types.HomeTemplates {
			if types.ManagedBackendRejectsTemplate(types.DriveBackendHostPath, tmpl) {
				t.Errorf("a host_path (share) drive refused template %s — the managed rule leaked onto a backend "+
					"whose directories are named by whoever owns the share", tmpl)
			}
		}
	})
}
