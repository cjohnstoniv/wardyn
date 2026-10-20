// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
)

// doctorOptions are `doctor`'s flags. A setting left out is read the way the service reads it: from the
// settings setup recorded for the service, then from this shell.
type doctorOptions struct {
	org, stateDir, proxyImage, relayListen, relayHostTarget, trustedCAFile string
	// serverVersion and runnerOnly serve `wardyn doctor`, which has run the network stages itself.
	serverVersion    string
	json, runnerOnly bool
}

// newDoctorCommand is `wardyn-runnerd doctor`: the connection checks `wardyn doctor` runs, then this runner's
// own: the engine, the proxy image and the relay. It writes nothing and changes no setting.
func newDoctorCommand() *cobra.Command {
	var o doctorOptions
	cmd := &cobra.Command{Use: "doctor", Short: "Check this runner's connection to its organisation and its local engine, read-only", Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		host, err := osSetupHost()
		if err != nil {
			return err
		}
		report := runDoctor(cmd.Context(), host, o, os.Getenv, doctor.Network{})
		write := report.WriteText
		if o.json {
			write = report.WriteJSON
		}
		if err := write(cmd.OutOrStdout()); err != nil {
			return err
		}
		if !report.OK() {
			return errors.New("doctor: one or more stages failed")
		}
		return nil
	}
	fl := cmd.Flags()
	fl.StringVar(&o.org, "org", "", "organisation public HTTPS URL (default: the OrgURL managed preference, else the service's setting)")
	fl.StringVar(&o.stateDir, "state-dir", "", "private runner state directory (default: the one setup used)")
	fl.StringVar(&o.proxyImage, "proxy-image", "", "Wardyn proxy image to check (default: the one the service uses)")
	fl.StringVar(&o.relayListen, "relay-listen", "", "address the run relay listens on (default: the service's setting)")
	fl.StringVar(&o.relayHostTarget, "relay-host-target", "", "what a run's proxy reaches the relay at (default: the service's setting)")
	fl.StringVar(&o.trustedCAFile, "trusted-ca-file", "", "PEM bundle of extra roots (default: the service's setting)")
	fl.BoolVar(&o.json, "json", false, "print the stages as JSON")
	fl.BoolVar(&o.runnerOnly, "runner-only", false, "run only the engine, proxy image and relay stages")
	fl.StringVar(&o.serverVersion, "server-version", "", "the organisation's version, for the proxy image's skew check when --runner-only")
	_ = cmd.Flags().MarkHidden("runner-only")
	_ = cmd.Flags().MarkHidden("server-version")
	return cmd
}

// doctorSettings reads each setting the way the service does, and says where it came from.
type doctorSettings struct {
	paths  setupPaths
	getenv func(string) string
}

// get is flag's value, else key as setup recorded it for the service, else key in this shell's environment.
func (s doctorSettings) get(flagName, flag, key string) (value, source string) {
	switch {
	case flag != "":
		return flag, "--" + flagName
	case s.paths.setting(key) != "":
		return s.paths.setting(key), key + " in the service's settings"
	case s.getenv(key) != "":
		return s.getenv(key), key
	}
	return "", ""
}

// runDoctor runs every stage. net carries what a test replaces (the resolver, the proxy function, the
// trust store); its organisation fields are filled here.
func runDoctor(ctx context.Context, h setupHost, o doctorOptions, getenv func(string) string, net doctor.Network) doctor.Report {
	var stages []doctor.Stage
	paths, err := h.paths()
	if err != nil {
		return doctor.Report{Stages: []doctor.Stage{doctor.FailStage("state", err.Error(), "WARDYN_RUNNER_STATE_DIR", "the directory of the identity this computer's service runs")}}.Scrubbed()
	}
	s := doctorSettings{paths, getenv}
	org, orgSource := s.get("org", o.org, "WARDYN_RUNNER_ORG_URL")
	if pref := managedPref("OrgURL"); o.org == "" && pref != "" {
		org, orgSource = pref, "the OrgURL managed preference"
	}
	serverVersion := o.serverVersion
	if !o.runnerOnly {
		var network []doctor.Stage
		network, serverVersion = runnerNetwork(ctx, s, o, org, orgSource, &net)
		stages = append(stages, network...)
	}
	return doctor.Report{Stages: append(stages, h.engineStages(ctx, s, o, org, serverVersion)...)}.Scrubbed()
}

