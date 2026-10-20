// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The kubectl injection RESOLVE: the control-plane half of the lane
// authorKubectlCLI authors at dispatch (cli_kubectl.go). Like the other arms,
// a resolve may refresh credential MATERIAL; it never re-authorizes the run
// against another row, cluster, account, namespace set or host than the one it
// was dispatched with.
//
//  1. the grant's own dispatch-time SNAPSHOT must name this row and an owner;
//  2. the snapshot's owner must be the run token's own subject;
//  3. the host the grant would inject onto must be the row's API server;
//  4. the switch must still be on, the run must still carry the row, and the
//     live row must still name the snapshot's cluster, CA, account, namespaces
//     and access;
//  5. read the registered token from the operator namespace (never a person's);
//  6. trade it at the cluster's TokenRequest API for a token of the same
//     account that lives kubectlTokenLifetimeSec, trusting only the row's CA.
//
// Only the minted token leaves this function, and only to the run's proxy.
// WHAT IT DOES NOT CLAIM: the minted token carries the account's whole RBAC on
// the cluster; the proxy's door, with that RBAC, holds a run to its row.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	kubectlResolveNotRecorded = "A kubectl cluster token is injected only through the grant Wardyn authors when a run launches with " +
		"that component; this grant carries no such record"
	kubectlResolveChanged = "This run's kubectl component was removed, changed or switched off after the run started, " +
		"so its cluster token is no longer injected. Relaunch the run."
	kubectlResolveHostPin      = "A kubectl cluster token may only be injected to the component's own API server"
	kubectlResolveUnreadable   = "Wardyn couldn't read this run's kubectl component just now, so its cluster token is not injected"
	kubectlResolveUnregistered = "The kubectl component this run uses has no service account token registered, so nothing was substituted. Ask your admin."
	kubectlResolveMintFailed   = "The cluster refused, or did not answer, Wardyn's request for a short-lived token for this run's kubectl, " +
		"so nothing was substituted. Ask your admin to check the component's service account."
)

// cliKubectlComponentID returns the component id a wardyn-cli-<id>-kubectl name names.
func cliKubectlComponentID(name string) (string, bool) {
	if !types.CLIKubectlTokenSecret(name) {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, cliSecretPrefix), "-"+types.CLIKubectlTokenPart)
	return id, id != ""
}

// resolveKubectlCLIInjection is the kubectl arm of handleInternalInjection.
// handled=false means this grant is not ours.
func (s *Server) resolveKubectlCLIInjection(w http.ResponseWriter, r *http.Request,
	claims *identity.Claims, minted broker.Minted, grantID uuid.UUID,
) bool {
	name := minted.Injection.SecretName
	id, ok := cliKubectlComponentID(name)
	if !ok {
		return false
	}
	ctx := secretstore.WithPurpose(r.Context(), secretstore.PurposeBrokerMint)
	fail := func(status int, reason, body string, extra map[string]any) bool {
		data := map[string]any{"reason": reason, "grant_id": grantID, "source": "cli", "cli": types.CLIKindKubectl, "component_id": id}
		for k, v := range extra {
			data[k] = v
		}
		s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID, "secret.read", name, "failure", mustJSON(data)))
		writeErrorReason(w, status, reason, body)
		return true
	}
	var sn kubectlGrantSnapshot
	if !s.grantSnapshot(ctx, claims.RunID, grantID, &sn) || !sn.authored() || sn.ComponentID != id {
		return fail(http.StatusForbidden, reasonMissingScopeSnapshot, kubectlResolveNotRecorded, nil)
	}
	if sn.OwnerSubject != claims.Sub {
		return fail(http.StatusForbidden, reasonOwnerNotCaller, kubectlResolveChanged, map[string]any{"owner": sn.OwnerSubject})
	}
	k := types.CLIKubectl{Server: sn.Server}
	if host, _, ok := k.HostPort(); !ok || !hostEqual(minted.Injection.Host, host) {
		return fail(http.StatusForbidden, reasonHostNotEndpoint, kubectlResolveHostPin, map[string]any{"host": minted.Injection.Host})
	}
	drift, live, registered, readErr, err := s.kubectlResolveRow(ctx, claims.RunID, sn, name)
	switch {
	case readErr != nil:
		status, reason, _ := storeReadRefusal(name, readErr)
		return fail(status, reason, kubectlResolveUnregistered, nil)
	case err != nil:
		return fail(http.StatusServiceUnavailable, reasonRunUnreadable, kubectlResolveUnreadable, nil)
	case drift != "":
		return fail(http.StatusForbidden, reasonScopeChanged, kubectlResolveChanged, map[string]any{"drift": drift})
	}
	token, expires, err := mintKubectlToken(ctx, *live, string(registered), s.cfg.Now())
	if err != nil {
		return fail(http.StatusBadGateway, reasonSinkResolveFailed, kubectlResolveMintFailed, map[string]any{"error": err.Error()})
	}
	value := formatInjectionValue("Bearer %s", []byte(token))
	if s.refuseUnmasked(w, r, claims, "injection.resolve", []byte(token), []byte(value)) {
		return true
	}
	s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID, "secret.read", name, "success", mustJSON(map[string]any{
		"purpose": "proxy-injection-kubectl-cli", "grant_id": grantID, "jti": minted.JTI, "source": "cli", "cli": types.CLIKindKubectl,
		"owner": sn.OwnerSubject, "component_id": id, "server": sn.Server,
		"service_account": sn.ServiceAccount.Namespace + "/" + sn.ServiceAccount.Name,
		"namespaces":      sn.Namespaces, "access": sn.Access, "token_expires_at": expires.UTC().Format(time.RFC3339),
	})))
	writeJSON(w, http.StatusOK, injectionResponse{
		Host: minted.Injection.Host, Header: "Authorization", Value: value, JTI: minted.JTI,
		ExpiresAt: s.subscriptionLease(minted, subscription.Token{ExpiresAt: expires}),
	})
	return true
}

