// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"
)

// #2276: a scan run never carries a person's Azure DevOps credential (TestSourceScan_NeverTakesTheADOLane), so the
// Scan door must not tell an unconnected per-user member to connect. The Build door, whose runs do carry it, still does.
func TestScanDoorDoesNotOfferToConnectToAzureDevOps(t *testing.T) {
	srv, st, _ := adoRunHarness(t, true)
	id := adoWorkspaceWithADORepo(t, st)
	cookie := adoOperatorSession(t)

	scan := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+id+"/scan", cookie, "{}")
	assertGitCredential422(t, scan)
	body := scan.Body.String()
	if !strings.Contains(body, sourceScanNoADOConnectionRefusal) {
		t.Errorf("scan refusal = %s, want the sentence %q", body, sourceScanNoADOConnectionRefusal)
	}
	if strings.Contains(body, gitCredentialNotConnectedRefusal) || strings.Contains(body, "start the run again") || strings.Contains(body, `"cause"`) {
		t.Errorf("scan refusal still offers to connect: %s", body)
	}

	build := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+id+"/build", cookie, "{}")
	assertGitCredential422(t, build)
	if !strings.Contains(build.Body.String(), gitCredentialNotConnectedRefusal) {
		t.Errorf("build refusal = %s, want the connect sentence unchanged", build.Body.String())
	}
}
