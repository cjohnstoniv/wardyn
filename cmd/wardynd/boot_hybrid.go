// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// secretOrgDeviceCredential holds this laptop's federation.Credential. Reserved
// in internal/api and internal/broker (TestPlatformSecretsAreReservedEverywhere).
const secretOrgDeviceCredential = "wardyn-org-device-credential"

// bootHybrid is hybrid enrolment's boot step (issue #103): with no org URL it
// does nothing and returns nil. Otherwise it loads the stored device
// credential, or enrols with the enrolment token and stores one, then starts
// the audit forwarder on rootCtx and returns its status accessor.
//
// Every failure refuses the boot rather than running unenrolled: no credential
// and no token names the token; an enrolment that fails — the org unreachable
// included — is left to the service manager's restart and the desktop converge
// job, the way OIDC discovery already is.
//
// A token whose hash differs from the one the stored credential was bought
// with is a fresh token, so the laptop re-enrols: that is how a revoked device
// comes back. The spent token MDM leaves in secret.env matches, and changes
// nothing. A new device identity starts an empty chain at the organisation, so
// the cursor goes back to 0, the revoked mark clears, and the whole local table
// is pushed again. Re-enrolment is the ONLY thing that clears that mark: a
// laptop the organisation revoked comes back up still refusing new runs, the
// organisation reachable or not.
func bootHybrid(ctx, rootCtx context.Context, orgURL, enrolToken string, secrets secretKeyStore, st federation.Store, rec audit.Recorder) (func() federation.Status, error) {
	if orgURL == "" {
		return nil, nil
	}
	client := federation.NewClient(orgURL)
	var enrolled *types.DeviceEnrolResponse
	raw, err := loadOrCreateSecret(ctx, secrets, secretOrgDeviceCredential,
		func(b []byte) bool {
			c, ok := parseOrgCredential(b)
			return ok && (enrolToken == "" || c.EnrolmentTokenSHA256 == federation.TokenSHA256(enrolToken))
		},
		func() ([]byte, error) {
			if enrolToken == "" {
				return nil, errors.New("refusing to start: WARDYN_ORG_URL is set but this device holds no org credential " +
					"and WARDYN_ORG_ENROLMENT_TOKEN is unset — deliver an enrolment token an org admin minted")
			}
			resp, err := client.Enrol(ctx, enrolToken)
			if err != nil {
				return nil, fmt.Errorf("refusing to start: enrolment at WARDYN_ORG_URL failed: %w", err)
			}
			enrolled = &resp
			return json.Marshal(federation.Credential{DeviceID: resp.DeviceID, Token: resp.Token,
				EnrolmentTokenSHA256: federation.TokenSHA256(enrolToken)})
		})
	if err != nil {
		return nil, err
	}
	// Put-then-Reset: loadOrCreateSecret only returns nil once the new
	// credential is durably stored, so the revoked mark from a prior enrolment
	// clears no earlier than that. Clearing it first (inside generate, before
	// Put) would leave the gate open with no new credential behind it if Put
	// then failed. A Reset failure here refuses the boot — the laptop keeps
	// refusing on the old mark rather than starting in an unknown state.
	if enrolled != nil {
		if err := st.ResetFederation(ctx); err != nil {
			return nil, fmt.Errorf("refusing to start: reset federation state after enrolment: %w", err)
		}
	}
	cred, _ := parseOrgCredential(raw)
	if enrolled != nil {
		slog.Info("wardynd: enrolled with the organisation", "device_id", cred.DeviceID, "name", enrolled.Name)
		data, _ := json.Marshal(map[string]any{"name": enrolled.Name})
		if err := rec.Record(ctx, types.AuditEvent{
			ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: federation.AuditActor,
			Action: "device.local.enrol", Target: cred.DeviceID.String(), Outcome: "success", Data: data,
		}); err != nil {
			return nil, fmt.Errorf("record device.local.enrol: %w", err)
		}
	}
	fwd := federation.NewForwarder(client, st, cred, rec)
	if err := fwd.Load(ctx); err != nil {
		return nil, fmt.Errorf("load org federation state: %w", err)
	}
	if fwd.Status().Revoked {
		slog.Error("wardynd: the organisation revoked this device; new runs are refused until it is re-enrolled with a fresh WARDYN_ORG_ENROLMENT_TOKEN",
			"device_id", cred.DeviceID)
	}
	go fwd.Run(rootCtx)
	return fwd.Status, nil
}

// checkPostureAndBootHybrid is validateMemberModePosture then bootHybrid, one
// call because run() sits at the gocyclo cap. The URL bootHybrid dials was
// already vetted by validateHybridPosture (validateBootPosture, before
// connectAndMigrate).
func checkPostureAndBootHybrid(ctx, rootCtx context.Context, f *bootFlags, localMode, oidcConfigured bool,
	secrets secretKeyStore, st federation.Store, rec audit.Recorder) (func() federation.Status, error) {
	if err := validateMemberModePosture(*f.memberMode, localMode, oidcConfigured); err != nil {
		return nil, err
	}
	return bootHybrid(ctx, rootCtx, *f.orgURL, *f.orgEnrolToken, secrets, st, rec)
}

func parseOrgCredential(b []byte) (federation.Credential, bool) {
	var c federation.Credential
	if json.Unmarshal(b, &c) != nil || c.DeviceID == uuid.Nil || !strings.HasPrefix(c.Token, "wdd_") {
		return federation.Credential{}, false
	}
	return c, true
}
