// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/types"
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

// doctorSettings reads each setting the way the service does, and says where it came from. file is runnerd.json,
// where setup records them.
type doctorSettings struct {
	paths  setupPaths
	getenv func(string) string
	file   runnerFile
}

// get is key's value and where it came from, resolved as serve resolves it (runnerField.resolve): the flag, the
// managed preference, the environment, then runnerd.json. The environment is the service's: the definition a
// setup before runnerd.json wrote, else this shell's. A key that is no runnerd.json field is read from the
// definition, then the shell.
func (s doctorSettings) get(flagName, flag, key string) (value, source string) {
	f, ok := fieldFor(key)
	if !ok {
		if v := s.paths.setting(key); v != "" {
			return v, key + " in the service's settings"
		}
		if v := s.getenv(key); v != "" {
			return v, key
		}
		return "", ""
	}
	return s.resolve(f, flagName, flag, key)
}

// org is the organisation's address and where it came from, resolved as serve does: --org, the OrgURL managed
// preference, then org_url in runnerd.json. serve has no environment variable for it, so this shell's is never
// read. An earlier install's unit passes --org ${WARDYN_RUNNER_ORG_URL} from its service definition (the env
// file), and serve ranks that argument as a flag, so an installed definition that passes one is doctor's flag
// when --org is not given. An environment file beside a unit that passes no --org is not read at all; only
// with no installed definition does that file name the address.
func (s doctorSettings) org(flag string) (value, source string) {
	const legacy = "WARDYN_RUNNER_ORG_URL"
	legacySource := ""
	if v, from := s.paths.serviceOrgArg(); flag == "" && v != "" {
		flag, legacySource = v, from
	}
	i := slices.IndexFunc(runnerFields, func(f runnerField) bool { return f.name == "org_url" })
	if value, source = s.resolve(runnerFields[i], "org", flag, "org_url"); value != "" {
		if legacySource != "" && source == "--org" {
			source = legacySource
		}
		return value, source
	}
	if s.paths.installedService() == "" {
		if v := s.paths.setting(legacy); v != "" {
			return v, legacy + " in the service's settings"
		}
	}
	return "", ""
}

// resolve is f's value and source; key names it in a source that is no flag or managed preference.
func (s doctorSettings) resolve(f runnerField, flagName, flag, key string) (value, source string) {
	return s.sourceOf(s.resolved(f, flag), f, flagName, key)
}

// resolved is f as the service resolves it.
func (s doctorSettings) resolved(f runnerField, flag string) resolvedField {
	in := runnerInputs{getenv: func(k string) string { return cmp.Or(s.paths.setting(k), s.getenv(k)) },
		managed: managedPref, lockRead: managedLockRead, file: s.file, filePath: s.paths.configFile()}
	if flag != "" {
		in.flag = func(name string) (string, bool) { return flag, name == f.flag }
	}
	in.locked, _ = securityLocked(in)
	return f.resolve(in)
}

// sourceOf is resolve's value and source, in the words doctor names a source in.
func (s doctorSettings) sourceOf(r resolvedField, f runnerField, flagName, key string) (value, source string) {
	switch {
	case r.Value == "" || r.Source == sourceDefault:
		return "", ""
	case strings.HasPrefix(r.Source, sourceFlag):
		return r.Value, "--" + flagName
	case strings.HasPrefix(r.Source, sourceManaged):
		return r.Value, "the " + f.managed + " managed preference"
	case strings.HasPrefix(r.Source, sourceEnv):
		if name := strings.TrimPrefix(r.Source, sourceEnv+" "); s.paths.setting(name) == "" {
			return r.Value, name
		}
	}
	return r.Value, key + " in the service's settings"
}

// driveRootStage says which folders drives bound to this runner may mount from, and where that setting came
// from, the way the other settings are named; a value serve would refuse fails it. With none set there is no
// stage, so a runner that offers no drive mounts reports what it always did.
func driveRootStage(s doctorSettings) (doctor.Stage, bool) {
	const setting, fix = "drive_root", "absolute, cleaned folders: `wardyn-runnerd setup --drive-root <dir>` (repeatable) writes runnerd.json's drive_root; the DriveRoot managed preference or WARDYN_DRIVE_ROOT overrides it"
	i := slices.IndexFunc(runnerFields, func(f runnerField) bool { return f.name == setting })
	f := runnerFields[i]
	r := s.resolved(f, "")
	if err := (runnerConfig{r}).checkDriveRoots("docker"); err != nil {
		return doctor.FailStage("drive-roots", oneLine(err.Error()), setting, fix), true
	}
	if len(r.List) == 0 {
		return doctor.Stage{}, false
	}
	_, source := s.sourceOf(r, f, "", setting)
	return doctor.OKStage("drive-roots", strings.Join(r.List, ", ")+" (from "+source+")"), true
}

