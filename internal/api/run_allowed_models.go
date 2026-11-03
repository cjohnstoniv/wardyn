// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The allowed_models limit (GovernanceLimits.AllowedModels), outside the composer: what binds a run,
// which model routes the run's proxy can check, the dispatch's share of the proxy config, and the run's
// record of the list.
//
// Who is bound is the effective ceiling's answer and nothing else: a run whose caller stands outside
// governance (runUngoverned) resolves no profile, so no list, and an admin governed under
// GovernAdminRuns resolves theirs like a member. The admin role is never read here.

// reasonAllowedModelsUnchecked refuses a run whose model calls the run's proxy could not hold to the
// allowed-models list, before it starts.
const reasonAllowedModelsUnchecked = "allowed_models_unchecked"

// allowedModelsUncheckedMsg is that refusal's sentence; %s is the provider's name.
const allowedModelsUncheckedMsg = "Your organisation limits the models runs may use, and Wardyn can't check the models %s is asked for. Choose another model provider."

// allowedModelsLimit is the list a run is held to and the profile that set it. The zero value is no
// limit; a set limit always holds at least one model, since an empty list is no limit.
type allowedModelsLimit struct {
	models    []string
	profileID uuid.UUID
	profile   string
}

// set reports whether the limit binds the run.
func (l allowedModelsLimit) set() bool { return len(l.models) > 0 }

// modelLimitOf is the limit ceiling c puts on a run: its assigned profile's effective list, or none
// for a caller with no profile (an operator's ceiling has none: effectiveCeiling resolves no profile
// for a run outside governance).
func modelLimitOf(c governanceCeiling) allowedModelsLimit {
	if c.Unit == nil || len(c.Limits.AllowedModels) == 0 {
		return allowedModelsLimit{}
	}
	return allowedModelsLimit{models: slices.Clone(c.Limits.AllowedModels), profileID: c.Unit.ID, profile: c.Unit.Name}
}

// proxyList is the list as the run's proxy config carries it: nil, so no key is written, when no limit
// binds the run. Never an empty list, which the proxy reads as every model refused.
func (l allowedModelsLimit) proxyList() *[]string {
	if !l.set() {
		return nil
	}
	list := slices.Clone(l.models)
	return &list
}

// providerModelsUnchecked reports whether the run's proxy could not hold a run on provider p to the
// list: an AWS sign-in whose requests the sandbox signs itself. The proxy cannot read inside them
// without breaking the signature, so it would refuse every one as an opaque connection; the doors
// refuse the run first. Re-signed at the proxy (WARDYN_AWS_SIGV4_AT_PROXY), the proxy terminates them
// and checks each. A local model is held to its own allowlist and is not a hosted provider.
func (s *Server) providerModelsUnchecked(p types.ModelProvider) bool {
	if p.Kind != types.ModelProviderBedrockSSO {
		return false
	}
	_, resigned := s.awsSigV4Hosts(providerBedrockHosts(p))
	return !resigned
}

// laneModelsUnchecked is providerModelsUnchecked for a dispatched lane: a Bedrock lane whose requests
// the sandbox signs, with no bearer the proxy injects and no re-signing at the proxy.
func laneModelsUnchecked(l llmTransport) bool {
	return l.bedrockReady && !l.injectBedrockBearer && !l.signsBedrock()
}

// uncheckedModelProvider is the first chosen provider of choices whose model calls the proxy could not
// check, or false.
func (s *Server) uncheckedModelProvider(choices []harnessChoice) (types.ModelProvider, bool) {
	for _, c := range choices {
		if c.choice.chosen && s.providerModelsUnchecked(c.choice.provider) {
			return c.choice.provider, true
		}
	}
	return types.ModelProvider{}, false
}

// stepAllowedModelsChecked refuses, at every door, a run held to an allowed-models list whose chosen
// provider's model calls the proxy could not check (providerModelsUnchecked). Dispatch refuses the
// same lane again (resolveLLMInjections), for a run that reached it another way.
func (s *Server) stepAllowedModelsChecked(f *runFold, _ *foldRecorder) bool {
	if !modelLimitOf(f.ceiling).set() {
		return true
	}
	choices := append([]harnessChoice{{choice: f.mpChoice}}, f.harnessChoices...)
	p, unchecked := s.uncheckedModelProvider(choices)
	if !unchecked {
		return true
	}
	writeErrorReason(f.w, http.StatusForbidden, reasonAllowedModelsUnchecked, fmt.Sprintf(allowedModelsUncheckedMsg, cmp.Or(p.Name, p.ID)))
	return false
}

