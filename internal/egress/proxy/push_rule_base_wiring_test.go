// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestNewServerHoldsAnEntryWithNoSetToThePushRuleBase: the base rules in the
// sidecar's config reach the Server's proxy, so a push to an SCM entry the run
// did not bring a rule set for is refused under the content-rules source rather
// than going through unchecked. The unit tests set the compiled base by hand;
// this one goes through the config the sidecar actually boots from.
func TestNewServerHoldsAnEntryWithNoSetToThePushRuleBase(t *testing.T) {
	cfg := &Config{
		RunID:           uuid.New(),
		ControlPlaneURL: "http://127.0.0.1:1", // never dialed: the push is refused before any mint
		RunToken:        "tok",
		Listen:          "127.0.0.1:0",
		PATGrants:       map[string]PATGrant{prSetsServerHost: {GrantID: uuid.New(), Username: "oauth2"}},
		PushRuleSets: []types.PushRuleSet{{
			Provider: string(types.GitProviderAzureDevOps), Host: prSetsServerHost, Org: "coll", BasePath: "/tfs",
			Rules: types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}},
		}},
		PushRuleBase: &types.PushRulesSpec{DenyPaths: []string{"secrets/**"}},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfigBytes(raw)
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	out := &bytes.Buffer{}
	srv, err := newServer(context.Background(), loaded, &http.Client{Timeout: time.Second}, out, serverHooks{res: publicResolver{}})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	body := string(recordedPush(t, "refs/heads/main", map[string]string{"secrets/key": "k\n"}))
	rec := httptest.NewRecorder()
	srv.proxy.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
		"/wardyn/git/"+prSetsServerHost+"/tfs/other/proj/_git/app/git-receive-pack", strings.NewReader(body)))
	if rec.Code != http.StatusForbidden || !strings.Contains(out.String(), `"rule_source":"`+ruleSourceGitRules+`"`) {
		t.Fatalf("a push to an entry with no set: %d %q log %q, want a %s refusal from the base rules",
			rec.Code, rec.Body.String(), out.String(), ruleSourceGitRules)
	}
}
