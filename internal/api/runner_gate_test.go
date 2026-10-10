// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func TestRunnerGateRefusesBeforeSideEffects(t *testing.T) {
	for _, mode := range []string{"absent", "disabled", "unreadable"} {
		for _, door := range []string{"self-token", "admin-token", "register", "claim"} {
			t.Run(mode+"/"+door, func(t *testing.T) {
				srv, f, h := newRunnerRegistrationServer(t)
				f.siteCfg.Runners = nil
				if mode == "disabled" {
					f.siteCfg.Runners = &types.RunnerSettings{}
				}
				want := http.StatusForbidden
				if mode == "unreadable" {
					f.configErr = errors.New("read failed")
					want = http.StatusServiceUnavailable
				}
				id := uuid.New()
				now := time.Now().UTC()
				raw := newBearer("wdr_")
				f.rows[id] = types.Runner{ID: id, Owner: "alice", KeyFingerprint: "fp", State: types.RunnerUnclaimed, CreatedAt: now, OrgURLSHA256: federation.OrgURLSHA256(srv.cfg.RunnerOrgURL)}
				f.tokens[raw] = types.RunnerRegistrationToken{ID: uuid.New(), Owner: "alice", CreatedAt: now, ExpiresAt: now.Add(time.Hour), OrgURLSHA256: federation.OrgURLSHA256(srv.cfg.RunnerOrgURL)}
				pub, _, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := json.Marshal(types.RunnerRegisterRequest{Token: raw, PublicKey: pub, Name: "laptop"})
				path := "/api/v1/runners/register"
				switch door {
				case "self-token":
					path = "/api/v1/me/runners/tokens"
					body = []byte("{}")
				case "admin-token":
					path = "/api/v1/runners/tokens"
					body = []byte(`{"owner":"alice"}`)
				case "claim":
					path = "/api/v1/me/runners/" + id.String() + "/claim"
					body = []byte(`{"fingerprint":"fp"}`)
				}
				cookie := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleAdmin, false)
				w := doSSO(t, srv, http.MethodPost, path, cookie, string(body))
				if w.Code != want {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if len(f.tokens) != 1 || f.tokens[raw].ConsumedAt != nil || len(f.rows) != 1 || f.rows[id].State != types.RunnerUnclaimed {
					t.Fatal("closed gate changed token or runner")
				}
				for _, ev := range h.audit.snapshot() {
					if ev.Action == "runner.token.create" || ev.Action == "runner.enrol" || ev.Action == "runner.claim" {
						t.Fatal("closed gate recorded success action")
					}
				}
			})
		}
	}
}
