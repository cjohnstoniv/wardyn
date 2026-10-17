// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The AWS CLI row's sign-in and what the component gate asks of it. POST
// /components/{id}/sign-in launches the AWS sign-in sandbox for an org row of
// cli.kind aws the caller may attach, seeded with the row's portal, region and
// account/role pin; the sandbox's own helper upload (ssotoken.go) lands the
// session in the caller's namespace under wardyn-cli-<id>-sso. A row deleted,
// or re-pointed at another portal, region, account or role, takes every
// person's sign-in for it with it.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	cliSignInNotAWS        = "this component is not an AWS CLI, so there is nothing to sign in to"
	cliSignInPreview       = "Exit the user view to sign in — the capture would land on your own identity."
	cliSignInCaptureChange = "the AWS CLI component this sign-in was for was removed or changed while it was open — start the sign-in again"
)

// cliRowLockPart keys the lock a capture's store (lockCLIRow) and a row's
// purge (handlePutComponent, handleDeleteComponent) both hold, so a purge
// never lands between a capture's check of the row and its write.
const cliRowLockPart = "cli-component"

func (s *Server) lockCLIRow(ctx context.Context, componentID string) (context.Context, func(), error) {
	return s.lock(ctx, db.SiteConfigLockClass, cliRowLockPart, componentID)
}

// componentCLIAdmitted is the component gate's AWS CLI step: one AWS CLI row
// per run (a sandbox holds one AWS profile), refused outright while the kill
// switch would put the session in the sandbox, and only once the caller has
// signed in for it. The preview reports a missing sign-in rather than refusing.
func (s *Server) componentCLIAdmitted(r *http.Request, adm *componentAdmission, c *attachedComponent, i int) *componentRefusal {
	if attachedAWSCLI(*c) == nil {
		return nil
	}
	switch {
	case adm.awsCLI:
		return componentRefused(*c, i, reasonComponentDefinitionInvalid,
			"a run can attach one AWS CLI component; remove the second", "is a second AWS CLI component, and a run attaches one.")
	case !s.cfg.AWSSSOProxyInject:
		return componentRefused(*c, i, reasonComponentCLIAWSInjectOff, cliAWSInjectOffRefusal, "is the AWS CLI, which this deployment has turned off.")
	}
	adm.awsCLI = true
	if s.callerOwnsSecret(r, adm, cliSecretName(c.snapshot.ComponentID.String(), providerSSOPart)) {
		return nil
	}
	if !adm.credentials {
		c.needsSignIn = true
		return nil
	}
	// The person's own step, so never "ask your admin" (componentRefused).
	return &componentRefusal{
		refusal: runError(http.StatusUnprocessableEntity, reasonComponentCLISignInRequired,
			fmt.Sprintf("components[%d]: sign in to AWS for %q first, then start the run.", i, c.snapshot.Name)),
		reason: reasonComponentCLISignInRequired, ordinal: i, source: c.source, id: c.snapshot.ComponentID,
	}
}

// cliSignInItem is the setup item a missing AWS CLI sign-in adds.
func cliSignInItem(componentID, name string) SetupItem {
	return SetupItem{Kind: "cli_access", ID: "cli_access:" + componentID, Label: "Sign in to AWS for " + name,
		RequiredBy: name, Status: "missing", Fix: &SetupFix{Action: fixSignIn, Target: signInAWSCLI, ID: componentID}}
}

