// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// Two scanned names the secret store keeps under one name give one row, required when either is.
func TestSeedSourceRequirements_RequiredWinsAtTheStoredName(t *testing.T) {
	for name, needs := range map[string][]workspacescan.SecretNeed{
		"the required name sorts first": {{Name: "SESSION_SECRET"}, {Name: "session_secret", Optional: true}},
		"the required name sorts later": {{Name: "SESSION_SECRET", Optional: true}, {Name: "session_secret"}},
	} {
		seed := seedSourceRequirements(types.SourceRepo, "", workspacescan.WorkspaceProfile{RequiredSecrets: needs})
		if got := seed["secret:session-secret"]; len(seed) != 1 || got.Level != "required" {
			t.Errorf("%s: seed = %v, want one required secret:session-secret row", name, seed)
		}
	}
}

// seedCaptureStore is scanSourceStore keeping the seed each local scan writes.
type seedCaptureStore struct {
	*scanSourceStore
	seeds map[uuid.UUID]map[string]types.WorkspaceRequirement
}

func (s *seedCaptureStore) SetSourceScanResultUnfenced(ctx context.Context, id uuid.UUID, profile []byte, status types.WorkspaceStatus, seed map[string]types.WorkspaceRequirement) (types.Source, error) {
	s.seeds[id] = seed
	return s.scanSourceStore.SetSourceScanResultUnfenced(ctx, id, profile, status, seed)
}

// A local folder scan that could not read part of the folder keeps the rows an earlier scan
// seeded. One that stopped at a scan budget drops a row it no longer finds, as a complete one does:
// that folder meets the same budget on every scan.
func TestScanSource_AnUnreadPartKeepsEarlierScanSeededRowsAndABudgetStopDoesNot(t *testing.T) {
	pastTheReadCap := strings.Repeat("#"+strings.Repeat("x", 1022)+"\n", 1100) // 1100 lines of 1024 bytes
	pastTheLineBudget := strings.Repeat("\n", 4000)                            // NEW_TOKEN's line makes 4001
	for name, tc := range map[string]struct {
		body       string
		unreadable bool // a folder beside the template that the server cannot read
		keeps      bool
	}{
		"complete":                 {"NEW_TOKEN=\n", false, false},
		"past the line budget":     {"NEW_TOKEN=\n" + pastTheLineBudget, false, false},
		"a file past the read cap": {"NEW_TOKEN=\n" + pastTheReadCap, false, true},
		"an unreadable folder":     {"NEW_TOKEN=\n", true, true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".env.example"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.unreadable {
				if runtime.GOOS == "windows" || os.Geteuid() == 0 {
					t.Skip("needs POSIX permissions that bind the scanning user")
				}
				locked := filepath.Join(dir, "locked")
				if err := os.Mkdir(locked, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
			}
			id := uuid.New()
			st := &seedCaptureStore{scanSourceStore: &scanSourceStore{sources: map[uuid.UUID]types.Source{id: {
				ID: id, Kind: types.SourceLocalDir, Locator: dir,
				Requirements: map[string]types.WorkspaceRequirement{"secret:old-token": {Level: "required", Provenance: "scan_seeded"}},
			}}}, seeds: map[uuid.UUID]map[string]types.WorkspaceRequirement{}}
			srv := New(baseTestConfig(newHarness(t), st))
			if w := do(t, srv, http.MethodPost, "/api/v1/sources/"+id.String()+"/scan", adminToken, ""); w.Code != http.StatusOK {
				t.Fatalf("scan = %d %s, want 200", w.Code, w.Body.String())
			}
			seed := st.seeds[id]
			if _, kept := seed["secret:old-token"]; kept != tc.keeps || seed["secret:new-token"].Level != "required" {
				t.Errorf("seed = %v; want secret:new-token required and secret:old-token kept = %v", seed, tc.keeps)
			}
		})
	}
}

// sourceScanPriorStore is sourceScanUploadStore with a source row to read, keeping the seed each
// upload writes.
type sourceScanPriorStore struct {
	*sourceScanUploadStore
	prior  map[string]types.WorkspaceRequirement
	getErr error
	seed   map[string]types.WorkspaceRequirement
}

func (s *sourceScanPriorStore) GetSource(_ context.Context, id uuid.UUID) (types.Source, error) {
	return types.Source{ID: id, Requirements: s.prior}, s.getErr
}

func (s *sourceScanPriorStore) SetSourceScanResult(ctx context.Context, id uuid.UUID, profile []byte, status types.WorkspaceStatus, runID uuid.UUID, seed map[string]types.WorkspaceRequirement) (types.Source, error) {
	s.seed = seed
	return s.sourceScanUploadStore.SetSourceScanResult(ctx, id, profile, status, runID, seed)
}

// A repo scan upload that says it could not read part of the clone keeps the rows an earlier scan
// seeded, and leaves the contract as it is when the source's rows cannot be read. One that says it
// stopped at a scan budget rebuilds them from what it found.
func TestUploadScanResult_AnUnreadScanKeepsEarlierScanSeededRowsAndABudgetStopDoesNot(t *testing.T) {
	prior := map[string]types.WorkspaceRequirement{"secret:old-token": {Level: "required", Provenance: "scan_seeded"}}
	for name, tc := range map[string]struct {
		completeness string // the facts' completeness field
		getErr       error
		want         []string // the seed's keys; nil with nilSeed is a seed of nil
		nilSeed      bool
	}{
		"complete":                      {`"truncated":false`, nil, []string{"secret:new-token"}, false},
		"past a scan budget":            {`"truncated":true`, nil, []string{"secret:new-token"}, false},
		"unread":                        {`"unread":true`, nil, []string{"secret:new-token", "secret:old-token"}, false},
		"unread, its rows not readable": {`"unread":true`, errors.New("db down"), nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			sourceID, runID := uuid.New(), uuid.New()
			st := &sourceScanPriorStore{
				sourceScanUploadStore: &sourceScanUploadStore{run: types.AgentRun{ID: runID, Task: "source scan", SourceID: &sourceID}},
				prior:                 prior, getErr: tc.getErr,
			}
			srv := New(baseTestConfig(h, st))
			facts := fmt.Sprintf(`{%s,"secret_requirements":[{"name":"NEW_TOKEN"}]}`, tc.completeness)
			if w := do(t, srv, http.MethodPut, "/api/v1/internal/scan-results/"+runID.String(), h.mintRunToken(t, runID), facts); w.Code != http.StatusOK {
				t.Fatalf("upload = %d %s, want 200", w.Code, w.Body.String())
			}
			if got := slices.Sorted(maps.Keys(st.seed)); (st.seed == nil) != tc.nilSeed || !slices.Equal(got, tc.want) {
				t.Errorf("seed = %v (nil %v); want keys %v (nil %v)", st.seed, st.seed == nil, tc.want, tc.nilSeed)
			}
		})
	}
}
