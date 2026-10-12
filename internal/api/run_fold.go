// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// foldMode is the door a run request is folded for. The three doors share one
// fold so Review and the policy preview cannot drift from the launch they
// describe; what differs between them is named by a step's modes in
// runFoldSteps or by an explicit f.mode test inside a step, both of which
// TestPreflightMirrorsLaunchGates reads.
type foldMode uint8

const (
	foldCreate    foldMode = 1 << iota // POST /runs: real gates, audited, renews, persists afterwards
	foldPreflight                      // POST /runs/preflight: launch's refusals, dry, no row
	foldPreview                        // the policy preview: no credential values, no capacity probes
	foldAll       = foldCreate | foldPreflight | foldPreview
)

// runFold is one request's trip through runFoldSteps: the inputs the door's
// prologue decided, and what each step resolved for the steps after it and for
// the door's own edge (create's persistence and audit, Review's grading, the
// preview's facts).
type runFold struct {
	mode    foldMode
	w       http.ResponseWriter
	r       *http.Request
	req     *createRunRequest
	ceiling governanceCeiling
	reqCC   types.ConfinementClass
	// baseline is the operator's egress baseline, read ONCE per request by stepBaseline: the
	// confinement floor, the autonomy posture and the door's own grading and facts all read this
	// value, so one request cannot grade on two sets.
	baseline composer.Baseline

	spec          types.RunPolicySpec
	policyID      *uuid.UUID
	policyWarns   []string
	source        policySourceRecord
	ephemeralDirs []string
	driveMount    *types.DriveMount
	// previewDriveReadOnly is the drive's posture at the preview, which resolves
	// no mount: nil when the run has no drive.
	previewDriveReadOnly *bool
	// seed is what seeding the request's workspaces decided (attached first,
	// the primary leading); wsRefs adds every other one the spec references.
	seed   workspaceSeed
	wsRefs []types.Workspace
	// attachments is the run's mount map and startFolder its one working
	// directory (WARDYN_START_FOLDER), both from stepWorkspaces.
	attachments       []client.RunAttachment
	startFolder       string
	present           map[string]bool
	reqEvents         []requirementAuditEntry
	directGitHubAdded []string
	comps             runComponents
	enforced          types.ConfinementClass
	mpChoice          runProviderChoice
	modelCred         modelCredentialFacts
	autonomy          types.AutonomyResolution
	autonomyWarns     []string
	scmSite           types.SiteConfig
	adoGrade          adoEntraGrade
	bedrockGrade      bedrockCredGrade
	adoNarrowed       string
	fitWarnings       []string
	end               createEnd
	prov              []provenanceRow
	pushSets          []types.PushRuleSet
	// place is where the run goes (stepPlacement): the resolved pool and the fully eligible targets.
	place runPlacement
	// mainDrive is the main drive's size (stepMainDrive).
	mainDrive mainDrive
	// queued says the run waits for a slot (pool-queue.md section 6): its pool has a concurrent-run cap,
	// set by stepPlacement at create and Review. A waiting run renews no credential, takes no host
	// capacity or deployment slot, and is refused for its size only when it can never fit; promotion
	// applies the rest when a slot frees.
	queued bool
	// promoted says the fold is the queue's start replaying a promoted run: promotion has already
	// admitted it against the host, the deployment cap and the fit, so those steps stand down.
	promoted bool
	// barrierOptions are Review's barriers for the resolved pool under this draft's floors (stepConfinement).
	barrierOptions []client.BarrierOption
	// harnesses is a run-mode request's harness set and image the image that
	// carries it (stepHarnessSet; nil when a custom image or workspace decides);
	// harnessChoices is each harness's model-provider choice (stepModelProvider).
	harnesses      []types.RunHarness
	image          *client.ImageFact
	harnessChoices []harnessChoice
	// localDelivery is the credential delivery policy a local run was judged on (stepLocalEligibility);
	// create persists its grants by the same policy, never a second read.
	localDelivery placement.DeliveryPolicy
}

// runFoldStep is one gate or fold of the shared sequence. run writes its own
// refusal and returns false once it has answered.
type runFoldStep struct {
	run   func(s *Server, f *runFold, rec *foldRecorder) bool
	modes foldMode
}