// refuseUncheckedModelLane fails a run held to a list whose lane the proxy could not check, and
// reports whether it did. ok=true: the run is already FAILED.
func (s *Server) refuseUncheckedModelLane(ctx context.Context, run types.AgentRun, limit allowedModelsLimit, llms []llmTransport) bool {
	if !limit.set() {
		return false
	}
	i := slices.IndexFunc(llms, laneModelsUnchecked)
	if i < 0 {
		return false
	}
	var kind types.ModelProviderKind
	name := run.ModelProviderID
	if c := llms[i].provider; c != nil {
		kind, name = c.provider.Kind, cmp.Or(c.provider.Name, c.provider.ID)
	}
	s.refuseProviderDispatch(ctx, run, kind, providerDenial{msg: fmt.Sprintf(allowedModelsUncheckedMsg, name)},
		map[string]any{"reason": reasonAllowedModelsUnchecked})
	return true
}

// modelGatewayMITMHosts are the "host:port" interception entries a run held to a list needs for the
// gateways its key lanes reach (a provider's https BaseURL): a sandbox's direct connection to one is then
// read and checked, where an opaque one is refused. Local model servers are left out (their own
// allowlist holds them), as is any host another lane already intercepts (have).
func modelGatewayMITMHosts(upstreams map[string]string, local map[string]types.LocalModelSettings, have []string) []string {
	bare := func(entry string) string {
		if h, _, err := net.SplitHostPort(entry); err == nil {
			entry = h
		}
		return strings.ToLower(strings.TrimSuffix(strings.Trim(entry, "[]"), "."))
	}
	seen := map[string]bool{}
	for _, h := range have {
		seen[bare(h)] = true
	}
	var out []string
	for _, base := range upstreams {
		if _, isLocal := local[base]; isLocal {
			continue
		}
		u, err := url.Parse(base)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			continue
		}
		entry := gatewayHostPort(base)
		if entry == "" || seen[bare(entry)] {
			continue
		}
		seen[bare(entry)] = true
		out = append(out, entry)
	}
	slices.Sort(out)
	return out
}

// recordAllowedModels keeps, on the run, the list its proxy was handed at this dispatch and the profile
// that set it, for the run page. Every dispatch writes it, an empty one clearing it. A store without the
// surface, or a write that fails, costs the run page its card only: the proxy holds the run either way.
func (s *Server) recordAllowedModels(ctx context.Context, runID uuid.UUID, limit allowedModelsLimit) {
	st, ok := s.cfg.Store.(store.RunAllowedModels)
	if !ok {
		return
	}
	var raw []byte
	if limit.set() {
		var err error
		raw, err = json.Marshal(client.RunAllowedModels{Models: limit.models,
			Profile: client.RunAllowedModelsProfile{ID: limit.profileID.String(), Name: limit.profile}})
		if err != nil {
			slog.WarnContext(ctx, "api: could not encode the run's allowed models", "run_id", runID, "error", err)
			return
		}
	}
	if err := st.SetRunAllowedModels(ctx, runID, raw); err != nil {
		slog.WarnContext(ctx, "api: could not record the run's allowed models", "run_id", runID, "error", err)
	}
}

// runAllowedModels is the record GET /runs/{id} serves as allowed_models; nil when no limit binds the
// run or the record cannot be read.
func (s *Server) runAllowedModels(ctx context.Context, id uuid.UUID) *client.RunAllowedModels {
	st, ok := s.cfg.Store.(store.RunAllowedModels)
	if !ok {
		return nil
	}
	raw, err := st.RunAllowedModels(ctx, id)
	if err != nil {
		slog.WarnContext(ctx, "api: could not read the run's allowed models", "run_id", id, "error", err)
		return nil
	}
	if len(raw) == 0 {
		return nil
	}
	var rec client.RunAllowedModels
	if err := json.Unmarshal(raw, &rec); err != nil || len(rec.Models) == 0 {
		slog.WarnContext(ctx, "api: could not decode the run's allowed models", "run_id", id, "error", err)
		return nil
	}
	return &rec
}
