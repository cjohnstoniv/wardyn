// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// safeSetting limits a value that reaches a unit file, a plist or the env file, so nothing can inject a
// line, a quote or a placeholder.
var safeSetting = regexp.MustCompile(`^[A-Za-z0-9._:/@%+=-]+$`)

// errProxyImageUnavailable is a relay probe that never ran because the proxy image is not on the engine and
// could not be pulled.
var errProxyImageUnavailable = errors.New("the proxy image is not on this engine and could not be pulled")

// setupOptions are `setup`'s flags.
type setupOptions struct {
	org, tokenFile, name             string
	proxyImage, relayListen, network string
	relayHostTarget, trustedCAFile   string
	// orgCAFile is --org-ca-file: roots for this runner's own connection to the organisation only.
	orgCAFile                     string
	claim, makeDefault, noService bool
	pool                          string
	limits                        registerLimits
}

// newSetupCommand is the one command a person or device management runs to turn a downloaded
// wardyn-runnerd into a registered runner that starts at login. It is safe to run again: a runner that
// is already registered is not registered again, and the service files are rewritten and restarted.
func newSetupCommand() *cobra.Command {
	var o setupOptions
	cmd := &cobra.Command{Use: "setup", Short: "Register this computer, install wardyn-runnerd as a login service and start it (or, with --no-service, only write its environment)", Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		// On a Mac with the configuration profile, --org is optional.
		if !cmd.Flags().Changed("org") {
			o.org = cmp.Or(managedPref("OrgURL"), o.org)
		}
		if o.org == "" {
			return errors.New(`required flag(s) "org" not set`)
		}
		host, err := osSetupHost()
		if err != nil {
			return err
		}
		return runSetup(cmd, host, o)
	}
	fl := cmd.Flags()
	fl.StringVar(&o.org, "org", "", "organisation public HTTPS URL (default: the OrgURL managed preference)")
	fl.StringVar(&o.tokenFile, "token-file", "", "file containing the single-use registration token (not needed once this computer is registered)")
	fl.StringVar(&o.name, "name", "", "runner name (defaults to the host name)")
	fl.StringVar(&o.proxyImage, "proxy-image", "", "Wardyn proxy image (defaults to the release image matching this binary; a development build needs it)")
	fl.StringVar(&o.relayListen, "relay-listen", "", "address the run relay listens on (found by a probe container when left out)")
	fl.StringVar(&o.relayHostTarget, "relay-host-target", "", "what a run's proxy reaches the relay at: host-gateway or an IP address (found by a probe container when left out)")
	fl.StringVar(&o.trustedCAFile, "trusted-ca-file", "", "PEM bundle of extra roots (e.g. a TLS-inspecting corporate proxy's CA) every run's proxy and sandbox trust; kept when setup runs again without it")
	fl.StringVar(&o.orgCAFile, "org-ca-file", os.Getenv("WARDYN_ORG_CA_FILE"), "PEM file of extra roots (e.g. a corporate root) that this runner's own connection to the organisation trusts, registration included, and nothing else: no run's proxy or sandbox gets them; "+
		"not writable by group or others, owned by you or root; kept when setup runs again without it")
	fl.StringVar(&o.network, "internal-network", "wardyn-internal", "Docker bridge for proxy sidecars (created when missing)")
	fl.BoolVar(&o.noService, "no-service", false, "write the runner's environment file and print the command that runs it, but install no systemd unit or LaunchAgent")
	fl.BoolVar(&o.claim, "claim", false, "claim the runner for yourself now; needs WARDYN_TOKEN, your own personal API token")
	fl.StringVar(&o.pool, "pool", "", "with --claim, also put the runner in your user pool of this name (made if you have none by it)")
	fl.BoolVar(&o.makeDefault, "default", false, "with --claim and --pool, also make that pool your default")
	o.limits.register(cmd)
	return cmd
}