// runFoldSteps is every gate between a decoded, authorized run request and
// create's mint, in the order all three doors meet them. Adding a gate is one
// entry here plus its name in TestPreflightMirrorsLaunchGates' pinned list.
//
// The ORDER is security-relevant and is the contract; do not sort it:
//   - The request contract first: it reads only the request, so a malformed or
//     not-yet-honoured field answers before any store read or credential lookup.
//   - The egress baseline is read once after request validation; the floor, the autonomy
//     posture and the door's grading read that one value.
//   - Policy first: the member clamp and grant narrowing run before anything
//     is granted, and every later step reads the clamped spec.
//   - Workspace seed after policy, the drive after the seed: seeding can set
//     req.Image, so its re-checks follow it, and its onboarding gate is the
//     chokepoint on the resolved spec that no drive may answer before.
//   - The harness set after the workspaces: every included harness meets the
//     primary's doors, and the set's image refusals read the seeded image and
//     the workspaces' base images.
//   - Workspace requirements, the direct-GitHub union and components before
//     the confinement floor: the CC3 blast-radius floor reads the grants and
//     hosts they add (invariant 5), and components see what admins already
//     credentialed. The dry doors widen workspace egress before the
//     requirements fold; create widens after the mint (unionRunEgress).
//   - A placement the caller's profile denies is refused first of the placement
//     steps, so no later step spends work on a run that cannot be placed.
//   - Placement straight after that: the pool resolves (a missing or stale default
//     refuses, never falls back), the run's size is bounded by the pool, governance,
//     the deployment and the runners' capacity, its candidates meet the policy floor and
//     that applied size one by one, and the fully eligible set waits for create to pick a
//     target by run id.
//   - Resources straight after placement: the applied size is written into the spec, so
//     host capacity, the quota fit and dispatch read what the dry doors report.
//   - Local eligibility straight after placement, before anything can renew
//     or mint: the admitted plan's refusals and the unsupported-local refusal
//     answer before the confinement floor and the provider's liveness step.
//   - Confinement, then the model provider, then autonomy: the autonomy grade
//     reads the enforced class and the chosen model credential.
//   - The tool lane after autonomy, the last step that can put the run on the
//     gated lane: a person's rule, or a policy rule that holds or blocks, puts
//     a background run there too, and the person's allows are held to the
//     policy with the lane's default under it.
//   - Azure DevOps standing and PAT narrowing read autonomy's site snapshot.
//   - The run's end reads the resolved pool, the governance ceiling and the deployment,
//     so it sits after placement and refuses before capacity.
//   - Host capacity, the run cap and the quota fit last, so a refusal leaves
//     no identity and no run row.
//   - The dry doors' derived egress after every gate: create adds those hosts
//     after the mint (unionRunEgress), so nothing above may grade them.
var runFoldSteps = []runFoldStep{
	{(*Server).stepRunContract, foldAll},
	{(*Server).stepBaseline, foldAll},
	{(*Server).stepPolicy, foldAll},
	{(*Server).stepSeedWorkspace, foldAll},
	{(*Server).stepDrive, foldAll},
	{(*Server).stepWorkspaces, foldAll},
	{(*Server).stepHarnessSet, foldAll},
	{(*Server).stepPushRuleSets, foldAll},
	{(*Server).stepPreviewEgress, foldPreflight | foldPreview},
	{(*Server).stepRequirements, foldAll},
	{(*Server).stepGitHubEgress, foldAll},
	{(*Server).stepComponents, foldAll},
	{(*Server).stepPlacementDenied, foldAll},
	{(*Server).stepPlacement, foldAll},
	{(*Server).stepResources, foldAll},
	{(*Server).stepLocalEligibility, foldAll},
	{(*Server).stepConfinement, foldAll},
	{(*Server).stepPlacementCapability, foldAll},
	{(*Server).stepMainDrive, foldAll},
	{(*Server).stepModelProvider, foldAll},
	{(*Server).stepAWSCLIProfile, foldAll},
	{(*Server).stepAutonomy, foldAll},
	{(*Server).stepToolPolicy, foldAll},
	{(*Server).stepGitHubCLIRepo, foldCreate | foldPreflight},
	{(*Server).stepADOStanding, foldAll},
	{(*Server).stepPATNarrowing, foldPreflight | foldPreview},
	{(*Server).stepRunEnd, foldAll},
	{(*Server).stepHostCapacity, foldCreate | foldPreflight},
	{(*Server).stepRunCap, foldCreate | foldPreflight},
	{(*Server).stepRunFit, foldCreate | foldPreflight},
	{(*Server).stepDerivedEgress, foldPreflight | foldPreview},
}

// foldRunRequest runs runFoldSteps for mode over a request the door has
// already decoded and authorized. ok=false means a step has answered.
//
// The preflight door passes no reqCC: it parses confinement_class inside
// stepConfinement, where launch's order meets it.
func (s *Server) foldRunRequest(w http.ResponseWriter, r *http.Request, mode foldMode,
	req *createRunRequest, ceiling governanceCeiling, reqCC types.ConfinementClass,
) (runFold, bool) {
	f := runFold{mode: mode, w: w, r: r, req: req, ceiling: ceiling, reqCC: reqCC, promoted: startingPromoted(r.Context())}
	rec := &foldRecorder{}
	for _, step := range runFoldSteps {
		if step.modes&mode != 0 && !step.run(s, &f, rec) {
			return runFold{}, false
		}
	}
	f.prov = rec.result()
	return f, true
}
