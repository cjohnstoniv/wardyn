// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_GovernanceDecisionSingleConnection(t *testing.T) {
	t.Setenv("WARDYN_GOVERNANCE_SECOND_HUMAN", "true")
	cfg := revocationPool(t).Config()
	cfg.MaxConns, cfg.MinConns = 1, 0
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	st := store.NewPG(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	typeID := "pool-type-" + uuid.NewString()
	if _, err := st.CreateUserType(ctx, types.UserType{ID: typeID, Name: typeID, CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	srv := api.New(api.Config{Store: st, OIDC: &oidc.Authenticator{}, SessionRevocations: &pgSessionRevocations{pool: pool}, Audit: &fakeAuditRecorder{}})
	token := func() string {
		raw := "wdn_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		sub := uuid.NewString()
		truncated := false
		if _, err := st.CreateAPIToken(ctx, types.APIToken{ID: uuid.New(), Principal: sub, Email: sub + "@example.test", Role: oidc.RoleAdmin, UserType: types.UserTypeStandard, Name: "pool approver", GroupsTruncated: &truncated}, raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	proposer, approver := token(), token()
	call := func(path, raw, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+raw)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}
	body := `{"subject_type":"user_type","subject":"` + typeID + `","capability":"agent","value":"claude-code","effect":"deny"}`
	w := call("/api/v1/permissions/grants", proposer, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("propose = %d %s", w.Code, w.Body)
	}
	var pending struct {
		Change types.GovernanceChange `json:"pending_change"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}

	if pending.Change.ID == uuid.Nil {
		t.Fatalf("no change ID: %s", w.Body)
	}
	w = call("/api/v1/governance/changes/"+pending.Change.ID.String()+"/approve", approver, "")
	if w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	ch, err := st.GetGovernanceChange(ctx, pending.Change.ID)
	if err != nil || ch.State != types.GovernanceChangeApplied {
		t.Fatalf("decision = %+v, %v", ch, err)
	}
	grants, err := st.ListCapabilityGrants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(grants, func(g types.CapabilityGrant) bool { return g.Subject == typeID }) {
		t.Fatal("the governed grant was not committed")
	}
}