// setupPlan is everything setup settles before its first side effect.
type setupPlan struct {
	setupOptions
	paths      setupPaths
	registered bool
	// proxy is the shell's proxy variables as the service will carry them, without credentials.
	proxy      []carriedProxy
	want       relayPair
	dockerHost string
	// eng is the engine preflight checked. flagged is whether --proxy-image was given (now or to an
	// earlier setup). proxyImage is the
	// best image known before registering (the flag, the saved identity's, else the release image) and
	// is "" only on a development build with neither, when the first probe waits for the organisation's answer.
	eng     dockerEngine
	flagged bool
	// fromIdentity is whether the image is the one saved in runner.json.
	fromIdentity bool
	// unreachable is the release image when a first registration could not get it before the token: the
	// organisation's answer is then probed after registering.
	unreachable string
}

func runSetup(cmd *cobra.Command, h setupHost, o setupOptions) error {
	out := cmd.OutOrStdout()
	plan, err := h.preflight(cmd, o)
	if err != nil {
		return err
	}
	if err := h.installBinary(cmd.Context(), plan.paths.installed, !plan.noService); err != nil {
		return err
	}
	if err := h.seedCLIProfile(plan.paths, plan.org, out); err != nil {
		return err
	}
	sayNoDiskCap(cmd.Context(), out)
	if !plan.registered {
		fmt.Fprintln(out, "Registering this computer. At a terminal it asks for the runner's limits (Enter takes each suggestion); otherwise it registers with the limits it suggests.")
		done, err := registerRunner(cmd, registerArgs{org: plan.org, tokenFile: plan.tokenFile, stateDir: plan.paths.stateDir, name: plan.name, limits: &plan.limits, proxyImage: plan.proxyImage,
			trustedCAFile: plan.trustedCAFile, orgCAFile: plan.orgCAFile})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "registered %s; key fingerprint %s\n  %s\n", done.name, done.fingerprint, limitsLine(done.limits))
		if !plan.flagged {
			if err := h.settleImageAfterRegistering(cmd, &plan, done.proxyImage); err != nil {
				return err
			}
		}
	} else if plan.tokenFile != "" {
		// A rerun keeps the identity, and with it the runner's run state and its queue at the organisation.
		fmt.Fprintf(out, "This computer is already registered, so the token in %s was not used.\n", plan.tokenFile)
	}
	start := h.startService
	if plan.noService {
		start = func(_ context.Context, plan setupPlan, out io.Writer) error { return h.writeRunConfig(plan, out) }
	}
	if err := start(cmd.Context(), plan, out); err != nil {
		return err
	}
	identity, err := runneridentity.Load(plan.paths.stateDir, plan.org)
	if err != nil {
		return err
	}
	return claimStep(cmd, plan, identity, os.Getenv("WARDYN_TOKEN"))
}

// settleImageAfterRegistering takes the organisation's answer into account once the token is spent. The
// image preflight probed (the release image, when there is one) stands unless the answer names another,
// which is then probed too; with no image known at all the first probe runs here. Either way the probe
// comes before the service is installed, and a refusal needs no new token: the identity is saved, so
// setup run again takes the registered path.
func (h setupHost) settleImageAfterRegistering(cmd *cobra.Command, plan *setupPlan, answered string) error {
	again := "\nThis computer is registered: " + h.imageHint(cmd.Context())
	if answered != "" && plan.proxyImage != "" && (!safeSetting.MatchString(answered) || !types.SafeImageRef(answered)) {
		// The image probed before the token works; an unusable answer does not unseat it.
		fmt.Fprintf(cmd.OutOrStdout(), "The organisation named a proxy image setup cannot use (%q); keeping %s, which this engine reached.\n", answered, plan.proxyImage)
		return nil
	}
	image := cmp.Or(answered, plan.proxyImage)
	switch {
	case image == "" && plan.unreachable != "":
		return fmt.Errorf("this computer is registered, but the organisation named no proxy image and the release image %s is not reachable from this engine: pass --proxy-image <ref> and run setup again (no new token is needed)", plan.unreachable)
	case image == "":
		return fmt.Errorf("this computer is registered, but the organisation named no proxy image and this is a development build (%s): pass --proxy-image <ref> and run setup again (no new token is needed)", buildVersion())
	case !safeSetting.MatchString(image) || !types.SafeImageRef(image):
		return fmt.Errorf("this computer is registered, but the proxy image the organisation named has unsupported characters (%s): %s", image, h.imageHint(cmd.Context()))
	case image == plan.proxyImage:
		return nil
	}
	plan.proxyImage = image
	pair, err := h.relay(cmd.Context(), image, plan.network, plan.eng, plan.want)
	if err != nil {
		return fmt.Errorf("%w%s", err, again)
	}
	plan.useRelay(cmd.OutOrStdout(), pair)
	return nil
}

