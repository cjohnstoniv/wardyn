// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// runner_resident delivery, the organisation's half (design 0.9 §7.4): a
// dispatch reads each resident value where its placeholder is authored, opens
// a Postgres obligation for the run, re-judges every value against the bound
// runner's current posture and the current delivery policy, and hands each to
// the runner after the lease and before the create that names it. Any error or
// ambiguity refuses the run before create. Once the run ends, the erase sweep
// asks the runner to erase the run's values and closes the obligation, or,
// for a revoked runner or a long-expired obligation, closes it unconfirmed.

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// residentDeliveryLease is the obligation's delivery lease, renewed before
	// each value: an erase claim waits for it to be released or to lapse.
	residentDeliveryLease = 2 * time.Minute
	// residentDeliverTimeout bounds one DeliverResident call, inside the lease.
	residentDeliverTimeout = time.Minute
	// residentReadCeiling is the latest not_after a delivery may carry, counted
	// from the delivery (owner ruling Q1): no read of a value after it.
	residentReadCeiling = 24 * time.Hour
	// residentMinLife refuses a delivery whose not_after is this close.
	residentMinLife = time.Minute
	// residentEraseBatch bounds the obligations one runner's erase claims at once.
	residentEraseBatch = 8
	// residentEraseTimeout bounds one EraseResident call.
	residentEraseTimeout = time.Minute
	// macKeychainCommandLimit is the longest command go-keyring v0.2.8 writes
	// to `security -i` on macOS; the runner refuses a longer one before it
	// starts the tool, and the org refuses it before sending.
	macKeychainCommandLimit = 4096
)

// residentValue is one runner_resident value a dispatch read for its run's
// runner: the grant it is delivered for, the grant's delivery class, and the
// value, which leaves dispatch only in DeliverResident.
type residentValue struct {
	grant types.CredentialGrant
	class string
	value []byte
}

// residentByGrantID orders resident values by grant id, the order they are
// delivered in.
func residentByGrantID(a, b residentValue) int {
	return strings.Compare(a.grant.ID.String(), b.grant.ID.String())
}

// residentClass is the delivery class a resident grant of kind is judged and
// delivered as; "" for a kind that is never runner_resident.
func residentClass(kind types.GrantKind) string {
	switch kind {
	case types.GrantEnvSecret:
		return placement.ClassEnvSecret
	case types.GrantFileSecret:
		return placement.ClassFileSecret
	case types.GrantSSHKey:
		return placement.ClassSSHKey
	case types.GrantGitPAT:
		return placement.ClassGitPATHelper
	}
	return ""
}

// residentADOGitPATRefused: an Azure DevOps git token is its owner's own (the
// shared one is retired), runner_resident delivers an operator's value, and a
// connection's runner_resident opt-in admits only its minted PAT, a class whose
// resident delivery is not built.
const residentADOGitPATRefused = "an Azure DevOps git token is never delivered runner_resident"

// residentADOGitPAT reports whether g is a git_pat for an Azure DevOps host
// under sc. Create refuses it resident and delivery checks it again against
// the site config of that moment.
func residentADOGitPAT(sc types.SiteConfig, g types.GrantSpec) bool {
	if g.Kind != types.GrantGitPAT {
		return false
	}
	pat, err := types.DecodeGitPATScope(g.Scope)
	if err != nil {
		return true
	}
	// A stored host that is not a hostname once folded (a scheme, a path or
	// userinfo written before the host was checked) cannot be shown not to be
	// Azure DevOps, so it is refused the same.
	host := gitPATHostKey(pat.Host)
	return !validGitPATHost(host) || host == "dev.azure.com" || strings.HasSuffix(host, ".dev.azure.com") || adoGrantHost(sc, host)
}

// gitPATHostKey is host as a forge names it: lowercased, trimmed of space and
// of a trailing dot, with a numeric port split off. A stored scope written before
// the host had to be bare (validGitPATHost) is judged on this form.
func gitPATHostKey(host string) string {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if h, port, err := net.SplitHostPort(host); err == nil && port != "" && strings.Trim(port, "0123456789") == "" {
		host = strings.TrimSuffix(h, ".") // a numeric port only: "https://x" keeps its scheme and is refused
	}
	return host
}

