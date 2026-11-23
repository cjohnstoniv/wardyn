// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Walls view's wire types. GET /api/v1/walls/topology answers a WallsTopology and GET /api/v1/runs/{id}/walls
// a RunWalls. A field marked "security only" is set only for a caller isSecurityOperator admits; a unit admin's
// payload never carries it. No type here holds a policy spec, a unit row, an overlay, a restriction or the
// boundary, and no key is named mounts (TestWallsPayloadsCarryNoMountsKey).

// WallsTopology is one snapshot of the organisation's units, runs and egress at AsOf. A continuation page
// carries Runs and Edges; Units, Hosts, Elsewhere and the security-only lists are on the first page only.
type WallsTopology struct {
	AsOf      time.Time `json:"as_of"`
	Scope     string    `json:"scope"`      // "org" for the security tier, "unit_subtree" for a unit admin
	WindowSec int       `json:"window_sec"` // the window the counters cover, in seconds
	// Partial is set when a bound cut the runs or the rows read. Volume outside a unit admin's units never sets it.
	Partial bool        `json:"partial,omitempty"`
	Units   []WallsUnit `json:"units,omitempty"`
	Runs    WallsRuns   `json:"runs"`
	Hosts   []WallsHost `json:"hosts,omitempty"` // at most 500, the busiest first
	Edges   WallsEdges  `json:"edges"`
	// Elsewhere is a unit admin's totals outside their units; never set for the security tier.
	Elsewhere *WallsElsewhere `json:"elsewhere,omitempty"`

	// Security only.
	Boundary     *WallsBoundary        `json:"boundary,omitempty"`
	Restrictions []WallsRestrictionRef `json:"restrictions,omitempty"`
	// Runners is every claimed organisation runner, each item what GET /api/v1/runners serves this caller for it
	// (runnerViews).
	Runners []types.RunnerView `json:"runners,omitempty"`
	Devices *WallsDevices      `json:"devices,omitempty"`
	Pools   []WallsPool        `json:"pools,omitempty"`
	// Sites is declared and not filled yet, as WallsPool.SiteID is, so both are absent.
	Sites []WallsSite `json:"sites,omitempty"`
	Gates []WallsGate `json:"gates,omitempty"`
}

// WallsRuns is one page of the snapshot's runs. NextCursor names the next page and is absent on the last one.
type WallsRuns struct {
	Items      []WallsRunItem `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// WallsEdges is the snapshot's unit-to-host edges (edges=summary), or one page of its run-to-host edges
// (edges=full, at most 8 a run) with NextCursor absent on the last page.
type WallsEdges struct {
	UnitHost   []WallsEdge `json:"unit_host,omitempty"`
	RunHost    []WallsEdge `json:"run_host,omitempty"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// WallsEdge is the traffic between one unit or run and one host:port. Exactly one of UnitID and RunID is set.
type WallsEdge struct {
	UnitID *uuid.UUID `json:"unit_id,omitempty"`
	RunID  *uuid.UUID `json:"run_id,omitempty"`
	Host   string     `json:"host"`
	Port   int        `json:"port"`
	// Counters is *WallsCounters for the security tier and *WallsUnitCounters for a unit admin.
	Counters any `json:"counters"`
}

// WallsHost is one destination host:port. The control plane's own host is never one.
type WallsHost struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Counters is *WallsCounters for the security tier and *WallsUnitCounters for a unit admin.
	Counters any `json:"counters"`
}

