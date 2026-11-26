// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An MCP server that starts from a command runs in a sandbox of its own: its container, on a network of its
// own, behind an MCP proxy that holds the server's sealed egress policy and is the one way the agent's proxy
// reaches it (the relay). Nothing of the agent's is in it, and nothing of the host: no mount, no drive, no
// device, no docker socket and no published port. This file is the contract the control plane and every
// substrate share.

const (
	// MCPSandboxNamePrefix starts the name of every object of an MCP sandbox, so a run's objects can be
	// told from the agent's and swept by name as well as by label.
	MCPSandboxNamePrefix = "wardyn-mcp-"
	// MCPHostPath is where the host program the MCP container runs (it starts the server from its launch
	// command and serves it over HTTP to the MCP proxy) is copied in. It sits under root-owned /etc, apart
	// from ManagedFileDir and from any harness's settings.
	MCPHostPath = "/etc/wardyn-mcp/wardyn-mcp-host"
)

// MCPSandboxID names one MCP sandbox: the run, the server's run_components ordinal, and the confinement
// class it runs under. Every object is named from it, so a row with no ref can still be killed.
type MCPSandboxID struct {
	RunID   uuid.UUID
	Ordinal int
	Class   types.ConfinementClass
}

// MCPSandboxSpec is everything a substrate needs to create one MCP sandbox. It holds no credential other
// than the server's own: the run's model keys, tokens and grants never enter it.
type MCPSandboxSpec struct {
	ID        MCPSandboxID
	ServerKey string
	// Image is the run's image: the server's command runs from it.
	Image  string
	Launch types.ComponentMCPLaunch
	// Env and SecretEnv are the server's own variables and secrets; ManagedFiles its own secret files.
	Env          map[string]string
	SecretEnv    map[string]string
	ManagedFiles []ManagedFile
	// PlatformEnv is what Wardyn sets: the proxy variables, NO_PROXY, the public CA PEM, the CA trust
	// variables and the launch. Its keys are a closed list (mcpPlatformEnvKeys).
	PlatformEnv map[string]string
	// ProxyConfig is the MCP proxy's config, rendered and sealed by the control plane.
	ProxyConfig []byte
	// RelayTokenSHA256 is the hex SHA-256 of the relay token the agent's proxy sends.
	RelayTokenSHA256 string
	Resources        Resources
	Labels           map[string]string
	// OnWaiting, non-nil, reports why the sandbox is not up yet, such as an image pull (must not block).
	// Call NotifyWaiting, never the field.
	OnWaiting func(detail string) `json:"-"`
}

// NotifyWaiting delivers one OnWaiting report, or does nothing when this spec has no listener.
func (s MCPSandboxSpec) NotifyWaiting(detail string) {
	if s.OnWaiting != nil {
		s.OnWaiting(detail)
	}
}

// mcpPlatformEnvKeys and the CA trust variables (sandboxCATrustVars) are the closed list of PlatformEnv keys.
var mcpPlatformEnvKeys = map[string]bool{
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "http_proxy": true, "https_proxy": true, "NO_PROXY": true, "no_proxy": true,
	"WARDYN_MITM_CA_PEM": true, "WARDYN_MCP_LAUNCH_JSON": true,
}

func mcpPlatformEnvKey(name string) bool {
	_, trust := sandboxCATrustVars()[name]
	return mcpPlatformEnvKeys[name] || trust
}

// Validate refuses a spec that would give the server more than its own: a mount, drive or device field set
// at any depth, a variable name a component may not set, a platform variable outside the closed list, or
// no proxy config.
func (s MCPSandboxSpec) Validate() error {
	if path := hostAccessField(reflect.ValueOf(s), "MCPSandboxSpec"); path != "" {
		return fmt.Errorf("mcp sandbox: %s is set; an MCP sandbox takes no mount, drive or device", path)
	}
	for _, env := range []struct {
		field string
		vars  map[string]string
	}{{"Env", s.Env}, {"SecretEnv", s.SecretEnv}} {
		for name := range env.vars {
			if err := types.ValidComponentEnvName(name); err != nil {
				return fmt.Errorf("mcp sandbox: %s: %w", env.field, err)
			}
		}
	}
	for name := range s.PlatformEnv {
		if !mcpPlatformEnvKey(name) {
			return fmt.Errorf("mcp sandbox: PlatformEnv: %q is not a variable Wardyn sets in an MCP sandbox", name)
		}
	}
	if len(s.ProxyConfig) == 0 {
		return errors.New("mcp sandbox: no proxy config")
	}
	return nil
}

// hostAccessField is the path of the first field, at any depth, whose name says it reaches the host (a
// mount, a volume, a bind, a drive or a device) and whose value is set; "" when there is none. It reads
// field names, so a field added later is refused without a change here.
func hostAccessField(v reflect.Value, path string) string {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return ""
		}
		return hostAccessField(v.Elem(), path)
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if p := hostAccessField(v.Index(i), fmt.Sprintf("%s[%d]", path, i)); p != "" {
				return p
			}
		}
	case reflect.Struct:
		for i := range v.NumField() {
			f, fv := v.Type().Field(i), v.Field(i)
			p := path + "." + f.Name
			if hostAccessName(f.Name) && !fv.IsZero() {
				return p
			}
			if p := hostAccessField(fv, p); p != "" {
				return p
			}
		}
	}
	return ""
}