// residentGrantRefusal is why create refuses g runner_resident although the
// policy and table admit its class; "" when it is admitted. A grant that needs
// an approval to mint is never resident (no approval reaches a value the runner
// holds); a git_pat is resident only while the PAT broker is off, since the
// broker serves it proxy-side; an Azure DevOps git token never is.
func (s *Server) residentGrantRefusal(ctx context.Context, g types.GrantSpec) string {
	switch {
	case g.RequiresApproval:
		return "a grant that requires approval is never delivered runner_resident"
	case g.Kind != types.GrantGitPAT:
		return ""
	case !s.cfg.DisableGitPATBroker:
		return residentPATBrokered
	}
	var sc types.SiteConfig
	if s.cfg.Store != nil {
		var err error
		if sc, err = s.cfg.Store.GetSiteConfig(ctx); err != nil {
			return "the site configuration could not be read"
		}
	}
	if residentADOGitPAT(sc, g) {
		return residentADOGitPATRefused
	}
	return ""
}

// residentForSandbox reports whether the runner may place a value of class in
// the sandbox (env_secret, file_secret) rather than its own proxy only.
func residentForSandbox(class string) bool {
	return class == placement.ClassEnvSecret || class == placement.ClassFileSecret
}

// residentFillOffered reports whether dispatch offers grant's value to the
// organisation's fill (WARDYN_RUNNER_SECRET_PLACEHOLDERS, fillResident): an
// env_secret or file_secret grant of a local run whose scope names a host.
func (s *Server) residentFillOffered(run types.AgentRun, grant types.CredentialGrant) bool {
	host, err := secretScopeHost(grant.Spec.Scope)
	return s.cfg.RunnerSecretPlaceholders && run.Placement == types.PlacementLocal &&
		residentForSandbox(residentClass(grant.Spec.Kind)) && err == nil && host != ""
}

// readResidentValue reads one runner_resident grant's value for the run's
// runner from the operator's namespace, the one localGrantOrigin proved for a
// resident grant, with no owner fallback. The read is marked SiteAudited and
// recorded here as secret.read with purpose runner-resident, the grant, the
// runner and its reported posture, or, for a value offered to the
// organisation's fill (residentFillOffered), fill via_org in place of the
// runner and posture: run.secret_placeholder.resolve then says where it went.
// The value then joins the run's masking manifest, before dispatch completes it. An error means the value is not to
// be used: the caller skips the grant as it skips any unreadable one.
func (s *Server) readResidentValue(ctx context.Context, run types.AgentRun, grant types.CredentialGrant, secretName string) (residentValue, error) {
	class := residentClass(grant.Spec.Kind)
	if class == "" || grant.Delivery != types.GrantDeliveryResident || grant.RunID != run.ID || run.RunnerID == nil || s.cfg.Secrets == nil {
		return residentValue{}, errors.New("api: the grant cannot be delivered runner_resident")
	}
	// The organisation's row is read here, so a ref bound to a person's own
	// row is never served (an operator-owned run's own row is that one).
	if grant.Spec.OwnerOnlyFor(secretName) && !run.OperatorOwned {
		return residentValue{}, errors.New("api: the grant reads its owner's own secret, which is never delivered runner_resident")
	}
	data := map[string]any{"purpose": string(secretstore.PurposeRunnerResident), "grant_id": grant.ID, "runner_id": *run.RunnerID,
		"delivery": class, "owner": "", "posture_source": types.RunnerPostureSourceRunnerAsserted}
	if posture, err := s.runnerPosture(ctx, *run.RunnerID); err == nil && posture != nil {
		data["posture"] = posture
	}
	if s.residentFillOffered(run, grant) {
		delete(data, "runner_id")
		delete(data, "posture")
		delete(data, "posture_source")
		data["fill"] = types.GrantDeliveryViaOrg
	}
	rctx, row := secretstore.SiteAudited(ctx)
	val, err := s.cfg.Secrets.For("").Get(rctx, secretName)
	if err == nil && len(val) == 0 {
		err = secretstore.ErrNotFound
	}
	if scope := row.Scope(); scope != "" {
		data["secret_scope"] = scope
	}
	data = withStoreRow(data, row)
	if err != nil {
		_, data["reason"], _ = storeReadRefusal(secretName, err)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "secret.read", secretName, "failure", mustJSON(data)))
		return residentValue{}, err
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "secret.read", secretName, "success", mustJSON(data)))
	renderings := [][]byte{val}
	if class != placement.ClassEnvSecret {
		renderings = fileSecretRenderings(val)
	}
	if err := s.maskDispatchValue(ctx, run.ID, renderings...); err != nil {
		clear(val)
		return residentValue{}, err
	}
	return residentValue{grant: grant, class: class, value: val}, nil
}