// WallsUnit is one governance unit. An ancestor of a unit admin's units carries ID, Name, ParentID, Depth and
// an EgressMode of "unknown", and nothing else.
type WallsUnit struct {
	ID       uuid.UUID  `json:"id"`
	Name     string     `json:"name"`
	ParentID *uuid.UUID `json:"parent_id,omitempty"`
	Kind     string     `json:"kind,omitempty"` // types.GovernanceUnit.Kind
	Mode     string     `json:"mode,omitempty"` // types.GovernanceUnit.Mode
	Depth    int        `json:"depth"`          // types.GovernanceUnit.Depth
	// Subjects is security only: the people and groups bound to the unit by a membership.
	Subjects []WallsSubject `json:"subjects,omitempty"`
	// EgressMode is the egress of the unit's resolved ceiling: "allowlist", "allow_all" or "sealed", or "unknown"
	// when the unit did not resolve.
	EgressMode string `json:"egress_mode"`
	// Error is security only: why the unit did not resolve.
	Error string         `json:"error,omitempty"`
	Runs  *WallsUnitRuns `json:"runs,omitempty"`
	// Egress is the unit's counters: *WallsCounters for the security tier and *WallsUnitCounters for a unit admin.
	Egress any `json:"egress,omitempty"`
}

// WallsSubject is one membership's subject (types.GovernanceMembership): a person or a group.
type WallsSubject struct {
	SubjectType types.CapabilitySubjectType `json:"subject_type"`
	Subject     string                      `json:"subject"`
}

// WallsUnitRuns counts a unit's runs in the snapshot: the live ones, the ones with a held row, and all of them
// by state.
type WallsUnitRuns struct {
	Live    int                    `json:"live"`
	Held    int                    `json:"held"`
	ByState map[types.RunState]int `json:"by_state"`
}

// WallsRunItem is one run. Every caller gets the fields from ID to Counters; the rest are security only. On
// GET /api/v1/runs/{id}/walls the owner's projection leaves out RestrictionIDs, Attention and OwnerKind and keeps
// Egress.
type WallsRunItem struct {
	ID               uuid.UUID              `json:"id"`
	UnitID           *uuid.UUID             `json:"unit_id,omitempty"`
	State            types.RunState         `json:"state"`
	ConfinementClass types.ConfinementClass `json:"confinement_class"`
	Placement        types.Placement        `json:"placement"`
	RunnerID         *uuid.UUID             `json:"runner_id,omitempty"`
	RunnerPoolID     *uuid.UUID             `json:"runner_pool_id,omitempty"`
	MachineKind      types.MachineKind      `json:"machine_kind,omitempty"`
	CreatedBy        string                 `json:"created_by"`
	CreatedAt        time.Time              `json:"created_at"`
	// Counters is the run's counters on a topology item: *WallsCounters for the security tier and
	// *WallsUnitCounters for a unit admin. RunWalls carries its run's counters beside the item instead.
	Counters any `json:"counters,omitempty"`

	// Security only.
	RestrictionIDs []uuid.UUID         `json:"restriction_ids,omitempty"`
	Attention      *types.RunAttention `json:"attention,omitempty"`
	// OwnerKind is the hosting runner's types.RunnerView.OwnerKind; absent for a run with no runner, or on a
	// runner the active-runner read does not list.
	OwnerKind types.RunnerOwnerKind `json:"owner_kind,omitempty"`
	// Egress is the run's door rule (wallsRunEgress): "sealed", "allowlist" or "open"; absent when the enforced
	// policy cannot be read.
	Egress string `json:"egress,omitempty"`
}

// WallsCounters are the security tier's and a run owner's counts of egress decision rows, each row counted by
// its weight (store.DecisionRow.Weight) under its wallsClassify class. A late row counts in Late only, and
// Guest is the part of Refused that a guest VM's flows make up.
type WallsCounters struct {
	Allowed      int64 `json:"allowed"`
	Refused      int64 `json:"refused"`
	Guest        int64 `json:"guest"`
	Held         int64 `json:"held"`
	Fault        int64 `json:"fault"`
	Fidelity     int64 `json:"fidelity"`
	ToolRule     int64 `json:"tool_rule"`
	Unclassified int64 `json:"unclassified"`
	Control      int64 `json:"control"`
	Config       int64 `json:"config"`
	Late         int64 `json:"late"`
}

// WallsUnitCounters are a unit admin's counts: one per row, never weighted, so they sum to the egress rows
// GET /api/v1/audit serves the same caller for the window, less the rows naming the control plane's host and
// the upstream proxy's start rows, which are left out. A late row counts in its class.
type WallsUnitCounters struct {
	Allowed      int64 `json:"allowed"`
	Refused      int64 `json:"refused"`
	Guest        int64 `json:"guest"`
	Held         int64 `json:"held"`
	Fault        int64 `json:"fault"`
	Fidelity     int64 `json:"fidelity"`
	Unclassified int64 `json:"unclassified"`
}

