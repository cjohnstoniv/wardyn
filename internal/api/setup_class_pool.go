// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// classPoolRows are setup status's two pool rows, from wardynd's own Kubernetes substrate (the two local
// lists, from setupRunnerInfo) and each connected runner's caps:
//   - k8s_isolated_pool: a pinned class's pool is labelled and not tainted, so general pods can land on it;
//   - k8s_class_any_node: a pinned class's RuntimeClass sets no scheduling, so its pods run on any node
//     with the runtime.
//
// An offline runner is left out, as runnersNetPolCheck leaves it out; an older runner never says.
func (s *Server) classPoolRows(ctx context.Context, untainted, anyNode []types.ConfinementClass) []SetupCheck {
	type found struct {
		classes []types.ConfinementClass
		where   []string
	}
	var pools, open found
	add := func(f *found, cs []types.ConfinementClass, place string) {
		if len(cs) > 0 {
			f.classes = append(f.classes, cs...)
			f.where = append(f.where, place)
		}
	}
	add(&pools, untainted, "this server's cluster")
	add(&open, anyNode, "this server's cluster")
	s.runnerStreams.subsMu.Lock()
	ids := slices.SortedFunc(maps.Keys(s.runnerStreams.subs), func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	eps := make([]runnerEndpoint, 0, len(ids))
	for _, id := range ids {
		eps = append(eps, s.runnerStreams.subs[id])
	}
	s.runnerStreams.subsMu.Unlock()
	for i, ep := range eps {
		if _, err := ep.sub.Classes(ctx); err != nil { // offline, or no caps yet
			continue
		}
		if c, ok := ep.sub.Caps(); ok {
			add(&pools, c.Support.UntaintedPools, "runner "+ids[i].String())
			add(&open, c.Support.AnyNodeClasses, "runner "+ids[i].String())
		}
	}
	var rows []SetupCheck
	if chk, ok := classPoolCheck("k8s_isolated_pool", pools.classes, pools.where); ok {
		rows = append(rows, chk)
	}
	if chk, ok := classPoolCheck("k8s_class_any_node", open.classes, open.where); ok {
		rows = append(rows, chk)
	}
	return rows
}

// classPoolCheck words one row, k8s_isolated_pool or k8s_class_any_node: a warning, never Blocking, naming
// the classes and up to three places.
func classPoolCheck(id string, classes []types.ConfinementClass, where []string) (SetupCheck, bool) {
	if len(where) == 0 {
		return SetupCheck{}, false
	}
	slices.Sort(classes)
	names := make([]string, 0, len(classes))
	for _, c := range slices.Compact(classes) {
		names = append(names, string(c))
	}
	list := strings.Join(names, " and ")
	places := strings.Join(where[:min(3, len(where))], ", ")
	if len(where) > 3 {
		places += fmt.Sprintf(" and %d more", len(where)-3)
	}
	if id == "k8s_class_any_node" {
		return SetupCheck{ID: id, Label: "Barrier class nodes", Status: "warn",
			Detail: list + " sandboxes can run on any node with the runtime that the runner's own placement allows: the RuntimeClass does not pin its pods (it sets no scheduling). Where: " + places + ".",
			Fix:    "To keep them on their own nodes, taint and label those nodes, for example isolated=true:NoSchedule and pool=isolated, and put the matching toleration and node selector in the RuntimeClass's scheduling. The runner checks when it starts."}, true
	}
	return SetupCheck{ID: id, Label: "Isolated pool", Status: "warn",
		Detail: "Nodes that the " + list + " RuntimeClass selects carry no taint it tolerates, so the proxy, the canary and CC1 runs can land on them: " + places + ".",
		Fix:    "Taint those nodes, for example isolated=true:NoSchedule, and add the matching toleration to the RuntimeClass's scheduling. The runner checks when it starts, with the node read on."}, true
}