// runnerNetwork is the network stages for the organisation this runner talks to, with the trusted-ca stage
// before them: the runner's session trusts that bundle beside the system roots.
func runnerNetwork(ctx context.Context, s doctorSettings, o doctorOptions, org, orgSource string, net *doctor.Network) ([]doctor.Stage, string) {
	var stages []doctor.Stage
	file, _ := s.get("trusted-ca-file", o.trustedCAFile, "WARDYN_TRUSTED_CA_FILE")
	switch {
	case file == "":
		stages = append(stages, doctor.SkipStage("trusted-ca", "no extra CA bundle is set: the system roots only"))
	default:
		bundle, n, err := loadTrustedCA(file)
		if err != nil {
			stages = append(stages, doctor.FailStage("trusted-ca", err.Error(), "WARDYN_TRUSTED_CA_FILE", "a PEM file of certificates (a proxy that inspects TLS: its root CA), 64 KiB at most"))
			break
		}
		net.ExtraCA = x509.NewCertPool()
		net.ExtraCA.AppendCertsFromPEM([]byte(bundle))
		detail := fmt.Sprintf("%d certificate(s) from %s", n, filepath.Base(file))
		_, warnings := trustedCAFileCheck(file, time.Now())
		stages = append(stages, doctor.OKStage("trusted-ca", detail+strings.Join(append([]string{""}, warnings...), "; warning: ")))
	}
	net.OrgURL, net.Source, net.OrgSetting, net.Strict = org, orgSource, "WARDYN_RUNNER_ORG_URL", true
	if orgSource == "" {
		net.Source = "nothing set"
	}
	net.ExtraCASource, net.ExtraCATrusted = "WARDYN_TRUSTED_CA_FILE", true
	if s.paths.installedService() != "" {
		net.ProxyNote = ": this shell's settings, which the installed service does not receive"
	}
	return append(stages, net.Run(ctx)...), net.ServerVersion
}

// engineStages are the runner's own stages: the local engine, the proxy image and the relay.
func (h setupHost) engineStages(ctx context.Context, s doctorSettings, o doctorOptions, org, serverVersion string) []doctor.Stage {
	if v, _ := s.get("", "", "DOCKER_HOST"); v != "" {
		// The engine the service uses, for this process only.
		_ = os.Setenv("DOCKER_HOST", v)
		h.dockerHost = v
	}
	want, wantErr := doctorRelayWant(s, o)
	eng, engErr := h.inspect(ctx, want)
	engine := engineStage(h.dockerHost, eng, engErr)
	image, state, imageStage := h.imageStage(ctx, s, o, org, serverVersion, engine.Status == doctor.StatusOK)
	return []doctor.Stage{engine, imageStage, h.relayStage(ctx, s, image, state, eng, want, wantErr, engine.Status == doctor.StatusOK)}
}

