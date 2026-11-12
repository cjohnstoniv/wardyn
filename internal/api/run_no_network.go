// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run launched with the built-in No network wall preset (types.WallPresetNoNetwork) gets no network at
// all. Create, Review's preflight and the policy preview refuse it while anything they can see would
// add a host (stepNoNetwork). Whatever only dispatch adds, the seal removes (wallPresetSealLayer); the
// run's proxy refuses every request (proxy.Config.NoNetwork, set at dispatch and replayed at revive);
// and every egress approval for the run is refused (wallsAdmitApproval).

// noNetworkPreset reports whether p is the built-in No network preset.
func noNetworkPreset(p *types.WallPresetOnRun) bool {
	return p != nil && p.Scope == types.WallPresetScopeBuiltin && p.ID == types.WallPresetNoNetwork
}

// noNetworkSkipsCredential is r, marked (withNoCredentialCheck) when the launch has the No network preset.
// The provider is still chosen and judged, but the caller's model credential is read for a chosen provider
// only, and stepNoNetwork refuses every chosen provider, the primary's and each extra harness's alike
// ("model provider ..." below), so for this preset the refusal is certain and never waits on, or tells a
// caller about, the credential. A harness with no chosen provider reads none either way.
func noNetworkSkipsCredential(r *http.Request, preset *types.WallPresetOnRun) *http.Request {
	if noNetworkPreset(preset) {
		return r.WithContext(withNoCredentialCheck(r.Context()))
	}
	return r
}

// stepNoNetwork refuses a No network launch that would need a host (422 no_network_needs_egress), naming
// each source, sorted and once each: the kinds deriveRunEgress adds after the mint (from a copy with an
// empty allowlist, so each kind reports every host it would add; its git grants only when the run clones a
// repository, noNetworkClonesRepo), the repository itself, a direct GitHub clone, each component that needs a host,
// the Azure DevOps sign-in, each harness's model and each chosen model provider, and whatever is still on
// the spec's allowlist, by the source the fold recorded for it. It runs after the tool policy, the last
// step that adds a host to the spec, in every door, so create, Review and the preview answer the same.
func (s *Server) stepNoNetwork(f *runFold, rec *foldRecorder) bool {
	if !noNetworkPreset(f.source.wallPreset) {
		return true
	}
	var out []string
	add := func(src string) {
		if !slices.Contains(out, src) {
			out = append(out, src)
		}
	}
	probe := f.spec.Clone()
	probe.AllowedDomains = nil
	grants := f.spec.EligibleGrants
	if noNetworkClonesRepo(f.req.Repo, f.spec.WorkspaceRepos) {
		// A clone needs the network wherever it comes from, a brokered GitHub one included, which adds no
		// host to the allowlist.
		add("repository")
	} else {
		grants = slices.DeleteFunc(slices.Clone(grants), gitLaneGrant)
	}
	wired := lanesWiredFor(f.scmSite, f.req.Agent, repoLocatorsOf(f.spec.WorkspaceRepos), grants)
	for _, d := range deriveRunEgress(&probe, f.wsRefs, f.req.Repo, f.scmSite, wired, f.ceiling.Spec.AllowedDomains) {
		if len(d.added) > 0 {
			add(noNetworkKind(d.kind))
		}
	}
	if len(f.directGitHubAdded) > 0 {
		add("github_direct")
	}
	hint := ""
	for i, c := range f.comps.attached {
		if noNetworkComponentNeedsHost(c) {
			add(noNetworkComponent(c.snapshot.Name, i))
			hint = " A local server launched through npx, uvx or docker run counts; launching its installed program directly does not."
		}
	}
	if f.adoGrade.org != "" {
		add("azure_devops_sign_in")
	}
	// The primary's model, and each extra harness's (the set stepModelProvider judged), chosen provider or not.
	for _, agent := range append([]string{f.req.Agent}, harnessChoiceIDs(f.harnessChoices)...) {
		if _, needsModel := agentLLMProvider(agent); needsModel && createDoorIsModelRun(*f.req) {
			add("the " + agent + " harness's model")
		}
	}
	if f.mpChoice.chosen {
		add("model provider " + noNetworkProvider(f.mpChoice.provider))
	}
	for _, hc := range f.harnessChoices {
		if hc.choice.chosen {
			add("model provider " + noNetworkProvider(hc.choice.provider))
		}
	}
	noNetworkSpecSources(f.spec, rec, add)
	if len(out) == 0 {
		return true
	}
	slices.Sort(out)
	return !runError(http.StatusUnprocessableEntity, reasonNoNetworkNeedsEgress,
		"This run uses the No network wall preset, and these need a network host: "+strings.Join(out, ", ")+
			". Remove them, or launch without No network."+hint).write(s, f.w, f.r)
}

