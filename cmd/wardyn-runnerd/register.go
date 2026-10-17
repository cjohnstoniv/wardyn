// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

func newRegisterCommand() *cobra.Command {
	var org, tokenFile, stateDir, name, sub, arch string
	var ifUnregistered bool
	var flags registerLimits
	cmd := &cobra.Command{Use: "register", Short: "Register this host with its limits; its owner must then verify and claim it", Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if sub != "docker" && sub != "k8s" {
			return fmt.Errorf("--substrate %q: want docker or k8s", sub)
		}
		if sub == "docker" && arch != "" {
			return fmt.Errorf("--arch (%s) applies only to --substrate=k8s: a Docker runner reports its engine's", archEnv)
		}
		if sub == "k8s" {
			if err := checkArch(arch); err != nil {
				return err
			}
		}
		if ifUnregistered {
			// The chart's init container runs on every pod start: a saved identity for this
			// organisation means registration already happened, and no token is read or spent.
			dir := stateDir
			if dir == "" {
				var err error
				if dir, err = runneridentity.DefaultDir(); err != nil {
					return err
				}
			}
			if _, err := runneridentity.Load(dir, org); err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "already registered; nothing to do")
				return nil
			}
		}
		done, err := registerRunner(cmd, registerArgs{org: org, tokenFile: tokenFile, stateDir: stateDir, name: name, limits: &flags, noMeasure: sub == "k8s", arch: arch})
		if err != nil {
			return err
		}
		// The limits it registered with stay on screen: with every limit given as a flag nothing else shows them.
		fmt.Fprintf(cmd.OutOrStdout(), "registered %s; key fingerprint %s\n  %s\n", done.name, done.fingerprint, limitsLine(done.limits))
		fmt.Fprintln(cmd.OutOrStdout(), "  claim it in the console (Your account → My runner) with this fingerprint, or\n"+
			"  WARDYN_TOKEN=<your personal API token> wardyn runner claim --pool <name> --default\n"+
			"  the admin token cannot claim; a claimed runner in no pool runs nothing")
		return nil
	}
	cmd.Flags().StringVar(&org, "org", "", "organisation public HTTPS URL")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "file containing the single-use registration token")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "private runner state directory (defaults to the user configuration directory)")
	cmd.Flags().StringVar(&name, "name", "", "runner name (defaults to the host name)")
	cmd.Flags().StringVar(&sub, "substrate", "docker", "where this runner's runs execute: docker or k8s (k8s measures nothing: give every total as a flag)")
	cmd.Flags().StringVar(&arch, "arch", os.Getenv(archEnv), "k8s, required: the sandbox node architecture this runner reports, amd64 or arm64")
	cmd.Flags().BoolVar(&ifUnregistered, "if-unregistered", false, "do nothing when this state directory already holds an identity for --org")
	flags.register(cmd)
	_ = cmd.MarkFlagRequired("org")
	_ = cmd.MarkFlagRequired("token-file")
	return cmd
}

func readRegistrationToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open registration token file: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", errors.New("read registration token file")
	}
	token := strings.TrimSpace(string(b))
	if len(b) > 4096 || len(token) != 68 || !strings.HasPrefix(token, "wdr_") {
		return "", errors.New("registration token file must contain one wdr_ token")
	}
	return token, nil
}

// registrationFailure keeps the server's own refusal and advises a fresh token
// only when the server refused this token or had already spent it. A timeout or a
// 5xx with no reason of its own says nothing about the token: the server may have
// spent it before it failed, so the person is sent to My runners, not back to the token.
func registrationFailure(err error) error {
	var refused *sdk.APIError
	if errors.As(err, &refused) && (refused.Reason == string(placement.ReasonRunnerTokenInvalid) || refused.Status == http.StatusConflict) {
		return fmt.Errorf("runner registration refused: %w. This token cannot be used again: obtain a fresh token and choose a new private state directory before retrying (the local key was preserved)", err)
	}
	if !errors.As(err, &refused) || (refused.Status >= http.StatusInternalServerError && refused.Reason == "") {
		return fmt.Errorf("runner registration failed: %w. The server may have spent the token before it failed: check Your account → My runners, and if this host is not there mint a new token and choose a new private state directory (the local key was preserved)", err)
	}
	return fmt.Errorf("runner registration failed: %w. The server did not refuse the token: retry with it and a new private state directory (the local key was preserved)", err)
}