// imageHint is what to do when the proxy image the organisation named (or the saved one) does not work: the
// person names the image for this computer's architecture, and no new token is needed.
func (h setupHost) imageHint(ctx context.Context) string {
	return "run setup again with --proxy-image <ref> for this computer's architecture (" + cmp.Or(engineArch(ctx), "unknown") + ") (no new token is needed)"
}

// useRelay records the pair the probe proved, and says so.
func (p *setupPlan) useRelay(out io.Writer, pair relayPair) {
	if !p.eng.native {
		// Pinned: serve checks this one pair rather than searching again.
		p.relayListen, p.relayHostTarget = pair.bind.String(), pair.target
	}
	fmt.Fprintf(out, "Docker engine %s: the run relay listens on %s and a run's proxy reaches it at %s (checked with a probe container).\n",
		p.eng.desc, pair.bind, pair.target)
}

// checkOrgAndPool validates the organisation URL and the --pool, --claim and --default combination.
func (o setupOptions) checkOrgAndPool() error {
	if err := federation.CheckOrgURL(o.org); err != nil {
		return err
	}
	if o.makeDefault && o.pool == "" {
		return errors.New("--default needs --pool")
	}
	if o.pool == "" {
		return nil
	}
	if !o.claim {
		return errors.New("--pool needs --claim")
	}
	return types.ValidateRunnerPoolName(o.pool)
}

// checkedTrustedCAFile makes a CA bundle path absolute and runs serve's own check on it, so a bad
// file is refused before the token is spent. given is --trusted-ca-file; without it, kept is the
// path an earlier setup wrote into from, and a refusal says so. Neither stays empty.
func checkedTrustedCAFile(given, kept, from string) (string, error) {
	path := cmp.Or(given, kept)
	if path == "" {
		return "", nil
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		_, _, err = loadTrustedCA(abs)
	}
	if err != nil && given == "" {
		return "", fmt.Errorf("%w: kept from the earlier setup (%s); pass --trusted-ca-file <new file> or remove WARDYN_TRUSTED_CA_FILE from it", err, from)
	}
	return abs, err
}

// checkedOrgCAFile is checkedTrustedCAFile for --org-ca-file, with serve's own check (loadOrgCA).
func checkedOrgCAFile(given, kept, from string) (string, error) {
	path := cmp.Or(given, kept)
	if path == "" {
		return "", nil
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		_, _, err = loadOrgCA(abs)
	}
	if err != nil && given == "" {
		return "", fmt.Errorf("%w: kept from the earlier setup (%s); pass --org-ca-file <new file> or remove WARDYN_ORG_CA_FILE from it", err, from)
	}
	return abs, err
}