// harnessChoiceIDs is the harness ids of choices, in the set's order.
func harnessChoiceIDs(choices []harnessChoice) []string {
	ids := make([]string, 0, len(choices))
	for _, hc := range choices {
		ids = append(ids, hc.harness)
	}
	return ids
}

// noNetworkClonesRepo reports whether the run names a repository to clone: the legacy repo field or a
// repository on the spec, the two dispatch clones from (buildRepoRecords). A workspace attached for its
// folders alone (repos: []) keeps its repository sources off the spec and clones none. Without a
// repository, a git grant the policy carries is a source of nothing: the launcher cannot remove it, and
// dispatch withholds its lane and credential from a No network run.
func noNetworkClonesRepo(legacyRepo string, specRepos []types.WorkspaceRepo) bool {
	return strings.TrimSpace(legacyRepo) != "" || len(specRepos) > 0
}

// gitLaneGrant reports whether g opens a git credential lane, the kinds deriveRunEgress counts as
// declaring a repository.
func gitLaneGrant(g types.GrantSpec) bool {
	switch g.Kind {
	case types.GrantGitHubToken, types.GrantGitPAT, types.GrantSSHKey:
		return true
	}
	return false
}

// noNetworkSpecSources adds the source of each host still on spec's allowlist, and allow_all_egress.
func noNetworkSpecSources(spec types.RunPolicySpec, rec *foldRecorder, add func(string)) {
	for _, h := range spec.AllowedDomains {
		add(noNetworkHostSource(rec, h))
	}
	if spec.AllowAllEgress {
		add("allow_all_egress")
	}
}

// noNetworkKind is a derived kind as the refusal names it: a workspace's own hosts and its clone hosts
// are both the workspace's, which is how the dry doors, which add the first before the requirements
// fold, record them too.
func noNetworkKind(kind string) string {
	if kind == derivedWorkspaceClone {
		return derivedWorkspace
	}
	return kind
}

// packageFetchers are the launch commands, by base name, that download the package they run before
// running it: a local MCP server started by one needs the network, wherever the program sits.
var packageFetchers = []string{"npx", "pnpx", "bunx", "uvx", "pipx"}

// fetcherOfflineFlags are, by fetcher, the options that keep it from downloading, so it runs only a package
// already on the machine. fetcherValueFlags are its options whose value is the next argument.
var (
	fetcherOfflineFlags = map[string][]string{"npx": {"--no-install", "--no", "--yes=false", "--offline"}, "uvx": {"--offline"}}
	fetcherValueFlags   = map[string][]string{
		"npx": {"-p", "--package", "-c", "--call"},
		"uvx": {"--from", "--with", "--with-editable", "--with-requirements", "-p", "--python", "--index",
			"--default-index", "--index-url", "--extra-index-url", "-c", "--constraints", "--overrides",
			"--cache-dir", "--directory", "--project", "--config-file"},
	}
)

// fetcherStaysOffline reports whether a package fetcher's options, those before the package it runs, include
// one of its fetcherOfflineFlags. An option after the package is the package's own.
func fetcherStaysOffline(base string, args []string) bool {
	offline, ok := fetcherOfflineFlags[base]
	if !ok {
		return false
	}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case slices.Contains(fetcherValueFlags[base], a):
			i++
		case !strings.HasPrefix(a, "-"):
			return false
		case slices.Contains(offline, a):
			return true
		}
	}
	return false
}

// launchFetches reports whether a local server's launch downloads what it runs: a package fetcher, or one
// behind env or a shell's -c, a container run (docker or podman pulls its image), or a deno run of an npm:,
// jsr: or URL module, or a package manager's fetching subcommand (npm exec, pnpm dlx). A wrapper is looked
// through at most twice.
func launchFetches(cmd string, args []string, depth int) bool {
	switch base := path.Base(cmd); {
	case slices.Contains(packageFetchers, base):
		return !fetcherStaysOffline(base, args)
	case base == "docker" || base == "podman":
		return containerRun(args)
	case base == "deno":
		return denoRunsRemoteEntry(args)
	case subcommandFetches(base, args):
		return true
	case depth >= 2:
		return false
	case base == "env":
		i := firstPositional(args, envValueFlags)
		for i >= 0 && shellAssignment(args[i]) {
			next := firstPositional(args[i+1:], nil)
			if next < 0 {
				return false
			}
			i += 1 + next
		}
		return i >= 0 && launchFetches(args[i], args[i+1:], depth+1)
	case base == "sh" || base == "bash" || base == "dash" || base == "zsh":
		i := slices.IndexFunc(args, func(a string) bool { // -c, or a flag group that holds it (-lc, -ec)
			return strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c")
		})
		return i >= 0 && i+1 < len(args) && shellScriptFetches(args[i+1], depth)
	}
	return false
}

