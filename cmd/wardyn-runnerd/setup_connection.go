// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
)

// checkConnection runs the stages `wardyn-runnerd doctor` runs for the connection, against the settings
// setup is about to install, and prints one line per stage. A failed stage that makes setup pointless
// (doctor.SetupBlocking: the runner could not register, or hold its WebSocket) refuses with doctor's own
// message, before the token is spent or a file is written; a failure of any other stage is a warning. The
// engine and relay are preflight's own probes, which doctor's stages run too.
func (h setupHost) checkConnection(cmd *cobra.Command, plan setupPlan) error {
	out := cmd.OutOrStdout()
	conn := runnerConn{org: plan.org, orgSource: "--org", trustedCA: plan.trustedCAFile, orgCA: plan.orgCAFile}
	net := h.network
	stages, serverVersion := h.connectionStages(cmd.Context(), plan.paths, plan.paths.stateDir, conn, &net, true)
	if skew, ok := proxyImageSkew(plan.proxyImage, plan.flagged, serverVersion); ok {
		stages = append(stages, skew)
	}
	report := doctor.Report{Stages: stages}.Scrubbed()
	fmt.Fprintln(out, "Checking the connection to the organisation, as `wardyn-runnerd doctor` does:")
	if err := report.WriteText(out); err != nil {
		return err
	}
	if st, ok := report.FirstFailure(doctor.SetupBlocking...); ok {
		return fmt.Errorf("the %s stage failed, so setup stops here: %s%s (nothing was changed and no token was spent)", st.Name, st.Message(), setupHint(st))
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
