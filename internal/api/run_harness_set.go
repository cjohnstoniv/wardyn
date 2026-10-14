// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The harness set (0.9 New Run): the harnesses a run-mode request includes, the
// image that carries their CLIs, and the doors every one of them meets. An
// older client's request has no set and is untouched here.
//
// The image comes from the set, never from the primary harness alone:
//
//	no CLI harness  -> agentImage("base"): the agent base image, no vendor CLI
//	one harness     -> its own image when that carries the CLI, else the
//	                   smallest declared set image that does
//	two or more     -> the smallest operator-declared set image carrying them
//	                   all (ties by key), never a custom image
//
// An operator declares a set image in WARDYN_AGENT_IMAGES under a key that
// joins two or more harness ids with "+" in sorted order, for example
// "claude-code+codex-cli". includedToolIDRE has no "+", so a set key never
// collides with an agent id.

// noHarnessCLI is "Your own tools": a harness with no vendor CLI, which runs on
// the agent base image like a run with no harness.
const noHarnessCLI = "none"

// harnessSetSeparator joins a declared set image's harness ids.
const harnessSetSeparator = "+"

// wardynAgentBaseRepos are Wardyn's own agent base image, which ships no vendor
// CLI: install.sh maps claude-code to it, and so does the catalogue fallback.
var wardynAgentBaseRepos = []string{"ghcr.io/cjohnstoniv/agent-base", "wardyn/agent-base"}

func isWardynAgentBase(ref string) bool {
	return slices.ContainsFunc(wardynAgentBaseRepos, func(repo string) bool {
		return ref == repo || strings.HasPrefix(ref, repo+":") || strings.HasPrefix(ref, repo+"@")
	})
}

// startsHarness reports whether id is the harness the run starts: an
// interactive harness startup's tool, or a background agent task's agent.
func startsHarness(req createRunRequest, id string) bool {
	if s := req.Startup; s != nil && s.Kind == client.StartupHarness {
		return s.Tool == id
	}
	return req.Workload != nil && req.Workload.Kind == client.WorkloadAgentTask && req.Workload.Agent == id
}

// harnessSet is a run-mode request's harnesses in tools[] order, each with the
// model provider it asked for; a background agent task that lists no tool
// includes its workload.agent. Nil for an older client's request.
func harnessSet(req createRunRequest) []types.RunHarness {
	if !req.UsesRunMode() {
		return nil
	}
	var set []types.RunHarness
	for _, t := range req.Tools {
		if t.Kind == client.IncludedToolHarness {
			set = append(set, types.RunHarness{Harness: t.ID, ModelProviderID: t.ModelProvider, Starts: startsHarness(req, t.ID)})
		}
	}
	if w := req.Workload; len(set) == 0 && w != nil && w.Kind == client.WorkloadAgentTask {
		set = append(set, types.RunHarness{Harness: w.Agent, Starts: true})
	}
	return set
}

// primaryHarness is the harness the legacy fields project (D1): the startup
// harness, else the first included harness.
func primaryHarness(req createRunRequest) (client.IncludedTool, bool) {
	isHarness := func(t client.IncludedTool) bool { return t.Kind == client.IncludedToolHarness }
	if s := req.Startup; s != nil && s.Kind == client.StartupHarness {
		isHarness = func(t client.IncludedTool) bool { return t.Kind == client.IncludedToolHarness && t.ID == s.Tool }
	}
	i := slices.IndexFunc(req.Tools, isHarness)
	if i < 0 {
		return client.IncludedTool{}, false
	}
	return req.Tools[i], true
}

// harnessRefusalTool is the tools[].id a refusal of req's primary harness is
// about: the projected agent of a run-mode request, "" for an older client's.
func harnessRefusalTool(req createRunRequest) string {
	if req.UsesRunMode() {
		return req.Agent
	}
	return ""
}

