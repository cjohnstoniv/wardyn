// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// macInspection is what this Mac's inspection CA (MacVMConfig.InspectionCA) does for one run: proxy is the
// spec the run's proxy starts from, signed whether the CA re-issued that proxy's CA (the guest is then not
// asked to trust the run CA), and state and sha256 what the run's machine records.
type macInspection struct {
	proxy         runner.SandboxSpec
	signed        bool
	state, sha256 string
}

// macVMInspection decides it. Nothing changes, and nothing is recorded, with the setting off, on a run whose
// proxy has no CA, and on one whose proxy terminates no host (the re-issued CA could name none). A Mac CA past
// its validity, or a base (or resumed kept machine) that does not record this CA trusted, keeps today's path and
// records why. Otherwise the proxy's copy of the run CA is re-issued by this Mac's CA, limited to the hosts that
// proxy terminates and to the run's lifetime cap (macvm.InspectionCA.CrossSign).
func (c *MacVMConfig) macVMInspection(spec runner.SandboxSpec, kept *macvm.KeptRun) (macInspection, error) {
	out := macInspection{proxy: spec}
	ca := c.InspectionCA
	if ca == nil || strings.TrimSpace(spec.ProxyConfig.MITMCACertPEM) == "" {
		return out, nil
	}
	pc := runner.ProxyConfigOf(uuid.Nil, spec.ProxyConfig, 0) // the one mapping the proxy's config comes from
	terminated := pc.TerminatedHosts()
	if permitted, _ := macvm.InspectedNames(terminated); len(permitted) == 0 {
		return out, nil
	}
	out.sha256 = ca.Fingerprint()
	if ca.Expired(time.Now()) {
		slog.Warn("macOS VM run: this Mac's inspection CA has expired, so it signs nothing; rotate it: remove inspection-ca in the VM directory, restart the runner, then run macos-vm prepare --new-image", "run", spec.RunID)
		out.state = types.InspectionCAExpired
		return out, nil
	}
	src, _ := c.Layout.NamedBaseDir(spec.MacOSBase) // refuseMacVMSpec checked the name
	if kept != nil && kept.Mode == macvm.KeptResume {
		src = c.Layout.KeptMachineDir(kept.ID) // a resume boots the machine's own disk
	}
	if !ca.TrustedIn(src) {
		out.state = types.InspectionCANotTrusted
		return out, nil
	}
	cross, err := ca.CrossSign(spec.ProxyConfig.MITMCACertPEM, terminated, spec.Resources.LifetimeCap)
	if err != nil {
		return macInspection{}, fmt.Errorf("docker: %w", err)
	}
	out.proxy.ProxyConfig.MITMCACertPEM = cross
	out.signed, out.state = true, types.InspectionCATrusted
	return out, nil
}

// withMacInspection is the guest's Init when this Mac's CA signed the run's proxy CA: no run CA for the System
// keychain, since macOS 26 refuses that trust with no person and the base already trusts this Mac's CA. The CA
// bundle is unchanged: it holds the run CA, which a bundle reader chains the proxy's leaves to as before.
func withMacInspection(in guestagent.Init, signed bool) guestagent.Init {
	if signed {
		in.RunCA = ""
	}
	return in
}
