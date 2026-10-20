// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The kubectl arm. An org row of cli.kind kubectl registers one cluster: its
// API server, its CA, one service account and the namespaces a run may reach.
// An admin stores that account's token through the row's own route
// (handlePutKubectlToken), under a name only Wardyn's own grant may name. The
// sandbox holds a kubeconfig whose token is an inert sentinel; the proxy's door
// on the API server (internal/egress/proxy/kube_api_door.go) holds each request
// to the row's namespaces and access and adds a token the control plane mints
// from the registered one with the cluster's TokenRequest API
// (injection_kubectl_cli.go), valid for ten minutes. Neither token is written
// into the sandbox.

// kubectlCLIOn is the switch (types.EnvKubectlCLI), off by default. Off, a
// kubectl row is refused at save and at launch exactly as before the kind
// existed, the catalogue card stays coming_soon, the token route is not there
// and a running run's resolve stops. Only wardynd reads it.
func kubectlCLIOn() bool { return types.KubectlCLIOn() }

const (
	// kubectlCLIMechanism is the grant snapshot's name for this lane.
	kubectlCLIMechanism = "kubectl_cli"
	// kubectlCLIConfigEnvVar carries the generated ~/.kube/config
	// (agent-run-lib.sh materialises it).
	kubectlCLIConfigEnvVar = "WARDYN_KUBECTL_CONFIG_B64"
	// kubectlCLITokenSentinel is the kubeconfig's token: it authenticates
	// nothing, and the door replaces it on every request it forwards.
	kubectlCLITokenSentinel = "wardyn-proxy-injects-kubectl-token"
	kubectlCLIGrantTTLSec   = 3600
	// kubectlTokenLifetimeSec is what each mint asks the cluster for: the
	// shortest lifetime the TokenRequest API grants.
	kubectlTokenLifetimeSec = 600
	maxKubectlTokenBytes    = 16 << 10
)

// Member- and admin-visible sentences of the arm.
const (
	kubectlCLISecondRowRefusal       = "a run can attach one kubectl component; remove the second"
	kubectlCLINoCertificateAuthority = "This run was not launched: its kubectl needs a per-run certificate authority to be added on the wire, " +
		"and none was provisioned. Without one the proxy could not check or credential kubectl's requests."
	kubectlTokenBody = `body must be {"value":"<the service account's token>"}`
)

func kubectlTokenSecretName(componentID string) string {
	return cliSecretName(componentID, types.CLIKubectlTokenPart)
}

func cliKubectlOf(d types.ComponentDefinition) *types.CLIKubectl {
	if d.CLI == nil || d.CLI.Kind != types.CLIKindKubectl {
		return nil
	}
	return d.CLI.Kubectl
}

// kubectlCLIPinChanged reports whether an org row's update points its
// registered token at another cluster or account: the server, the CA that
// vouches for it, or the service account changed. Such an edit takes the token
// with it, so a token is never sent to a URL it was not registered for.
func kubectlCLIPinChanged(before, after types.ComponentDefinition) bool {
	was := cliKubectlOf(before)
	if was == nil {
		return false
	}
	now := cliKubectlOf(after)
	return now == nil || now.Server != was.Server || now.CAPEM != was.CAPEM || now.ServiceAccount != was.ServiceAccount
}

// kubectlTokenRegistered reports whether the operator namespace holds the
// row's registered token.
func (s *Server) kubectlTokenRegistered(ctx context.Context, componentID string) (bool, error) {
	if s.cfg.Secrets == nil {
		return false, nil
	}
	names, err := s.cfg.Secrets.For("").List(secretstore.WithPurpose(ctx, secretstore.PurposeStatus))
	if err != nil {
		return false, err
	}
	return slices.Contains(names, kubectlTokenSecretName(componentID)), nil
}

// componentKubectlCLIAdmitted is the component gate's kubectl step: one row
// per run (the proxy holds one door), and only once an admin has registered
// the row's token. The preview records a missing token rather than refusing.
func (s *Server) componentKubectlCLIAdmitted(r *http.Request, adm *componentAdmission, c *attachedComponent, i int) *componentRefusal {
	if attachedKubectlCLI(*c) == nil {
		return nil
	}
	if adm.kubectlCLI {
		return componentRefused(*c, i, reasonComponentDefinitionInvalid, kubectlCLISecondRowRefusal, "is a second kubectl component, and a run attaches one.")
	}
	adm.kubectlCLI = true
	ok, err := s.kubectlTokenRegistered(r.Context(), c.snapshot.ComponentID.String())
	switch {
	case err != nil:
		return &componentRefusal{refusal: runServerError("read the kubectl token", err), reason: reasonStoreError,
			ordinal: i, source: c.source, id: c.snapshot.ComponentID}
	case ok:
		return nil
	case !adm.credentials:
		c.needsAdminSecret = true
		return nil
	}
	return &componentRefusal{
		refusal: runError(http.StatusUnprocessableEntity, reasonComponentSecretMissing,
			fmt.Sprintf("components[%d]: this component needs a secret your admin has not provided yet. Ask your admin.", i)),
		reason: reasonComponentSecretMissing, ordinal: i, source: c.source, id: c.snapshot.ComponentID,
	}
}

