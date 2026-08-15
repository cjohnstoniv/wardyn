// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// The invariant that once broke live: every row the scan seeder writes must
// pass the SAME validation any PUT of that contract goes through — a scan
// detects env-var names ("AWS_DEFAULT_REGION") the secret store can never
// hold, so the seeder maps them onto the storable grammar instead of writing
// keys that wedge the Requirements save with a 400 forever.
func TestSeedSourceRequirements_EveryRowValidates(t *testing.T) {
	p := workspacescan.WorkspaceProfile{
		RequiredSecrets: []workspacescan.SecretNeed{
			{Name: "AWS_DEFAULT_REGION"},
			{Name: "DATABASE_URL", Optional: true},
			{Name: "already-storable.key"},
			{Name: "___"}, // nothing storable remains — must be skipped, not seeded
		},
		EgressDomains: []string{"registry.npmjs.org"},
	}
	seed := seedSourceRequirements(types.SourceLocalDir, "/srv/app", p)

	for key, req := range seed {
		if msg := validateWorkspaceRequirement(key, req); msg != "" {
			t.Fatalf("seeder wrote a row its own validator rejects: %s", msg)
		}
	}
	if row := seed["secret:aws-default-region"]; row.Level != "required" || row.Provenance != "scan_seeded" {
		t.Fatalf("env-var name not mapped to storable secret name: %+v (keys=%v)", row, keysOf(seed))
	}
	if row := seed["secret:database-url"]; row.Level != "optional" {
		t.Fatalf("optional flag lost in mapping: %+v", row)
	}
	if _, ok := seed["secret:already-storable.key"]; !ok {
		t.Fatalf("already-storable name must pass through unchanged: keys=%v", keysOf(seed))
	}
	if len(seed) != 5 { // 3 secrets + 1 egress + 1 write (unstorable name skipped)
		t.Fatalf("want 5 seeded rows, got %d: %v", len(seed), keysOf(seed))
	}
}

// TestSeedSourceRequirements_SuggestedEgressNeverSeeded is the end-to-end half
// of the W6-S1-3 fix, at the exact boundary the finding's acceptance
// criterion names: "an AI-suggested host is NOT auto-unioned into a run's
// allowlist without operator approval". workspacescan.AdviseProfile (ai.go)
// now lands an AI-suggested host in SuggestedEgress, never EgressDomains (see
// ai_test.go); this test locks in the OTHER half of that guarantee — that
// seedSourceRequirements (the only producer of the "required"/auto-unioned
// egress:<host> contract rows applyWorkspaceRequirements folds into a run's
// AllowedDomains) reads ONLY EgressDomains, so a host that exists ONLY in
// SuggestedEgress never becomes a contract row — even for a future profile
// carrying nothing BUT an AI suggestion, egress can never auto-widen.
func TestSeedSourceRequirements_SuggestedEgressNeverSeeded(t *testing.T) {
	p := workspacescan.WorkspaceProfile{
		SuggestedEgress: []string{"evil.example.com"},
	}
	seed := seedSourceRequirements(types.SourceRepo, "", p)
	if len(seed) != 0 {
		t.Fatalf("a profile with only SuggestedEgress must seed nothing (it is display-only, never a contract row): %v", keysOf(seed))
	}
}

func keysOf(m map[string]types.WorkspaceRequirement) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// localDirScanStore is the minimal store.Store scanLocalDirSource needs:
// SetSourceScanResultUnfenced, captured so the test can inspect exactly what
// was persisted.
type localDirScanStore struct {
	store.Store
	saved  []byte
	status types.WorkspaceStatus
}

func (s *localDirScanStore) SetSourceScanResultUnfenced(_ context.Context, _ uuid.UUID, profile []byte, status types.WorkspaceStatus, _ map[string]types.WorkspaceRequirement) (types.Source, error) {
	s.saved, s.status = profile, status
	return types.Source{Status: status}, nil
}

// TestScanLocalDirSource_ConsultsAIAdvisor is the W9-S1-6 regression:
// WARDYN_SCAN_AI_ADVISOR used to run ONLY for the sandboxed repo-scan upload
// lane (uploadSourceScanResult) — a local_dir source's host-side scan called
// workspacescan.Scan directly and never consulted s.cfg.ScanAIAdvisor at all,
// even though the flag's own help text makes no repo-only distinction. A real
// on-disk unrecognized build file (setup.py — unmappedBuildFiles,
// markers.go) makes ShouldAdvise true exactly the way a real scan would.
func TestScanLocalDirSource_ConsultsAIAdvisor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "setup.py"), []byte("# unmapped build file"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	invoked := false
	adv := func(_ context.Context, _ workspacescan.ScanFacts, base workspacescan.WorkspaceProfile) workspacescan.WorkspaceProfile {
		invoked = true
		out := base
		out.Tools = []string{"advised-tool"}
		out.Source = workspacescan.SourceAIAssisted
		return out
	}
	st := &localDirScanStore{}
	srv := New(Config{Store: st, ScanAIAdvisor: adv})

	src := types.Source{ID: uuid.New(), Kind: types.SourceLocalDir, Locator: dir}
	profile, aiRan, aiChanged, detail, ok := srv.scanLocalDirSource(context.Background(), src)
	if !ok {
		t.Fatalf("scanLocalDirSource failed: %s", detail)
	}
	if !invoked {
		t.Fatal("the AI advisor was never invoked for a local_dir source (W9-S1-6)")
	}
	if !aiRan || !aiChanged {
		t.Fatalf("aiRan/aiChanged = %v/%v, want true/true", aiRan, aiChanged)
	}
	if len(profile.Tools) != 1 || profile.Tools[0] != "advised-tool" {
		t.Fatalf("advisor addition not reflected in the returned profile: tools=%v", profile.Tools)
	}
	var saved workspacescan.WorkspaceProfile
	if err := json.Unmarshal(st.saved, &saved); err != nil {
		t.Fatalf("persisted profile: %v", err)
	}
	if len(saved.Tools) != 1 || saved.Tools[0] != "advised-tool" {
		t.Fatalf("advisor addition not persisted: tools=%v", saved.Tools)
	}
}

// TestScanLocalDirSource_AIDisabled_ByteIdentical: nil advisor (the flag off)
// must leave scanLocalDirSource's persisted profile byte-identical to the
// deterministic DeriveProfile — the same "OFF is a no-op" guarantee the
// sandboxed repo-scan upload lane pins (TestUploadScanResult_AIDisabled_ByteIdentical).
func TestScanLocalDirSource_AIDisabled_ByteIdentical(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "setup.py"), []byte("# unmapped build file"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	st := &localDirScanStore{}
	srv := New(Config{Store: st}) // no ScanAIAdvisor: feature off

	src := types.Source{ID: uuid.New(), Kind: types.SourceLocalDir, Locator: dir}
	_, aiRan, aiChanged, detail, ok := srv.scanLocalDirSource(context.Background(), src)
	if !ok {
		t.Fatalf("scanLocalDirSource failed: %s", detail)
	}
	if aiRan || aiChanged {
		t.Fatalf("aiRan/aiChanged = %v/%v, want false/false with the advisor disabled", aiRan, aiChanged)
	}
	want := mustJSON(workspacescan.DeriveProfile(workspacescan.CollectFacts(dir)))
	if string(st.saved) != string(want) {
		t.Fatalf("disabled profile not byte-identical to deterministic derive\n got=%s\nwant=%s", st.saved, want)
	}
}