// registerArgs are register's inputs: setup passes the same ones from its own flags.
type registerArgs struct {
	org, tokenFile, stateDir, name string
	limits                         *registerLimits
	// proxyImage is the image that measures a VM-backed engine's disk; "" is the release image.
	proxyImage string
	// noMeasure (k8s): no engine to measure or ask, so every total comes from a flag.
	noMeasure bool
	// arch (k8s) is --arch, reported in place of the engine's.
	arch string
}

// registeredRunner is what a registration left on this host and with the organisation.
type registeredRunner struct {
	name, fingerprint string
	limits            types.RunnerLimits
	// proxyImage is the image the organisation answered for this runner's architecture, as received
	// (so it can be unsafe), or "" when it named none.
	proxyImage string
}

// registerRunner measures this host, settles the limits, spends the token and saves the identity. A
// refused limit or a missing token file returns before anything is spent.
func registerRunner(cmd *cobra.Command, a registerArgs) (registeredRunner, error) {
	org, tokenFile, stateDir, name := a.org, a.tokenFile, a.stateDir, a.name
	if err := federation.CheckOrgURL(org); err != nil {
		return registeredRunner{}, err
	}
	// The limits are measured and checked before anything is spent: a refused limit keeps the token.
	var measured types.RunnerAmounts
	if !a.noMeasure {
		var err error
		if measured, err = measureWithVMDisk(cmd.Context(), a.proxyImage); err != nil {
			measured = types.RunnerAmounts{}
		}
		fmt.Fprintln(cmd.OutOrStdout(), reports(measured))
	}
	if in, out := cmd.InOrStdin(), cmd.OutOrStdout(); !a.noMeasure && asksAt(in, out) {
		if err := a.limits.ask(cmd, in, out, measured); err != nil {
			return registeredRunner{}, err
		}
	}
	limits, err := a.limits.limits(cmd, measured)
	if err != nil {
		return registeredRunner{}, err
	}
	if stateDir == "" {
		var err error
		stateDir, err = runneridentity.DefaultDir()
		if err != nil {
			return registeredRunner{}, err
		}
	}
	// The machine name is what the host calls itself, reported beside the display name; a host that
	// cannot say registers without one.
	machine, _ := os.Hostname()
	if name == "" {
		if machine == "" {
			return registeredRunner{}, errors.New("this host reports no name: pass --name")
		}
		name = machine
	}
	token, err := readRegistrationToken(tokenFile)
	if err != nil {
		return registeredRunner{}, err
	}
	key, err := runneridentity.Generate(stateDir)
	if err != nil {
		return registeredRunner{}, err
	}
	client := sdk.New(strings.TrimSpace(org), "")
	client.HTTPClient = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("runner registration does not follow redirects")
	}}
	arch := a.arch
	if arch == "" && !a.noMeasure {
		arch = engineArch(cmd.Context())
	}
	registered, err := client.RegisterRunner(cmd.Context(), sdk.RunnerRegisterRequest{Token: token, PublicKey: key.Public().(ed25519.PublicKey), Name: name, Limits: &limits, MachineName: machine, Arch: arch})
	if err != nil {
		return registeredRunner{}, registrationFailure(err)
	}
	fingerprint := runnerwire.Fingerprint(key.Public().(ed25519.PublicKey))
	if registered.RunnerID == uuid.Nil || registered.State != types.RunnerUnclaimed || registered.Fingerprint != fingerprint || registered.OrgURLSHA256 != federation.OrgURLSHA256(org) {
		return registeredRunner{}, errors.New("registration response does not match this key and organisation; identity was not activated")
	}
	id := runneridentity.Identity{RunnerID: registered.RunnerID, PrivateKey: key, Fingerprint: fingerprint, OrgURLSHA256: registered.OrgURLSHA256}
	out := cmd.OutOrStdout()
	switch image := registered.ProxyImage; {
	case image == "":
	case !types.SafeImageRef(image):
		fmt.Fprintf(out, "the organisation named a proxy image this runner cannot use (%q); it was not saved. Pass --proxy-image <ref> to serve or setup.\n", image)
	default:
		fmt.Fprintf(out, "proxy image for this runner (%s): %s\n", cmp.Or(arch, "architecture unknown"), image)
		// A release tag is not saved: it would pin a public tag that the next wardyn-runnerd upgrade
		// should move. runner.json is decoded strictly, so a 0.9.0 wardyn-runnerd cannot load an
		// identity that carries proxy_image.
		if image != releaseProxyImage() {
			id.ProxyImage = image
		}
	}
	if err := runneridentity.Save(stateDir, id); err != nil {
		return registeredRunner{}, err
	}
	return registeredRunner{name: name, fingerprint: fingerprint, limits: limits, proxyImage: registered.ProxyImage}, nil
}
