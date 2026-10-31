// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/net/http/httpproxy"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
)

// checkConnection runs the stages `wardyn-runnerd doctor` runs for the connection, against the settings
// setup is about to install, and prints one line per stage. A failed stage that makes setup pointless
// (doctor.SetupBlocking: the runner could not register, or hold its WebSocket) refuses with doctor's own
// message, before the token is spent or a file is written; a failure of any other stage is a warning. The
// engine and relay are preflight's own probes, which doctor's stages run too.
func (h setupHost) checkConnection(cmd *cobra.Command, plan setupPlan) error {
	out := cmd.OutOrStdout()
	conn := runnerConn{org: plan.org, orgSource: cmp.Or(plan.orgFrom, "--org"), trustedCA: plan.trustedCAFile, orgCA: plan.orgCAFile,
		proxyNote: ": the settings setup will install for the service, not this shell's"}
	shellProxy := cmp.Or(strings.TrimSpace(os.Getenv("HTTPS_PROXY")), strings.TrimSpace(os.Getenv("https_proxy")))
	var usedShellCredential bool
	conn.proxyFor, usedShellCredential = serviceProxyFor(plan.runnerFile(), shellProxy)
	// The registration that follows takes this route too, so it cannot pass on a path the check did not.
	cmd.SetContext(context.WithValue(cmd.Context(), registrationRouteKey{}, conn.proxyFor))
	net := h.network
	net.ProxyAuthFix = proxyAuthFix
	// A credential the shell did not supply may be one the service has already been given another way.
	credentialFile := ""
	if !usedShellCredential {
		credentialFile = h.proxyCredentialFile(plan.paths)
		net.ProxyAuthAdvisory = credentialFile != ""
	}
	net.WireDespiteHealthz = true
	stages, serverVersion := h.connectionStages(cmd.Context(), plan.paths, plan.paths.stateDir, conn, &net, true)
	if skew, ok := proxyImageSkew(plan.proxyImage, plan.flagged, serverVersion); ok {
		stages = append(stages, skew)
	}
	report := doctor.Report{Stages: stages}.Scrubbed()
	fmt.Fprintln(out, "Checking the connection to the organisation, as `wardyn-runnerd doctor` does:")
	if err := report.WriteText(out); err != nil {
		return err
	}
	if st, ok := report.FirstBlocking(doctor.SetupBlocking...); ok {
		return fmt.Errorf("the %s stage failed, so setup stops here: %s%s (nothing was changed and no token was spent)", st.Name, st.Message(), setupHint(st))
	}
	for _, st := range report.Stages {
		if st.ProxyAuth && st.Advisory {
			fmt.Fprintf(out, "warning: the proxy answered 407 to this check. %s exists, so the service gets its proxy credential from it; setup did not open it and has not checked that credential\n", credentialFile)
			break
		}
	}
	var warned []string
	for _, st := range report.Stages {
		if st.Status == doctor.StatusFail {
			warned = append(warned, st.Name)
		}
	}
	if len(warned) > 0 {
		fmt.Fprintf(out, "warning: the %s stage failed; setup goes on, as that does not stop a registration\n", strings.Join(slices.Compact(warned), ", "))
	}
	return nil
}

// setupHint says what setup takes for a setting a stage names, where it takes it as a flag.
func setupHint(st doctor.Stage) string {
	if st.Name == "tls" && st.Setting == "WARDYN_TRUSTED_CA_FILE" {
		return "; setup takes the root with --org-ca-file <file> for this runner's own connection to the organisation only, or --trusted-ca-file <file> to give it to every run's proxy and sandbox as well"
	}
	return ""
}

// proxyFunc routes a request through https for an HTTPS address, except the hosts noProxy lists (x/net/http/httpproxy,
// the semantics the runner's own connection has).
func proxyFunc(https, noProxy string) func(*http.Request) (*url.URL, error) {
	proxy := (&httpproxy.Config{HTTPSProxy: https, NoProxy: noProxy}).ProxyFunc()
	return func(r *http.Request) (*url.URL, error) { return proxy(r.URL) }
}

// registrationRouteKey carries the route the connection check took, on the command's context, to the
// registration call that follows it in the same setup.
type registrationRouteKey struct{}

// serviceProxyFor routes a request the way the runner service will: by the HTTPS_PROXY and NO_PROXY that
// runnerd.json carries for it (serve exports them), credential-stripped. The one exception is the operator's
// own credential: when shellProxy, this shell's HTTPS_PROXY, names the same proxy host and port with a user
// name and password, the check uses it, so a proxy that wants one can be checked at all. It goes to that
// proxy and nowhere else, and is never printed. used says whether it was taken.
func serviceProxyFor(f runnerFile, shellProxy string) (route func(*http.Request) (*url.URL, error), used bool) {
	https := f.RunnerProxy
	if given, installed := proxyURL(shellProxy), proxyURL(f.RunnerProxy); given != nil && installed != nil && given.User != nil && strings.EqualFold(given.Host, installed.Host) {
		https, used = shellProxy, true
	}
	return proxyFunc(https, f.RunnerNoProxy), used
}

// proxyURL parses a proxy address, with or without a scheme, nil when it is empty or unreadable.
func proxyURL(v string) *url.URL {
	if v = strings.TrimSpace(v); v == "" {
		return nil
	}
	if !strings.Contains(v, "://") {
		v = "http://" + v
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return nil
	}
	return u
}

// proxyCredentialFile is where the docs put a service's proxy credential on Linux, beside runnerd.json. Setup
// checks that it exists, and never opens it: that credential is not setup's to read.
func (h setupHost) proxyCredentialFile(p setupPaths) string {
	if h.goos != "linux" {
		return ""
	}
	file := filepath.Join(filepath.Dir(p.configFile()), "runnerd-proxy.env")
	if _, err := os.Stat(file); err != nil {
		return ""
	}
	return file
}

// proxyAuthFix is what a 407 asks of a person running setup.
const proxyAuthFix = "the proxy's address with the credential it accepts in this shell (setup uses it for this check and the registration only and never prints it), " +
	"and give the service the same credential through a systemd drop-in: see docs/runner-registration.md#a-runner-behind-a-corporate-proxy"