// runnerPosture is the runner's stored, self-reported posture; nil when it
// never reported one.
func (s *Server) runnerPosture(ctx context.Context, runnerID uuid.UUID) (*types.RunnerPosture, error) {
	st, ok := s.cfg.Store.(interface {
		GetRunner(context.Context, uuid.UUID) (types.Runner, error)
	})
	if !ok {
		return nil, nil
	}
	rn, err := st.GetRunner(ctx, runnerID)
	if err != nil || rn.PostureReportedAt == nil {
		return nil, err
	}
	return &rn.Posture, nil
}

// residentNotAfter is the last moment the runner may read a value delivered
// now (owner ruling Q1): the run's lifetime cap or its end, whichever comes
// first, and never later than 24 hours after delivery.
func residentNotAfter(run types.AgentRun, now time.Time) time.Time {
	notAfter := now.Add(residentReadCeiling)
	for _, t := range []*time.Time{run.LifetimeCapAt, run.EndsAt} {
		if t != nil && t.Before(notAfter) {
			notAfter = *t
		}
	}
	return notAfter
}

// macKeychainCommandTooLong reports whether the runner's macOS keychain write of
// value would pass macKeychainCommandLimit: go-keyring's add-generic-password
// command for the runner's service (wardyn-runnerd:<runner>) and the account
// <run>/<grant>, with the value base64-encoded behind its prefix. The runner
// checks the same rule before it starts the tool.
func macKeychainCommandTooLong(runnerID, runID, grantID uuid.UUID, value []byte) bool {
	service := "wardyn-runnerd:" + runnerID.String()
	account := runID.String() + "/" + grantID.String()
	n := len("add-generic-password -U -s  -a  -w \n") + len(service) + len(account) +
		len("go-keyring-base64:") + base64.StdEncoding.EncodedLen(len(value))
	return n > macKeychainCommandLimit
}

// residentDeliveryReason is the refusal reason of a failed DeliverResident.
func residentDeliveryReason(err error) placement.Reason {
	var we *runnerwire.Error
	switch {
	case errors.Is(err, runner.ErrRunnerOffline):
		return placement.ReasonRunnerOffline
	case errors.As(err, &we) && we.Code == runnerwire.CodeRefused &&
		strings.HasPrefix(we.Message, string(placement.ReasonRunnerKeystoreUnavailable)):
		return placement.ReasonRunnerKeystoreUnavailable
	}
	return placement.ReasonPlacementCredential
}