// checkServiceChoice checks the service setup will install, or with --no-service that none is installed
// already for this runner.
func (h setupHost) checkServiceChoice(ctx context.Context, noService bool, planPaths *setupPaths, paths setupPaths) error {
	if !noService {
		return h.checkServiceManager(ctx, planPaths)
	}
	installed := paths.installedService()
	if xdg := filepath.Join(h.xdgConfig, "systemd", "user", serviceUnit); installed == "" && h.goos == "linux" && h.xdgConfig != "" {
		// paths() names only the HOME unit; a service setup may have put it where the manager's
		// UnitPath looks first, $XDG_CONFIG_HOME/systemd/user (checkServiceManager).
		if _, err := os.Stat(xdg); err == nil {
			installed = xdg
		}
	}
	if installed != "" {
		// Refused rather than disabled: --no-service promises to touch no service, and a unit left running beside
		// the foreground command would be a second process with this one identity, which the organisation
		// refuses.
		return fmt.Errorf("a service is already installed for this runner (%s): running the runner by hand as well starts a second process with one identity. "+
			"Stop and remove the service first (Linux: systemctl --user disable --now %s; macOS: launchctl bootout gui/%d/%s), then delete that file and run setup --no-service again; no token was spent", installed, serviceUnit, h.uid, serviceLabel)
	}
	return nil
}

// checkSettings refuses a setting the service files cannot carry as written.
func (h setupHost) checkSettings(plan setupPlan) error {
	settings := []string{plan.org, plan.proxyImage, plan.network, h.dockerHost, plan.relayListen, plan.relayHostTarget}
	if h.goos == "linux" {
		// The unit and the env file carry these paths unquoted; a LaunchAgent plist escapes its own.
		settings = append(settings, plan.paths.installed, plan.paths.stateDir, plan.trustedCAFile, plan.orgCAFile)
	}
	for _, v := range settings {
		if v != "" && !safeSetting.MatchString(v) {
			return fmt.Errorf("unsupported characters in a setting: %s", v)
		}
	}
	// A plist escapes its own values, but the environment file --no-service writes is read back line by
	// line: a path with a line break would not survive it.
	for _, v := range []string{plan.paths.installed, plan.paths.stateDir, plan.trustedCAFile, plan.orgCAFile} {
		if strings.ContainsFunc(v, unicode.IsControl) {
			return fmt.Errorf("unsupported characters in a setting: %q", v)
		}
	}
	return nil
}

// proxyForService settles the proxy variables the service carries: the shell's, else the ones an earlier setup
// carried. Only a proxy address in the shell replaces the earlier set; a shell that sets just NO_PROXY keeps the
// earlier addresses and takes its own exclusion list.
func proxyForService(out io.Writer, paths setupPaths) ([]carriedProxy, error) {
	carried, stripped, err := carryProxy(os.Getenv)
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(carried, func(c carriedProxy) bool { return !isNoProxyName(c.name) }) {
		fmt.Fprintln(out, proxyCarryLine(carried, stripped))
		if len(stripped) > 0 {
			fmt.Fprintln(out, proxyCredentialWarning(stripped))
		}
		return carried, nil
	}
	earlier, _, err := carryProxy(paths.setting)
	if err != nil {
		return nil, fmt.Errorf("%w (kept from the earlier setup, %s)", err, cmp.Or(paths.envFile, paths.plistFile))
	}
	if len(carried) == 0 {
		if len(earlier) > 0 {
			fmt.Fprintln(out, proxyKeptLine(earlier))
		}
		return earlier, nil
	}
	fmt.Fprintln(out, proxyCarryLine(carried, stripped))
	kept := slices.DeleteFunc(earlier, func(c carriedProxy) bool { return isNoProxyName(c.name) })
	if len(kept) > 0 {
		fmt.Fprintln(out, keptAddressesLine(kept))
	}
	return append(kept, carried...), nil
}

// proxyEnvNames are the proxy variables set in the environment that setup does not carry, by name: a value
// may hold a credential. The runner's connection reads only the variables carryProxy carries.
func proxyEnvNames(getenv func(string) string) []string {
	var set []string
	for _, k := range []string{"ALL_PROXY", "all_proxy"} {
		if getenv(k) != "" {
			set = append(set, k)
		}
	}
	return set
}

