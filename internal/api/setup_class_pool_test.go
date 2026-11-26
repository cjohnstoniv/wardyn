// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const thisCluster = "this server's cluster"

func TestClassPoolCheck_WordsTheRow(t *testing.T) {
	if _, ok := classPoolCheck("k8s_isolated_pool", []types.ConfinementClass{types.CC3}, nil); ok {
		t.Fatal("a row with no place to name")
	}
	chk, ok := classPoolCheck("k8s_isolated_pool", []types.ConfinementClass{types.CC3}, []string{thisCluster})
	if !ok || chk.ID != "k8s_isolated_pool" || chk.Status != "warn" || !strings.Contains(chk.Detail, thisCluster) {
		t.Fatalf("local CC3 = %+v, %v; want a warn row naming %q", chk, ok, thisCluster)
	}
	where := []string{"runner a", "runner b", "runner c", "runner d", "runner e"}
	chk, _ = classPoolCheck("k8s_isolated_pool", []types.ConfinementClass{types.CC3, types.CC3, types.CC2}, where)
	if !strings.Contains(chk.Detail, "runner a, runner b, runner c and 2 more") || strings.Contains(chk.Detail, "runner d") {
		t.Errorf("five runners: %q, want three named and \"and 2 more\"", chk.Detail)
	}
	if !strings.Contains(chk.Detail, "CC2 and CC3 RuntimeClass") || strings.Count(chk.Detail, "CC3") != 1 {
		t.Errorf("repeated classes: %q, want each class once", chk.Detail)
	}
	chk, ok = classPoolCheck("k8s_class_any_node", []types.ConfinementClass{types.CC3}, []string{thisCluster})
	if !ok || chk.Status != "warn" || !strings.HasPrefix(chk.Detail, "CC3 sandboxes can run on any node with the runtime that the runner's own placement allows") {
		t.Fatalf("any-node row = %+v, %v", chk, ok)
	}
	s := &Server{}
	rows := s.classPoolRows(context.Background(), []types.ConfinementClass{types.CC3}, []types.ConfinementClass{types.CC3})
	if len(rows) != 2 || rows[0].ID != "k8s_isolated_pool" || rows[1].ID != "k8s_class_any_node" {
		t.Fatalf("rows = %+v, want k8s_isolated_pool then k8s_class_any_node", rows)
	}
	if rows := s.classPoolRows(context.Background(), nil, nil); len(rows) != 0 {
		t.Fatalf("no lists gave rows: %+v", rows)
	}
}
