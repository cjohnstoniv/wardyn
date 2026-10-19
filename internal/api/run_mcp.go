// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A run's remote MCP servers (an MCP component). The gate decides which of the
// run's harnesses each server is turned on for; dispatch gives every such
// harness one random token, the proxy only its digest, and writes the harness's
// own configuration pointing at the proxy's broker route
// (/wardyn/mcp/<token>/<server key>). The broker is the only path to a server,
// holds its credential and applies the run's tool rules to its tools.

const (
	// claudeMCPInteractivePath is Claude Code's managed MCP file, which every
	// interactive session reads. Claude Code refuses --strict-mcp-config while it
	// exists, so an autonomous run never gets it.
	claudeMCPInteractivePath = runner.ManagedFileDir + "/managed-mcp.json"
	// claudeMCPAutonomousPath is the file agent-run passes as a second
	// --mcp-config, under --strict-mcp-config, on both autonomous lanes.
	claudeMCPAutonomousPath = runner.ManagedFileDir + "/wardyn-mcp.json"
	// codexMCPEnv carries Codex CLI's servers, key to broker URL, which its
	// agent-run writes into config.toml. Secret env: each URL holds the token.
	codexMCPEnv = "WARDYN_MCP_SERVERS_JSON"
	// claudeHarness is Claude Code's harness id.
	claudeHarness = "claude-code"
	// mcpServerKeyMax leaves room for a numeric suffix within the broker's
	// 32-byte key.
	mcpServerKeyMax = 28
)

// mcpIncludedHarnesses is the request's harness ids: its included harnesses,
// else the agent an older client names.
func mcpIncludedHarnesses(req createRunRequest) []string {
	var ids []string
	for _, h := range harnessSet(req) {
		ids = append(ids, h.Harness)
	}
	if len(ids) == 0 && req.Agent != "" {
		ids = []string{req.Agent}
	}
	return ids
}

// mcpHarnessUnsupported is why harness cannot take an MCP server, "" when it
// can. Claude Code and Codex CLI read the configuration Wardyn writes; another
// harness gets none.
func mcpHarnessUnsupported(harness string) string {
	if harness == claudeHarness || harness == codexHarness {
		return ""
	}
	name := harness
	if d, ok := harnessByID(harness); ok {
		name = d.Display
	}
	return strings.Replace(accessSentences["mcp_harness_unsupported"], "{harness}", name, 1)
}

// mcpHarnessProblem is a refusal of a reference's mcp_harnesses.
type mcpHarnessProblem struct {
	status      int
	reason, msg string
}

// admitMCPHarnesses decides which of the run's harnesses an MCP component's
// server is turned on for: those the reference lists, each included and able
// to take it, or, when it lists none, every included harness that can.
func admitMCPHarnesses(def types.ComponentDefinition, asked, included []string) ([]string, *mcpHarnessProblem) {
	if def.MCP == nil {
		if asked != nil {
			return nil, &mcpHarnessProblem{http.StatusBadRequest, reasonComponentRefInvalid,
				"mcp_harnesses: only a component with an mcp server is turned on for harnesses"}
		}
		return nil, nil
	}
	if asked == nil {
		usable := slices.DeleteFunc(slices.Clone(included), func(h string) bool { return mcpHarnessUnsupported(h) != "" })
		if len(usable) == 0 {
			return nil, &mcpHarnessProblem{http.StatusUnprocessableEntity, reasonRequestFieldUnavailable,
				"mcp_harnesses: this run has no harness that can use an MCP server. Add Claude Code or Codex CLI, or remove the component."}
		}
		return usable, nil
	}
	for i, h := range asked {
		if !slices.Contains(included, h) {
			return nil, &mcpHarnessProblem{http.StatusBadRequest, reasonComponentRefInvalid,
				fmt.Sprintf("mcp_harnesses[%d]: %q is not a harness of this run", i, h)}
		}
		if why := mcpHarnessUnsupported(h); why != "" {
			return nil, &mcpHarnessProblem{http.StatusUnprocessableEntity, reasonRequestFieldUnavailable,
				fmt.Sprintf("mcp_harnesses[%d]: %s", i, why)}
		}
	}
	return slices.Clone(asked), nil
}

// rawEgressHosts is what a component adds to the run's allowlist: its hosts,
// less an MCP server's host at any port, which the broker alone reaches.
func rawEgressHosts(def types.ComponentDefinition) []string {
	if def.MCP == nil {
		return def.Hosts
	}
	server, err := types.MCPURLDestination(def.MCP.URL)
	return slices.DeleteFunc(slices.Clone(def.Hosts), func(h string) bool {
		d, perr := types.ParseDestination(h)
		return err != nil || perr != nil || d.Host == server.Host
	})
}