// forTool marks a refusal as about included tool id: the body's `tool`, and the
// authz.denied row's detail. A server error names no tool.
func (f *runRefusal) forTool(id string) *runRefusal {
	if f == nil || id == "" || f.err != nil || f.drive != nil {
		return f
	}
	if f.decision != nil {
		d := f.decision.With("tool", id)
		f.decision = &d
	}
	if f.status != 0 && f.status < http.StatusInternalServerError {
		f.body.Tool = id
	}
	return f
}

// declaredHarnessSets is every set image's harness ids, sorted by key.
func declaredHarnessSets(images map[string]string) [][]string {
	var sets [][]string
	for _, key := range sortedKeys(images) {
		if strings.Contains(key, harnessSetSeparator) {
			sets = append(sets, strings.Split(key, harnessSetSeparator))
		}
	}
	return sets
}

// ValidateHarnessSetImages checks WARDYN_AGENT_IMAGES' set keys at boot: two
// or more valid harness ids joined by "+", each once, in sorted order, naming
// an image.
func ValidateHarnessSetImages(images map[string]string) error {
	for _, key := range sortedKeys(images) {
		if !strings.Contains(key, harnessSetSeparator) {
			continue
		}
		ids := strings.Split(key, harnessSetSeparator)
		valid := len(ids) >= 2 && strings.TrimSpace(images[key]) != "" && slices.IsSorted(ids) &&
			len(slices.Compact(slices.Clone(ids))) == len(ids) && !slices.ContainsFunc(ids, func(id string) bool { return !includedToolIDRE.MatchString(id) })
		if !valid {
			return fmt.Errorf("WARDYN_AGENT_IMAGES: %q is not a harness-set key: join two or more harness ids with +, each once and in sorted order, and name an image", key)
		}
	}
	return nil
}

// harnessImageCarries reports whether the image a run of id alone starts from
// carries its CLI: an operator image for it that is not Wardyn's agent base, or
// Wardyn's own published image for it. "Your own tools" needs no CLI.
func harnessImageCarries(id string, images map[string]string) bool {
	if id == noHarnessCLI {
		return true
	}
	if ref, ok := images[id]; ok {
		return !isWardynAgentBase(ref)
	}
	def, ok := harnessByID(id)
	return ok && def.PublishedCLIImage
}

// harnessInstalled is the setup status's `installed` fact and the
// harness_not_installed gate: some sandbox image this server launches carries
// id's CLI, either alone or in a declared set image. It is never guessed: a
// harness whose only image is Wardyn's agent base, or whose image does not
// resolve, is not installed.
func harnessInstalled(id string, images map[string]string) bool {
	return harnessImageCarries(id, images) ||
		slices.ContainsFunc(declaredHarnessSets(images), func(set []string) bool { return slices.Contains(set, id) })
}

// harnessCLIs is the set's harnesses that need a CLI in the image.
func harnessCLIs(set []types.RunHarness) []string {
	var ids []string
	for _, h := range set {
		if h.Harness != noHarnessCLI {
			ids = append(ids, h.Harness)
		}
	}
	return ids
}

// smallestSetImage is the declared set image with the fewest harnesses that
// carries every id; ties go to the first key in sorted order.
func smallestSetImage(ids []string, images map[string]string) (string, bool) {
	best := ""
	bestSize := 0
	for _, set := range declaredHarnessSets(images) {
		if (bestSize == 0 || len(set) < bestSize) && !slices.ContainsFunc(ids, func(id string) bool { return !slices.Contains(set, id) }) {
			best, bestSize = strings.Join(set, harnessSetSeparator), len(set)
		}
	}
	return best, bestSize > 0
}