// handleCLISignIn launches the caller's own AWS sign-in for one AWS CLI row:
//
//	POST /api/v1/components/{id}/sign-in
//
// No body: the portal, region and pin are the row's. A row the caller may not
// attach answers exactly as the run doors answer it, so whether it exists is
// not observable.
func (s *Server) handleCLISignIn(w http.ResponseWriter, r *http.Request) {
	owner := s.credentialOwner(w, r)
	if owner == "" {
		return
	}
	cs, ok := s.componentStoreOr501(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "component")
	if !ok {
		return
	}
	if s.componentAttachRefusal(r, id.String()).write(s, w, r) {
		return
	}
	row, err := cs.GetComponent(r.Context(), id, "")
	if err != nil {
		writeServerError(w, r, "read component", err)
		return
	}
	aws := attachedAWSCLI(attachedComponent{snapshot: types.RunComponent{ComponentID: &id, Definition: row.Definition}})
	if aws == nil {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonCLISignInNotAWS, cliSignInNotAWS)
		return
	}
	if previewHidesOwnCredential(r.Context()) {
		writeErrorReason(w, http.StatusConflict, reasonCLISignInPreviewBlocked, cliSignInPreview)
		return
	}
	hl, _ := harnessLoginByProvider(awsSSOProvider)
	t := loginTarget{
		startURL: aws.StartURL, region: aws.SSORegion, scope: cliAWSScope(id.String(), owner),
		pin: awsSSOPin{AccountID: aws.AccountID, RoleName: aws.RoleName}, cliComponent: id.String(),
	}
	_, actor := actorFromRequest(r)
	run, dispatch, err := s.launchHarnessLoginRun(r.Context(), actor, hl, t)
	switch {
	case errors.Is(err, errRecordCeilingLimit):
		writeErrorReasonPolicy(w, http.StatusForbidden, reasonRecordCeilingLimit, strings.TrimPrefix(err.Error(), errRecordCeilingLimit.Error()+": "), recordCeilingRef(err))
		return
	case errors.Is(err, errSignInBusy):
		writeErrorReason(w, http.StatusServiceUnavailable, reasonCaptureSignInBusy, signInBusyRefusal)
		return
	case err != nil:
		writeServerError(w, r, "launch login sandbox", err)
		return
	}
	writeJSON(w, http.StatusOK, harnessLoginResponse{RunID: run.ID.String(), State: string(run.State)})
	ctx := context.WithoutCancel(r.Context())
	s.goBackground(func() { s.finishHarnessLoginLaunch(ctx, run, dispatch) })
}

// storeCLISignIn stores an AWS CLI sign-in only while its row is still the
// org's AWS CLI row with the portal, region and pin the sign-in was launched
// with, under the lock the row's purge holds. changed=true: refused, nothing
// stored.
func (s *Server) storeCLISignIn(ctx context.Context, stamp loginRunStamp, scope awsSSOScope, blob awsSSOBlob) (bool, error) {
	id, err := uuid.Parse(stamp.CLIComponentID)
	if err != nil {
		return true, nil
	}
	ctx, unlock, err := s.lockCLIRow(ctx, id.String())
	if err != nil {
		return false, err
	}
	defer unlock()
	cs, ok := s.cfg.Store.(store.ComponentStore)
	if !ok {
		return true, nil
	}
	row, err := cs.GetComponent(ctx, id, "")
	if err != nil {
		return true, nil
	}
	aws := attachedAWSCLI(attachedComponent{snapshot: types.RunComponent{ComponentID: &id, Definition: row.Definition}})
	if aws == nil || aws.StartURL != stamp.SSOStartURL || aws.SSORegion != stamp.SSORegion ||
		aws.AccountID != stamp.SSOAccountID || aws.RoleName != stamp.SSORoleName {
		return true, nil
	}
	return false, s.storeAWSSSOBlob(ctx, scope, blob)
}

// cliSignInPinChanged reports whether an org row's update leaves its AWS
// sign-ins pointing at a portal, region, account or role it no longer names.
func cliSignInPinChanged(before, after types.ComponentDefinition) bool {
	was := cliAWSOf(before)
	if was == nil {
		return false
	}
	now := cliAWSOf(after)
	return now == nil || now.StartURL != was.StartURL || now.SSORegion != was.SSORegion ||
		now.AccountID != was.AccountID || now.RoleName != was.RoleName
}

func cliAWSOf(d types.ComponentDefinition) *types.CLIAWS {
	if d.CLI == nil || d.CLI.Kind != types.CLIKindAWS {
		return nil
	}
	return d.CLI.AWS
}

// purgeCLISignIns deletes every person's sign-in for one org row. The caller
// holds lockCLIRow and calls it before the row's write, so a failed purge
// refuses the write and a capture can never land in between.
func (s *Server) purgeCLISignIns(ctx context.Context, componentID string) error {
	if s.cfg.Secrets == nil {
		return nil
	}
	_, err := s.deleteCredentialsEverywhere(ctx, []string{
		cliSecretName(componentID, providerSSOPart), cliSecretName(componentID, providerEntraPart),
	})
	if err != nil {
		return fmt.Errorf("purge cli sign-ins: %w", err)
	}
	return nil
}

// auditCLICapture records cli.signin.capture for a CLI sign-in's upload:
// success when it landed, failure with the refusal's reason when it did not.
func (s *Server) auditCLICapture(r *http.Request, runID uuid.UUID, actor string, scope awsSSOScope, outcome, reason string) {
	data := map[string]any{"cli": types.CLIKindAWS, "component_id": scope.cli, "owner": scope.owner,
		"credential_source": awsSSOCredentialSourceLabel(scope)}
	if reason != "" {
		data["reason"] = reason
	}
	s.recordAudit(r.Context(), s.auditEvent(&runID, types.ActorAgent, actor, "cli.signin.capture", scope.ssoSecret(), outcome, mustJSON(data)))
}