// mcpServerKeys names each attached MCP server, index-aligned with attached
// ("" for any other component). It is the <server> of the tool names
// mcp__<server>__<tool> rules name. Each is its name's slug ("mcp" without
// one). An organisation's servers claim theirs first, in component-id order:
// two organisation MCP components never share a key (orgMCPKeyFree), so an
// organisation server's key is the same on every run that attaches it. A
// person's own server takes a numeric suffix on a collision, and so can never
// draw an organisation's rules for one of its servers onto itself.
func mcpServerKeys(attached []attachedComponent) []string {
	var org, own []int
	for i, a := range attached {
		switch {
		case a.builtin != nil || a.snapshot.Definition.MCP == nil:
		case !a.snapshot.SelfDefined && a.snapshot.ComponentID != nil:
			org = append(org, i)
		default:
			own = append(own, i)
		}
	}
	slices.SortFunc(org, func(x, y int) int {
		return strings.Compare(attached[x].snapshot.ComponentID.String(), attached[y].snapshot.ComponentID.String())
	})
	keys := make([]string, len(attached))
	taken := map[string]bool{}
	for _, i := range slices.Concat(org, own) {
		base := mcpSlug(attached[i].snapshot.Name)
		key := base
		for n := 2; taken[key]; n++ {
			key = base + "-" + strconv.Itoa(n)
		}
		keys[i], taken[key] = key, true
	}
	return keys
}

// orgMCPKeyLockPart names the one lock (db.ComponentKeyLockClass) an organisation MCP server's save holds
// across orgMCPKeyFree and its write.
const orgMCPKeyLockPart = "org-mcp-key"

// orgMCPKeyFree refuses an organisation MCP component whose server key (its
// name's slug, as cut for the key) is another organisation MCP component's:
// that key is what an organisation's rules name, so it must name one server
// whichever of them a run attaches. 409 component_name_conflict; it reports
// whether the save may go on.
func orgMCPKeyFree(w http.ResponseWriter, r *http.Request, cs store.ComponentStore, c types.Component) bool {
	if c.Definition.MCP == nil {
		return true
	}
	rows, err := cs.ListComponents(r.Context(), "")
	if err != nil {
		writeServerError(w, r, "list components", err)
		return false
	}
	key := mcpSlug(c.Name)
	for _, row := range rows {
		if row.ID != c.ID && row.Definition.MCP != nil && mcpSlug(row.Name) == key {
			writeErrorReason(w, http.StatusConflict, reasonComponentNameConflict, fmt.Sprintf(
				"another organisation MCP server's name gives the same server key %q, which tool rules name; choose a name that differs in letters or digits", key))
			return false
		}
	}
	return true
}

// mcpSlug is name in lower-case letters, digits and single hyphens.
func mcpSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > mcpServerKeyMax {
		s = strings.TrimRight(s[:mcpServerKeyMax], "-")
	}
	if s == "" {
		return "mcp"
	}
	return s
}

// componentMCP is one MCP server as dispatch configures it.
type componentMCP struct {
	// Ordinal is the component's run_components ordinal: its position among
	// the attached components that have a row, so a built-in is not counted.
	Ordinal   int
	Key       string
	URL       string
	Host      string // the URL's bare host, where a header credential is bound
	Harnesses []string
	// Credentialed: the component carries a header secret, whose injection
	// dispatch moves to the broker.
	Credentialed bool
}

// mcpDispatch is what dispatch hands on for the run's MCP servers.
type mcpDispatch struct {
	servers  []proxy.MCPServerConfig
	bindings []proxy.MCPBinding
	files    []runner.ManagedFile
}

// dispatchMCP is dispatch's MCP half. Each server's header credential moves
// off injections, which the generic injector reads, onto the server's broker
// entry; a server whose credential a phase withheld is dropped rather than
// brokered without it. Each harness a server is on for gets one token and its
// own configuration: Claude Code a managed file, Codex CLI the env its
// agent-run writes into config.toml (env, set here into sandboxEnv).
func dispatchMCP(servers []componentMCP, injections *[]runner.InjectionGrant, proxyURL string, interactive bool, env map[string]string) mcpDispatch {
	var out mcpDispatch
	on := map[string][]string{}
	var order []string
	for _, m := range servers {
		cfg := proxy.MCPServerConfig{Key: m.Key, URL: m.URL, Ordinal: m.Ordinal}
		if m.Credentialed {
			j := slices.IndexFunc(*injections, func(in runner.InjectionGrant) bool { return hostEqual(in.Rule.Host, m.Host) })
			if j < 0 {
				continue
			}
			id := (*injections)[j].GrantID
			cfg.GrantID = &id
			*injections = slices.Delete(*injections, j, j+1)
		}
		out.servers = append(out.servers, cfg)
		for _, h := range m.Harnesses {
			if _, seen := on[h]; !seen {
				order = append(order, h)
			}
			on[h] = append(on[h], m.Key)
		}
	}
	base := strings.TrimRight(proxyURL, "/") + "/wardyn/mcp/"
	for _, h := range order {
		token, digest := mintMCPToken()
		out.bindings = append(out.bindings, proxy.MCPBinding{Harness: h, SHA256: digest, Servers: on[h]})
		urls := map[string]string{}
		for _, k := range on[h] {
			urls[k] = base + token + "/" + k
		}
		switch h {
		case claudeHarness:
			out.files = append(out.files, claudeMCPFile(urls, interactive))
		case codexHarness:
			env[codexMCPEnv] = string(mustJSON(urls))
		}
	}
	return out
}