// envValueFlags are env's options whose value is the next argument.
var envValueFlags = []string{"-u", "--unset", "-C", "--chdir"}

// containerValueFlags are docker's and podman's global options whose value is the next argument.
var containerValueFlags = []string{"-c", "--context", "-H", "--host", "--config", "-l", "--log-level",
	"--tlscacert", "--tlscert", "--tlskey", "--connection", "--url", "--identity", "--root", "--runroot"}

// containerRun reports whether args are a docker or podman run (or container run), which pulls its image.
func containerRun(args []string) bool {
	i := firstPositional(args, containerValueFlags)
	if i >= 0 && args[i] == "container" {
		j := firstPositional(args[i+1:], nil)
		return j >= 0 && args[i+1+j] == "run"
	}
	return i >= 0 && args[i] == "run"
}

// shellAssignment reports whether w is a NAME=value word.
func shellAssignment(w string) bool {
	i := strings.IndexByte(w, '=')
	return i > 0 && !strings.ContainsAny(w[:i], "-/.$")
}

// shellScriptFetches reports whether any simple command in a shell -c script fetches what it runs, past a
// leading exec and NAME=value words. It is not a shell parser: when shellCommands cannot split the script,
// only its first command, read by spaces, is judged.
func shellScriptFetches(script string, depth int) bool {
	cmds, ok := shellCommands(script)
	if !ok {
		cmds = [][]string{strings.Fields(script)}
	}
	for _, words := range cmds {
		for len(words) > 0 && (words[0] == "exec" || shellAssignment(words[0])) {
			words = words[1:]
		}
		if len(words) > 0 && launchFetches(words[0], words[1:], depth+1) {
			return true
		}
		if !ok {
			break
		}
	}
	return false
}

// shellCommands splits a shell script into its simple commands' words. Quotes and backslash escapes keep
// their text in one word; ;, &, | and a newline end a command only outside them; # starts a comment. It
// reports false for what it does not follow: an unclosed quote, a trailing backslash, $( or a backtick
// (outside single quotes), a here-document, and compound or function grammar (an unquoted ( or ), or a
// shellCompoundWords keyword starting a command), where a command's words need not run.
func shellCommands(script string) ([][]string, bool) {
	var cmds [][]string
	var cmd []string
	var word strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			cmd, inWord = append(cmd, word.String()), false
			word.Reset()
		}
	}
	for i := 0; i < len(script); i++ {
		c := script[i]
		switch {
		case shellSubstitutes(script[i:]) || strings.HasPrefix(script[i:], "<<") || c == '(' || c == ')':
			return nil, false
		case c == '\\':
			if i++; i == len(script) {
				return nil, false
			}
			word.WriteByte(script[i])
			inWord = true
		case c == '\'' || c == '"':
			end := shellQuoteEnd(script, i)
			if end < 0 {
				return nil, false
			}
			word.WriteString(script[i+1 : end])
			i, inWord = end, true
		case c == '#' && !inWord:
			for i < len(script) && script[i] != '\n' {
				i++
			}
			i--
		case c == ' ' || c == '\t':
			endWord()
		case c == '&' && inWord && strings.HasSuffix(word.String(), ">"): // 2>&1 is a redirection
			word.WriteByte(c)
		case c == ';' || c == '&' || c == '|' || c == '\n':
			endWord()
			if len(cmd) > 0 {
				cmds, cmd = append(cmds, cmd), nil
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	endWord()
	if len(cmd) > 0 {
		cmds = append(cmds, cmd)
	}
	compound := slices.ContainsFunc(cmds, func(words []string) bool { return slices.Contains(shellCompoundWords, words[0]) })
	return cmds, !compound
}

// shellCompoundWords are the reserved words that open or close a shell compound command or function body.
var shellCompoundWords = []string{"{", "}", "if", "then", "elif", "else", "fi", "for", "while", "until", "do",
	"done", "case", "esac", "function", "select"}

// shellQuoteEnd is the index of the quote that closes the one at script[open], or -1 when none does or a
// double-quoted run substitutes a command. A double-quoted backslash escapes the next byte; a single-quoted
// one is literal.
func shellQuoteEnd(script string, open int) int {
	q := script[open]
	for i := open + 1; i < len(script); i++ {
		switch {
		case script[i] == q:
			return i
		case q == '"' && shellSubstitutes(script[i:]):
			return -1
		case q == '"' && script[i] == '\\':
			i++
		}
	}
	return -1
}

// shellSubstitutes reports whether s starts a command substitution: a backtick or $( .
func shellSubstitutes(s string) bool {
	return strings.HasPrefix(s, "`") || strings.HasPrefix(s, "$(")
}

// denoRunValueFlags are the deno run options whose value is the next argument; every other option is a
// switch, or takes its value after "=".
var denoRunValueFlags = []string{"-c", "--config", "--import-map", "--cert", "--location", "--seed", "--ext",
	"-L", "--log-level", "--preload", "--import", "--require", "--conditions", "--lock",
	"--minimum-dependency-age", "--min-dep-age"}

// subcommandFetchers are package managers, by base name, with the subcommands that download the package
// they run; "tool run" is two words.
var subcommandFetchers = map[string][]string{
	"npm": {"exec", "x"}, "pnpm": {"dlx"}, "yarn": {"dlx"}, "bun": {"x"}, "uv": {"tool run"},
}

// subcommandFetches reports whether args name one of subcommandFetchers' subcommands for base.
func subcommandFetches(base string, args []string) bool {
	verbs, ok := subcommandFetchers[base]
	i := firstPositional(args, nil)
	if !ok || i < 0 {
		return false
	}
	if j := firstPositional(args[i+1:], nil); j >= 0 && slices.Contains(verbs, args[i]+" "+args[i+1+j]) {
		return true
	}
	return slices.Contains(verbs, args[i])
}

// firstPositional is the index of args' first argument that is neither an option nor the value of one of
// valueFlags, or -1.
func firstPositional(args, valueFlags []string) int {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case slices.Contains(valueFlags, a):
			i++
		case !strings.HasPrefix(a, "-"):
			return i
		}
	}
	return -1
}