// harnessSetImage is the image a run-mode run starts from when no custom image
// applies (see the file comment). custom: the request names its own image, a
// build or a workspace base image, which keeps today's precedence for one
// harness and is refused for two or more, since nothing knows which CLIs it holds.
func harnessSetImage(set []types.RunHarness, images map[string]string, custom bool) (client.ImageFact, *runRefusal) {
	ids := harnessCLIs(set)
	switch {
	case len(ids) == 0:
		return client.ImageFact{Ref: agentImage("base", images), Source: client.ImageSource{Kind: client.ImageSourceBase}}, nil
	case len(ids) == 1 && (custom || harnessImageCarries(ids[0], images)):
		return client.ImageFact{Ref: agentImage(ids[0], images), Source: client.ImageSource{Kind: client.ImageSourceAgent, Name: ids[0]}}, nil
	case len(ids) > 1 && custom:
		return client.ImageFact{}, runError(http.StatusUnprocessableEntity, reasonHarnessSetCustomImage, harnessSetCustomImageMsg()).forTool(ids[1])
	}
	key, ok := smallestSetImage(ids, images)
	switch {
	case ok:
		return client.ImageFact{Ref: images[key], Source: client.ImageSource{Kind: client.ImageSourceHarnessSet, Name: key}}, nil
	case len(ids) == 1:
		// Not installed: a run-mode request was refused before this (harnessInstalled).
		return client.ImageFact{Ref: agentImage(ids[0], images), Source: client.ImageSource{Kind: client.ImageSourceAgent, Name: ids[0]}}, nil
	}
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = harnessName(id)
	}
	return client.ImageFact{}, runError(http.StatusUnprocessableEntity, reasonHarnessSetNoImage, harnessSetNoImageMsg(names...)).forTool(ids[len(ids)-1])
}

// harnessName is a harness's display name, or its id when the catalogue has none.
func harnessName(id string) string {
	if def, ok := harnessByID(id); ok {
		return def.Display
	}
	return id
}

// customImageRequested reports whether the run names its own image: an image,
// a devcontainer build, an allowed image, or a workspace's chosen base image.
func customImageRequested(req createRunRequest, wsRefs []types.Workspace) bool {
	if req.Image != "" || req.DevcontainerRepo != "" || req.AllowedImage != "" {
		return true
	}
	return slices.ContainsFunc(wsRefs, func(ws types.Workspace) bool {
		return ws.BaseImage != nil && ws.BaseImage.Kind != "recommended" && strings.TrimSpace(ws.BaseImage.Image) != ""
	})
}

// workspaceBuildsImage reports whether the primary of wsRefs would build the run's image at launch
// (resolveCreateRunImage): a scanned workspace, with an image builder wired, that chose no base image
// (a chosen one is customImageRequested's). A workspace with no profile, or no builder, leaves the
// run on its set image.
func (s *Server) workspaceBuildsImage(wsRefs []types.Workspace) bool {
	if len(wsRefs) == 0 || s.cfg.ImageBuilder == nil {
		return false
	}
	_, scanned := workspaceProfile(wsRefs[0])
	return scanned
}

// harnessSetRefusalFor holds every included harness to the doors the primary
// meets before the fold (the agent capability and the org roster), refuses a
// harness no image here carries (a run-mode request only; an older client is
// unchanged), and resolves the image that carries the set. image is nil when
// a custom image or a workspace decides it instead.
func (s *Server) harnessSetRefusalFor(r *http.Request, req createRunRequest, wsRefs []types.Workspace) ([]types.RunHarness, *client.ImageFact, *runRefusal) {
	if !req.UsesRunMode() {
		return nil, nil, nil
	}
	set := harnessSet(req)
	// A run that names its own image is not known to lack a CLI: the installed
	// gate reads only the images this server declares.
	custom := customImageRequested(req, wsRefs)
	for _, h := range set {
		if refusal := s.runCapabilityRefusal(r, capAgent, h.Harness, "runs.tools",
			"you are not granted agent "+h.Harness+" — ask an admin to grant it, or remove it from the run"); refusal != nil {
			return nil, nil, refusal.forTool(h.Harness)
		}
		msg, err := s.agentRosterRefusal(r.Context(), h.Harness)
		if err != nil {
			return nil, nil, runServerError("get site config", err)
		}
		if msg != "" {
			return nil, nil, runError(http.StatusUnprocessableEntity, reasonAgentNotEnabled, msg).forTool(h.Harness)
		}
		if !custom && !harnessInstalled(h.Harness, s.cfg.AgentImages) {
			return nil, nil, runError(http.StatusUnprocessableEntity, reasonHarnessNotInstalled, harnessNotInstalledMsg(harnessName(h.Harness))).forTool(h.Harness)
		}
	}
	// Two or more harnesses never run on a custom image, and a scanned workspace that builds one
	// (its repo's devcontainer or a generated profile) replaces the set image at launch with an
	// image that carries at most one CLI.
	if !custom && len(harnessCLIs(set)) > 1 && s.workspaceBuildsImage(wsRefs) {
		custom = true
	}
	image, refusal := harnessSetImage(set, s.cfg.AgentImages, custom)
	if refusal != nil {
		return nil, nil, refusal
	}
	if custom || len(wsRefs) > 0 {
		return set, nil, nil
	}
	return set, &image, nil
}