// shellProxyWarning says that a proxy variable setup does not carry reaches neither the service nor the
// runner's own connection to the organisation, which reads HTTPS_PROXY, HTTP_PROXY and NO_PROXY.
func shellProxyWarning(names []string) string {
	verb := "is"
	if len(names) > 1 {
		verb = "are"
	}
	return strings.Join(names, ", ") + " " + verb + " set in this shell, but setup does not carry it into the service, and the " +
		"runner's own connection to the organisation reads only HTTPS_PROXY, HTTP_PROXY and NO_PROXY, so it will not use " +
		"that proxy. Set HTTPS_PROXY in this shell and run setup again: see docs/runner-registration.md#a-runner-behind-a-corporate-proxy."
}

// proxyCredentialWarning is the line setup prints for each carried proxy variable that held a user name and
// password, naming the variables only: the service gets the address without them.
func proxyCredentialWarning(stripped []string) string {
	return strings.Join(stripped, ", ") + " in this shell contains a credential; the service gets it without the credential. " +
		"For a proxy that needs a password, use a systemd drop-in on Linux (systemctl --user edit wardyn-runnerd): see " +
		"docs/runner-registration.md#a-runner-behind-a-corporate-proxy. Never put a proxy credential in the LaunchAgent: every local account can read it."
}

