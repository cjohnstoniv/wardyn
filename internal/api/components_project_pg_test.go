// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// racingStore runs one write immediately before a named read: an admin's change
// committing between two statements of one member request, made deterministic.
type racingStore struct {
	store.PG
	once                              sync.Once
	beforeOrgList, beforeRestrictions func()
}

func (s *racingStore) ListComponents(ctx context.Context, owner string) ([]types.Component, error) {
	if owner == "" && s.beforeOrgList != nil {
		s.once.Do(s.beforeOrgList)
	}
	return s.PG.ListComponents(ctx, owner)
}

func (s *racingStore) ListCapabilityRestrictions(ctx context.Context) (map[string]map[string]bool, error) {
	if s.beforeRestrictions != nil {
		s.once.Do(s.beforeRestrictions)
	}
	return s.PG.ListCapabilityRestrictions(ctx)
}

// TestPG_Components_ListJudgesNoRowOnAnOlderSnapshot: an org component created
// while a member's GET /me/components is in flight is never shown to a member
// nobody granted it to, wherever in the request its create commits. The list
// reads the rows first and the capability snapshot after, so every row it
// judges is judged on a snapshot that already holds that row's restriction.
func TestPG_Components_ListJudgesNoRowOnAnOlderSnapshot(t *testing.T) {
	for _, point := range []string{"before the org rows are listed", "before the restrictions are read"} {
		t.Run(point, func(t *testing.T) {
			e := newComponentsPG(t)
			id := uuid.New()
			create := func() {
				_, err := e.st.CreateRestrictedComponent(context.Background(), types.Component{
					ID: id, Name: "Payroll bridge", CreatedBy: "sub-admin",
					Definition: types.ComponentDefinition{Hosts: []string{"payroll-internal.example.com"}, Config: map[string]string{"TENANT": "x"}},
				}, capComponent, "sub-admin")
				if err != nil {
					t.Errorf("racing create: %v", err)
				}
			}
			rs := &racingStore{PG: e.st}
			if point == "before the org rows are listed" {
				rs.beforeOrgList = create
			} else {
				rs.beforeRestrictions = create
			}
			e.h.srv.cfg.Store = rs
			e.h.srv.router = e.h.srv.routes()

			w := e.do(t, http.MethodGet, "/api/v1/me/components", e.member, "")
			if w.Code != http.StatusOK {
				t.Fatalf("list = %d: %s", w.Code, w.Body.String())
			}
			if !e.restricted(t, id) {
				t.Fatal("the racing create did not run, so the list was never raced")
			}
			for _, leak := range []string{id.String(), "Payroll bridge", "payroll-internal", "TENANT"} {
				if strings.Contains(w.Body.String(), leak) {
					t.Errorf("an ungranted member's list carries %q of an org component created during the request: %s", leak, w.Body.String())
				}
			}
		})
	}
}