// handlePutKubectlToken registers a kubectl row's service-account token:
//
//	PUT /api/v1/components/{id}/kubectl-token   {"value": "<token>"}
//
// Operator only. The token is stored in the operator namespace under the row's
// reserved name, which the secrets API refuses and no grant but the row's own
// may name, and is never returned. An edit of the row that points it at
// another cluster or account deletes it (kubectlCLIPinChanged), and so does
// deleting the row. With the switch off the route is not there.
func (s *Server) handlePutKubectlToken(w http.ResponseWriter, r *http.Request) {
	if !kubectlCLIOn() {
		http.NotFound(w, r)
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
	var body putSecretRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&body); err != nil ||
		body.Value == "" || len(body.Value) > maxKubectlTokenBytes || strings.ContainsAny(body.Value, " \t\r\n") {
		writeErrorReason(w, http.StatusBadRequest, reasonSecretBodyInvalid, kubectlTokenBody)
		return
	}
	if len(body.Value) < secretmask.MinLen {
		writeErrorReason(w, http.StatusBadRequest, reasonSecretValueTooShort,
			fmt.Sprintf("secret too short: must be at least %d bytes to be masked and scanned", secretmask.MinLen))
		return
	}
	// The row's lock: an edit that would purge the token cannot land between
	// the read of the row and the write of its token.
	r, unlock, ok := s.lockDoor(w, r, db.SiteConfigLockClass, cliRowLockPart, id.String())
	if !ok {
		return
	}
	defer unlock()
	row, err := cs.GetComponent(r.Context(), id, "")
	if notFoundIf(w, err, "component", reasonComponentNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "read component", err)
		return
	}
	if cliKubectlOf(row.Definition) == nil {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonComponentDefinitionInvalid, "this component is not a kubectl component")
		return
	}
	name := kubectlTokenSecretName(id.String())
	record := mustJSON(newKubectlTokenRecord(*cliKubectlOf(row.Definition), body.Value))
	if r, ok = s.beginRequestWrite(w, r, ""); !ok {
		return
	}
	if err := s.putOwned(r.Context(), "", func(ctx context.Context) error {
		return s.cfg.Secrets.For("").Put(ctx, name, record)
	}); err != nil {
		s.auditRowNotWritten(r.Context(), err, actorTypeFromRequest(r), principalFromRequest(r), "", name)
		writeServerError(w, r, "store kubectl token", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"secret.write", name, "success", mustJSON(map[string]any{"cli": types.CLIKindKubectl, "component_id": id.String(), "owner": ""})))
	w.WriteHeader(http.StatusNoContent)
}

// kubectlCLIDispatch is the kubectl row a run attached, as the gate admitted it.
type kubectlCLIDispatch struct {
	ComponentID string
	Kubectl     types.CLIKubectl
}

// kubectlCLI is the run's kubectl row, nil when it attached none.
func (c runComponents) kubectlCLI() *kubectlCLIDispatch {
	for _, a := range c.attached {
		if k := attachedKubectlCLI(a); k != nil {
			return &kubectlCLIDispatch{ComponentID: a.snapshot.ComponentID.String(), Kubectl: *k}
		}
	}
	return nil
}

// attachedKubectlCLI is an attached org row's kubectl block, nil for anything else.
func attachedKubectlCLI(a attachedComponent) *types.CLIKubectl {
	if a.snapshot.SelfDefined || a.snapshot.ComponentID == nil {
		return nil
	}
	return cliKubectlOf(a.snapshot.Definition)
}

// kubectlGrantSnapshot is what dispatch records on the lane's grant: whose run
// it is, for which row, and the cluster, account, namespaces and access the
// run's proxy was configured with. The sink re-derives each from the live row
// and refuses on any difference.
// kubectlTokenRecord is what the token route stores: the token with the
// server, CA and account it was registered for. A resolve uses it only for a
// grant whose snapshot names the same three, so a token registered for one
// cluster never reaches another, whatever happened to the row in between.
type kubectlTokenRecord struct {
	Server         string                  `json:"server"`
	CASHA256       string                  `json:"ca_sha256"`
	ServiceAccount types.CLIServiceAccount `json:"service_account"`
	Value          string                  `json:"value"`
}

func newKubectlTokenRecord(k types.CLIKubectl, value string) kubectlTokenRecord {
	sn := kubectlSnapshotOf(k)
	return kubectlTokenRecord{Server: sn.Server, CASHA256: sn.CASHA256, ServiceAccount: sn.ServiceAccount, Value: value}
}