// harnessProvidersConflict is the one-lane-per-harness invariant (D5) over a
// run's choices: the second harness of the first pair that breaks it
// (harnessProviderPairsConflict).
func harnessProvidersConflict(choices []harnessChoice) (first, second string, conflict bool) {
	for j, b := range choices {
		for _, a := range choices[:j] {
			if a.choice.chosen && b.choice.chosen && harnessProviderPairsConflict(a.harness, a.choice.provider, b.harness, b.choice.provider) {
				return a.harness, b.harness, true
			}
		}
	}
	return "", "", false
}

// harnessProviderNeedsPrimary: an AWS sign-in's sink and its renewal hold
// resolve the run's own provider alone (providerSSOScopeAt, reauthScopeForRun),
// so on any other harness of the set it would launch with no working
// credential. Create and dispatch both refuse it there.
func harnessProviderNeedsPrimary(p types.ModelProvider) bool {
	return p.Kind == types.ModelProviderBedrockSSO
}

// harnessProviderPairsConflict is the one predicate create and dispatch share
// for "harness a on provider pa and harness b on provider pb cannot both be
// credentialed in one sandbox". The proxy injects by destination host and
// cannot tell two harnesses' requests to one host apart, so a host both reach
// conflicts unless the two would author the same grant: one key provider's
// key, to one host, sent the same way. A run carries at most one Azure
// Foundry lane, so a second one always conflicts.
func harnessProviderPairsConflict(a string, pa types.ModelProvider, b string, pb types.ModelProvider) bool {
	ka, keyA := providerKeyLaneFor(pa, a)
	kb, keyB := providerKeyLaneFor(pb, b)
	if keyA && keyB && pa.ID == pb.ID && pa.UID == pb.UID && hostEqual(ka.host, kb.host) && ka.header == kb.header && ka.format == kb.format {
		return false
	}
	if pa.Kind == types.ModelProviderAzureFoundry && pb.Kind == types.ModelProviderAzureFoundry {
		return true
	}
	return slices.ContainsFunc(providerInjectionHosts(pa, a), func(h string) bool {
		return slices.ContainsFunc(providerInjectionHosts(pb, b), func(x string) bool { return hostEqual(x, h) })
	})
}

// providerInjectionHosts is every host the proxy injects p's credential on
// for harness, whatever the kind: the key or endpoint host, the Azure Foundry
// endpoint, a Claude subscription's host (and its gateway when it routes
// through one), Bedrock's runtime and control hosts, and an AWS sign-in's
// portal host. A provider's sign-in is stored only for its own region
// (storeProviderSignIn), so the portal is known before dispatch; the endpoint
// override is a test seam this predicate does not see.
func providerInjectionHosts(p types.ModelProvider, harness string) []string {
	var hosts []string
	if lane, ok := providerKeyLaneFor(p, harness); ok {
		hosts = append(hosts, strings.ToLower(lane.host))
	}
	if lane, ok := providerAzureLaneFor(p, harness); ok {
		hosts = append(hosts, lane.host)
	}
	switch {
	case !p.Serves(harness):
	case p.Kind == types.ModelProviderAnthropicSubscription:
		hosts = append(hosts, strings.ToLower(gatewayHost(providerSubscriptionBase(p))))
		if p.BaseURL != "" { // the grant author also drops the vendor host's injections
			hosts = append(hosts, subscriptionInjectionHost)
		}
	case p.Kind.IsBedrock():
		region := providerBedrockSettings(p).Region
		hosts = append(hosts, strings.ToLower(providerBedrockRuntimeHost(p)), bedrockControlHost(region))
		if p.Kind == types.ModelProviderBedrockSSO {
			hosts = append(hosts, ssoPortalHost(region, ""))
		}
	}
	return hosts
}

