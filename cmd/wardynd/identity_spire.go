// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	// The spire identity provider self-registers from its init(); WARDYN_IDENTITY=spire selects it.
	_ "github.com/cjohnstoniv/wardyn/internal/identity/spire"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// refuseSpireOffKubernetes refuses boot when the spire identity provider is selected with a runner other
// than k8s: this release attests run proxies on Kubernetes only, so Docker and remote runners keep the
// bearer token under the embedded provider.
func refuseSpireOffKubernetes(runnerSel string) error {
	if runnerSel != "k8s" {
		return errors.New("The spire identity provider needs the k8s runner in this release.")
	}
	return nil
}

// newIdentityProvider is identity.New behind refuseSpireOffKubernetes.
func newIdentityProvider(identitySel, runnerSel string, d identity.Deps) (identity.Provider, error) {
	if identitySel == "spire" {
		if err := refuseSpireOffKubernetes(runnerSel); err != nil {
			return nil, err
		}
	}
	return identity.New(identitySel, d)
}

// pgTokenBindings is the store's identity_token_bindings as the identity.TokenBindingStore a provider
// whose token cannot carry its delegation chain (spire) writes and reads.
type pgTokenBindings struct{ pg store.PG }

var _ identity.TokenBindingStore = pgTokenBindings{}

func (b pgTokenBindings) PutBinding(ctx context.Context, x identity.TokenBinding) error {
	return b.pg.PutTokenBinding(ctx, x)
}

func (b pgTokenBindings) GetBinding(ctx context.Context, jti string) (identity.TokenBinding, error) {
	return b.pg.GetTokenBinding(ctx, jti)
}

func (b pgTokenBindings) PruneBindings(ctx context.Context, cutoff time.Time) (int64, error) {
	return b.pg.PruneTokenBindings(ctx, cutoff)
}