// deliverResident hands the run's runner_resident values to its runner, after
// the lease and before the create; true means every value is held there, or
// there was none. It opens the obligation row under a delivery lease first,
// then re-judges every value against the runner's current posture and the
// current policy, and refuses on macOS a value too long for the keychain,
// before anything is sent. Values go one DeliverResident each, in grant-id
// order, through the runner's one substrate, the lease renewed before each.
// Any error, timeout, cancellation or refusal fails the run with a run.dispatch
// failure row ({reason, grant_id, runner_id}) and kicks the erase; create is
// never reached. The values are overwritten before it returns.
func (s *Server) deliverResident(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec, values []residentValue) bool {
	if len(values) == 0 {
		return true
	}
	defer func() {
		for _, v := range values {
			clear(v.value)
		}
	}()
	var runnerID uuid.UUID
	if run.RunnerID != nil {
		runnerID = *run.RunnerID
	}
	refused := false
	// The refusal is written on a context a kill (which cancels ctx) cannot cut.
	bg := context.WithoutCancel(ctx)
	refuse := func(reason placement.Reason, grant uuid.UUID, detail string) bool {
		refused = true
		data := map[string]any{"reason": reason, "runner_id": runnerID}
		if grant != uuid.Nil {
			data["grant_id"] = grant
		}
		s.failAndRevoke(bg, run.ID, types.RunStarting, "This run was not launched on your own runner: "+detail)
		s.recordAudit(bg, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch", run.ID.String(), "failure", mustJSON(data)))
		return false
	}
	st, ok := s.cfg.Store.(store.PG)
	if !ok || run.RunnerID == nil {
		return refuse(placement.ReasonPlacementCredential, uuid.Nil, "a runner_resident credential needs a run bound to a runner and the Postgres store")
	}
	now := s.cfg.Now()
	notAfter := residentNotAfter(run, now)
	if !notAfter.After(now.Add(residentMinLife)) {
		return refuse(placement.ReasonPlacementCredential, uuid.Nil, "the run ends within a minute, too soon to hand its runner a runner_resident credential")
	}
	holder := uuid.New()
	if err := st.BeginResidentDelivery(ctx, run.ID, runnerID, holder, now, now.Add(residentDeliveryLease), notAfter); err != nil {
		slog.WarnContext(ctx, "wardynd: a runner_resident delivery could not open its obligation", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return refuse(placement.ReasonPlacementCredential, uuid.Nil, "its runner_resident credentials could not be bound to the run and its runner")
	}
	defer func() {
		if err := st.EndResidentDelivery(bg, run.ID, holder); err != nil {
			slog.WarnContext(ctx, "wardynd: a runner_resident delivery lease was not released; it lapses", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
		if refused {
			s.kickResidentErase(runnerID)
		}
	}()

	// Re-read now, not taken from earlier in dispatch: the stored delivery
	// policy, the bound runner's posture and the run's floor as they are at
	// delivery.
	sc, err := st.GetSiteConfig(ctx)
	p := placement.LocalPlan{Spec: runner.SandboxSpec{RunID: run.ID, ProxyConfig: runner.ProxyConfig{Policy: policy}}}
	if err == nil {
		err = s.localPlanFacts(ctx, &p, egressBaselineOf(sc), run.RunnerID)
	}
	if err != nil {
		return refuse(placement.ReasonPlacementCredential, uuid.Nil, "the delivery policy or the runner's posture could not be read")
	}
	values = slices.SortedFunc(slices.Values(values), residentByGrantID)
	for _, v := range values {
		if v.grant.Delivery != types.GrantDeliveryResident || v.grant.RunID != run.ID {
			return refuse(placement.ReasonPlacementCredential, v.grant.ID, "only a grant of this run recorded runner_resident is handed to its runner")
		}
		if residentADOGitPAT(sc, v.grant.Spec) {
			return refuse(placement.ReasonPlacementCredential, v.grant.ID, residentADOGitPATRefused)
		}
		origin := placement.CredentialOrigin{Class: placement.ClassOperator, Delivery: v.class, GrantKind: v.grant.Spec.Kind}
		if r := placement.ResidentAdmits(origin, p, "runner_resident."+v.class); r != nil {
			return refuse(r.Reason, v.grant.ID, r.Field+": "+r.Detail)
		}
		if p.Posture != nil && p.Posture.OS == "darwin" && macKeychainCommandTooLong(runnerID, run.ID, v.grant.ID, v.value) {
			return refuse(placement.ReasonPlacementCredential, v.grant.ID, "a runner_resident value is too long for the macOS keychain (over about 3 KB)")
		}
	}
	sub := s.runnerEndpoint(runnerID).sub
	for _, v := range values {
		if err := st.RenewResidentDelivery(ctx, run.ID, holder, s.cfg.Now().Add(residentDeliveryLease)); err != nil {
			return refuse(placement.ReasonPlacementCredential, v.grant.ID, "the run left STARTING, or its credentials' erase began, while they were being handed to the runner")
		}
		callCtx, cancel := context.WithTimeout(ctx, residentDeliverTimeout)
		err := sub.DeliverResident(callCtx, runnerwire.DeliverResidentArgs{RunID: run.ID, GrantID: v.grant.ID, Kind: string(v.grant.Spec.Kind),
			Value: string(v.value), NotAfter: notAfter, Sandbox: residentForSandbox(v.class)})
		cancel()
		if err != nil {
			reason := residentDeliveryReason(err)
			slog.WarnContext(ctx, "wardynd: a runner_resident value was not handed to the runner", slog.String("run_id", run.ID.String()),
				slog.String("grant_id", v.grant.ID.String()), slog.Any("err", err))
			return refuse(reason, v.grant.ID, "a runner_resident credential could not be handed to the runner ("+string(reason)+")")
		}
	}
	return true
}

// leaseDeliverCreate is dispatch's last three steps, in order: the runner lease
// (a runner refuses a create no lease authorizes), the runner_resident delivery,
// and the create that names those values. refused means the delivery refused the
// run, which deliverResident has already failed and recorded; no create was
// asked for.
func (s *Server) leaseDeliverCreate(ctx, createCtx context.Context, run types.AgentRun, spec runner.SandboxSpec,
	values []residentValue) (runnerExecution, runner.Sandbox, bool, error) {
	execution, err := s.leaseRunOnRunner(ctx, run)
	if err != nil {
		return execution, runner.Sandbox{}, false, err
	}
	if !s.deliverResident(createCtx, run, spec.ProxyConfig.Policy, values) {
		return execution, runner.Sandbox{}, true, nil
	}
	sb, err := s.createSandboxAtTarget(createCtx, run, spec)
	return execution, sb, false, err
}

// kickResidentErase starts runnerID's resident erase now, off the caller's
// path, when this replica holds the runner's session; the sweep retries
// whatever it leaves.
func (s *Server) kickResidentErase(runnerID uuid.UUID) {
	if s.runnerOnline(runnerID) {
		s.startResidentErase(runnerID)
	}
}

// kickRunResidentErase is kickResidentErase for the runner a run that has
// just ended was bound to (design §5.4, from revokeRunCascade).
func (s *Server) kickRunResidentErase(ctx context.Context, runID uuid.UUID) {
	if _, ok := s.cfg.Store.(store.PG); !ok || s.cfg.RunnerOnline == nil {
		return
	}
	if run, err := s.cfg.Store.GetRun(ctx, runID); err == nil && run.RunnerID != nil {
		s.kickResidentErase(*run.RunnerID)
	}
}

// startResidentErase runs runnerID's resident erase in the background. The
// runner-ready path calls it directly: the session it was told of is this
// replica's, though the hub reports it online only once READY returns.
func (s *Server) startResidentErase(runnerID uuid.UUID) {
	st, ok := s.cfg.Store.(store.PG)
	if !ok || s.cfg.BaseCtx == nil {
		return
	}
	s.goBackground(func() {
		if err := s.eraseResidentOn(s.cfg.BaseCtx, st, runnerID); err != nil {
			slog.WarnContext(s.cfg.BaseCtx, "wardynd: runner resident erase kick", slog.String("runner_id", runnerID.String()), slog.Any("err", err))
		}
	})
}

// sweepResidentErase is the run watcher's resident erase pass, on every
// replica: it closes, unconfirmed and audited, the obligations of revoked
// runners and those long past not_after, then erases the due obligations of
// the runners whose session this replica holds. A runner offline here waits
// for the replica that holds it.
func (s *Server) sweepResidentErase(ctx context.Context) error {
	st, ok := s.cfg.Store.(store.PG)
	if !ok {
		return nil
	}
	now := s.cfg.Now()
	var abandonErr error
	if s.cfg.RunnerAuditPrepare != nil { // abandonment writes its audit rows through it; erasing does not need it
		_, abandonErr = st.AbandonResidentErase(ctx, now, s.cfg.RunnerAuditPrepare)
	}
	runners, err := st.DueResidentEraseRunners(ctx, now)
	if err != nil {
		return errors.Join(abandonErr, err)
	}
	errs := []error{abandonErr}
	for _, id := range runners {
		if s.runnerOnline(id) {
			errs = append(errs, s.eraseResidentOn(ctx, st, id))
		}
	}
	return errors.Join(errs...)
}

// eraseResidentOn claims runnerID's due obligations and asks the runner to
// erase each run's values, the calls in parallel. The runner answers nil only
// once every value of the run is erased and its report accepted; then the
// obligation closes. Otherwise the reason is kept and the claim's backoff says
// when the next attempt is due.
func (s *Server) eraseResidentOn(ctx context.Context, st store.PG, runnerID uuid.UUID) error {
	runs, err := st.ClaimResidentErase(ctx, runnerID, s.cfg.Now(), residentEraseBatch)
	if err != nil || len(runs) == 0 {
		return err
	}
	sub := s.runnerEndpoint(runnerID).sub
	errs := make([]error, len(runs))
	var wg sync.WaitGroup
	for i, runID := range runs {
		wg.Go(func() {
			callCtx, cancel := context.WithTimeout(ctx, residentEraseTimeout)
			defer cancel()
			if eraseErr := sub.EraseResident(callCtx, runID); eraseErr != nil {
				errs[i] = cmp.Or(st.NoteResidentEraseFailed(ctx, runID, eraseErr.Error()), eraseErr)
				return
			}
			errs[i] = st.MarkResidentErased(ctx, runID, s.cfg.Now())
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