// fieldFor is the runnerd.json field the environment variable key stands for.
func fieldFor(key string) (runnerField, bool) {
	i := slices.IndexFunc(runnerFields, func(f runnerField) bool {
		return slices.Contains(f.env, key)
	})
	if i < 0 {
		return runnerField{}, false
	}
	return runnerFields[i], true
}

// stateDir is --state-dir, else the one setup used.
func (s doctorSettings) stateDir(o doctorOptions) string {
	if o.stateDir != "" {
		abs, _ := filepath.Abs(o.stateDir)
		return abs
	}
	return s.paths.stateDir
}

// runDoctor runs every stage. net carries what a test replaces (the resolver, the proxy function, the
// trust store); its organisation fields are filled here.
func runDoctor(ctx context.Context, h setupHost, o doctorOptions, getenv func(string) string, net doctor.Network) doctor.Report {
	paths, err := h.paths()
	if err != nil {
		return doctor.Report{Stages: []doctor.Stage{doctor.FailStage("state", err.Error(), "WARDYN_RUNNER_STATE_DIR", "the directory of the identity this computer's service runs")}}.Scrubbed()
	}
	s := doctorSettings{paths: paths, getenv: getenv}
	var fileErr error
	s.file, fileErr = loadRunnerFile(paths.configFile(), false)
	org, orgSource := s.org(o.org)
	conn := runnerConn{org: org, orgSource: orgSource}
	conn.trustedCA, _ = s.get("trusted-ca-file", o.trustedCAFile, "WARDYN_TRUSTED_CA_FILE")
	conn.orgCA, _ = s.get("", "", "WARDYN_ORG_CA_FILE")
	// The runner's connection goes through the HTTPS_PROXY and NO_PROXY serve exports: the environment's, else
	// runnerd.json's runner_proxy and runner_no_proxy.
	proxy, proxySource := s.get("", "", "HTTPS_PROXY")
	noProxy, _ := s.get("", "", "NO_PROXY")
	if proxy != "" {
		conn.proxyFor = proxyFunc(proxy, noProxy)
	}
	if proxySource != "" && !strings.HasSuffix(proxySource, "in the service's settings") && paths.installedService() != "" {
		conn.proxyNote = ": this shell's settings, which the installed service does not receive"
	}
	stages, serverVersion := h.connectionStages(ctx, paths, s.stateDir(o), conn, &net, !o.runnerOnly)
	if o.runnerOnly {
		serverVersion = o.serverVersion
	}
	stages = append(stages, h.engineStages(ctx, s, o, org, serverVersion)...)
	if st, ok := driveRootStage(s); ok {
		stages = append(stages, st)
	}
	stages = append(stages, macOSVMDoctorStages()...)
	if fileErr != nil {
		bad := doctor.FailStage("config", oneLine(fileErr.Error()), "", "fix that field, or remove the file and run `wardyn-runnerd setup` again")
		stages = append([]doctor.Stage{bad}, stages...)
	}
	return doctor.Report{Stages: stages}.Scrubbed()
}

// runnerConn is what the connection stages are told about the runner's organisation: its address and where
// that came from, the CA bundles the runner trusts for it (trustedCA is WARDYN_TRUSTED_CA_FILE, orgCA is
// WARDYN_ORG_CA_FILE) and a note for the proxy stage about what the diagnosed environment lacks.
type runnerConn struct {
	org, orgSource, trustedCA, orgCA, proxyNote string
	// proxyFor routes the connection; nil follows this shell's environment.
	proxyFor func(*http.Request) (*url.URL, error)
}

// connectionStages are the stages `doctor` and `setup` both run, in order: the CA bundles and the network
// stages, when network is set, then the service and the runner's status. It also returns the organisation's
// version, "" when /healthz did not report one.
func (h setupHost) connectionStages(ctx context.Context, p setupPaths, stateDir string, c runnerConn, net *doctor.Network, network bool) (stages []doctor.Stage, serverVersion string) {
	if network {
		stages, serverVersion = runnerNetwork(ctx, c, net)
	}
	return append(stages, h.serviceStage(ctx, p), statusStage(statusPath(stateDir))), serverVersion
}