// WallsElsewhere is a unit admin's totals outside their units, over the runs active in the last 15 minutes:
// Runs and Live count runs, Held counts the runs with a held row, and NotAllowed and Allowed count rows by
// action. Allowed so keeps the rows naming the control plane's host and the upstream proxy's start rows that the
// caller's own counters leave out, and the two do not compare. Under 5 runs, every field reads "<5".
type WallsElsewhere struct {
	Runs       int64 `json:"runs"`
	Live       int64 `json:"live"`
	Held       int64 `json:"held"`
	NotAllowed int64 `json:"not_allowed"`
	Allowed    int64 `json:"allowed"`
}

// MarshalJSON writes every field as the string "<5" when Runs is under 5, else the numbers.
func (e WallsElsewhere) MarshalJSON() ([]byte, error) {
	if e.Runs < 5 {
		return []byte(`{"runs":"<5","live":"<5","held":"<5","not_allowed":"<5","allowed":"<5"}`), nil
	}
	type counts WallsElsewhere // the same fields without this method
	return json.Marshal(counts(e))
}

// WallsBoundary is the Wardyn boundary's mode; its spec is never served.
type WallsBoundary struct {
	Mode types.BoundaryMode `json:"mode"`
}

// WallsRestrictionRef names one restriction; its spec is never served.
type WallsRestrictionRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// WallsDevices is one total over the claimed person-owned runners: how many there are, how many this replica
// sees online, and the runs they hold. It names no device and no person.
type WallsDevices struct {
	Count  int `json:"count"`
	Online int `json:"online"`
	Runs   int `json:"runs"`
}

// WallsPool is one organisation runner pool. RunnerIDs are its members among WallsTopology.Runners, in id order.
type WallsPool struct {
	ID        uuid.UUID   `json:"id"`
	Name      string      `json:"name"`
	RunnerIDs []uuid.UUID `json:"runner_ids"`
	SiteID    *uuid.UUID  `json:"site_id,omitempty"`
}

// WallsSite is one site.
type WallsSite struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// WallsGate is one way out of the organisation (wallsGates): the direct gate, the site's upstream proxy, or a
// runner's own upstream proxy. Ids are the same on both routes: "direct", "upstream-site" and
// "upstream-runner-<runner_id>". Ordinal is a label only and carries no address.
type WallsGate struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"` // "direct" or "upstream"
	Ordinal  int        `json:"ordinal"`
	Source   string     `json:"source"`              // "site" or "runner"
	RunnerID *uuid.UUID `json:"runner_id,omitempty"` // on a runner's gate
	// CATrusted is whether the deployment trusts a CA for the site's upstream; absent on a runner's gate, whose
	// CA cannot be known here.
	CATrusted *bool  `json:"ca_trusted,omitempty"`
	Addr      string `json:"addr,omitempty"` // host:port, for a super admin only
}

// RunWalls is GET /api/v1/runs/{id}/walls: one run's walls as the security tier or the run's owner reads them.
// cursor and limit page Struck; Exits, Gates and Inner are on the first page only.
type RunWalls struct {
	Run        WallsRunItem     `json:"run"`
	Projection string           `json:"projection"` // "security" or "owner"
	Layers     []RunWallsLayer  `json:"layers"`
	Paths      []RunWallsPath   `json:"paths"`
	Struck     []RunWallsStruck `json:"struck"`
	Grants     []RunWallsGrant  `json:"grants"`
	Holds      []RunWallsHold   `json:"holds"`
	Doors      []RunWallsDoor   `json:"doors"`
	// Counters is the run's weighted counters over the last 15 minutes.
	Counters   WallsCounters   `json:"counters"`
	Partial    bool            `json:"partial,omitempty"` // a bound cut the rows Counters reads
	NextCursor string          `json:"next_cursor,omitempty"`
	Exits      []RunWallsExit  `json:"exits,omitempty"`
	Gates      []WallsGate     `json:"gates,omitempty"`
	Inner      []RunWallsInner `json:"inner,omitempty"`
}

