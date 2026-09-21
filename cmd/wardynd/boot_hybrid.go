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
// the cursor goes back to 0 and the whole local table is pushed again.
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
			if err := st.SetFederationCursor(ctx, 0); err != nil {
				return nil, err
			}
			enrolled = &resp
			return json.Marshal(federation.Credential{DeviceID: resp.DeviceID, Token: resp.Token,
				EnrolmentTokenSHA256: federation.TokenSHA256(enrolToken)})
		})
	if err != nil {
		return nil, err
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
	go fwd.Run(rootCtx)
	return fwd.Status, nil
}

// checkPostureAndBootHybrid is checkMemberAndHybridBootPosture then
// bootHybrid, one call for the same reason that function is: run() sits at the
// gocyclo cap, and hybrid boot must follow the posture check that vets its URL.
func checkPostureAndBootHybrid(ctx, rootCtx context.Context, f *bootFlags, localMode, oidcConfigured bool,
	secrets secretKeyStore, st federation.Store, rec audit.Recorder) (func() federation.Status, error) {
	if err := checkMemberAndHybridBootPosture(*f.memberMode, localMode, oidcConfigured,
		*f.orgURL, *f.orgEnrolToken, *f.allowPlaintextListen); err != nil {
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