// runnerNetwork is the network stages for the organisation this runner talks to, with the CA stages
// before them: the runner's session trusts those bundles beside the system roots.
func runnerNetwork(ctx context.Context, c runnerConn, net *doctor.Network) ([]doctor.Stage, string) {
	var stages []doctor.Stage
	pool, sources := x509.NewCertPool(), []string(nil)
	switch {
	case c.trustedCA == "":
		stages = append(stages, doctor.SkipStage("trusted-ca", "no extra CA bundle is set: the system roots only"))
	default:
		bundle, n, err := loadTrustedCA(c.trustedCA)
		if err != nil {
			stages = append(stages, doctor.FailStage("trusted-ca", err.Error(), "WARDYN_TRUSTED_CA_FILE", "a PEM file of certificates (a proxy that inspects TLS: its root CA), 64 KiB at most"))
			break
		}
		pool.AppendCertsFromPEM([]byte(bundle))
		sources = append(sources, "WARDYN_TRUSTED_CA_FILE")
		detail := fmt.Sprintf("%d certificate(s) from %s", n, filepath.Base(c.trustedCA))
		_, warnings := trustedCAFileCheck(c.trustedCA, time.Now())
		stages = append(stages, doctor.OKStage("trusted-ca", detail+strings.Join(append([]string{""}, warnings...), "; warning: ")))
	}
	if c.orgCA != "" {
		bundle, n, err := loadOrgCA(c.orgCA)
		if err != nil {
			stages = append(stages, doctor.FailStage("org-ca", err.Error(), "WARDYN_ORG_CA_FILE", "a PEM file of the roots this runner's own connection to the organisation trusts"))
		} else {
			pool.AppendCertsFromPEM([]byte(bundle))
			sources = append(sources, "WARDYN_ORG_CA_FILE")
			stages = append(stages, doctor.OKStage("org-ca", fmt.Sprintf("%d certificate(s) from %s", n, filepath.Base(c.orgCA))))
		}
	}
	if len(sources) > 0 {
		net.ExtraCA, net.ExtraCASource = pool, strings.Join(sources, " and ")
	}
	net.OrgURL, net.Source, net.OrgSetting, net.Strict = c.org, c.orgSource, orgSetting, true
	if c.orgSource == "" {
		net.Source = "nothing set"
	}
	net.ExtraCATrusted, net.Wire, net.ProxyNote = true, true, c.proxyNote
	if net.ProxyFor == nil {
		net.ProxyFor = c.proxyFor
	}
	return append(stages, net.Run(ctx)...), net.ServerVersion
}

// serviceStage says whether the service setup installed is there and, where systemd can say, running. No
// service is not a failure: --no-service runs the runner by hand.
func (h setupHost) serviceStage(ctx context.Context, p setupPaths) doctor.Stage {
	file := p.installedService()
	if file == "" {
		return doctor.SkipStage("service", "no service is installed for this runner: it runs only where you start `wardyn-runnerd serve`, and `wardyn-runnerd setup` installs one")
	}
	if h.goos != "linux" || h.run == nil {
		return doctor.OKStage("service", "installed ("+filepath.Base(file)+"); whether it is running is not checked on this OS")
	}
	out, err := h.run(ctx, "systemctl", "--user", "is-active", serviceUnit)
	state := strings.TrimSpace(string(out))
	switch {
	case err == nil, state == "activating", state == "reloading":
		return doctor.OKStage("service", fmt.Sprintf("%s is %s", serviceUnit, cmp.Or(state, "active")))
	case state == "":
		return doctor.SkipStage("service", "installed, but systemd did not say whether it is running: "+oneLine(err.Error()))
	}
	return doctor.FailStage("service", fmt.Sprintf("%s is installed but systemd reports it %s", serviceUnit, state), "",
		"start it with `systemctl --user start "+serviceUnit+"`; `journalctl --user -u wardyn-runnerd` says why it stopped")
}

// orgSetting names where the organisation's address is set, for every hint that fixes it: the settings serve
// reads, and no others.
const orgSetting = "the organisation address (--org, the OrgURL managed preference, or org_url in runnerd.json)"

// statusSetting is the setting, and what to set it to, that fixes a connection that failed at stage.
var statusSetting = map[string][2]string{
	types.RunnerStageDNS:        {orgSetting, "an address that resolves from this computer (or fix its DNS or VPN)"},
	types.RunnerStageProxy:      {"HTTPS_PROXY", "a proxy address the service environment can reach, or unset it (NO_PROXY lists hosts to reach directly)"},
	types.RunnerStageTLS:        {"WARDYN_TRUSTED_CA_FILE", "a PEM file holding the root CA of the certificate the organisation presents"},
	types.RunnerStageHealthz:    {orgSetting, "your organisation's public HTTPS address"},
	types.RunnerStageEngine:     {"DOCKER_HOST", "the engine's socket if it is not the default"},
	types.RunnerStageProxyImage: {"WARDYN_PROXY_IMAGE", "an image this engine can pull for this computer's architecture"},
	types.RunnerStageRelay:      {"WARDYN_RELAY_LISTEN", "an address of this host that the engine's containers reach"},
}

