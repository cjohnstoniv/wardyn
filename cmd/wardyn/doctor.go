// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/doctor"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runnerDoctorBound is how long the installed runner's own stages may take: its relay check starts a probe
// container, which may wait on a slow engine.
const runnerDoctorBound = 4 * time.Minute

// doctorCmd is `wardyn doctor`: the connection to your organisation, stage by stage, read-only.
func doctorCmd(client func() *sdk.Client) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check, stage by stage, how this computer reaches your organisation (read-only)",
		Long: `Run the connection checks in order and print one line per stage: the organisation URL and where it came from,
name resolution, the proxy route, the TLS handshake (the issuer presented and which trust store verified it),
/healthz and the server version, and who /api/v1/me says you are. When a runner is installed on this computer, its
own stages follow (its engine, proxy image and relay). A failing stage names the setting that fixes it.

Names are shown, values are not: no token, password or proxy credential appears in either format. doctor writes
nothing and changes no setting. It exits 0 when no stage failed and 1 otherwise; --json prints the same stages as data.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client()
			net := doctor.Network{OrgURL: c.BaseURL, Source: urlSource(cmd), OrgSetting: "WARDYN_URL", Identity: true, Token: c.Token, TokenSource: tokenSource(cmd),
				ExtraCASource: "WARDYN_TRUSTED_CA_FILE", ExtraCA: trustedCAPool(os.Getenv("WARDYN_TRUSTED_CA_FILE"))}
			report := doctor.Report{Stages: net.Run(cmd.Context())}
			report.Stages = append(report.Stages, runnerStages(cmd.Context(), net.ServerVersion)...)
			report = report.Scrubbed()
			write := report.WriteText
			if asJSON {
				write = report.WriteJSON
			}
			if err := write(cmd.OutOrStdout()); err != nil {
				return err
			}
			if !report.OK() {
				return &exitError{code: 1, err: errors.New("doctor: one or more stages failed")}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the stages as JSON")
	return cmd
}

// urlSource names where the URL the command dials came from, in resolveURL's own order.
func urlSource(cmd *cobra.Command) string {
	switch {
	case cmd.Flags().Changed("url"):
		return "--url"
	case os.Getenv("WARDYN_URL") != "":
		return "WARDYN_URL"
	case urlFromDefault:
		return "the built-in default: nothing named an organisation"
	}
	return "the CLI profile that wardyn login saved"
}

// tokenSource names the setting the token came from; the token itself is never read into any output.
func tokenSource(cmd *cobra.Command) string {
	switch {
	case cmd.Flags().Changed("token"):
		return "--token"
	case os.Getenv("WARDYN_ADMIN_TOKEN") != "":
		return "WARDYN_ADMIN_TOKEN"
	}
	return "WARDYN_TOKEN"
}

// trustedCAPool is the bundle WARDYN_TRUSTED_CA_FILE names, nil when it names none or cannot be read: a
// runner reads that setting, and doctor only uses it to say whether it would have vouched for the server.
func trustedCAPool(path string) *x509.CertPool {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	raw, _ := io.ReadAll(io.LimitReader(f, 1<<20))
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil
	}
	return pool
}

// runnerStages are the installed runner's own stages, from its own doctor, so the two commands never
// disagree: the binary setup installed for this person is run with --runner-only. No binary there means no
// runner is installed here.
func runnerStages(ctx context.Context, serverVersion string) []doctor.Stage {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	path := doctor.RunnerdPath(runtime.GOOS, home, os.Getenv("XDG_DATA_HOME"))
	if _, err := os.Stat(path); path == "" || err != nil {
		return []doctor.Stage{doctor.SkipStage("runner", "no runner is installed on this computer")}
	}
	args := []string{"doctor", "--runner-only", "--json"}
	if serverVersion != "" {
		args = append(args, "--server-version", serverVersion)
	}
	ctx, cancel := context.WithTimeout(ctx, runnerDoctorBound)
	defer cancel()
	c := exec.CommandContext(ctx, path, args...) // #nosec G204 -- the binary setup installed for this person
	c.Env = cliutil.ScrubChildEnv(os.Environ())
	// On timeout, interrupt rather than kill, so the child's deferred probe-container removal runs.
	c.Cancel = func() error { return c.Process.Signal(os.Interrupt) }
	c.WaitDelay = 15 * time.Second
	// A failing stage exits 1 and still prints its stages, so the output decides, not the exit code.
	out, runErr := c.Output()
	var report struct{ Stages []doctor.Stage }
	if err := json.Unmarshal(out, &report); err != nil || len(report.Stages) == 0 {
		return []doctor.Stage{doctor.FailStage("runner", fmt.Sprintf("the installed runner (%s) did not report its stages: %v", path, errors.Join(runErr, err)), "WARDYN_RUNNER_STATE_DIR", "the directory of the identity this computer's runner uses; run wardyn-runnerd doctor yourself to see why it printed no stages")}
	}
	return report.Stages
}
