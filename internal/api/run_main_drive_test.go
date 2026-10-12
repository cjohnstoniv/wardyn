// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func finite(v int) types.LimitAmount { return types.LimitAmount{Value: v} }

// TestMainDriveFor pins the one size expression: the request is soft, a size the run's policy
// writes is hard and a request may only lower it, and the effective cap cuts what remains.
func TestMainDriveFor(t *testing.T) {
	capped := func(def, capv int, capSrc types.LimitSource) types.EffectiveLimit {
		return types.EffectiveLimit{Default: finite(def), Cap: finite(capv), DefaultSource: types.LimitSourceDeployment, CapSource: capSrc}
	}
	unlimited := types.EffectiveLimit{Default: types.LimitAmount{Unlimited: true}, Cap: types.LimitAmount{Unlimited: true}}
	for _, tc := range []struct {
		name            string
		policy, request int
		lim             types.EffectiveLimit
		want            mainDrive
	}{{
		name: "a request is soft", request: 4096, lim: unlimited,
		want: mainDrive{MiB: 4096, RequestedMiB: 4096, Basis: client.MainDriveBasisRequest, Soft: true},
	}, {
		name: "a policy size is hard", policy: 8192, lim: unlimited,
		want: mainDrive{MiB: 8192, Basis: client.MainDriveBasisPolicy},
	}, {
		name: "a request above the policy is cut to it and stays hard", policy: 10240, request: 51200, lim: unlimited,
		want: mainDrive{MiB: 10240, RequestedMiB: 51200, Basis: client.MainDriveBasisRequest, ClampedBy: client.MainDriveClampPolicy},
	}, {
		name: "a request below the policy is hard with the request's number", policy: 10240, request: 2048, lim: unlimited,
		want: mainDrive{MiB: 2048, RequestedMiB: 2048, Basis: client.MainDriveBasisRequest},
	}, {
		name: "a request above the cap takes the cap's source", request: 100000, lim: capped(2048, 8192, types.LimitSourcePool),
		want: mainDrive{MiB: 8192, RequestedMiB: 100000, Basis: client.MainDriveBasisRequest, ClampedBy: client.MainDriveClampPool, Soft: true},
	}, {
		name: "a policy size above the cap is cut and stays hard", policy: 50000, lim: capped(2048, 8192, types.LimitSourceGovernance),
		want: mainDrive{MiB: 8192, Basis: client.MainDriveBasisPolicy, ClampedBy: client.MainDriveClampGovernance},
	}, {
		name: "the default is soft", lim: capped(2048, 8192, types.LimitSourceDeployment),
		want: mainDrive{MiB: 2048, Basis: client.MainDriveBasisDefault, DefaultSource: types.LimitSourceDeployment, Soft: true},
	}, {
		// A cap that stands in as the default (here a profile's maximum) is reported as the server's:
		// default_source is pool or deployment only.
		name: "a cap standing in as the default reports a closed default source", lim: types.EffectiveLimit{
			Default: finite(2048), Cap: finite(2048), DefaultSource: types.LimitSourceGovernance, CapSource: types.LimitSourceGovernance},
		want: mainDrive{MiB: 2048, Basis: client.MainDriveBasisDefault, DefaultSource: types.LimitSourceDeployment, Soft: true},
	}, {
		name: "a pool's default stays the pool's", lim: types.EffectiveLimit{Default: finite(1024), Cap: finite(4096), DefaultSource: types.LimitSourcePool, CapSource: types.LimitSourcePool},
		want: mainDrive{MiB: 1024, Basis: client.MainDriveBasisDefault, DefaultSource: types.LimitSourcePool, Soft: true},
	}, {
		name: "none is no size", lim: unlimited,
		want: mainDrive{Basis: client.MainDriveBasisNone},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mainDriveFor(tc.policy, tc.request, tc.lim); got != tc.want {
				t.Errorf("mainDriveFor = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestPolicyDiskStaysHardUnderRequest: a policy of 10 GiB and a request of 50 GiB is 10 GiB, and
// dispatch keeps it hard, so a host that cannot cap it refuses rather than running uncapped.
func TestPolicyDiskStaysHardUnderRequest(t *testing.T) {
	md := mainDriveFor(10240, 51200, types.EffectiveLimit{Cap: types.LimitAmount{Unlimited: true}, Default: types.LimitAmount{Unlimited: true}})
	if md.MiB != 10240 || md.Soft {
		t.Fatalf("size = %d soft = %v, want 10240 hard", md.MiB, md.Soft)
	}
	res, _, _ := ephemeralDispatchWith(t, types.RunPolicySpec{Resources: &types.ResourceLimits{DiskMiB: 10240}}, md.decision())
	if res.DiskMiB != 10240 || res.DiskMiBSoft {
		t.Errorf("dispatch got %d MiB soft=%v, want 10240 hard", res.DiskMiB, res.DiskMiBSoft)
	}
}

// TestDispatchHonoursTheMainDriveDecision: the fold's size and soft bit reach the runner, and the
// envelope records the basis beside the older bit. A decision never reads back out of the spec,
// so a requested size is soft even though dispatch wrote it onto the policy.
func TestDispatchHonoursTheMainDriveDecision(t *testing.T) {
	md := mainDriveFor(0, 4096, types.EffectiveLimit{Cap: finite(8192), Default: finite(2048), CapSource: types.LimitSourcePool})
	res, events, runID := ephemeralDispatchWith(t, types.RunPolicySpec{}, md.decision())
	if res.DiskMiB != 4096 || !res.DiskMiBSoft {
		t.Fatalf("dispatch got %d MiB soft=%v, want 4096 soft", res.DiskMiB, res.DiskMiBSoft)
	}
	ev := findAudit(events, runID, "run.policy.resolve", "success")
	if ev == nil {
		t.Fatal("no run.policy.resolve envelope")
	}
	var datum map[string]any
	if err := json.Unmarshal(ev.Data, &datum); err != nil {
		t.Fatal(err)
	}
	if datum["disk_mib_basis"] != string(client.MainDriveBasisRequest) || datum["disk_mib_filled"] != true {
		t.Errorf("envelope basis=%v filled=%v, want request and true (kept for older readers)", datum["disk_mib_basis"], datum["disk_mib_filled"])
	}
}

// ephemeralDispatchWith drives one dispatch carrying the fold's main-drive decision.
func ephemeralDispatchWith(t *testing.T, policy types.RunPolicySpec, md *mainDriveDecision) (runner.Resources, []types.AuditEvent, uuid.UUID) {
	t.Helper()
	fr := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	srv.cfg.Store = ceilingDispatchStore{dispatchTestStore: st}
	run.Task = ""
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest", Policy: policy, MainDrive: md,
	})
	return fr.lastSpec.Resources, audit.events, run.ID
}

// TestResourcesDiskBound: resources.disk_mib is 0 to 16 TiB; past either end is 400 resources_invalid.
func TestResourcesDiskBound(t *testing.T) {
	for _, tc := range []struct {
		mib     int
		refused bool
	}{{0, false}, {1, false}, {maxRunResourceDisk, false}, {maxRunResourceDisk + 1, true}, {-1, true}} {
		ref := resourcesRefusal(createRunRequest{Resources: &client.RunResources{DiskMiB: tc.mib}})
		if tc.refused != (ref != nil) || (ref != nil && (ref.status != 400 || ref.body.Reason != reasonResourcesInvalid)) {
			t.Errorf("disk_mib %d: refusal %+v, want refused=%v as 400 %s", tc.mib, ref, tc.refused, reasonResourcesInvalid)
		}
	}
}

// TestRunFitSpecAsksTheQuotaAboutTheDecidedSize: the quota fit judges the size the run gets, on a
// copy: the caller's spec and its Resources block are never written.
func TestRunFitSpecAsksTheQuotaAboutTheDecidedSize(t *testing.T) {
	own := &types.ResourceLimits{CPUMillis: 1000, DiskMiB: 100}
	spec := types.RunPolicySpec{Resources: own}
	got := (&Server{}).runFitSpec(spec, mainDrive{MiB: 4096})
	if got.Resources.DiskMiB != 4096 || got.Resources.CPUMillis != 1000 {
		t.Fatalf("fit spec resources = %+v, want 4096 MiB disk and the 1000 m CPU kept", got.Resources)
	}
	if own.DiskMiB != 100 || spec.Resources != own {
		t.Errorf("the caller's spec was written: %+v", own)
	}
}

// TestMainDriveUnenforceableUnpooledSentence: a run in no pool is not told to choose another pool.
func TestMainDriveUnenforceableUnpooledSentence(t *testing.T) {
	srv := &Server{cfg: Config{Runner: &fakeRunner{capsDisk: types.StorageEnforcementNone}}}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/preflight", nil)
	f := &runFold{mode: foldPreflight, w: rec, r: r, req: &createRunRequest{},
		spec:  types.RunPolicySpec{Resources: &types.ResourceLimits{DiskMiB: 4096}},
		place: runPlacement{limits: types.EffectiveRunLimits{DiskMiB: types.EffectiveLimit{Cap: types.LimitAmount{Unlimited: true}, Default: types.LimitAmount{Unlimited: true}}}}}
	if srv.stepMainDrive(f, &foldRecorder{}) || rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an unpooled policy size on a runner that cannot cap disk: %d %s", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); !strings.Contains(body, mainDriveUnenforceableUnpooledMsg()) || strings.Contains(body, "pool") {
		t.Errorf("body = %s, want the unpooled sentence", body)
	}
}

// TestPreviewKeepsThePolicyCutWarning: the policy preview's warning whitelist passes the sentence a
// request cut by the run's own policy gets, so the three doors say the same words.
func TestPreviewKeepsThePolicyCutWarning(t *testing.T) {
	got := previewSafeWarnings([]string{warnDiskPolicyCut, "not on the list"})
	if len(got) != 1 || got[0] != warnDiskPolicyCut {
		t.Errorf("previewSafeWarnings = %v, want only %q", got, warnDiskPolicyCut)
	}
}
