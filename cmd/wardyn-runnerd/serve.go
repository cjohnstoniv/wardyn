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
	// proxyImagePinned is whether proxyImage was named on purpose (proxyImagePinned), so the
	// organisation's answer at connect does not replace it.
	proxyImagePinned bool
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
		// A pinned image (the flag, whose default is the environment) outranks everything. Otherwise
		// the organisation's answer at each connect wins; until the first connect, the image it named
		// at registration, else this binary's release image, else an unmarked image an earlier setup left.
		opts.proxyImagePinned = proxyImagePinned(opts.proxyImage, cmd.Flags().Changed("proxy-image"), os.Getenv)
		if !opts.proxyImagePinned {
			opts.proxyImage = cmp.Or(identity.ProxyImage, releaseProxyImage(), opts.proxyImage)
		}
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
	cmd.Flags().StringVar(&org, "org", "", "organisation public HTTPS URL used at registration")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "private runner state directory (defaults to the user configuration directory)")
	cmd.Flags().StringVar(&opts.proxyImage, "proxy-image", os.Getenv("WARDYN_PROXY_IMAGE"), "Wardyn proxy image matching this daemon version (defaults to the image the organisation named at registration, else the release image on a release build)")
	cmd.Flags().StringVar(&opts.relayListen, "relay-listen", os.Getenv("WARDYN_RELAY_LISTEN"), "address the run relay listens on: loopback, the native Docker bridge's gateway or Rancher Desktop's veth-rd-wsl address (found by a probe container when left out)")
	cmd.Flags().StringVar(&opts.relayHostTarget, "relay-host-target", os.Getenv("WARDYN_RELAY_HOST_TARGET"), "what a run's proxy reaches the relay at: host-gateway or an IP address (found by a probe container when left out)")
	cmd.Flags().StringVar(&opts.network, "internal-network", cmp.Or(os.Getenv("WARDYN_INTERNAL_NETWORK"), "wardyn-internal"), "Docker bridge for proxy sidecars (created when missing)")
	cmd.Flags().StringVar(&opts.trustedCAFile, "trusted-ca-file", os.Getenv("WARDYN_TRUSTED_CA_FILE"), "PEM bundle of extra roots (e.g. a TLS-inspecting corporate proxy's CA) every run's proxy and sandbox trust beside the system roots; when unset, SSL_CERT_FILE, else SSL_CERT_DIR, is used. Read at startup only")
	cmd.Flags().StringVar(&opts.recording, "recording-store", cmp.Or(os.Getenv("WARDYN_RECORDING_STORE"), "pg"), "organisation recording mode: pg, fs or off (off disables local capture)")
	_ = cmd.MarkFlagRequired("org")
	return cmd
}