// RunWallsLayer is one band of the run wall: a layer that set knobs of the served policy, from the enforced
// envelope's provenance (policyref.Provenance). Sorted by layer, then name.
type RunWallsLayer struct {
	Layer  string   `json:"layer"`            // policyref.Provenance.Layer; owner: a restriction layer reads "policy"
	Name   string   `json:"name,omitempty"`   // absent for the owner on a restriction layer, and for a hidden personal preset
	Knobs  []string `json:"knobs"`            // the knobs this layer set, sorted
	Locked []string `json:"locked,omitempty"` // the knobs of Knobs it locked (policyref.Provenance.Locked)
}

// RunWallsPath is one way out the served policy allows.
type RunWallsPath struct {
	Entry string `json:"entry"` // an allowed_domains entry as written, or "*" for allow-all
	Kind  string `json:"kind"`  // "host", "wildcard" (a *. entry) or "allow_all"
}

// RunWallsStruck is one entry the run's policy removed or denied, with the explain cause and, when the re-fold
// verified it, the layer that removed it.
type RunWallsStruck struct {
	Entry     string             `json:"entry"`
	Field     string             `json:"field"` // "allowed_domains" or "denied_domains"
	Cause     string             `json:"cause"`
	RemovedBy *RunWallsRemovedBy `json:"removed_by,omitempty"`
}

// RunWallsRemovedBy is the layer that removed a struck entry. It is set only when the re-fold verified it.
type RunWallsRemovedBy struct {
	Layer    string `json:"layer"`
	Name     string `json:"name,omitempty"` // absent for the owner on a restriction
	Verified bool   `json:"verified"`
}

// RunWallsGrant is one credential grant of the run, and whether its value is inside the sandbox (grantResidency).
type RunWallsGrant struct {
	ID            uuid.UUID       `json:"id"` // types.CredentialGrant.ID
	Kind          types.GrantKind `json:"kind"`
	InsideSandbox *bool           `json:"inside_sandbox,omitempty"` // absent when it cannot be said
}

// RunWallsHold is one pending approval of the run, as GET /approvals serves it to this caller.
type RunWallsHold struct {
	ID          uuid.UUID `json:"id"`           // types.ApprovalRequest.ID
	Kind        string    `json:"kind"`         // types.ApprovalRequest.Kind
	RequestedAt time.Time `json:"requested_at"` // types.ApprovalRequest.RequestedAt
}

// RunWallsDoor is one way in, which opens no network path out. Its one kind is "ui_app", an app the served
// policy's ui_apps declares: the terminal, SSH and desktop doors have no source in the run's policy view, so none
// is served.
type RunWallsDoor struct {
	Kind    string `json:"kind"`              // "ui_app"
	Name    string `json:"name,omitempty"`    // types.UIApp.Name
	Port    int    `json:"port,omitempty"`    // types.UIApp.Port
	Inbound bool   `json:"inbound,omitempty"` // types.UIApp.Inbound
}

// RunWallsExit is where one allowed path leaves (wallsRunExits). Route is "upstream", "direct" or "internal",
// and is absent when the route was not reported. Gate names a WallsGate.ID and Inner a RunWallsInner.ID.
type RunWallsExit struct {
	Path       string `json:"path"`
	Route      string `json:"route,omitempty"`
	Gate       string `json:"gate,omitempty"`
	Inner      string `json:"inner,omitempty"`
	Hairpin    bool   `json:"hairpin"`
	Discovered string `json:"discovered,omitempty"` // "approval", "wildcard" or "allow_all"
}

// RunWallsInner is one node inside the organisation an exit can land on.
type RunWallsInner struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // "internal_host", "redirect", "control_plane" or "secret_store"
	Label  string `json:"label"`
	Suffix string `json:"suffix,omitempty"` // for a super admin only
}
