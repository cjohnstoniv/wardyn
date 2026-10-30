// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// listedRunners is ownedRunners that also lists its rows, as setup does.
type listedRunners struct{ ownedRunners }

func (l listedRunners) ListRunners(context.Context) ([]types.Runner, error) {
	return slices.Collect(maps.Values(l.rows)), nil
}

// With WARDYN_AWS_SIGV4_AT_PROXY on and an AWS sign-in provider whose lane is re-signed, setup names
// each claimed runner connected here whose caps do not name aws_sigv4, one row per runner: its runs
// are refused there. A runner that names it, an unclaimed one, one not connected here, the switch off
// and a key-only provider list say nothing.
func TestAWSSigV4RefusedRunnerChecks(t *testing.T) {
	rows := map[uuid.UUID]types.Runner{}
	s := &Server{cfg: Config{AWSSigV4AtProxy: true, AWSSSOProxyInject: true, Store: listedRunners{ownedRunners{rows: rows}}}}
	s.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	install := func(name string, state types.RunnerState, settings []string) uuid.UUID {
		id := uuid.New()
		rows[id] = types.Runner{ID: id, Owner: capsOwner, Name: name, State: state}
		data, err := json.Marshal(runnerwire.Caps{ProxySettings: settings, Version: "test"})
		if err != nil {
			t.Fatal(err)
		}
		sub := remote.New(id.String(), &capsTransport{online: true}, remote.Options{})
		sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
		s.runnerStreams.subs[id] = runnerEndpoint{sub: sub, link: &remote.Link{}}
		return id
	}
	old := install("old-mac", types.RunnerClaimed, nil)
	install("new-mac", types.RunnerClaimed, runnerwire.ProxySettings)
	install("waiting", types.RunnerUnclaimed, nil)
	away := uuid.New()
	rows[away] = types.Runner{ID: away, Owner: capsOwner, Name: "away", State: types.RunnerClaimed}

	sc := awsSSOTestSite()
	mp := []SetupModelProvider{{ID: awsSSOTestProvider().ID, Kind: string(types.ModelProviderBedrockSSO)}}
	want := SetupCheck{ID: "aws_sigv4_refused:" + old.String(), Label: "AWS signing at the proxy: old-mac", Status: "warn",
		Detail: "Runs on old-mac are REFUSED while WARDYN_AWS_SIGV4_AT_PROXY is on: its wardyn-runnerd is too old to sign AWS requests at its proxy.",
		Fix:    "Upgrade wardyn-runnerd on that computer to 0.10 or later."}
	if got := s.awsSigV4RefusedRunnerChecks(capsCtx, sc, mp); len(got) != 1 || got[0] != want {
		t.Fatalf("checks = %+v, want only %+v", got, want)
	}
	key := brBearerProvider()
	keyOnly := types.SiteConfig{ModelProviders: providerBlock(key)}
	if got := s.awsSigV4RefusedRunnerChecks(capsCtx, keyOnly, []SetupModelProvider{{ID: key.ID, Kind: string(key.Kind)}}); len(got) != 0 {
		t.Fatalf("a key provider only: checks = %+v, want none", got)
	}
	s.cfg.AWSSigV4AtProxy = false
	if got := s.awsSigV4RefusedRunnerChecks(capsCtx, sc, mp); len(got) != 0 {
		t.Fatalf("switch off: checks = %+v, want none", got)
	}
}
