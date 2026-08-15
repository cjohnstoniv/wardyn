// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The hydrate pass is the three-tier split's compat engine: a workspace whose
// composition lives in ATTACHMENTS reads back with the same derived
// Sources/Profile/Status/BaseImage view every pre-split consumer expects, plus
// the folded EffectiveRequirements the run path consumes.

func hydrateStore(t *testing.T) store.PG {
	t.Helper()
	pool := databaseBefore(t, "zzz") // every migration applied
	return store.PG{Pool: pool}
}

func mkSource(t *testing.T, s store.PG, kind types.SourceKind, locator string, reqs map[string]types.WorkspaceRequirement) types.Source {
	t.Helper()
	src, err := s.UpsertSource(context.Background(), types.Source{
		ID: uuid.New(), Kind: kind, Locator: locator, Name: "src",
		Requirements: reqs, Status: types.WorkspacePendingScan,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	return src
}

func TestPG_HydrateAttachments_DerivedViewAndFold(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()

	src := mkSource(t, s, types.SourceLocalDir, "/home/me/payments", map[string]types.WorkspaceRequirement{
		"egress:api.stripe.com":     {Level: "required", Provenance: "scan_seeded"},
		"secret:stripe-key":         {Level: "required", Provenance: "scan_seeded"},
		"write:/home/me/payments":   {Level: "optional", Provenance: "operator_set"},
		"write:/home/me/other-repo": {Level: "required", Provenance: "operator_set"}, // cross-path: must drop
	})
	prof, _ := json.Marshal(map[string]any{"languages": []string{"go"}, "confidence": "high"})
	if _, err := s.SetSourceScanResultUnfenced(ctx, src.ID, prof, types.WorkspaceScanned, nil); err != nil {
		t.Fatalf("set source scan: %v", err)
	}

	img, err := s.UpsertBaseImage(ctx, types.BaseImageEntry{
		ID: uuid.New(), Kind: "registry", Name: "go", Image: "docker.io/library/golang:1.22",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("upsert image: %v", err)
	}

	ws, err := s.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "pay", Status: types.WorkspacePendingScan,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral}}, // legacy column: ignored once attachments exist
		Attachments: []types.WorkspaceAttachment{
			{Ephemeral: true, Target: "/scratch"},
			{SourceID: &src.ID, Target: "/work/pay", Writable: true,
				Overrides: map[string]string{"secret:stripe-key": types.OverrideOff}},
		},
		BaseImageID: &img.ID,
		Requirements: map[string]types.WorkspaceRequirement{
			"integration:corp-artifactory": {Level: "required", Provenance: "operator_set"},
		},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Derived Sources view, in attachment order, per-attachment knobs applied.
	if len(ws.Sources) != 2 || ws.Sources[0].Type != types.WorkspaceSourceTypeEphemeral {
		t.Fatalf("Sources = %+v, want [ephemeral, local_dir]", ws.Sources)
	}
	if ws.Sources[1].Path != "/home/me/payments" || !ws.Sources[1].Writable || ws.Sources[1].Target != "/work/pay" {
		t.Errorf("Sources[1] = %+v, want the attached dir with writable+target", ws.Sources[1])
	}
	// Status derives worst-of (one scanned source + ephemeral = scanned).
	if ws.Status != types.WorkspaceScanned {
		t.Errorf("Status = %s, want scanned (derived)", ws.Status)
	}
	// Profile derives from the source's scan.
	if ws.Profile == nil {
		t.Error("Profile must derive from the attached source's scan")
	}
	// BaseImage derives from the catalog ref.
	if ws.BaseImage == nil || ws.BaseImage.Image != "docker.io/library/golang:1.22" {
		t.Errorf("BaseImage = %+v, want the catalog row's view", ws.BaseImage)
	}
	// The fold: source rows minus the off-override minus the cross-path write,
	// plus the workspace overlay.
	eff := ws.EffectiveRequirements
	if _, present := eff["secret:stripe-key"]; present {
		t.Error("off-overridden source row must not fold in")
	}
	if _, present := eff["write:/home/me/other-repo"]; present {
		t.Error("cross-path write claim must be dropped by the fold")
	}
	if eff["egress:api.stripe.com"].Level != "required" {
		t.Errorf("source egress row missing from fold: %+v", eff)
	}
	if eff["integration:corp-artifactory"].Level != "required" {
		t.Errorf("overlay row missing from fold: %+v", eff)
	}
	// The overlay itself stays JUST the overlay.
	if len(ws.Requirements) != 1 {
		t.Errorf("Requirements (overlay) = %+v, want only the workspace's own row", ws.Requirements)
	}
}

// A scan's discoveries land on the SOURCE's own contract as a PROVENANCE-AWARE
// REBUILD: scan_seeded rows appear, an operator's existing (non-scan_seeded)
// row is NEVER overwritten by a re-scan (level OR provenance), and the
// workspace level only ever aggregates via the fold.
func TestPG_SourceScanSeedsOwnContract(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()

	src := mkSource(t, s, types.SourceLocalDir, "/home/me/payments", map[string]types.WorkspaceRequirement{
		// The operator already declared this secret OPTIONAL — a scan that
		// detects it as required must not flip it.
		"secret:STRIPE_KEY": {Level: "optional", Provenance: "operator_set"},
	})
	prof, _ := json.Marshal(map[string]any{"confidence": "high"})
	seed := map[string]types.WorkspaceRequirement{
		"secret:STRIPE_KEY":       {Level: "required", Provenance: "scan_seeded"},
		"egress:api.stripe.com":   {Level: "required", Provenance: "scan_seeded"},
		"write:/home/me/payments": {Level: "optional", Provenance: "scan_seeded"},
	}
	got, err := s.SetSourceScanResultUnfenced(ctx, src.ID, prof, types.WorkspaceScanned, seed)
	if err != nil {
		t.Fatalf("scan write: %v", err)
	}
	if row := got.Requirements["secret:STRIPE_KEY"]; row.Level != "optional" || row.Provenance != "operator_set" {
		t.Errorf("operator row overwritten by scan seed: %+v", row)
	}
	if row := got.Requirements["egress:api.stripe.com"]; row.Level != "required" || row.Provenance != "scan_seeded" {
		t.Errorf("scan-discovered egress row missing: %+v", got.Requirements)
	}
	if row := got.Requirements["write:/home/me/payments"]; row.Level != "optional" || row.Provenance != "scan_seeded" {
		t.Errorf("dir write row missing: %+v", got.Requirements)
	}

	// The FENCED lane seeds identically (the governed repo-scan upload path).
	repo := mkSource(t, s, types.SourceRepo, "github.com/acme/lib", nil)
	runID := uuid.New()
	if err := s.ClaimSourceActiveRun(ctx, repo.ID, runID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, err = s.SetSourceScanResult(ctx, repo.ID, prof, types.WorkspaceScanned, runID, map[string]types.WorkspaceRequirement{
		"egress:proxy.golang.org": {Level: "required", Provenance: "scan_seeded"},
	})
	if err != nil {
		t.Fatalf("fenced scan write: %v", err)
	}
	if got.Requirements["egress:proxy.golang.org"].Provenance != "scan_seeded" {
		t.Errorf("fenced lane did not seed: %+v", got.Requirements)
	}
}

// TestPG_SourceScanSeed_RebuildDropsStaleScanSeededRows is the junk-secrets-
// wall regression at the store: a rescan's seed REBUILDS the scan_seeded
// subset of a source's contract instead of only filling gaps, so a name a
// prior scan found but this one no longer does is DROPPED — while an
// operator's own row survives untouched regardless of what the new seed
// contains.
func TestPG_SourceScanSeed_RebuildDropsStaleScanSeededRows(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	prof, _ := json.Marshal(map[string]any{"confidence": "high"})

	assertRebuilt := func(t *testing.T, label string, reqs map[string]types.WorkspaceRequirement) {
		t.Helper()
		if _, present := reqs["secret:junk"]; present {
			t.Errorf("%s: stale scan_seeded row must be dropped on rescan, still: %+v", label, reqs)
		}
		if row := reqs["secret:real"]; row.Level != "required" || row.Provenance != "operator_set" {
			t.Errorf("%s: operator_set row must survive a rescan untouched: %+v", label, reqs)
		}
		if row := reqs["secret:real2"]; row.Level != "required" || row.Provenance != "scan_seeded" {
			t.Errorf("%s: newly-seeded row missing: %+v", label, reqs)
		}
	}
	seedReal2 := map[string]types.WorkspaceRequirement{
		"secret:real2": {Level: "required", Provenance: "scan_seeded"},
	}
	starting := map[string]types.WorkspaceRequirement{
		"secret:junk": {Level: "optional", Provenance: "scan_seeded"},
		"secret:real": {Level: "required", Provenance: "operator_set"},
	}

	// Unfenced (local_dir inline scan) lane.
	dirSrc := mkSource(t, s, types.SourceLocalDir, "/home/me/junky", starting)
	got, err := s.SetSourceScanResultUnfenced(ctx, dirSrc.ID, prof, types.WorkspaceScanned, seedReal2)
	if err != nil {
		t.Fatalf("unfenced rescan write: %v", err)
	}
	assertRebuilt(t, "unfenced", got.Requirements)

	// Fenced (governed repo-scan upload) lane — same rebuild, different $N.
	repoSrc := mkSource(t, s, types.SourceRepo, "github.com/acme/junky", starting)
	runID := uuid.New()
	if err := s.ClaimSourceActiveRun(ctx, repoSrc.ID, runID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, err = s.SetSourceScanResult(ctx, repoSrc.ID, prof, types.WorkspaceScanned, runID, seedReal2)
	if err != nil {
		t.Fatalf("fenced rescan write: %v", err)
	}
	assertRebuilt(t, "fenced", got.Requirements)

	// The other half of the fix: an EMPTY (non-nil) seed — a rescan that
	// legitimately finds nothing — must still drop stale scan_seeded rows, not
	// leave them stranded because an empty map used to serialize to SQL NULL,
	// indistinguishable from "scan failed, don't touch anything".
	assertEmptyRebuilt := func(t *testing.T, label string, reqs map[string]types.WorkspaceRequirement) {
		t.Helper()
		if _, present := reqs["secret:junk"]; present {
			t.Errorf("%s: stale scan_seeded row must be dropped by an EMPTY rescan too, still: %+v", label, reqs)
		}
		if row := reqs["secret:real"]; row.Level != "required" || row.Provenance != "operator_set" {
			t.Errorf("%s: operator_set row must survive an empty rescan untouched: %+v", label, reqs)
		}
	}
	emptySeed := map[string]types.WorkspaceRequirement{} // non-nil, zero entries

	dirSrc2 := mkSource(t, s, types.SourceLocalDir, "/home/me/junky-empty", starting)
	got, err = s.SetSourceScanResultUnfenced(ctx, dirSrc2.ID, prof, types.WorkspaceScanned, emptySeed)
	if err != nil {
		t.Fatalf("unfenced empty-seed rescan write: %v", err)
	}
	assertEmptyRebuilt(t, "unfenced empty-seed", got.Requirements)

	repoSrc2 := mkSource(t, s, types.SourceRepo, "github.com/acme/junky-empty", starting)
	runID2 := uuid.New()
	if err := s.ClaimSourceActiveRun(ctx, repoSrc2.ID, runID2); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, err = s.SetSourceScanResult(ctx, repoSrc2.ID, prof, types.WorkspaceScanned, runID2, emptySeed)
	if err != nil {
		t.Fatalf("fenced empty-seed rescan write: %v", err)
	}
	assertEmptyRebuilt(t, "fenced empty-seed", got.Requirements)
}

// An EPHEMERAL-ONLY composition (the wizard's default scratch floor) must
// derive scanned + the deterministic empty profile — what the legacy scan
// stamped for it. Deriving nil here regressed the Requirements step to
// "No contract yet": the four tabs never mounted on the Getting-started
// wizard's default path.
func TestPG_HydrateEphemeralOnly_DeterministicProfile(t *testing.T) {
	s := hydrateStore(t)
	ws, err := s.CreateWorkspace(context.Background(), types.Workspace{
		ID: uuid.New(), Name: "scratch", Status: types.WorkspacePendingScan,
		Attachments: []types.WorkspaceAttachment{{Ephemeral: true, Target: "/home/agent/work"}},
		CreatedAt:   time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ws.Status != types.WorkspaceScanned {
		t.Errorf("Status = %s, want scanned (nothing to scan)", ws.Status)
	}
	var prof struct {
		Confidence string `json:"confidence"`
		Source     string `json:"source"`
	}
	if err := json.Unmarshal(ws.Profile, &prof); err != nil {
		t.Fatalf("Profile = %s, want the deterministic empty profile: %v", ws.Profile, err)
	}
	if prof.Confidence != "high" || prof.Source != "deterministic" {
		t.Errorf("Profile = %+v, want confidence=high source=deterministic", prof)
	}
}

// Pre-split rows (empty attachments) pass through byte-identically: embedded
// sources stay the truth, and EffectiveRequirements IS the overlay.
func TestPG_HydrateLegacyRow_PassthroughIdentity(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	overlay := map[string]types.WorkspaceRequirement{
		"secret:acme-key": {Level: "required", Provenance: "operator_set"},
	}
	ws, err := s.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "legacy", Status: types.WorkspacePendingScan,
		Sources:      []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/x", Writable: true}},
		Requirements: overlay,
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(ws.Sources) != 1 || ws.Sources[0].Path != "/x" {
		t.Errorf("legacy Sources must pass through: %+v", ws.Sources)
	}
	if ws.Status != types.WorkspacePendingScan {
		t.Errorf("legacy Status must pass through, got %s", ws.Status)
	}
	a, _ := json.Marshal(ws.EffectiveRequirements)
	b, _ := json.Marshal(overlay)
	if string(a) != string(b) {
		t.Errorf("EffectiveRequirements = %s, want the overlay identity %s", a, b)
	}
}

func TestPG_MergeWorkspaceRequirements_AtomicAndCapped(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	ws, err := s.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "m", Status: types.WorkspacePendingScan,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Merge adds without clobbering.
	if _, err := s.MergeWorkspaceRequirements(ctx, ws.ID, map[string]types.WorkspaceRequirement{
		"egress:a.example": {Level: "required", Provenance: "operator_set"},
	}); err != nil {
		t.Fatalf("merge 1: %v", err)
	}
	got, err := s.MergeWorkspaceRequirements(ctx, ws.ID, map[string]types.WorkspaceRequirement{
		"egress:b.example": {Level: "required", Provenance: "operator_set"},
	})
	if err != nil {
		t.Fatalf("merge 2: %v", err)
	}
	if len(got.Requirements) != 2 {
		t.Errorf("overlay after two merges = %+v, want both rows (|| is additive)", got.Requirements)
	}

	// The cap is evaluated on the POST-merge total, not the pre-merge count:
	// 2 rows already stored + 254 new = exactly 256 must succeed; 256 + 1
	// more must refuse. A pre-merge-only check would let the bulk merge
	// overshoot to 258, which is the bug this pins.
	big := map[string]types.WorkspaceRequirement{}
	for i := 0; i < 254; i++ {
		big["egress:h"+uuid.NewString()[:8]+".example"] = types.WorkspaceRequirement{Level: "optional", Provenance: "operator_set"}
	}
	got, err = s.MergeWorkspaceRequirements(ctx, ws.ID, big)
	if err != nil {
		t.Fatalf("bulk merge up to the cap: %v", err)
	}
	if len(got.Requirements) != 256 {
		t.Fatalf("overlay after bulk merge = %d keys, want exactly 256 (at the cap)", len(got.Requirements))
	}
	_, err = s.MergeWorkspaceRequirements(ctx, ws.ID, map[string]types.WorkspaceRequirement{
		"egress:one-more.example": {Level: "required", Provenance: "operator_set"},
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("merge past the cap = %v, want ErrConflict", err)
	}
	// The refused merge must never have landed: the total stays at 256.
	final, err := s.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Requirements) != 256 {
		t.Errorf("overlay after a refused merge = %d keys, want unchanged at 256 (never overshoot)", len(final.Requirements))
	}

	// Unknown workspace is NotFound, not Conflict.
	if _, err := s.MergeWorkspaceRequirements(ctx, uuid.New(), map[string]types.WorkspaceRequirement{
		"egress:x.example": {Level: "required", Provenance: "operator_set"},
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("merge on missing workspace = %v, want ErrNotFound", err)
	}
}

// Delete-in-use is loud: names, 409-shaped; force detaches.
// TestPG_DeleteSourceInUse pins STORE-1: a workspace whose ONLY attachment is
// the source being force-deleted must NOT be silently emptied to '[]' —
// hydrate reads an empty attachments array as the pre-split marker and
// resurrects the deleted source from the workspace's stale legacy `sources`
// column, so DeleteSource(force=true) refuses instead, naming the
// would-be-orphaned workspaces. A workspace that attaches the SAME source
// alongside something else (an ephemeral scratch dir here) is unaffected by
// the refusal and force-detaches normally.
func TestPG_DeleteSourceInUse(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	src := mkSource(t, s, types.SourceRepo, "acme/widgets", nil)
	// ws-a and ws-b attach ONLY this source: force-deleting it would leave
	// them with zero attachments.
	for _, name := range []string{"ws-a", "ws-b"} {
		if _, err := s.CreateWorkspace(ctx, types.Workspace{
			ID: uuid.New(), Name: name, Status: types.WorkspacePendingScan,
			Sources:     []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral}},
			Attachments: []types.WorkspaceAttachment{{SourceID: &src.ID}},
			CreatedAt:   time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	// ws-c attaches the SAME source ALONGSIDE an ephemeral scratch dir —
	// force-detaching it leaves ws-c with one attachment, not zero.
	if _, err := s.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "ws-c", Status: types.WorkspacePendingScan,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral}},
		Attachments: []types.WorkspaceAttachment{
			{SourceID: &src.ID}, {Ephemeral: true, Target: "/home/agent/scratch"},
		},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create ws-c: %v", err)
	}
	names, err := s.WorkspacesAttaching(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 || names[0] != "ws-a" || names[1] != "ws-b" || names[2] != "ws-c" {
		t.Fatalf("attaching = %v, want all three, sorted", names)
	}

	// Refused: ws-a/ws-b would be orphaned. Nothing detaches — not even ws-c,
	// which would have survived on its own (all-or-nothing per force call).
	err = s.DeleteSource(ctx, src.ID, true)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("force delete with a would-be-orphaned workspace = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "ws-a") || !strings.Contains(err.Error(), "ws-b") {
		t.Errorf("error must name the would-be-orphaned workspaces, got: %v", err)
	}
	if strings.Contains(err.Error(), "ws-c") {
		t.Errorf("ws-c is not orphaned (it keeps its ephemeral attachment) and must not be named: %v", err)
	}
	left, err := s.WorkspacesAttaching(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 3 {
		t.Errorf("a refused force-delete must not detach anyone, still: %v", left)
	}

	// Delete ws-a/ws-b outright (their attachment IS the source's only use to
	// them) — now nothing would be orphaned, and the SAME force-delete succeeds.
	for _, name := range []string{"ws-a", "ws-b"} {
		id := findWorkspaceIDByName(ctx, t, s, name)
		if err := s.DeleteWorkspace(ctx, id); err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
	}
	if err := s.DeleteSource(ctx, src.ID, true); err != nil {
		t.Fatalf("force delete once nothing would be orphaned: %v", err)
	}
	left, err = s.WorkspacesAttaching(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("attachments must be stripped on a successful force delete, still: %v", left)
	}
}

// findWorkspaceIDByName is TestPG_DeleteSourceInUse's helper: the store keys
// workspaces by id, but the test only ever named them.
func findWorkspaceIDByName(ctx context.Context, t *testing.T, s store.PG, name string) uuid.UUID {
	t.Helper()
	all, err := s.ListWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ws := range all {
		if ws.Name == name {
			return ws.ID
		}
	}
	t.Fatalf("no workspace named %q", name)
	return uuid.UUID{}
}

// A genuinely-absent id must read as NotFound, not Conflict: the atomic
// `DELETE ... WHERE NOT EXISTS` predicate's RowsAffected()==0 alone can't tell
// "no such row" from "still attached", so DeleteSource probes existence to
// report the honest verdict once the delete has already refused.
func TestPG_DeleteSourceUnknownIDIs404(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	if err := s.DeleteSource(ctx, uuid.New(), false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown id (non-force) = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSource(ctx, uuid.New(), true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown id (force) = %v, want ErrNotFound", err)
	}
}

// Delete-in-use is loud for the catalog too — the base-image twin of
// TestPG_DeleteSourceInUse: refuses (Conflict) while referenced, names the
// referencing workspaces, force detaches (base_image_id -> NULL, the derived
// recommended build) and deletes.
func TestPG_DeleteBaseImageInUse(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	img, err := s.UpsertBaseImage(ctx, types.BaseImageEntry{
		ID: uuid.New(), Kind: "custom", Name: "img", Image: "ubuntu:24.04",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("upsert base image: %v", err)
	}
	for _, name := range []string{"ws-a", "ws-b"} {
		if _, err := s.CreateWorkspace(ctx, types.Workspace{
			ID: uuid.New(), Name: name, Status: types.WorkspacePendingScan,
			Sources:     []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral}},
			BaseImageID: &img.ID,
			CreatedAt:   time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	names, err := s.WorkspacesUsingBaseImage(ctx, img.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "ws-a" || names[1] != "ws-b" {
		t.Fatalf("using = %v, want both names, sorted", names)
	}
	if err := s.DeleteBaseImage(ctx, img.ID, false); !errors.Is(err, store.ErrConflict) {
		t.Errorf("non-force delete while in use = %v, want ErrConflict", err)
	}
	// Force-detach then delete: references cleared, catalog row gone.
	if err := s.DeleteBaseImage(ctx, img.ID, true); err != nil {
		t.Fatalf("force delete: %v", err)
	}
	left, err := s.WorkspacesUsingBaseImage(ctx, img.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("references must be stripped on force delete, still: %v", left)
	}
}

// TestPG_UpsertBaseImage_IdentityHitNeverTouchesName pins the OTHER half of
// W7-S1-3 — the safety property a first attempt at this finding got wrong by
// folding `name = EXCLUDED.name` straight into UpsertBaseImage's own ON
// CONFLICT clause: this upsert is not only the Add dialog's create path, it
// is also the PASSTHROUGH a workspace/run resolves its declared base-image
// spec through (internal/api/sources.go's attachSourcesAndBaseImage-shaped
// caller), which always derives an auto-placeholder name
// (lastPathSegment(image)) with NO rename intent whatsoever. Applying
// EXCLUDED.name unconditionally meant every such passthrough call silently
// renamed an operator's custom-named catalog row back to that placeholder —
// the exact "can never be renamed" promise this finding exists to fix, just
// inverted. An identity hit must leave name alone; see
// TestPG_UpdateBaseImageName_Renames for the actual (scoped) rename path.
func TestPG_UpsertBaseImage_IdentityHitNeverTouchesName(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()

	first, err := s.UpsertBaseImage(ctx, types.BaseImageEntry{
		ID: uuid.New(), Kind: "custom", Name: "my-custom-devbox", Image: "ubuntu:24.04",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("upsert base image (create): %v", err)
	}

	// Same identity (kind, image, steps), a different id and an
	// auto-derived placeholder name — exactly the shape a passthrough
	// caller with no rename intent sends (never an operator's typed value).
	hit, err := s.UpsertBaseImage(ctx, types.BaseImageEntry{
		ID: uuid.New(), Kind: "custom", Name: "ubuntu:24.04", Image: "ubuntu:24.04",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("upsert base image (identity hit): %v", err)
	}
	if hit.ID != first.ID {
		t.Fatalf("identity hit returned a different row: got %s, want the original %s", hit.ID, first.ID)
	}
	if hit.Name != "my-custom-devbox" {
		t.Errorf("identity-hit Name = %q, want the ORIGINAL %q — a passthrough upsert must never clobber a custom name", hit.Name, "my-custom-devbox")
	}

	// Confirm the write actually landed (not just the RETURNING row).
	reread, err := s.GetBaseImage(ctx, first.ID)
	if err != nil {
		t.Fatalf("get base image: %v", err)
	}
	if reread.Name != "my-custom-devbox" {
		t.Errorf("reread Name = %q, want %q", reread.Name, "my-custom-devbox")
	}
}

// TestPG_UpdateBaseImageName_Renames pins the ACTUAL W7-S1-3 fix: the
// scoped rename path handleCreateBaseImage calls on an identity hit where
// the request carried an explicit name (the Add dialog's re-POST-to-rename
// shape) — the only route (UI, API, CLI, SDK) to rename a catalog row at
// all, since none of them ever gained a dedicated edit endpoint.
func TestPG_UpdateBaseImageName_Renames(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()

	first, err := s.UpsertBaseImage(ctx, types.BaseImageEntry{
		ID: uuid.New(), Kind: "custom", Name: "original-name", Image: "ubuntu:22.04",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("upsert base image (create): %v", err)
	}

	renamed, err := s.UpdateBaseImageName(ctx, first.ID, "renamed")
	if err != nil {
		t.Fatalf("update base image name: %v", err)
	}
	if renamed.ID != first.ID || renamed.Name != "renamed" {
		t.Fatalf("UpdateBaseImageName = %+v, want id %s name %q", renamed, first.ID, "renamed")
	}

	reread, err := s.GetBaseImage(ctx, first.ID)
	if err != nil {
		t.Fatalf("get base image: %v", err)
	}
	if reread.Name != "renamed" {
		t.Errorf("reread Name = %q, want %q — the Add dialog's rename must not be silently discarded", reread.Name, "renamed")
	}
}

// A genuinely-absent id must read as NotFound, not Conflict — the base-image
// twin of TestPG_DeleteSourceUnknownIDIs404.
func TestPG_DeleteBaseImageUnknownIDIs404(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	if err := s.DeleteBaseImage(ctx, uuid.New(), false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown id (non-force) = %v, want ErrNotFound", err)
	}
	if err := s.DeleteBaseImage(ctx, uuid.New(), true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown id (force) = %v, want ErrNotFound", err)
	}
}