// pinnedTo reports whether the record was registered for the snapshot's server, CA and account.
func (rec kubectlTokenRecord) pinnedTo(sn kubectlGrantSnapshot) bool {
	return rec.Value != "" && rec.Server == sn.Server && rec.CASHA256 == sn.CASHA256 && rec.ServiceAccount == sn.ServiceAccount
}

type kubectlGrantSnapshot struct {
	Mechanism      string                  `json:"mechanism"`
	ComponentID    string                  `json:"component_id"`
	OwnerSubject   string                  `json:"owner_subject"`
	Server         string                  `json:"server"`
	CASHA256       string                  `json:"ca_sha256"`
	ServiceAccount types.CLIServiceAccount `json:"service_account"`
	Namespaces     []string                `json:"namespaces"`
	Access         string                  `json:"access"`
}

func (sn kubectlGrantSnapshot) authored() bool {
	return sn.Mechanism == kubectlCLIMechanism && sn.ComponentID != "" && sn.OwnerSubject != "" && sn.Server != "" &&
		sn.ServiceAccount.Name != "" && len(sn.Namespaces) > 0
}

// kubectlSnapshotOf is the part of a snapshot a row decides.
func kubectlSnapshotOf(k types.CLIKubectl) kubectlGrantSnapshot {
	sum := sha256.Sum256([]byte(k.CAPEM))
	return kubectlGrantSnapshot{Mechanism: kubectlCLIMechanism, Server: strings.TrimSuffix(k.Server, "/"), CASHA256: hex.EncodeToString(sum[:]),
		ServiceAccount: k.ServiceAccount, Namespaces: slices.Sorted(slices.Values(k.Namespaces)), Access: k.Access}
}

// kubectlCLIFiles is the ~/.kube/config a run's kubectl reads: the row's
// server, the run's own CA (the proxy terminates the connection), the first
// namespace as the default and the inert token. It takes no credential, so it
// can write none.
func kubectlCLIFiles(k types.CLIKubectl, runCACertPEM string) map[string]string {
	cfg := map[string]any{
		"apiVersion": "v1", "kind": "Config", "current-context": "wardyn", "preferences": map[string]any{},
		"clusters": []any{map[string]any{"name": "wardyn", "cluster": map[string]any{
			"server": strings.TrimSuffix(k.Server, "/"), "certificate-authority-data": base64.StdEncoding.EncodeToString([]byte(runCACertPEM)),
		}}},
		"users": []any{map[string]any{"name": "wardyn", "user": map[string]any{"token": kubectlCLITokenSentinel}}},
		"contexts": []any{map[string]any{"name": "wardyn", "context": map[string]any{
			"cluster": "wardyn", "user": "wardyn", "namespace": k.Namespaces[0],
		}}},
	}
	return map[string]string{".kube/config": string(mustJSON(cfg))}
}

// kubectlCLIPlan is what the lane adds to one run's sidecar configuration.
type kubectlCLIPlan struct {
	gate   *proxy.KubeAPIGrant
	opened []string // the allowlist entry the lane itself added, if the policy did not carry it
}

// closeWithheld is azureCLIPlan.closeWithheld for the kubectl door: when a
// later phase withheld the credential (the ceiling denies the server), the door
// goes and the entry the lane opened is taken back. It returns the door the run
// keeps.
func (pl kubectlCLIPlan) closeWithheld(policy *types.RunPolicySpec, injections []runner.InjectionGrant) *proxy.KubeAPIGrant {
	if pl.gate == nil {
		return nil
	}
	if slices.ContainsFunc(injections, func(in runner.InjectionGrant) bool { return hostEqual(in.Rule.Host, pl.gate.Host) }) {
		return pl.gate
	}
	policy.AllowedDomains = slices.DeleteFunc(slices.Clone(policy.AllowedDomains), func(entry string) bool {
		return slices.Contains(pl.opened, entry)
	})
	return nil
}

// kubectlCLIReviveRefusal refuses the revive of a run that has the door once
// its owner's profile denies the cluster: as for the Azure CLI, a stored
// config cannot say which allowlist entry the lane opened.
func kubectlCLIReviveRefusal(cfg *proxy.Config, deny []string) *reviveError {
	if cfg.KubeAPI == nil || !ceilingDenies(deny, cfg.KubeAPI.Host) {
		return nil
	}
	return reviveRefused(http.StatusConflict, reasonReviveCeilingDeniesKubectlCLI,
		"the owner's governance profile now denies the cluster this run's kubectl reaches; start a new run")
}

