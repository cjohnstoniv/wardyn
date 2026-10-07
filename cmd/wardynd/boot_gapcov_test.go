// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// gapCovPostureFlags is a flag set validateBootPosture accepts, for a test to perturb in one field.
func gapCovPostureFlags() *bootFlags {
	off, tail, retention, rate := false, 65536, 30, 20
	seal, runner, rec, listen := "off", "none", "pg", ":8080"
	empty, controlURL, internal := "", "https://wardynd:8443", ":8443"
	return &bootFlags{
		ha: &off, allowMultiInstance: &off, runnerSel: &runner, recordingSel: &rec,
		auditSeal: &seal, runOutputTailBytes: &tail, runOutputRetention: &retention, preflightRatePerMin: &rate,
		basePath: &empty, oidcIssuer: &empty, oidcInternalIss: &empty, uiAdvertise: &empty, oidcRedirectURL: &empty,
		controlURL: &controlURL, listen: &listen, uiListen: &empty, sshListen: &empty, uiOriginTemplate: &empty,
		uiStripCookies: &empty, allowPlaintextListen: &off, orgURL: &empty, orgEnrolToken: &empty, memberMode: &off,
		internalListen: &internal, metricsListen: &empty,
	}
}

func TestGapCovValidateBootPostureRefusals(t *testing.T) {
	bad, token, none, negative, on := "bogus", "scim-bearer", "", -1, true
	for _, tc := range []struct {
		name    string
		mutate  func(f *bootFlags)
		wantErr string
	}{
		{"the control", func(*bootFlags) {}, ""},
		{"an unknown audit seal mode", func(f *bootFlags) { f.auditSeal = &bad }, "refusing to start"},
		{"the removed multi-instance flag", func(f *bootFlags) { f.allowMultiInstance = &on }, "-allow-multi-instance was removed"},
		{"a negative output retention", func(f *bootFlags) { f.runOutputRetention = &negative }, "WARDYN_RUN_OUTPUT_RETENTION_DAYS is -1"},
		{"SCIM without OIDC", func(f *bootFlags) { f.scimToken, f.scimTokenNext = &token, &none }, "WARDYN_SCIM_TOKEN is set but OIDC is not configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gapCovPostureFlags()
			tc.mutate(f)
			err := validateBootPosture(f, tlsPosture{})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateBootPosture = %v, want it accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateBootPosture = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// Each one-shot mode that is a mode of its own refuses to run beside another, before it opens
// anything; -audit-split-legacy alone, with no database named, stops at that.
func TestGapCovMaintenanceModeRefusesModesThatAreModesOfTheirOwn(t *testing.T) {
	flags := func(set func(f *bootFlags)) *bootFlags {
		f := rekeyFlags("", "", "")
		f.rewrap, f.migrateOnly, f.rewrapPrincipalKeys = new(bool), new(bool), new(bool)
		f.migrateSecrets, f.reconcile, f.auditSplitLegacy = new(bool), new(bool), new(bool)
		f.rotateAgeKey, f.migrateDSN = new(string), new(string)
		set(f)
		return f
	}
	for _, tc := range []struct {
		name    string
		set     func(f *bootFlags)
		wantErr string
	}{
		{"-migrate-only beside -audit-split-legacy", func(f *bootFlags) { *f.migrateOnly, *f.auditSplitLegacy = true, true }, "-migrate-only is a mode of its own"},
		{"-rewrap-principal-keys beside -reconcile", func(f *bootFlags) { *f.rewrapPrincipalKeys, *f.reconcile = true, true }, "-rewrap-principal-keys is a mode of its own"},
		{"-audit-split-legacy beside -rewrap", func(f *bootFlags) { *f.auditSplitLegacy, *f.rewrap = true, true }, "-audit-split-legacy is a mode of its own"},
		{"-audit-split-legacy alone needs a database", func(f *bootFlags) { *f.auditSplitLegacy = true }, "-audit-split-legacy needs a database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran, err := maintenanceMode(flags(tc.set))
			if !ran || err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("maintenanceMode = (%v, %v), want a run that refuses with %q", ran, err, tc.wantErr)
			}
		})
	}
}

// With high availability on, no single-instance lock is taken: the claim never dials the database
// (lazyPool) and its release does nothing.
func TestGapCovClaimSingleInstanceUnderHAHoldsNothing(t *testing.T) {
	release, err := claimSingleInstance(context.Background(), lazyPool(t, 3), true)
	if err != nil || release == nil {
		t.Fatalf("claimSingleInstance = %v, %v; want a release func and no error", release != nil, err)
	}
	release()
}

func TestGapCovRefusedToIsAMigrateRefusal(t *testing.T) {
	err := refusedTo("split the legacy audit partition", "%s: %s", "not the owner", "run it as the table owner")
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != exitMigrateRefused {
		t.Fatalf("refusedTo = %v, want an exitCodeError with code %d", err, exitMigrateRefused)
	}
	if want := "refusing to split the legacy audit partition: not the owner: run it as the table owner"; err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
}