// claudeMCPFile is Claude Code's MCP configuration: the managed file an
// interactive session reads, or the file agent-run passes an autonomous one.
func claudeMCPFile(urls map[string]string, interactive bool) runner.ManagedFile {
	servers := map[string]map[string]string{}
	for k, u := range urls {
		servers[k] = map[string]string{"type": "http", "url": u}
	}
	path := claudeMCPAutonomousPath
	if interactive {
		path = claudeMCPInteractivePath
	}
	content, _ := json.Marshal(map[string]any{"mcpServers": servers})
	return runner.ManagedFile{Path: path, Content: content}
}

// mintMCPToken makes one harness's random MCP token and its SHA-256 digest.
// crypto/rand.Read never returns an error.
func mintMCPToken() (token, digest string) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	token = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:])
}

// reviveMCPBindings re-derives which harness reaches which MCP server from the
// run's component rows, never from the stored config alone: a server whose row
// is gone or erased is dropped, and each harness keeps only the servers its
// row's mcp_harnesses record. It only narrows.
func (s *Server) reviveMCPBindings(ctx context.Context, run types.AgentRun, cfg *proxy.Config) *reviveError {
	if len(cfg.MCPServers) == 0 {
		return nil
	}
	comps, err := s.runComponentSnapshot(ctx, run.ID)
	if err != nil {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveOwnerAuthorityUnreadable, loggedMsg(ctx, "re-check the run's MCP servers", err))
	}
	narrowMCPBindings(cfg, comps)
	return nil
}

// narrowMCPBindings is reviveMCPBindings' narrowing over the rows it read.
func narrowMCPBindings(cfg *proxy.Config, comps []types.RunComponent) {
	on := map[string][]string{}
	cfg.MCPServers = slices.DeleteFunc(cfg.MCPServers, func(sv proxy.MCPServerConfig) bool {
		i := slices.IndexFunc(comps, func(c types.RunComponent) bool { return c.Ordinal == sv.Ordinal })
		if i < 0 || comps[i].Erased || comps[i].Definition.MCP == nil {
			return true
		}
		on[sv.Key] = comps[i].MCPHarnesses
		return false
	})
	for i := range cfg.MCPBindings {
		b := &cfg.MCPBindings[i]
		b.Servers = slices.DeleteFunc(b.Servers, func(k string) bool { return !slices.Contains(on[k], b.Harness) })
	}
}

// What step 5 shows of the run's MCP servers.
const (
	accessKindMCP         = "mcp"        // an MCP component's access row
	viaMCPBroker          = "mcp_broker" // its server's host: the broker route, never raw egress
	toolEnforcerMCPBroker = "mcp_broker" // a harness whose rules only the broker enforces, on its MCP tools
)

// attachedMCP is the MCP binding of the attached component a fact describes,
// and its server's host; nil when it carries no server.
func attachedMCP(comps runComponents, factID string) (*client.AccessMCP, string) {
	keys := mcpServerKeys(comps.attached)
	for i, a := range comps.attached {
		def := a.snapshot.Definition
		if def.MCP == nil || componentProvSource(a, i).ID != factID {
			continue
		}
		server, _ := types.MCPURLDestination(def.MCP.URL)
		return &client.AccessMCP{ServerKey: keys[i], Transport: def.MCP.Transport,
			Harnesses: slices.Concat([]string{}, a.snapshot.MCPHarnesses)}, server.Host
	}
	return nil, ""
}

// mcpServersFor is the server keys turned on for harness, in request order.
func mcpServersFor(comps runComponents, harness string) []string {
	out := []string{}
	keys := mcpServerKeys(comps.attached)
	for i, a := range comps.attached {
		if keys[i] != "" && slices.Contains(a.snapshot.MCPHarnesses, harness) {
			out = append(out, keys[i])
		}
	}
	return out
}
