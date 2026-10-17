// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

type runnerRuntime struct {
	hooks runnerwire.RunnerHooks
	run   func(context.Context) error
	close func() error
}

type runnerHooksBuilder func(context.Context, runneridentity.Identity, string, serveOptions) (runnerRuntime, error)

type serveOptions struct {
	proxyImage, relayListen, relayHostTarget, network, recording string
	// trustedCAFile is --trusted-ca-file; trustedCA is the bundle validated at startup, and
	// trustedCAPath the file it was written to for sandboxes to mount (trustedCAAtStartup).
	trustedCAFile, trustedCA, trustedCAPath string
}

func (o serveOptions) recordingEnabled() (bool, error) {
	switch o.recording {
	case "pg", "fs":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, errors.New("runner recording mode must be pg, fs or off")
	}
}

func newServeCommand(build runnerHooksBuilder) *cobra.Command {
	var org, stateDir string
	var opts serveOptions
	cmd := &cobra.Command{Use: "serve", Short: "Connect this registered runner to its organisation", Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) (retErr error) {
		// The Docker client reads DOCKER_HOST itself; a profile's DockerHost outranks the environment.
		applyManagedServe(cmd, &org, &opts)
		if org == "" {
			return errors.New(`required flag(s) "org" not set`)
		}
		if h := managedPref("DockerHost"); h != "" {
			if err := os.Setenv("DOCKER_HOST", h); err != nil {
				return err
			}
		}
		if stateDir == "" {
			var err error
			stateDir, err = runneridentity.DefaultDir()
			if err != nil {
				return err
			}
		}
		var err error
		stateDir, err = filepath.Abs(stateDir)
		if err != nil {
			return err
		}
		// Validate the persisted key, mode and organisation binding before a
		// builder can contact Docker or any network endpoint.
		identity, err := runneridentity.Load(stateDir, org)
		if err != nil {
			return err
		}
		// Two serves on one identity would fight over its connection; a second one stops here.
		unlock, err := lockSingleInstance(stateDir)
		if err != nil {
			return err
		}
		defer unlock()
		// The flag (whose default is the environment) outranks the image the organisation named at
		// registration, which outranks this binary's release image.
		opts.proxyImage = cmp.Or(opts.proxyImage, identity.ProxyImage, releaseProxyImage())
		if err := trustedCAAtStartup(cmd.Context(), &opts, stateDir); err != nil {
			return err
		}
		if build == nil {
			return errors.New("runner execution and durable offline handlers are unavailable")
		}
		runtime, err := build(cmd.Context(), identity, stateDir, opts)
		if err != nil {
			return err
		}
		if runtime.close != nil {
			defer func() { retErr = errors.Join(retErr, runtime.close()) }()
		}
		if runtime.run == nil || runtime.close == nil {
			return errors.New("runner containment runtime is unavailable")
		}
		client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: org, RunnerID: identity.RunnerID.String(), PrivateKey: identity.PrivateKey, Hooks: runtime.hooks})
		if err != nil {
			return err
		}
		return runRunnerServices(cmd.Context(), client.Run, runtime.run)
	}
	cmd.Flags().StringVar(&org, "org", "", "organisation public HTTPS URL used at registration (default: the OrgURL managed preference)")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "private runner state directory (defaults to the user configuration directory)")
	cmd.Flags().StringVar(&opts.proxyImage, "proxy-image", os.Getenv("WARDYN_PROXY_IMAGE"), "Wardyn proxy image matching this daemon version (defaults to the image the organisation named at registration, else the release image on a release build)")
	cmd.Flags().StringVar(&opts.relayListen, "relay-listen", os.Getenv("WARDYN_RELAY_LISTEN"), "address the run relay listens on: loopback, the native Docker bridge's gateway or Rancher Desktop's veth-rd-wsl address (found by a probe container when left out)")
	cmd.Flags().StringVar(&opts.relayHostTarget, "relay-host-target", os.Getenv("WARDYN_RELAY_HOST_TARGET"), "what a run's proxy reaches the relay at: host-gateway or an IP address (found by a probe container when left out)")
	cmd.Flags().StringVar(&opts.network, "internal-network", cmp.Or(os.Getenv("WARDYN_INTERNAL_NETWORK"), "wardyn-internal"), "Docker bridge for proxy sidecars (created when missing)")
	cmd.Flags().StringVar(&opts.trustedCAFile, "trusted-ca-file", os.Getenv("WARDYN_TRUSTED_CA_FILE"), "PEM bundle of extra roots (e.g. a TLS-inspecting corporate proxy's CA) every run's proxy and sandbox trust beside the system roots; 64 KiB at most. SSL_CERT_FILE and SSL_CERT_DIR are not read. Read at startup only")
	cmd.Flags().StringVar(&opts.recording, "recording-store", cmp.Or(os.Getenv("WARDYN_RECORDING_STORE"), "pg"), "organisation recording mode: pg, fs or off (off disables local capture)")
	return cmd
}

// applyManagedServe lets the managed preferences (macOS configuration profile) replace the environment
// defaults of the flags the command line left alone. Read here, not when the command is built, so only
// serve pays for it.
func applyManagedServe(cmd *cobra.Command, org *string, o *serveOptions) {
	for _, f := range []struct {
		flag, key string
		dst       *string
	}{{"org", "OrgURL", org}, {"proxy-image", "ProxyImage", &o.proxyImage}, {"relay-listen", "RelayListen", &o.relayListen},
		{"relay-host-target", "RelayHostTarget", &o.relayHostTarget}, {"internal-network", "InternalNetwork", &o.network},
		{"trusted-ca-file", "TrustedCAFile", &o.trustedCAFile}, {"recording-store", "RecordingStore", &o.recording}} {
		if v := managedPref(f.key); v != "" && !cmd.Flags().Changed(f.flag) {
			*f.dst = v
		}
	}
}