// kubectlResolvePause is a test seam between the row check and the token read.
var kubectlResolvePause = func() {}

// kubectlResolveRow checks the live row against the snapshot and reads its
// registered token under the lock the row's purge and the token route hold
// (lockCLIRow), so the token read is the one registered for the row checked:
// never one registered for a server, CA or account the row moved to between.
// readErr is the token read's own failure, once the row has held.
func (s *Server) kubectlResolveRow(ctx context.Context, runID uuid.UUID, sn kubectlGrantSnapshot, name string,
) (drift string, live *types.CLIKubectl, registered []byte, readErr, err error) {
	ctx, unlock, err := s.lockCLIRow(ctx, sn.ComponentID)
	if err != nil {
		return "", nil, nil, nil, err
	}
	defer unlock()
	if drift, live, err = s.kubectlCLIDrift(ctx, runID, sn); err != nil || drift != "" {
		return drift, nil, nil, nil, err
	}
	kubectlResolvePause()
	// The operator namespace only: a person's row of the same name is never read.
	if registered, readErr = s.cfg.Secrets.For("").Get(ctx, name); readErr != nil {
		return "", nil, nil, readErr, nil
	}
	return "", live, registered, nil, nil
}

// kubectlMintTimeout bounds one TokenRequest round trip.
const kubectlMintTimeout = 15 * time.Second

// mintKubectlToken asks the row's API server for a token of the row's service
// account that lives kubectlTokenLifetimeSec, authenticating with the
// registered token and trusting only the row's CA bundle (the system roots
// when it has none). It returns the token and when it expires.
func mintKubectlToken(ctx context.Context, k types.CLIKubectl, registered string, now time.Time) (string, time.Time, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if k.CAPEM != "" {
		tlsCfg.RootCAs = x509.NewCertPool()
		if !tlsCfg.RootCAs.AppendCertsFromPEM([]byte(k.CAPEM)) {
			return "", time.Time{}, errors.New("the component's CA bundle holds no certificate")
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: kubectlMintTimeout,
		// A redirect would carry the registered token to wherever it points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	target := strings.TrimSuffix(k.Server, "/") + "/api/v1/namespaces/" + url.PathEscape(k.ServiceAccount.Namespace) +
		"/serviceaccounts/" + url.PathEscape(k.ServiceAccount.Name) + "/token"
	body := mustJSON(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "TokenRequest",
		"spec": map[string]any{"expirationSeconds": kubectlTokenLifetimeSec}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+registered)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		// The cluster's own Status message, never the body whole: it is the
		// cluster's to word, and it carries no credential.
		var st struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &st)
		return "", time.Time{}, fmt.Errorf("token request answered %d: %.200s", resp.StatusCode, st.Message)
	}
	var tr2 struct {
		Status struct {
			Token               string    `json:"token"`
			ExpirationTimestamp time.Time `json:"expirationTimestamp"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &tr2); err != nil || tr2.Status.Token == "" {
		return "", time.Time{}, errors.New("token request answered without a token")
	}
	exp := tr2.Status.ExpirationTimestamp
	if exp.IsZero() || !exp.After(now) {
		return "", time.Time{}, errors.New("token request answered with a token that has no future expiry")
	}
	return tr2.Status.Token, exp, nil
}