// statusStage reads the file the service writes (status.json) for its last error and when it happened. With
// no file the service has not run, and nothing is dialled to find out: this stage only reads.
func statusStage(path string) doctor.Stage {
	st, err := loadStatus(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return doctor.SkipStage("status", "the service has not run yet: there is no "+statusFile+" in "+filepath.Dir(path))
	case err != nil:
		return doctor.FailStage("status", oneLine(err.Error()), "WARDYN_RUNNER_STATE_DIR", "the directory of the identity this computer's service runs")
	}
	last := ""
	if st.LastError != "" && st.LastErrorAt != nil {
		last = fmt.Sprintf("; last error at %s: %s", st.LastErrorAt.UTC().Format(time.RFC3339), st.LastError)
	}
	if st.PeersOnlyRefusedBy != "" {
		last += "; warning: peers-only not in effect: organisation older (its proxy image " + st.PeersOnlyRefusedBy +
			" refused allowed_peers, so run proxies serve any peer until it is upgraded)"
	}
	since := st.Since.UTC().Format(time.RFC3339)
	// A failure is a runner that is not connected and either stopped at a stage or is offline. An error left
	// from an earlier drop, on a runner that has since started or stopped cleanly, is history.
	if st.State == "connected" || (st.Stage == "" && st.State != "offline") {
		return doctor.OKStage("status", fmt.Sprintf("%s since %s%s", st.State, since, last))
	}
	set := statusSetting[st.Stage]
	if st.Stage == types.RunnerStageIdentity {
		return doctor.FailStage("status", fmt.Sprintf("%s since %s (stage identity)%s", st.State, since, last), "",
			"register this runner again: the organisation refused the proof it signs with its stored key (revoked or unknown)")
	}
	if set[0] == "" {
		set = [2]string{orgSetting, "your organisation's public HTTPS address"}
	}
	return doctor.FailStage("status", fmt.Sprintf("%s since %s (stage %s)%s", st.State, since, cmp.Or(st.Stage, "none"), last), set[0], set[1])
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
	if org != "" {
		if id, err := runneridentity.Load(s.stateDir(o), org); err == nil {
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
	if err != nil {
		return image, "", doctor.FailStage("proxy-image", fmt.Sprintf("%s cannot be found on this engine or in its registry: %s", image, oneLine(err.Error())), "WARDYN_PROXY_IMAGE", fix)
	}
	if skew, ok := proxyImageSkew(image, pinned, serverVersion); ok {
		return image, state, skew
	}
	where := map[string]string{"local": "is on this engine", "registry": "is not on this engine yet but its registry has it"}[state]
	return image, state, doctor.OKStage("proxy-image", image+" "+where)
}

// proxyImageSkew is the failure for a pinned image whose release is not the organisation's, and whether
// there is one.
func proxyImageSkew(image string, pinned bool, serverVersion string) (doctor.Stage, bool) {
	if !pinned || serverVersion == "" || !releaseVersion.MatchString(serverVersion) || imageReleaseTag(image) == "" || imageReleaseTag(image) == serverVersion {
		return doctor.Stage{}, false
	}
	return doctor.FailStage("proxy-image", fmt.Sprintf("version skew: the pinned image %s is release %s but the organisation is release %s, and a proxy of another release may refuse what this runner sends",
		image, imageReleaseTag(image), serverVersion), "WARDYN_PROXY_IMAGE", "the release the organisation runs, or remove the pin (WARDYN_PROXY_IMAGE_PINNED) to follow the organisation"), true
}

// noPullKey marks a context whose relay probe must not pull the proxy image: doctor's contract.
type noPullKey struct{}

func withoutPulling(ctx context.Context) context.Context {
	return context.WithValue(ctx, noPullKey{}, true)
}

func pullsAllowed(ctx context.Context) bool { return ctx.Value(noPullKey{}) == nil }

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
		return doctor.FailStage("relay", fmt.Sprintf("could not look for the Docker network %s: %s", network, oneLine(err.Error())), "DOCKER_HOST", "the engine's socket if it is not the default (see the engine stage)")
	case !ok:
		return doctor.FailStage("relay", fmt.Sprintf("the Docker network %s that run proxies attach to does not exist on this engine", network), "WARDYN_INTERNAL_NETWORK",
			"the name of an existing network, or run `wardyn-runnerd setup` or restart the runner service, which creates "+network)
	}
	pair, err := h.relay(withoutPulling(ctx), image, network, eng, want)
	if err != nil {
		return doctor.FailStage("relay", oneLine(err.Error()), "WARDYN_RELAY_LISTEN", fix)
	}
	return doctor.OKStage("relay", fmt.Sprintf("the relay listens on %s and a run's proxy reaches it at %s (checked with a throwaway container)", pair.bind, pair.target))
}
