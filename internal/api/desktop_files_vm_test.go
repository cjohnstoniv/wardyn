// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// desktopKindStore gives the env's run the machine kind a case asks for.
type desktopKindStore struct {
	*envelopeStore
	kind types.MachineKind
}

func (s *desktopKindStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	run, err := s.envelopeStore.GetRun(ctx, id)
	run.MachineKind = s.kind
	return run, err
}

// newBuiltInDesktopEnv is a files env whose run is of kind and whose policy declares no UI app.
func newBuiltInDesktopEnv(t *testing.T, kind types.MachineKind, desktop *types.DesktopControls) (*desktopFilesEnv, *envelopeStore) {
	t.Helper()
	e := newDesktopFilesEnv(t, desktop)
	env := e.srv.cfg.Store.(*envelopeStore)
	env.spec.UIApps = nil
	e.srv.cfg.Store = &desktopKindStore{envelopeStore: env, kind: kind}
	return e, env
}

// A Linux VM run's built-in desktop, with no UI app declared, moves files under the run's desktop rules
// as a declared desktop does.
func TestDesktopFiles_LinuxVMBuiltInDesktopFollowsTheRules(t *testing.T) {
	e, _ := newBuiltInDesktopEnv(t, types.MachineLinuxVM, &types.DesktopControls{Upload: types.TransferAllowed, Download: types.TransferAllowed})
	if w := doSSO(t, e.srv, http.MethodPut, e.path("notes.csv"), e.ses["owner"], fileSentinel); w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	if got := string(e.folder.files["notes.csv"]); got != fileSentinel {
		t.Fatalf("the desktop got %q", got)
	}
	if w := doSSO(t, e.srv, http.MethodGet, e.path(""), e.ses["owner"], ""); w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	w := doSSO(t, e.srv, http.MethodGet, e.path("agent-report.csv"), e.ses["owner"], "")
	if w.Code != http.StatusOK || w.Body.String() != "agent,wrote,this\n" {
		t.Fatalf("download: %d %q", w.Code, w.Body.String())
	}
	if len(e.transferRows("upload")) != 1 || len(e.transferRows("download")) != 1 {
		t.Fatalf("each transfer is audited: %+v", e.audit.snapshot())
	}
}

// The built-in desktop never widens what a run may move: the run's own desktop rules and its owner's walls
// (sealed into the rules the run was dispatched under) still block, and nothing reaches the sandbox.
func TestDesktopFiles_LinuxVMBuiltInDesktopStaysBlocked(t *testing.T) {
	allowed := &types.DesktopControls{Upload: types.TransferAllowed, Download: types.TransferAllowed}
	wsrv, wst, _ := uvServer(t)
	saveWalls(t, wst, wallsSub, types.PersonalWallsSpec{Desktop: &types.DesktopControls{Upload: types.TransferBlocked}})
	walled := types.RunPolicySpec{MinConfinementClass: types.CC2, Desktop: allowed}
	sealPolicy(&walled, wsrv.personalWallsSealLayer(context.Background(), types.AgentRun{ID: uuid.New(), CreatedBy: wallsSub}))
	if walled.Desktop.Effective().Upload != types.TransferBlocked {
		t.Fatalf("the walls did not block upload: %+v", walled.Desktop)
	}

	for _, c := range []struct {
		what string
		spec types.RunPolicySpec
	}{
		{"the run's policy", types.RunPolicySpec{MinConfinementClass: types.CC2, Desktop: &types.DesktopControls{Upload: types.TransferBlocked, Download: types.TransferBlocked}}},
		{"the owner's walls", walled},
	} {
		t.Run(c.what, func(t *testing.T) {
			e, env := newBuiltInDesktopEnv(t, types.MachineLinuxVM, nil)
			env.spec = c.spec
			w := doSSO(t, e.srv, http.MethodPut, e.path("in.txt"), e.ses["owner"], "x")
			if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonDesktopTransferBlocked {
				t.Fatalf("upload: %d %s, want 403 %s", w.Code, w.Body.String(), reasonDesktopTransferBlocked)
			}
			if c.spec.Desktop.Effective().Download == types.TransferBlocked {
				w = doSSO(t, e.srv, http.MethodGet, e.path("agent-report.csv"), e.ses["owner"], "")
				if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonDesktopTransferBlocked {
					t.Fatalf("download: %d %s, want 403 %s", w.Code, w.Body.String(), reasonDesktopTransferBlocked)
				}
			}
			if e.folder.execs != 0 {
				t.Fatalf("%d execs reached the sandbox", e.folder.execs)
			}
		})
	}
}

// The profile's deny_ui_apps and deny_interactive close the built-in desktop's Files as they close a declared one.
func TestDesktopFiles_LinuxVMBuiltInDesktopUnderAProfile(t *testing.T) {
	for _, limits := range []types.GovernanceLimits{{DenyUIApps: true}, {DenyInteractive: true}} {
		e, env := newBuiltInDesktopEnv(t, types.MachineLinuxVM, &types.DesktopControls{Upload: types.TransferAllowed, Download: types.TransferAllowed})
		unit := types.GovernanceUnit{ID: uuid.New(), Name: "ci", Limits: limits}
		e.srv.cfg.Store = &desktopDoorUnitStore{envelopeStore: env, unit: unit, kind: types.MachineLinuxVM}
		w := doSSO(t, e.srv, http.MethodPut, e.path("in.txt"), e.ses["owner"], "x")
		if w.Code != http.StatusForbidden || reasonOf(t, w) != "governance_unit" || e.folder.execs != 0 {
			t.Fatalf("limits %+v: %d %s, %d execs; want 403 governance_unit and nothing sent", limits, w.Code, w.Body.String(), e.folder.execs)
		}
	}
}

// With no UI app declared, only a Linux VM has a desktop: a container run and a macOS VM run still answer
// that there is none.
func TestDesktopFiles_NoDeclaredAppAndNoBuiltInDesktop(t *testing.T) {
	for _, kind := range []types.MachineKind{"", types.MachineContainer, types.MachineMacOSVM} {
		e, _ := newBuiltInDesktopEnv(t, kind, &types.DesktopControls{Upload: types.TransferAllowed, Download: types.TransferAllowed})
		w := doSSO(t, e.srv, http.MethodPut, e.path("in.txt"), e.ses["owner"], "x")
		if w.Code != http.StatusConflict || reasonOf(t, w) != reasonDesktopTransferUnavailable || e.folder.execs != 0 {
			t.Errorf("machine kind %q: %d %s, %d execs; want 409 %s", kind, w.Code, w.Body.String(), e.folder.execs, reasonDesktopTransferUnavailable)
		}
	}
}