func hostAccessName(name string) bool {
	n := strings.ToLower(name)
	for _, w := range []string{"mount", "volume", "bind", "drive", "device"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// MCPSandbox is one MCP sandbox as a substrate reports it.
type MCPSandbox struct {
	ID MCPSandboxID
	// RelayAddr is the MCP proxy's pinned address on the run's relay link (ip:port), the one address the
	// agent's proxy dials for this server.
	RelayAddr string
	// Live is false when the MCP container or its MCP proxy is missing or not running. A cut stops the
	// proxy and leaves the container, so a cut sandbox reads as dead.
	Live bool
}

// MCPSandboxer is an OPTIONAL Runner capability: MCP sandboxes for the machine kinds Capabilities lists in
// MCPSandboxKinds. Create and start are two calls, so the control plane can record the row between them.
type MCPSandboxer interface {
	// CreateMCPSandbox pulls the images, creates every object and copies the files in, and starts nothing.
	// The result holds the pinned relay address.
	CreateMCPSandbox(ctx context.Context, spec MCPSandboxSpec) (MCPSandbox, error)
	// StartMCPSandbox starts the MCP proxy, waits for its ready marker, then starts the container.
	StartMCPSandbox(ctx context.Context, id MCPSandboxID) error
	// KillRunMCPSandboxes kills the run's MCP objects found by listing, by the run's label and the MCP
	// components only, never the relay link or an agent object. It is the end's fallback when the rows
	// cannot be read.
	KillRunMCPSandboxes(ctx context.Context, runID uuid.UUID) error
	KillMCPSandbox(ctx context.Context, id MCPSandboxID) error
	MCPSandboxStatus(ctx context.Context, id MCPSandboxID) (MCPSandbox, error)
}

// MCPProxyStopper is an OPTIONAL Runner capability: stop, gracefully, every MCP proxy of the run whose agent
// sandbox is agentRef, so its last decisions are flushed before it goes.
type MCPProxyStopper interface {
	StopMCPProxies(ctx context.Context, agentRef string) error
}

// The cut helpers are the one way the control plane cuts a run's egress or ends its agent. Each calls the
// agent's method and returns its answer unchanged, so every caller's err == nil and errors.Is branch behaves
// as before. After the call, also after a failed one, a receiver that is an MCPProxyStopper stops the run's
// MCP proxies too, so a cut leaves no MCP server a path out. A failure to stop them is logged and never
// joined into the answer: the run's end, which ends every row, is the backstop.

// CutEgress is StopProxy, then the run's MCP proxies.
func CutEgress(ctx context.Context, s ProxyStopper, ref string) error {
	err := s.StopProxy(ctx, ref)
	stopMCPProxies(ctx, s, ref)
	return err
}

// CutEgressAt is StopProxyAt, then the run's MCP proxies, except when a newer revive owns the run
// (ErrProxyGenerationNewer): its MCP proxies are left running.
func CutEgressAt(ctx context.Context, s ProxyGenerationStopper, ref string, gen int) error {
	err := s.StopProxyAt(ctx, ref, gen)
	if !errors.Is(err, ErrProxyGenerationNewer) {
		stopMCPProxies(ctx, s, ref)
	}
	return err
}

// FenceEgress is FenceProxy, then the run's MCP proxies.
func FenceEgress(ctx context.Context, f ProxyFencer, ref string) error {
	err := f.FenceProxy(ctx, ref)
	stopMCPProxies(ctx, f, ref)
	return err
}

// EndSandboxCut is EndSandbox, then the run's MCP proxies.
func EndSandboxCut(ctx context.Context, e SandboxEnder, ref string) error {
	err := e.EndSandbox(ctx, ref)
	stopMCPProxies(ctx, e, ref)
	return err
}

// EndSandboxCutAt is EndSandboxAt, then the run's MCP proxies, except when a newer revive owns the run
// (ErrProxyGenerationNewer).
func EndSandboxCutAt(ctx context.Context, s ProxyGenerationStopper, ref string, gen int) error {
	err := s.EndSandboxAt(ctx, ref, gen)
	if !errors.Is(err, ErrProxyGenerationNewer) {
		stopMCPProxies(ctx, s, ref)
	}
	return err
}

// LostProxyStopper stops the proxy of a run marked lost but not ended, and keeps its agent with no network,
// acting for policy generation gen (0: none). ErrEndUnsupported: this run cannot be kept; tear it down.
// nil only once the proxy is confirmed gone. Any other error leaves containment unresolved, with the agent
// stopped too if the proxy could not be confirmed gone.
type LostProxyStopper interface {
	StopLostProxyAt(ctx context.Context, ref string, gen int) error
}

// CutLostEgressAt is StopLostProxyAt, then the run's MCP proxies, except when a newer revive owns the run
// (ErrProxyGenerationNewer).
func CutLostEgressAt(ctx context.Context, lp LostProxyStopper, ref string, gen int) error {
	err := lp.StopLostProxyAt(ctx, ref, gen)
	if !errors.Is(err, ErrProxyGenerationNewer) {
		stopMCPProxies(ctx, lp, ref)
	}
	return err
}

// stopMCPProxies stops the run's MCP proxies when the receiver can, and logs a failure.
func stopMCPProxies(ctx context.Context, recv any, ref string) {
	m, ok := recv.(MCPProxyStopper)
	if !ok {
		return
	}
	if err := m.StopMCPProxies(ctx, ref); err != nil {
		slog.Warn("runner: could not stop the run's MCP proxies", slog.String("ref", ref), slog.Any("err", err))
	}
}
