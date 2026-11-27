// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package spire is the spire identity provider (experimental, off by default, Kubernetes only). A SPIRE
// Server signs each run token as a JWT-SVID for the run's SPIFFE ID. That token can carry only sub, aud
// and exp, so wardynd writes the delegation chain (subject, sponsor, operator_owned) to a binding row
// keyed by the token's jti and reads it back at Verify. At a run's first mint wardynd also registers one
// SPIRE entry, so that only the run's proxy, matched by namespace, service account, component and run,
// is attested for the run's X.509-SVID.
package spire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/svidsource"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	providerName = "spire"
	// spireBootWait bounds the constructor's three waits: the first X.509-SVID, the JWT bundle, and the
	// server's entry list.
	spireBootWait = 10 * time.Second
	// entryX509SVIDTTL is a run entry's X.509-SVID lifetime: how long a killed run's certificate can
	// outlive its entry, since certificates are not revoked.
	entryX509SVIDTTL = 10 * time.Minute
	// maxSPIFFEIDLength caps a run's SPIFFE ID, as embedded does.
	maxSPIFFEIDLength = 2048
	// serviceAccountNamespaceFile is wardynd's own namespace, from its service-account projection.
	serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// ErrCloudSTSNotBuilt is CheckGrants' refusal.
var ErrCloudSTSNotBuilt = errors.New("spire identity: cloud_sts federation is not built")

// Provider is the spire identity.Provider.
type Provider struct {
	td          spiffeid.TrustDomain
	cfg         config
	server      Server
	source      svidsource.Source
	bindings    identity.TokenBindingStore
	revocations identity.RevocationStore
	rec         audit.Recorder
	now         func() time.Time
}

var (
	_ identity.Provider          = (*Provider)(nil)
	_ identity.WorkloadRunMinter = (*Provider)(nil)
	_ svidsource.Holder          = (*Provider)(nil)
)

// seams are what the constructor reaches outside the process through, so tests can stand in for them.
type seams struct {
	getenv        func(string) string
	readNamespace func() (string, error)
	open          func(ctx context.Context, addr string, want spiffeid.ID) (svidsource.Source, error)
	dial          func(addr string, src svidsource.Source, td spiffeid.TrustDomain) (Server, error)
	now           func() time.Time
}

func init() {
	identity.Register(providerName, func(d identity.Deps) (identity.Provider, error) {
		return construct(d, seams{getenv: os.Getenv, readNamespace: readNamespaceFile, open: svidsource.Open, dial: dialGRPC, now: time.Now})
	})
}

func readNamespaceFile() (string, error) {
	b, err := os.ReadFile(serviceAccountNamespaceFile)
	return strings.TrimSpace(string(b)), err
}

// construct checks the config, then, all under one spireBootWait child of d.BootCtx, waits for wardynd's
// control-plane X.509-SVID and the trust domain's JWT bundle, and checks wardynd's own admin entry. Any
// failure refuses: wardynd never serves with a provider it could not check.
func construct(d identity.Deps, s seams) (_ identity.Provider, err error) {
	if d.Revocations == nil || d.Audit == nil || d.Bindings == nil {
		return nil, errors.New("spire identity: the revocation store, audit recorder and token binding store are required")
	}
	td, err := spiffeid.TrustDomainFromString(d.TrustDomain)
	if err != nil {
		return nil, fmt.Errorf("spire identity: trust domain %q: %w", d.TrustDomain, err)
	}
	cfg, err := loadConfig(s.getenv, td)
	if err != nil {
		return nil, err
	}
	namespace, err := s.readNamespace()
	if err != nil || namespace == "" {
		return nil, fmt.Errorf("spire identity: read wardynd's namespace from %s: %v", serviceAccountNamespaceFile, err)
	}
	parent := d.BootCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, spireBootWait)
	defer cancel()

	src, srv, err := connect(ctx, td, cfg, s)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, srv.Close(), src.Close())
		}
	}()
	if err := checkAdminEntry(ctx, td, cfg, srv, namespace); err != nil {
		return nil, err
	}
	return &Provider{td: td, cfg: cfg, server: srv, source: src, bindings: d.Bindings,
		revocations: d.Revocations, rec: d.Audit, now: s.now}, nil
}

// connect opens wardynd's control-plane source (first X.509-SVID and bundles), checks the JWT bundle, and
// dials the server.
func connect(ctx context.Context, td spiffeid.TrustDomain, cfg config, s seams) (svidsource.Source, Server, error) {
	cp, err := spiffeid.FromPath(td, identity.ControlPlanePath)
	if err != nil {
		return nil, nil, err
	}
	src, err := s.open(ctx, cfg.agentSocket, cp)
	if err != nil {
		return nil, nil, waitErr("the Workload API's first X.509-SVID for "+cp.String(), envAgentSocket, cfg.agentSocket, err)
	}
	if b, err := src.GetJWTBundleForTrustDomain(td); err != nil || b.Empty() {
		return nil, nil, errors.Join(fmt.Errorf("spire identity: trust domain %s has no JWT bundle (%s=%s): %v",
			td, envAgentSocket, cfg.agentSocket, err), src.Close())
	}
	srv, err := s.dial(cfg.serverAddress, src, td)
	if err != nil {
		return nil, nil, errors.Join(err, src.Close())
	}
	return src, srv, nil
}

func checkAdminEntry(ctx context.Context, td spiffeid.TrustDomain, cfg config, srv Server, namespace string) error {
	cp, err := spiffeid.FromPath(td, identity.ControlPlanePath)
	if err != nil {
		return err
	}
	entries, err := srv.ListEntriesByID(ctx, cp)
	if err != nil {
		return waitErr("the server's entry list for "+cp.String(), envServerAddress, cfg.serverAddress, err)
	}
	return checkControlPlaneEntries(entries, namespace, cfg.podSA)
}

func waitErr(step, name, value string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("spire identity: %s did not answer within %s (%s=%s)", step, spireBootWait, name, value)
	}
	return fmt.Errorf("spire identity: %s (%s=%s): %w", step, name, value, err)
}

// Name implements identity.Provider.
func (p *Provider) Name() string { return providerName }

// SVIDSource implements svidsource.Holder: wardynd's control-plane SVID and the trust bundle.
func (p *Provider) SVIDSource() svidsource.Source { return p.source }

// CheckGrants refuses cloud_sts, which is not built.
func (p *Provider) CheckGrants(grants []types.GrantSpec) error {
	for _, g := range grants {
		if g.Kind == types.GrantCloudSTS {
			return ErrCloudSTSNotBuilt
		}
	}
	return nil
}

func (p *Provider) spiffeIDForRun(w identity.Workload, runID uuid.UUID) (spiffeid.ID, error) {
	path, err := identity.RunPath(w, runID)
	var id spiffeid.ID
	if err == nil {
		id, err = spiffeid.FromPath(p.td, path)
	}
	if err == nil && len(id.String()) > maxSPIFFEIDLength {
		err = fmt.Errorf("it is longer than %d bytes", maxSPIFFEIDLength)
	}
	if err != nil {
		return spiffeid.ID{}, fmt.Errorf("spire identity: build spiffe id for run %s: %w", runID, err)
	}
	return id, nil
}

// audit records an attribution event; a dropped write is logged, never returned.
func (p *Provider) audit(ctx context.Context, runID uuid.UUID, actor, action, jti, outcome string) {
	run := runID
	ev := types.AuditEvent{ID: uuid.New(), Time: p.now(), RunID: &run, ActorType: types.ActorSystem,
		Actor: actor, Action: action, Target: jti, Outcome: outcome}
	if err := p.rec.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}
}
