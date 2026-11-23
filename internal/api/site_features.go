// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync/atomic"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The site config's features block (types.SiteFeatureSwitches). This file is the block's only reader outside tests
// (TestSiteFeatureSwitchesReadOnlyThroughAccessor). A feature registers a hook for its key from an init in its own
// file, as governance change kinds do (portal_issuer_governance.go): until it does, the key cannot be turned on or
// set, and reads off whatever is stored.

// siteFeatureKey is a key of the features block, by its JSON name.
type siteFeatureKey string

const (
	featureOrgSites              siteFeatureKey = "org_sites"
	featureWindowsVM             siteFeatureKey = "windows_vm"
	featureRunMove               siteFeatureKey = "run_move"
	featureMachineSnapshots      siteFeatureKey = "machine_snapshots"
	featureLocalWindow           siteFeatureKey = "local_window"
	featurePreviewURLs           siteFeatureKey = "preview_urls"
	featureComponentRequestRules siteFeatureKey = "component_request_rules"
	featureOrgRunnerDrives       siteFeatureKey = "org_runner_drives"
)

// siteFeatureRow is one key: its Go field in types.SiteFeatureSwitches and the feature as a refusal names it.
type siteFeatureRow struct {
	key   siteFeatureKey
	field string
	what  string
}

// siteFeatureRows is every key in the block's order. TestSiteFeatureRowsMatchTheBlock pins it to the struct.
var siteFeatureRows = []siteFeatureRow{
	{featureOrgSites, "OrgSites", "sites"},
	{featureWindowsVM, "WindowsVM", "Windows VM sandboxes"},
	{featureRunMove, "RunMove", "moving a run"},
	{featureMachineSnapshots, "MachineSnapshots", "snapshots of Linux VM sandboxes"},
	{featureLocalWindow, "LocalWindow", "a window on your own computer onto a Linux VM run"},
	{featurePreviewURLs, "PreviewURLs", "preview URLs"},
	{featureComponentRequestRules, "ComponentRequestRules", "request rules on custom components"},
	{featureOrgRunnerDrives, "OrgRunnerDrives", "keeping a drive on an organisation runner"},
}

// siteFeatureHook is what a feature registers for its key.
type siteFeatureHook struct {
	// available reports whether this release can turn the feature on. Nil reads false.
	available func() bool
	// validate checks a PUT /site-config that changes the key, with the block as it would be stored (next) and as it
	// is stored (prev); either may be nil. It may read server config through s. Nil checks nothing beyond the
	// decode. A *siteFeatureRefusal answers with its own reason; any other error answers site_config_invalid.
	validate func(ctx context.Context, s *Server, next, prev *types.SiteFeatureSwitches) error
	// afterWrite runs once a saved PUT /site-config changed the key, with the documents before and after. The
	// document is stored already, so it cannot refuse; it records its own failures.
	afterWrite func(ctx context.Context, s *Server, before, after types.SiteConfig)
}

// siteFeatureHooks holds the registered keys. Only registerSiteFeature writes it, and only from an init
// (TestSiteFeatureSwitchesReadOnlyThroughAccessor); tests swap it with withSiteFeatureHooks.
var siteFeatureHooks = map[siteFeatureKey]siteFeatureHook{}

// siteFeatureHooksRead is set by the first read of siteFeatureHooks. A registration after it panics, so the set of
// keys a running server can turn on never changes under a request.
var siteFeatureHooksRead atomic.Bool

// registerSiteFeature registers k's hook. Call it from an init in the feature's own file. It panics on a key the
// features block does not have, on a second registration of k, and once any request has read the hooks.
func registerSiteFeature(k siteFeatureKey, h siteFeatureHook) {
	if siteFeatureHooksRead.Load() {
		panic(fmt.Sprintf("site features: %s registered after the hooks were read", k))
	}
	known := false
	for _, row := range siteFeatureRows {
		known = known || row.key == k
	}
	if !known {
		panic(fmt.Sprintf("site features: %s is not a key of the features block", k))
	}
	if _, dup := siteFeatureHooks[k]; dup {
		panic(fmt.Sprintf("site features: %s registered twice", k))
	}
	siteFeatureHooks[k] = h
}

// siteFeatureHookFor is the one read of siteFeatureHooks.
func siteFeatureHookFor(k siteFeatureKey) (siteFeatureHook, bool) {
	siteFeatureHooksRead.Store(true)
	h, ok := siteFeatureHooks[k]
	return h, ok
}

// siteFeatureRefusal is a validate error that answers with its own reason, a const from a reasons file.
type siteFeatureRefusal struct{ reason, msg string }

func (e *siteFeatureRefusal) Error() string { return e.msg }

