// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
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
	// orgCAFile is --org-ca-file: extra roots for runnerd's own connection to the organisation only. It is
	// read by serve and register, and never reaches a sandbox or a run's proxy.
	orgCAFile string
	// proxyImagePinned is whether proxyImage was named on purpose (proxyImagePinned), so the
	// organisation's answer at connect does not replace it.
	proxyImagePinned bool
	// upstreamProxy is --upstream-proxy: this runner's own corporate upstream proxy URL, validated at
	// startup and handed to every run's proxy. It is never sent to the organisation.
	upstreamProxy string
	// substrate is --substrate: docker (default) or k8s. The rest are k8s only (design K1 §3.3).
	substrate, runsNamespace, relayService, arch string
	maxPodLifetime                               time.Duration
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
		// A Docker runner reports its engine's arch, so --arch's environment twin counts as set too.
		changed := func(f string) bool { return cmd.Flags().Changed(f) || f == "arch" && opts.arch != "" }
		if err := checkSubstrateFlags(opts.substrate, changed); err != nil {
			return err
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
		// A pinned image (the flag, whose default is the environment) outranks everything. Otherwise
		// the organisation's answer at each connect wins; until the first connect, the image it named
		// at registration, else this binary's release image, else an unmarked image an earlier setup left.
		opts.proxyImagePinned = proxyImagePinned(opts.proxyImage, identity.ProxyImage, proxyImageGiven(cmd), os.Getenv)
		if !opts.proxyImagePinned {
			opts.proxyImage = cmp.Or(identity.ProxyImage, releaseProxyImage(), opts.proxyImage)
		}
		if err := trustedCAAtStartup(cmd.Context(), &opts, stateDir); err != nil {
			return err
		}
		// The org-only roots stay in this function: opts goes to the runtime that builds the sandboxes' trust.
		orgCA, orgCAs, err := loadOrgCA(opts.orgCAFile)
		if err != nil {
			return err
		}
		if orgCA != "" {
			slog.InfoContext(cmd.Context(), "organisation-only CA trust configured", "file", opts.orgCAFile, "cert_count", orgCAs)
		}
		// The variable is read here, not as the flag's default: pflag prints a default in --help, which would
		// show a credential.
		opts.upstreamProxy = strings.TrimSpace(cmp.Or(opts.upstreamProxy, os.Getenv("WARDYN_UPSTREAM_PROXY")))
		if err := proxy.ValidUpstreamProxyURL(opts.upstreamProxy); err != nil {
			return fmt.Errorf("--upstream-proxy / WARDYN_UPSTREAM_PROXY: %w (the URL is not repeated here: it may hold a credential)", err)
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
		httpClient, err := orgHTTPClient(opts.trustedCA + orgCA)
		if err != nil {
			return err
		}
		status := newStatusWriter(stateDir)
		defer status.stopped()
		client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: org, RunnerID: identity.RunnerID.String(), PrivateKey: identity.PrivateKey, Hooks: runtime.hooks, HTTPClient: httpClient, LinkChanged: status.linkChanged})
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
	cmd.Flags().StringVar(&opts.trustedCAFile, "trusted-ca-file", os.Getenv("WARDYN_TRUSTED_CA_FILE"), "PEM bundle of extra roots (e.g. a TLS-inspecting corporate proxy's CA) that runnerd's own org session and every run's proxy and sandbox trust "+
		"beside the system roots; 64 KiB at most. SSL_CERT_FILE and SSL_CERT_DIR are not read. Read at startup only")
	cmd.Flags().StringVar(&opts.orgCAFile, "org-ca-file", os.Getenv("WARDYN_ORG_CA_FILE"), "PEM file of extra roots that runnerd's own connection to the organisation trusts beside the system roots, and nothing else: no run's proxy or sandbox gets them. "+
		"It must not be writable by group or others and must be owned by you or root. Read at startup only")
	cmd.Flags().StringVar(&opts.upstreamProxy, "upstream-proxy", "", "this runner's own corporate upstream proxy, http://[user:pass@]host[:port], that every run's proxy chains its egress through; "+
		"never sent to the organisation. Prefer the WARDYN_UPSTREAM_PROXY environment variable when it holds a credential (it is read after the flags are parsed, so --help never shows it). Read at startup only")
	cmd.Flags().StringVar(&opts.recording, "recording-store", cmp.Or(os.Getenv("WARDYN_RECORDING_STORE"), "pg"), "organisation recording mode: pg, fs or off (off disables local capture)")
	cmd.Flags().StringVar(&opts.substrate, "substrate", "docker", "where runs execute: docker (this host's engine) or k8s (a runs namespace of the cluster runnerd runs in)")
	cmd.Flags().StringVar(&opts.runsNamespace, "runs-namespace", "", "k8s: the dedicated namespace runs are created in; never runnerd's own")
	cmd.Flags().StringVar(&opts.relayService, "relay-service", "", "k8s: the relay Service's DNS name (<name>.<namespace>.svc.<cluster domain>), which must resolve to exactly one ClusterIP")
	cmd.Flags().StringVar(&opts.arch, "arch", os.Getenv(archEnv), "k8s, required: the sandbox node architecture, amd64 or arm64, pinned as every run pod's kubernetes.io/arch node selector")
	cmd.Flags().DurationVar(&opts.maxPodLifetime, "max-pod-lifetime", 0, "k8s: the most any run pod may live (activeDeadlineSeconds), e.g. 24h")
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
		{"trusted-ca-file", "TrustedCAFile", &o.trustedCAFile}, {"org-ca-file", "OrgCAFile", &o.orgCAFile}, {"recording-store", "RecordingStore", &o.recording}} {
		if v := managedPref(f.key); v != "" && !cmd.Flags().Changed(f.flag) {
			*f.dst = v
		}
	}
}