// harnessChoice is one included harness's model-provider choice.
type harnessChoice struct {
	harness string
	choice  runProviderChoice
}

// harnessSetFact is the dry doors' harness_set fact: each harness, whether it
// starts, and its model credential. Nil for a run with no harness.
func harnessSetFact(set []types.RunHarness, choices []harnessChoice) *client.HarnessSetFact {
	if len(set) == 0 {
		return nil
	}
	fact := &client.HarnessSetFact{Harnesses: make([]client.HarnessFact, len(set))}
	credentialed := 0
	for i, h := range set {
		fact.Harnesses[i] = client.HarnessFact{ID: h.Harness, Starts: h.Starts}
		j := slices.IndexFunc(choices, func(c harnessChoice) bool { return c.harness == h.Harness })
		if j < 0 || !choices[j].choice.chosen {
			continue
		}
		mc := choices[j].choice.modelCredential()
		fact.Harnesses[i].ModelCredential = &client.HarnessModelCredential{Provider: mc.Provider, Kind: mc.Kind, Residency: string(mc.Residency)}
		credentialed++
	}
	fact.SharedModelAccess = credentialed >= 2
	return fact
}

// mostResident is the choice whose credential lands deepest: a sandbox-resident
// one over a proxy-injected one, else the primary's (the first).
func mostResident(choices []harnessChoice) runProviderChoice {
	for _, c := range choices {
		if c.choice.chosen && kindResidency(c.choice.provider.Kind) == residencySandbox {
			return c.choice
		}
	}
	return choices[0].choice
}

// persistRunHarnesses writes a run-mode run's harness set, once, right after
// the run row: it is the authorisation record a renew, revive or queued start
// re-checks. A run with no harness writes no rows.
func (s *Server) persistRunHarnesses(ctx context.Context, runID uuid.UUID, set []types.RunHarness) error {
	if len(set) == 0 {
		return nil
	}
	st, ok := s.cfg.Store.(store.RunHarnessStore)
	if !ok {
		return errors.New("api: the store cannot record run harnesses")
	}
	return st.PutRunHarnesses(ctx, runID, set)
}

// stampHarnessSet adds the harness set and the image's source to run.create's
// data. An older client's run carries neither.
func stampHarnessSet(data map[string]any, set []types.RunHarness, image *client.ImageFact) {
	if len(set) > 0 {
		rows := make([]map[string]any, len(set))
		for i, h := range set {
			rows[i] = map[string]any{"id": h.Harness, "model_provider": h.ModelProviderID, "starts": h.Starts}
		}
		data["harnesses"] = rows
	}
	if image != nil {
		data["image_source"] = image.Source
	}
}

// visibleHarnessSets is setup status's harness_sets: each declared combination
// whose every harness is one of harnesses (already narrowed to this caller).
func visibleHarnessSets(images map[string]string, harnesses []SetupHarnessTool) [][]string {
	sets := [][]string{}
	for _, set := range declaredHarnessSets(images) {
		if !slices.ContainsFunc(set, func(id string) bool {
			return !slices.ContainsFunc(harnesses, func(h SetupHarnessTool) bool { return h.ID == id })
		}) {
			sets = append(sets, set)
		}
	}
	return sets
}

// grantHarnessProviders records on each harness of set the provider it was
// granted: its run_harnesses row and run.create's harnesses carry the chosen
// provider, never only the requested one.
func grantHarnessProviders(set []types.RunHarness, choices []harnessChoice) {
	for i, c := range choices {
		if c.choice.chosen {
			set[i].ModelProviderID = c.choice.provider.ID
		}
	}
}
