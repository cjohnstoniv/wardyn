// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// safeSetting limits a value that reaches a unit file, a plist or the env file, so nothing can inject a
// line, a quote or a placeholder.
var safeSetting = regexp.MustCompile(`^[A-Za-z0-9._:/@%+=-]+$`)

// setupOptions are `setup`'s flags.
type setupOptions struct {
	org, tokenFile, name             string
	proxyImage, relayListen, network string
	claim, makeDefault               bool
	pool                             string
	limits                           registerLimits
}

// newSetupCommand is the one command a person or device management runs to turn a downloaded
// wardyn-runnerd into a registered runner that starts at login. It is safe to run again: a runner that
// is already registered is not registered again, and the service files are rewritten and restarted.
func newSetupCommand() *cobra.Command {
	var o setupOptions
	cmd := &cobra.Command{Use: "setup", Short: "Register this computer, install wardyn-runnerd as a login service and start it", Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		host, err := osSetupHost()
		if err != nil {
			return err
		}
		return runSetup(cmd, host, o)
	}
	fl := cmd.Flags()
	fl.StringVar(&o.org, "org", "", "organisation public HTTPS URL")
	fl.StringVar(&o.tokenFile, "token-file", "", "file containing the single-use registration token (not needed once this computer is registered)")
	fl.StringVar(&o.name, "name", "", "runner name (defaults to the host name)")
	fl.StringVar(&o.proxyImage, "proxy-image", "", "Wardyn proxy image (defaults to the release image matching this binary; a development build needs it)")
	fl.StringVar(&o.relayListen, "relay-listen", "", "address the run relay listens on (Docker Desktop is given 127.0.0.1 when this is left out)")
	fl.StringVar(&o.network, "internal-network", "wardyn-internal", "Docker bridge for proxy sidecars (created when missing)")
	fl.BoolVar(&o.claim, "claim", false, "claim the runner for yourself now; needs WARDYN_TOKEN, your own personal API token")
	fl.StringVar(&o.pool, "pool", "", "with --claim, also put the runner in your user pool of this name (made if you have none by it)")
	fl.BoolVar(&o.makeDefault, "default", false, "with --claim and --pool, also make that pool your default")
	o.limits.register(cmd)
	_ = cmd.MarkFlagRequired("org")
	return cmd
}

// setupEngine is what setup learns from the engine: whether it is Docker Desktop and, when the relay
// address was asked for, what serve's explicitRelayBind needs to judge it.
type setupEngine struct {
	desktop bool
	gateway netip.Addr
	bridge  []netip.Addr
}

// setupPlan is everything setup settles before its first side effect.
type setupPlan struct {
	setupOptions
	paths      setupPaths
	registered bool
	dockerHost string
}

func runSetup(cmd *cobra.Command, h setupHost, o setupOptions) error {
	out := cmd.OutOrStdout()
	plan, err := h.preflight(cmd, o)
	if err != nil {
		return err
	}
	if err := h.installBinary(cmd.Context(), plan.paths.installed); err != nil {
		return err
	}
	if !plan.registered {
		fmt.Fprintln(out, "Registering this computer. At a terminal it asks for the runner's limits (Enter takes each suggestion); otherwise it registers with the limits it suggests.")
		done, err := registerRunner(cmd, registerArgs{org: plan.org, tokenFile: plan.tokenFile, stateDir: plan.paths.stateDir, name: plan.name, limits: &plan.limits})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "registered %s; key fingerprint %s\n  %s\n", done.name, done.fingerprint, limitsLine(done.limits))
	}
	if err := h.startService(cmd.Context(), plan, out); err != nil {
		return err
	}
	identity, err := runneridentity.Load(plan.paths.stateDir, plan.org)
	if err != nil {
		return err
	}
	return claimStep(cmd, plan, identity, os.Getenv("WARDYN_TOKEN"))
}

// preflight validates every input and checks Docker, so that a refusal comes before the single-use
// token is spent or any file is written.
func (h setupHost) preflight(cmd *cobra.Command, o setupOptions) (setupPlan, error) {
	if h.uid == 0 {
		return setupPlan{}, errors.New("run setup as the person who owns this runner, not as root: the runner identity lives in that person's own configuration directory")
	}
	if err := federation.CheckOrgURL(o.org); err != nil {
		return setupPlan{}, err
	}
	if o.makeDefault && o.pool == "" {
		return setupPlan{}, errors.New("--default needs --pool")
	}
	if o.pool != "" {
		if !o.claim {
			return setupPlan{}, errors.New("--pool needs --claim")
		}
		if err := types.ValidateRunnerPoolName(o.pool); err != nil {
			return setupPlan{}, err
		}
	}
	if o.relayListen != "" {
		if _, err := netip.ParseAddr(o.relayListen); err != nil {
			return setupPlan{}, fmt.Errorf("--relay-listen: %w", err)
		}
	}
	if o.proxyImage == "" {
		if o.proxyImage = releaseProxyImage(); o.proxyImage == "" {
			return setupPlan{}, fmt.Errorf("this wardyn-runnerd is a development build (%s): pass --proxy-image <ref>", buildVersion())
		}
	}
	plan := setupPlan{setupOptions: o, dockerHost: h.dockerHost}
	var err error
	if plan.paths, err = h.paths(); err != nil {
		return setupPlan{}, err
	}
	settings := []string{o.org, o.proxyImage, o.network, h.dockerHost, o.relayListen}
	if h.goos == "linux" {
		// The unit and the env file carry these paths unquoted; a LaunchAgent plist escapes its own.
		settings = append(settings, plan.paths.installed, plan.paths.stateDir)
	}
	for _, v := range settings {
		if v != "" && !safeSetting.MatchString(v) {
			return setupPlan{}, fmt.Errorf("unsupported characters in a setting: %s", v)
		}
	}
	if _, err := os.Stat(plan.paths.identity()); err == nil {
		if _, err := runneridentity.Load(plan.paths.stateDir, o.org); err != nil {
			return setupPlan{}, fmt.Errorf("this computer is registered, but not for this organisation or not intact: %w", err)
		}
		plan.registered = true
	} else {
		if o.tokenFile == "" {
			return setupPlan{}, errors.New("no runner identity yet: pass --token-file <file> holding the wdr_ token an administrator minted for you")
		}
		if _, err := os.Stat(o.tokenFile); err != nil {
			return setupPlan{}, fmt.Errorf("token file not found: %s", o.tokenFile)
		}
	}
	if err := h.checkServiceManager(cmd.Context(), plan.paths); err != nil {
		return setupPlan{}, err
	}
	eng, err := h.docker(cmd.Context(), o.network, o.relayListen != "")
	if err != nil {
		return setupPlan{}, err
	}
	if o.relayListen != "" {
		// Parsed above. serve refuses the same addresses at startup, after the token is spent.
		if err := explicitRelayBind(netip.MustParseAddr(o.relayListen), eng.desktop, eng.gateway, eng.bridge); err != nil {
			return setupPlan{}, err
		}
	}
	if eng.desktop && plan.relayListen == "" {
		plan.relayListen = "127.0.0.1"
		fmt.Fprintln(cmd.OutOrStdout(), "Docker Desktop detected: the run relay will listen on 127.0.0.1 (expected on macOS, not yet verified).")
	}
	return plan, nil
}