func engineStage(host string, eng dockerEngine, err error) doctor.Stage {
	switch {
	case errors.Is(err, errSetupNeedsDocker):
		return doctor.SkipStage("engine", "this wardyn-runnerd was built without Docker support")
	case err != nil:
		return doctor.FailStage("engine", oneLine(err.Error()), "DOCKER_HOST", "the engine's socket if it is not the default (on Linux, also add yourself to the docker group)")
	}
	kind := "native"
	if !eng.native {
		kind = "VM-backed: " + eng.why
	}
	return doctor.OKStage("engine", fmt.Sprintf("%s answers: Docker Engine %s, %s (%s)", host, eng.version, eng.desc, kind))
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

func doctorRelayWant(s doctorSettings, o doctorOptions) (relayPair, error) {
	listen, _ := s.get("relay-listen", o.relayListen, "WARDYN_RELAY_LISTEN")
	target, _ := s.get("relay-host-target", o.relayHostTarget, "WARDYN_RELAY_HOST_TARGET")
	return parseRelayWant(listen, target)
}

// proxyImageChoice is the image the service would run: --proxy-image, the service's setting, the image the
// organisation named at registration, else this binary's release image. pinned is serve's own rule
// (proxyImagePinned) applied to it.
func (h setupHost) proxyImageChoice(s doctorSettings, o doctorOptions, org string) (image string, pinned bool) {
	var fromIdentity string
	stateDir := s.paths.stateDir
	if o.stateDir != "" {
		stateDir, _ = filepath.Abs(o.stateDir)
	}
	if org != "" {
		if id, err := runneridentity.Load(stateDir, org); err == nil {
			fromIdentity = id.ProxyImage
		}
	}
	given, _ := s.get("proxy-image", o.proxyImage, "WARDYN_PROXY_IMAGE")
	getenv := func(k string) string { v, _ := s.get("", "", k); return v }
	if given == "" {
		return cmp.Or(fromIdentity, releaseProxyImage()), false
	}
	return given, proxyImagePinned(given, fromIdentity, o.proxyImage != "", getenv)
}

// imageStage checks the image resolves on this engine or from its registry, and, when it is pinned, that its
// release is the organisation's.
func (h setupHost) imageStage(ctx context.Context, s doctorSettings, o doctorOptions, org, serverVersion string, engineUp bool) (image, state string, st doctor.Stage) {
	image, pinned := h.proxyImageChoice(s, o, org)
	const fix = "an image this engine can pull for this computer's architecture (docker pull it as this user first when its registry needs a login)"
	switch {
	case image == "":
		return image, "", doctor.FailStage("proxy-image", "this is a development build and no proxy image is named", "WARDYN_PROXY_IMAGE", fix)
	case !engineUp:
		return image, "", doctor.SkipStage("proxy-image", "not checked: the engine did not answer")
	}
	state, err := h.image(ctx, image)
	switch {
	case err != nil:
		return image, "", doctor.FailStage("proxy-image", fmt.Sprintf("%s cannot be found on this engine or in its registry: %s", image, oneLine(err.Error())), "WARDYN_PROXY_IMAGE", fix)
	case pinned && serverVersion != "" && releaseVersion.MatchString(serverVersion) && imageReleaseTag(image) != "" && imageReleaseTag(image) != serverVersion:
		return image, state, doctor.FailStage("proxy-image", fmt.Sprintf("version skew: the pinned image %s is release %s but the organisation is release %s, and a proxy of another release may refuse what this runner sends",
			image, imageReleaseTag(image), serverVersion), "WARDYN_PROXY_IMAGE", "the release the organisation runs, or remove the pin (WARDYN_PROXY_IMAGE_PINNED) to follow the organisation")
	}
	where := map[string]string{"local": "is on this engine", "registry": "is not on this engine yet but its registry has it"}[state]
	return image, state, doctor.OKStage("proxy-image", image+" "+where)
}

// relayStage is setup's own relay check (findRelay, through h.relay) on a throwaway container, run only
// when the image is already on the engine: doctor never pulls.
func (h setupHost) relayStage(ctx context.Context, s doctorSettings, image, state string, eng dockerEngine, want relayPair, wantErr error, engineUp bool) doctor.Stage {
	const fix = "an address of this host that the engine's containers reach (WARDYN_RELAY_HOST_TARGET is what they reach it at: host-gateway or an IP address)"
	switch {
	case wantErr != nil:
		return doctor.FailStage("relay", wantErr.Error(), "WARDYN_RELAY_LISTEN", fix)
	case !engineUp:
		return doctor.SkipStage("relay", "not checked: the engine did not answer")
	case want.bind.IsValid():
		if err := admitRelayBind(want.bind, eng); err != nil {
			return doctor.FailStage("relay", oneLine(err.Error()), "WARDYN_RELAY_LISTEN", fix)
		}
	}
	if state != "local" {
		return doctor.SkipStage("relay", "not checked: the proxy image is not on this engine, and doctor never pulls it")
	}
	network, _ := s.get("internal-network", "", "WARDYN_INTERNAL_NETWORK")
	network = cmp.Or(network, "wardyn-internal")
	// setup and serve create the proxy bridge; doctor only looks, so a missing one is its own finding.
	switch ok, err := h.hasNetwork(ctx, network); {
	case err != nil:
		return doctor.FailStage("relay", fmt.Sprintf("could not look for the Docker network %s: %s", network, oneLine(err.Error())), "WARDYN_INTERNAL_NETWORK", "the engine answering as this user (see the engine stage)")
	case !ok:
		return doctor.FailStage("relay", fmt.Sprintf("the Docker network %s that run proxies attach to does not exist on this engine", network), "WARDYN_INTERNAL_NETWORK",
			"the name of an existing network, or run `wardyn-runnerd setup` or restart the runner service, which creates "+network)
	}
	pair, err := h.relay(ctx, image, network, eng, want)
	if err != nil {
		return doctor.FailStage("relay", oneLine(err.Error()), "WARDYN_RELAY_LISTEN", fix)
	}
	return doctor.OKStage("relay", fmt.Sprintf("the relay listens on %s and a run's proxy reaches it at %s (checked with a throwaway container)", pair.bind, pair.target))
}