// denoRunsRemoteEntry reports whether args run an npm:, jsr: or URL entry module, with deno run or, as Deno 2
// allows, with no subcommand. Only the entry module counts: the arguments after it are the program's own
// (Deno.args), not modules.
func denoRunsRemoteEntry(args []string) bool {
	i := firstPositional(args, denoRunValueFlags)
	if i >= 0 && args[i] == "run" {
		j := firstPositional(args[i+1:], denoRunValueFlags)
		if j < 0 {
			return false
		}
		i += 1 + j
	}
	if i < 0 {
		return false
	}
	entry := args[i]
	return strings.HasPrefix(entry, "npm:") || strings.HasPrefix(entry, "jsr:") ||
		strings.HasPrefix(entry, "http:") || strings.HasPrefix(entry, "https:")
}

// noNetworkComponentNeedsHost reports whether an attached component needs the network: a built-in Git
// provider, a CLI credential, a remote MCP server (whose host only the broker reaches, so it is never on
// the allowlist), a local MCP server whose launch fetches what it runs (launchFetches), a header secret,
// or any host on its definition. One with only config, env or file secrets, or a local server run from
// another program in the image, needs none.
func noNetworkComponentNeedsHost(c attachedComponent) bool {
	if c.builtin != nil {
		return true
	}
	d := c.snapshot.Definition
	if len(d.Hosts) > 0 || d.CLI != nil || d.RemoteMCP() {
		return true
	}
	if d.LocalMCP() && d.MCP.Launch != nil && launchFetches(d.MCP.Launch.Command, d.MCP.Launch.Args, 0) {
		return true
	}
	return slices.ContainsFunc(d.Secrets, func(sec types.ComponentSecret) bool {
		return sec.Delivery.Mode == types.ComponentDeliveryHeader
	})
}

func noNetworkComponent(name string, ordinal int) string {
	if name == "" {
		return "component #" + strconv.Itoa(ordinal+1)
	}
	return "component " + name
}

func noNetworkProvider(p types.ModelProvider) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

// noNetworkHostSource names the source the fold recorded adding host, or the run's policy when none did.
func noNetworkHostSource(rec *foldRecorder, host string) string {
	for _, row := range rec.rows {
		if row.Field != fieldAllowed || row.Value != host || row.Effect != provEffectAdded {
			continue
		}
		switch row.Source.Kind {
		case provKindWorkspace:
			return derivedWorkspace
		case provKindComponent:
			return "component " + row.Source.Name
		case provKindModelProvider:
			return "model provider " + row.Source.Name
		case provKindHarness:
			return "harness " + row.Source.Name
		case provKindPosture:
			return "the harness's tool policy"
		case provKindPerson:
			return "your request"
		}
	}
	return "the run's policy"
}
