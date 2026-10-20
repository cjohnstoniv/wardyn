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
	// proxyImagePinned is whether proxyImage was named on purpose (proxyImagePinned), so the
	// organisation's answer at connect does not replace it.
	proxyImagePinned bool
	// proxyImageMirror is the repository an unpinned proxy image is pulled from, at the tag the organisation names.
	proxyImageMirror string
	// upstreamProxy is --upstream-proxy: this runner's own corporate upstream proxy URL, validated at
	// startup and handed to every run's proxy. It is never sent to the organisation.
	upstreamProxy string
	// substrate is --substrate: docker (default) or k8s. The rest are k8s only (design K1 §3.3).
	substrate, runsNamespace, relayService, arch string
	maxPodLifetime                               time.Duration
	// folderRoots is --folder-root: the folders a --substrate folders runner shares (default: home).
	folderRoots []string
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
	var org, stateDir, configPath string
	var opts serveOptions
	cmd := &cobra.Command{Use: "serve", Short: "Connect this registered runner to its organisation", Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) (retErr error) {
		cfg, err := serveConfig(cmd, configPath)
		if err != nil {
			return err
		}
		cfg.applyTo(&org, &stateDir, &opts)
		if org == "" {
			return errors.New(`required flag(s) "org" not set`)
		}
		// The Docker client reads DOCKER_HOST itself. A setting from the profile, the environment or the file
		// is handed to it here; with none, the engine is the one the docker CLI's context names.
		if h := cfg.get("docker_host"); h != "" {
			if err := os.Setenv("DOCKER_HOST", h); err != nil {
				return err
			}
		} else if err := followDockerContext(); err != nil {
			return err
		}
		// http.ProxyFromEnvironment reads the variable once, at the first connection.
		if f := cfg.field("runner_proxy"); strings.HasPrefix(f.Source, sourceFile) {
			if err := os.Setenv("HTTPS_PROXY", f.Value); err != nil {
				return err
			}
		}
		if stateDir == "" {
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
		opts.proxyImagePinned = proxyImagePinned(opts.proxyImage, identity.ProxyImage, proxyImageGiven(cfg), os.Getenv)
		if !opts.proxyImagePinned {
			opts.proxyImage = mirrorProxyImage(opts.proxyImageMirror, cmp.Or(identity.ProxyImage, releaseProxyImage(), opts.proxyImage))
		} else {
			warnPinnedProxyImageSkew(opts.proxyImage, buildVersion())
		}
		if err := trustedCAAtStartup(cmd.Context(), &opts, stateDir); err != nil {
			return err
		}
		if err := proxy.ValidUpstreamProxyURL(opts.upstreamProxy); err != nil {
			return fmt.Errorf("--upstream-proxy / WARDYN_UPSTREAM_PROXY / runnerd.json upstream_proxy (%s): %w (the URL is not repeated here: it may hold a credential)", cfg.field("upstream_proxy").Source, err)
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
		httpClient, err := orgHTTPClient(opts.trustedCA)
		if err != nil {
			return err
		}
		client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: org, RunnerID: identity.RunnerID.String(), PrivateKey: identity.PrivateKey, Hooks: runtime.hooks, HTTPClient: httpClient})
		if err != nil {
			return err
		}
		slog.InfoContext(cmd.Context(), "runner's connection to the organisation", "route", runnerwire.OrgRoute(org, identity.RunnerID.String()))
		return runRunnerServices(cmd.Context(), client.Run, runtime.run)
	}
	cmd.Flags().StringVar(&org, "org", "", "organisation public HTTPS URL used at registration (default: the OrgURL managed preference)")
	cmd.Flags().StringVar(&configPath, "config", "", "the runner's settings file (default: <user configuration directory>/wardyn/runnerd.json, which may be absent); the file never holds a credential")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "private runner state directory (defaults to the user configuration directory)")
	cmd.Flags().StringVar(&opts.proxyImage, "proxy-image", "", "Wardyn proxy image matching this daemon version (defaults to the image the organisation named at registration, else the release image on a release build)")
	cmd.Flags().StringVar(&opts.relayListen, "relay-listen", "", "address the run relay listens on: loopback, the native Docker bridge's gateway or Rancher Desktop's veth-rd-wsl address (found by a probe container when left out)")
	cmd.Flags().StringVar(&opts.relayHostTarget, "relay-host-target", "", "what a run's proxy reaches the relay at: host-gateway or an IP address (found by a probe container when left out)")
	cmd.Flags().StringVar(&opts.network, "internal-network", "", "Docker bridge for proxy sidecars (created when missing; default wardyn-internal)")
	cmd.Flags().StringVar(&opts.trustedCAFile, "trusted-ca-file", "", "PEM bundle of extra roots (e.g. a TLS-inspecting corporate proxy's CA) that runnerd's own org session and every run's proxy and sandbox trust "+
		"beside the system roots; 64 KiB at most. SSL_CERT_FILE and SSL_CERT_DIR are not read. Read at startup only")
	cmd.Flags().StringVar(&opts.upstreamProxy, "upstream-proxy", "", "this runner's own corporate upstream proxy, http://[user:pass@]host[:port], that every run's proxy chains its egress through; "+
		"never sent to the organisation. Prefer the WARDYN_UPSTREAM_PROXY environment variable when it holds a credential (it is read after the flags are parsed, so --help never shows it). Read at startup only")
	cmd.Flags().StringVar(&opts.recording, "recording-store", "", "organisation recording mode: pg, fs or off (off disables local capture; default pg)")
	cmd.Flags().StringVar(&opts.substrate, "substrate", "docker", "where runs execute: docker (this host's engine) or k8s (a runs namespace of the cluster runnerd runs in); "+
		"folders runs nothing and only shares folders on this computer with runs elsewhere")
	cmd.Flags().StringArrayVar(&opts.folderRoots, "folder-root", nil, "folders: a folder this runner may share, with everything under it (repeatable; default: your home folder)")
	cmd.Flags().StringVar(&opts.runsNamespace, "runs-namespace", "", "k8s: the dedicated namespace runs are created in; never runnerd's own")
	cmd.Flags().StringVar(&opts.relayService, "relay-service", "", "k8s: the relay Service's DNS name (<name>.<namespace>.svc.<cluster domain>), which must resolve to exactly one ClusterIP")
	cmd.Flags().StringVar(&opts.arch, "arch", os.Getenv(archEnv), "k8s, required: the sandbox node architecture, amd64 or arm64, pinned as every run pod's kubernetes.io/arch node selector")
	cmd.Flags().DurationVar(&opts.maxPodLifetime, "max-pod-lifetime", 0, "k8s: the most any run pod may live (activeDeadlineSeconds), e.g. 24h")
	return cmd
}

// serveConfig resolves every setting for serve: the flags the command line gave, the managed preferences,
// the environment, then runnerd.json. Read here, not when the command is built, so only serve pays for it.
func serveConfig(cmd *cobra.Command, configPath string) (runnerConfig, error) {
	flag := func(name string) (string, bool) {
		v, err := cmd.Flags().GetString(name)
		return v, err == nil && cmd.Flags().Changed(name)
	}
	return effectiveRunnerConfig(configPath, flag)
}

// applyTo copies the effective settings into serve's own variables.
func (c runnerConfig) applyTo(org, stateDir *string, o *serveOptions) {
	*org, *stateDir = c.get("org_url"), c.get("state_dir")
	o.proxyImage, o.proxyImageMirror = c.get("proxy_image"), c.get("proxy_image_mirror")
	o.relayListen, o.relayHostTarget, o.network = c.get("relay_listen"), c.get("relay_host_target"), c.get("internal_network")
	o.trustedCAFile, o.upstreamProxy, o.recording = c.get("trusted_ca_file"), c.get("upstream_proxy"), c.get("recording_store")
}
