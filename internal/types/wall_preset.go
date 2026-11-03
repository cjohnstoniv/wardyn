// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
)

// Wall presets (migration 0248_wall_presets): a named, narrow-only walls document a person picks at
// launch, kept for one person, a group or the organisation, or built in. Who may read and write one is
// the run templates' authority (TemplateAuthorize on Owner()); the built-ins are constants, read by
// anyone and written by nobody.

// WallPresetScope is who a wall preset is for: the three template scopes, or built in.
type WallPresetScope string

const (
	WallPresetScopeBuiltin WallPresetScope = "builtin"
	WallPresetScopePerson  WallPresetScope = "person"
	WallPresetScopeOrg     WallPresetScope = "org"
	WallPresetScopeGroup   WallPresetScope = "group"
)

// WallPreset is one wall preset as it is stored and answered.
type WallPreset struct {
	// ID is a uuid, or the built-in's name ("open", "read-only", "sealed").
	ID          string            `json:"id"`
	Scope       WallPresetScope   `json:"scope"`
	GroupID     string            `json:"group_id,omitempty"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Spec        PersonalWallsSpec `json:"spec"`
	Version     int               `json:"version"`
	UpdatedAt   *time.Time        `json:"updated_at,omitempty"`
	// UpdatedBy is sent for org and group scope only, never for person scope.
	UpdatedBy string `json:"updated_by,omitempty"`
	// OwnerID is the owning person's canonical subject, person scope only. The server keys authority
	// on it and never sends it.
	OwnerID string `json:"-"`
}

// Owner is the preset's catalogue key in the templates' shape, for TemplateAuthorize. A built-in has
// no valid owner, so TemplateAuthorize refuses it; callers answer a built-in before asking.
func (p WallPreset) Owner() TemplateOwner {
	return TemplateOwner{Scope: TemplateScope(p.Scope), Person: p.OwnerID, Group: p.GroupID}
}

// WallPresetOnRun is the frozen copy the server keeps for a run. AgentRun.WallPreset holds it with
// json:"-"; the run read sends client.RunWallPreset{id, name, scope, group_id, version} instead, never
// the spec.
type WallPresetOnRun struct {
	ID, Name string
	Scope    WallPresetScope
	GroupID  string
	Version  int
	Spec     PersonalWallsSpec
}

// The built-in presets' ids. Each is also its name, lower-cased, and every audience's name is checked
// against them, so a stored preset cannot shadow one.
const (
	WallPresetOpen     = "open"
	WallPresetReadOnly = "read-only"
	WallPresetSealed   = "sealed"
)

// builtinWallPresetVersion is the built-ins' version: bumped when a release changes what one narrows.
const builtinWallPresetVersion = 1

// BuiltinWallPresets returns Open, Read-only and Sealed, widest first, each a fresh copy a caller may
// change. They nest: every knob Read-only names, Sealed names at least as narrowly.
//
// TODO(rc12-push-deny-all): Read-only and Sealed also set push_rules.deny_all: true once that lane adds
// the knob; until then neither refuses a push.
func BuiltinWallPresets() []WallPreset {
	readOnly := readOnlyWalls()
	sealed := readOnlyWalls()
	none, never := []string{}, FirstUseAlwaysDeny
	sealed.AllowedDomains, sealed.FirstUseApproval = &none, &never
	sealed.Desktop = &DesktopControls{PasteIn: TransferBlocked, CopyOut: TransferBlocked, Upload: TransferBlocked, Download: TransferBlocked}
	return []WallPreset{
		{ID: WallPresetOpen, Scope: WallPresetScopeBuiltin, Name: "Open", Version: builtinWallPresetVersion,
			Description: "Your policy as it stands."},
		{ID: WallPresetReadOnly, Scope: WallPresetScopeBuiltin, Name: "Read-only", Version: builtinWallPresetVersion, Spec: readOnly,
			Description: "Keeps only the Azure DevOps and GitHub read capabilities, so the run can read but not write through them."},
		{ID: WallPresetSealed, Scope: WallPresetScopeBuiltin, Name: "Sealed", Version: builtinWallPresetVersion, Spec: sealed,
			Description: "Read-only, plus only the sites the run's own setup needs; an unlisted site is refused, not asked about, and no desktop transfers."},
	}
}

// readOnlyWalls keeps every grantable read capability of both forges and nothing else, so it removes
// writes only.
func readOnlyWalls() PersonalWallsSpec {
	gh := []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapIssuesRead, ghscope.CapActionsRead,
		ghscope.CapPackagesRead, ghscope.CapOrgRead, ghscope.CapIdentity}
	ado := []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapWorkRead, adoscope.CapWikiRead,
		adoscope.CapBuildRead, adoscope.CapReleaseRead, adoscope.CapServiceEndpointRead, adoscope.CapLibraryRead,
		adoscope.CapPackagingRead, adoscope.CapTestRead, adoscope.CapProjectRead, adoscope.CapIdentityRead}
	return PersonalWallsSpec{GitHubCapabilities: &gh, AzureDevOpsCapabilities: &ado}
}

// BuiltinWallPreset returns the built-in whose id is id, compared as given (ids are lower case).
func BuiltinWallPreset(id string) (WallPreset, bool) {
	for _, p := range BuiltinWallPresets() {
		if p.ID == id {
			return p, true
		}
	}
	return WallPreset{}, false
}

// IsBuiltinWallPresetName reports whether name is a built-in's name in any case, which no stored preset
// may take.
func IsBuiltinWallPresetName(name string) bool {
	_, ok := BuiltinWallPreset(strings.ToLower(strings.TrimSpace(name)))
	return ok
}