// authorKubectlCLI stages the run's kubectl row: the kubeconfig in the
// sandbox, the server on its allowlist, and the grant and door that put a
// minted token on the wire. A run with no per-run certificate authority is
// REFUSED rather than degraded: the only other way to serve kubectl is a real
// token in the sandbox. ok=false: the run was marked FAILED and dispatch stops.
func (s *Server) authorKubectlCLI(ctx context.Context, run types.AgentRun, cli *kubectlCLIDispatch, caCertPEM, caKeyPEM string,
	policy *types.RunPolicySpec, sandboxEnv map[string]string, injections []runner.InjectionGrant,
) ([]runner.InjectionGrant, kubectlCLIPlan, bool) {
	if cli == nil {
		return injections, kubectlCLIPlan{}, true
	}
	refuse := func(msg string) ([]runner.InjectionGrant, kubectlCLIPlan, bool) {
		s.failAndRevoke(ctx, run.ID, types.RunStarting, msg)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": msg, "component_id": cli.ComponentID})))
		return injections, kubectlCLIPlan{}, false
	}
	host, port, ok := cli.Kubectl.HostPort()
	switch {
	case caCertPEM == "" || caKeyPEM == "":
		return refuse(kubectlCLINoCertificateAuthority)
	case !ok:
		return refuse("could not read the kubectl component's API server")
	}
	sn := kubectlSnapshotOf(cli.Kubectl)
	sn.ComponentID, sn.OwnerSubject = cli.ComponentID, runIdentitySubject(ctx, run.CreatedBy)
	scope := map[string]any{
		"host": host, "header": "Authorization", "format": "Bearer %s",
		"secret_name": kubectlTokenSecretName(cli.ComponentID),
		// TLS only and unpinned: the door, not the rule, decides which requests carry the token.
		"require_tls": true, "snapshot": sn,
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		return refuse("could not author kubectl's credential injection")
	}
	grantID := uuid.New()
	if _, err := s.createDispatchGrant(ctx, run, types.CredentialGrant{
		ID: grantID, RunID: run.ID, CreatedAt: time.Now(),
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: raw, TTLSeconds: kubectlCLIGrantTTLSec},
	}, false); err != nil {
		return refuse(loggedMsg(ctx, "could not record kubectl's credential grant", err))
	}
	rule, err := injectionRuleFromScope(raw)
	if err != nil {
		return refuse(fmt.Sprintf("could not compile kubectl's credential injection: %v", err))
	}
	injections = append(injections, runner.InjectionGrant{GrantID: grantID, Rule: rule})
	sandboxEnv[kubectlCLIConfigEnvVar] = encodeArtifactConfig(kubectlCLIFiles(cli.Kubectl, caCertPEM))
	opened := unionAllowedDomains(policy, []string{net.JoinHostPort(host, strconv.Itoa(port))})
	return injections, kubectlCLIPlan{opened: opened, gate: &proxy.KubeAPIGrant{
		Host: host, Port: port, CAPEM: cli.Kubectl.CAPEM, Namespaces: slices.Clone(cli.Kubectl.Namespaces),
		Write: cli.Kubectl.Access == types.ComponentAccessWrite,
	}}, true
}

// kubectlCLIDrift names the first thing that moved between the dispatch-time
// snapshot and now ("" = nothing): the switch was turned off, the run no longer
// carries the row, the row is gone or no longer a kubectl row, or it names
// another cluster, CA, account, namespaces or access. A store that cannot
// answer is an error, never a drift.
func (s *Server) kubectlCLIDrift(ctx context.Context, runID uuid.UUID, sn kubectlGrantSnapshot) (string, *types.CLIKubectl, error) {
	if !kubectlCLIOn() {
		return "switched_off", nil, nil
	}
	cs, ok := s.cfg.Store.(store.ComponentStore)
	id, perr := uuid.Parse(sn.ComponentID)
	if !ok || perr != nil {
		return "component", nil, nil
	}
	attached, err := cs.ListRunComponents(ctx, runID)
	if err != nil {
		return "", nil, err
	}
	if !slices.ContainsFunc(attached, func(rc types.RunComponent) bool {
		return !rc.SelfDefined && rc.ComponentID != nil && *rc.ComponentID == id
	}) {
		return "component", nil, nil
	}
	row, err := cs.GetComponent(ctx, id, "")
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "row_withdrawn", nil, nil
	case err != nil:
		return "", nil, err
	}
	k := cliKubectlOf(row.Definition)
	if k == nil {
		return "row_withdrawn", nil, nil
	}
	live := kubectlSnapshotOf(*k)
	switch {
	case live.Server != sn.Server || live.CASHA256 != sn.CASHA256:
		return "cluster", nil, nil
	case live.ServiceAccount != sn.ServiceAccount:
		return "service_account", nil, nil
	case !slices.Equal(live.Namespaces, sn.Namespaces):
		return "namespaces", nil, nil
	case live.Access != sn.Access:
		return "access", nil, nil
	}
	return "", k, nil
}