// siteFeatureAvailable reports whether this release registered k and can turn it on.
func siteFeatureAvailable(k siteFeatureKey) bool {
	h, ok := siteFeatureHookFor(k)
	return ok && h.available != nil && h.available()
}

// siteFeatureOn is the one reader of a feature's switch: on only when this release can turn k on and the stored
// enabled is true. A stored true this release cannot act on reads off. Callers read the site config themselves and
// refuse when it cannot be read.
func siteFeatureOn(sc types.SiteConfig, k siteFeatureKey) bool {
	if !siteFeatureAvailable(k) {
		return false
	}
	e := featureEnabled(siteFeatureRowFor(k).in(sc.FeatureSwitches))
	return e != nil && *e
}

// siteFeatureSettings is the stored block with every switch taken out. A feature reads its own settings here,
// whether it is on or off, and asks siteFeatureOn whether it may act. Slices are the stored ones: read only.
func siteFeatureSettings(sc types.SiteConfig) types.SiteFeatureSwitches {
	var out types.SiteFeatureSwitches
	if sc.FeatureSwitches == nil {
		return out
	}
	out = *sc.FeatureSwitches
	for _, row := range siteFeatureRows {
		v := row.in(&out)
		if v.IsNil() {
			continue
		}
		c := reflect.New(v.Type().Elem())
		c.Elem().Set(v.Elem())
		c.Elem().FieldByName("Enabled").SetZero()
		v.Set(c)
	}
	return out
}

// carryForwardSiteFeatures keeps what the body leaves unsaid, on the macos_vm terms (carryForwardSiteMacOSVM). A
// body that does not name the block keeps it. Inside a named block (null reads as {}), a key left out keeps its
// stored value, and a key sent without enabled keeps its stored switch: a switch changes only when sent. The rest of
// a named key is as sent. A key with nothing set, and a block with no key, are stored as absent.
func carryForwardSiteFeatures(cfg *types.SiteConfig, existing types.SiteConfig, present map[string]bool) {
	if !present["features"] {
		cfg.FeatureSwitches = existing.FeatureSwitches
		return
	}
	var next types.SiteFeatureSwitches
	if cfg.FeatureSwitches != nil {
		next = *cfg.FeatureSwitches
	}
	for _, row := range siteFeatureRows {
		n, old := row.in(&next), row.in(existing.FeatureSwitches)
		switch {
		case n.IsNil():
			if old.IsValid() {
				n.Set(old)
			}
		case featureEnabled(n) == nil && featureEnabled(old) != nil:
			n.Elem().FieldByName("Enabled").Set(old.Elem().FieldByName("Enabled"))
		}
		if !n.IsNil() && n.Elem().IsZero() {
			n.SetZero()
		}
	}
	cfg.FeatureSwitches = nil
	if next != (types.SiteFeatureSwitches{}) {
		cfg.FeatureSwitches = &next
	}
}

// admitSiteFeatures refuses, on w, a PUT /site-config that turns on or sets a key this release cannot turn on, or
// that a key's own validate refuses; false means it answered. It runs under the site-config lock after the
// carry-forward, and checks only the keys the write changes, so a re-applied document is never refused for what is
// already stored.
func (s *Server) admitSiteFeatures(w http.ResponseWriter, r *http.Request, cfg *types.SiteConfig, existing types.SiteConfig) bool {
	for _, row := range siteFeatureRows {
		n := row.in(cfg.FeatureSwitches)
		if !featureChanged(n, row.in(existing.FeatureSwitches)) {
			continue
		}
		if !siteFeatureAvailable(row.key) {
			if featureOnlyOff(n) {
				continue
			}
			writeErrorReason(w, http.StatusBadRequest, reasonSiteFeatureUnavailable, fmt.Sprintf(
				`features.%s: %s is not available in this release, so it can't be turned on or set. Leave it out, or send {"enabled": false}.`,
				row.key, row.what))
			return false
		}
		h, _ := siteFeatureHookFor(row.key)
		if h.validate == nil {
			continue
		}
		if err := h.validate(r.Context(), s, cfg.FeatureSwitches, existing.FeatureSwitches); err != nil {
			reason := reasonSiteConfigInvalid
			var fr *siteFeatureRefusal
			if errors.As(err, &fr) {
				reason = fr.reason
			}
			writeErrorReason(w, http.StatusBadRequest, reason, "invalid site config: "+err.Error())
			return false
		}
	}
	return true
}

// afterSiteConfigSave runs what follows a saved PUT /site-config: sign-ins its model providers no longer answer
// end, and each registered feature whose key the write changed gets its afterWrite.
func (s *Server) afterSiteConfigSave(ctx context.Context, existing, saved types.SiteConfig) {
	s.endUnanswerableSignIns(ctx, existing.ModelProviders, saved.ModelProviders)
	for _, row := range siteFeatureRows {
		h, ok := siteFeatureHookFor(row.key)
		if !ok || h.afterWrite == nil || !featureChanged(row.in(saved.FeatureSwitches), row.in(existing.FeatureSwitches)) {
			continue
		}
		h.afterWrite(ctx, s, existing, saved)
	}
}

