// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package svidsource opens a SPIFFE Workload API source pinned to one SPIFFE ID: the X.509-SVID it
// serves is always that ID's, never a fallback identity the agent also issues the workload.
package svidsource

import (
	"context"
	"errors"
	"fmt"

	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// Source serves one workload's X.509-SVID and its trust domain's X.509 and JWT bundles.
type Source interface {
	x509svid.Source
	x509bundle.Source
	jwtbundle.Source
	Close() error
}

// Holder is implemented by an identity provider that holds a Source (spire).
type Holder interface {
	SVIDSource() Source
}

// Open connects to the Workload API at addr (empty means SPIFFE_ENDPOINT_SOCKET), waits under ctx for the
// first X.509 update and the first bundle update, and refuses unless the SVID it picks is want's. ctx
// bounds only those first waits; the source keeps watching after it returns.
func Open(ctx context.Context, addr string, want spiffeid.ID) (Source, error) {
	var client []workloadapi.ClientOption
	if addr != "" {
		client = append(client, workloadapi.WithAddr(addr))
	}
	x509, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(client...),
		workloadapi.WithDefaultX509SVIDPicker(pickID(want)))
	if err != nil {
		return nil, fmt.Errorf("svidsource: first X.509-SVID: %w", err)
	}
	bundles, err := workloadapi.NewBundleSource(ctx, workloadapi.WithClientOptions(client...))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("svidsource: first bundle: %w", err), x509.Close())
	}
	s := &source{x509: x509, bundles: bundles, want: want}
	if _, err := s.GetX509SVID(); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return s, nil
}

// pickID picks want's SVID, else the first, which GetX509SVID then refuses.
func pickID(want spiffeid.ID) func([]*x509svid.SVID) *x509svid.SVID {
	return func(svids []*x509svid.SVID) *x509svid.SVID {
		for _, s := range svids {
			if s.ID == want {
				return s
			}
		}
		return svids[0]
	}
}

type source struct {
	x509    *workloadapi.X509Source
	bundles *workloadapi.BundleSource
	want    spiffeid.ID
}

// GetX509SVID errors if the picked SVID is not want's, at Open and on every later rotation.
func (s *source) GetX509SVID() (*x509svid.SVID, error) {
	svid, err := s.x509.GetX509SVID()
	if err != nil {
		return nil, err
	}
	if svid.ID != s.want {
		return nil, fmt.Errorf("svidsource: the Workload API serves %s, not %s", svid.ID, s.want)
	}
	return svid, nil
}

func (s *source) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return s.bundles.GetX509BundleForTrustDomain(td)
}

func (s *source) GetJWTBundleForTrustDomain(td spiffeid.TrustDomain) (*jwtbundle.Bundle, error) {
	return s.bundles.GetJWTBundleForTrustDomain(td)
}

func (s *source) Close() error { return errors.Join(s.x509.Close(), s.bundles.Close()) }