// preflight validates every input and checks Docker, so that a refusal comes before the single-use
// token is spent or any file is written.
func (h setupHost) preflight(cmd *cobra.Command, o setupOptions) (setupPlan, error) {
	if h.uid == 0 {
		return setupPlan{}, errors.New("run setup as the person who owns this runner, not as root: the runner identity lives in that person's own configuration directory")
	}
	if err := o.checkOrgAndPool(); err != nil {
		return setupPlan{}, err
	}
	want, err := parseRelayWant(o.relayListen, o.relayHostTarget)
	if err != nil {
		return setupPlan{}, err
	}
	o.relayHostTarget = want.target
	paths, err := h.paths()
	if err != nil {
		return setupPlan{}, err
	}
	// A rerun keeps what an earlier setup was told explicitly.
	keptImage, keptCA, keptOrgCA := paths.keptPins()
	// serve never reads the ambient SSL_CERT_FILE, so setup only points at it, unless a CA is named
	// or kept.
	if ssl := os.Getenv("SSL_CERT_FILE"); ssl != "" && cmp.Or(o.trustedCAFile, keptCA, o.orgCAFile, keptOrgCA) == "" {
		fmt.Fprintf(cmd.OutOrStdout(), "SSL_CERT_FILE is set (%s); pass --trusted-ca-file %s if runs must trust it (64 KiB at most), or --org-ca-file %s to trust it for this runner's connection to the organisation only (the CLI's own anchors: wardyn ca add)\n", ssl, ssl, ssl)
	}
	if names := proxyEnvNames(os.Getenv); len(names) > 0 && !o.noService {
		fmt.Fprintln(cmd.OutOrStdout(), shellProxyWarning(names))
	}
	if o.trustedCAFile, err = checkedTrustedCAFile(o.trustedCAFile, keptCA, cmp.Or(paths.envFile, paths.plistFile)); err != nil {
		return setupPlan{}, err
	}
	if o.orgCAFile, err = checkedOrgCAFile(o.orgCAFile, keptOrgCA, cmp.Or(paths.envFile, paths.plistFile)); err != nil {
		return setupPlan{}, err
	}
	carried, err := proxyForService(cmd.OutOrStdout(), paths)
	if err != nil {
		return setupPlan{}, err
	}
	_, caWarnings := trustedCAFileCheck(o.trustedCAFile, time.Now())
	for _, w := range caWarnings {
		fmt.Fprintln(cmd.OutOrStdout(), "warning: "+w)
	}
	plan := setupPlan{proxy: carried, setupOptions: o, paths: paths, dockerHost: h.dockerHost, want: want, flagged: o.proxyImage != ""}
	if _, err := os.Stat(plan.paths.identity()); err == nil {
		id, err := runneridentity.Load(plan.paths.stateDir, o.org)
		if err != nil {
			return setupPlan{}, fmt.Errorf("this computer is registered, but not for this organisation or not intact: %w", err)
		}
		plan.registered = true
		if o.proxyImage == "" && keptImage != id.ProxyImage {
			// An earlier --proxy-image, unless it is the organisation's own answer, saved anyway.
			o.proxyImage = keptImage
			plan.setupOptions.proxyImage, plan.flagged = keptImage, keptImage != ""
		}
		// --proxy-image, else the image the organisation named at registration, else this binary's own.
		plan.fromIdentity = o.proxyImage == "" && id.ProxyImage != ""
		if plan.proxyImage = cmp.Or(o.proxyImage, id.ProxyImage, releaseProxyImage()); plan.proxyImage == "" {
			return setupPlan{}, fmt.Errorf("this wardyn-runnerd is a development build (%s): pass --proxy-image <ref>", buildVersion())
		}
	} else {
		// A first registration without the flag probes the release image, which the answer may replace.
		plan.proxyImage = cmp.Or(o.proxyImage, releaseProxyImage())
		if o.tokenFile == "" {
			return setupPlan{}, errors.New("no runner identity yet: pass --token-file <file> holding the wdr_ token an administrator minted for you")
		}
		if _, err := os.Stat(o.tokenFile); err != nil {
			return setupPlan{}, fmt.Errorf("token file not found: %s", o.tokenFile)
		}
	}
	if err := h.checkSettings(plan); err != nil {
		return setupPlan{}, err
	}
	if err := h.checkServiceChoice(cmd.Context(), o.noService, &plan.paths, paths); err != nil {
		return setupPlan{}, err
	}
	// Always, flags or not: serve runs the same check at every start, after the token is spent.
	if plan.eng, err = h.engine(cmd.Context(), o.network, want); err != nil {
		return setupPlan{}, fmt.Errorf("%w\nNo token was spent", err)
	}
	if plan.proxyImage == "" {
		// A development build, no flag, no identity: the probe runs after the answer names the image.
		return plan, nil
	}
	pair, err := h.relay(cmd.Context(), plan.proxyImage, o.network, plan.eng, want)
	if errors.Is(err, errProxyImageUnavailable) && !plan.registered && !plan.flagged {
		// Only the release default is given up on: a flag or a saved image was named by the person. A probe
		// that ran and failed always refuses.
		fmt.Fprintf(cmd.OutOrStdout(), "The release proxy image %s is not reachable from this engine; the organisation's answer will be used.\n", plan.proxyImage)
		plan.unreachable, plan.proxyImage = plan.proxyImage, ""
		return plan, nil
	}
	if err != nil && plan.fromIdentity {
		return setupPlan{}, fmt.Errorf("%w\nThis computer's saved proxy image (runner.json) did not work: %s", err, h.imageHint(cmd.Context()))
	}
	if err != nil {
		return setupPlan{}, fmt.Errorf("%w\nNo token was spent", err)
	}
	plan.useRelay(cmd.OutOrStdout(), pair)
	return plan, nil
}

func isNoProxyName(name string) bool { return strings.HasSuffix(strings.ToUpper(name), "NO_PROXY") }

// keptAddressesLine is the line setup prints when the shell sets only an exclusion list: the earlier proxy
// addresses stay, named by variable only.
func keptAddressesLine(carried []carriedProxy) string {
	names := make([]string, len(carried))
	for i, c := range carried {
		names[i] = c.name
	}
	return "This shell sets no proxy address, so the runner service keeps " + strings.Join(names, ", ") + " from the earlier setup."
}