// auditSiteBlocks records the image rules and the features block on site_config.write, in one call so
// handlePutSiteConfig stays inside funlen.
func auditSiteBlocks(datum map[string]any, existing, saved types.SiteConfig, present map[string]bool) {
	auditSiteImages(datum, existing.Images, saved.Images, present["images"])
	auditSiteFeatures(datum, existing, saved, present["features"])
}

// auditSiteFeatures records, once a features block is stored or the body named one (so a deployment with none
// writes the row it always wrote), features_enabled: the keys whose stored enabled is true, and features_changed:
// the keys this write changed, each in the block's order. A key this release cannot turn on is listed as stored.
func auditSiteFeatures(datum map[string]any, before, after types.SiteConfig, named bool) {
	if before.FeatureSwitches == nil && after.FeatureSwitches == nil && !named {
		return
	}
	enabled, changed := []string{}, []string{}
	for _, row := range siteFeatureRows {
		a := row.in(after.FeatureSwitches)
		if e := featureEnabled(a); e != nil && *e {
			enabled = append(enabled, string(row.key))
		}
		if featureChanged(a, row.in(before.FeatureSwitches)) {
			changed = append(changed, string(row.key))
		}
	}
	datum["features_enabled"], datum["features_changed"] = enabled, changed
}

// siteFeatureFlags is f with setup's flags for the features block, each read through siteFeatureOn. An unreadable
// site config (ok false) leaves them off, which the console reads as off.
func siteFeatureFlags(f SetupFeatures, sc types.SiteConfig, ok bool) SetupFeatures {
	if !ok {
		return f
	}
	f.OrgSites = siteFeatureOn(sc, featureOrgSites)
	f.WindowsVMs = siteFeatureOn(sc, featureWindowsVM)
	f.RunMove = siteFeatureOn(sc, featureRunMove)
	f.MachineSnapshots = siteFeatureOn(sc, featureMachineSnapshots)
	f.LocalWindow = siteFeatureOn(sc, featureLocalWindow)
	f.ComponentRequestRules = siteFeatureOn(sc, featureComponentRequestRules)
	f.OrgRunnerDrives = siteFeatureOn(sc, featureOrgRunnerDrives)
	if f.PreviewURLs = siteFeatureOn(sc, featurePreviewURLs); f.PreviewURLs {
		f.PreviewMaxVisibility = "private" // an absent ceiling reads private
		if p := siteFeatureSettings(sc).PreviewURLs; p != nil && p.MaxVisibility == "org" {
			f.PreviewMaxVisibility = "org" // any other stored value, a hand-edited "public" or "bogus" included, reads private; PV-5 adds "public" here
		}
	}
	return f
}

// siteFeatureRowFor is k's row; an unknown key gets the zero row, whose field reads absent, so it reads off.
func siteFeatureRowFor(k siteFeatureKey) siteFeatureRow {
	for _, row := range siteFeatureRows {
		if row.key == k {
			return row
		}
	}
	return siteFeatureRow{}
}

// in is row's field in f: a pointer to the key's struct, possibly nil, or the zero Value when f is nil.
func (row siteFeatureRow) in(f *types.SiteFeatureSwitches) reflect.Value {
	if f == nil {
		return reflect.Value{}
	}
	return reflect.ValueOf(f).Elem().FieldByName(row.field)
}

// featureEnabled is the switch of the key value v, nil when v is absent.
func featureEnabled(v reflect.Value) *bool {
	if !v.IsValid() || v.IsNil() {
		return nil
	}
	e, _ := v.Elem().FieldByName("Enabled").Interface().(*bool)
	return e
}

// featureChanged reports whether two values of one key differ; absent and a nil key are the same.
func featureChanged(a, b reflect.Value) bool {
	iface := func(v reflect.Value) any {
		if !v.IsValid() || v.IsNil() {
			return nil
		}
		return v.Interface()
	}
	return !reflect.DeepEqual(iface(a), iface(b))
}

// featureOnlyOff reports whether v sets nothing but a switch that is off: absent, {} or {"enabled": false}. It is
// all a key this release cannot turn on accepts.
func featureOnlyOff(v reflect.Value) bool {
	if !v.IsValid() || v.IsNil() {
		return true
	}
	if e := featureEnabled(v); e != nil && *e {
		return false
	}
	c := reflect.New(v.Type().Elem()).Elem()
	c.Set(v.Elem())
	c.FieldByName("Enabled").SetZero()
	return c.IsZero()
}
