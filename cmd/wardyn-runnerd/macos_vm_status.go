// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// What this runner tells its own organisation about its macOS VMs, as codes and numbers (runnerwire's
// MacVMCaps.Reason and NoSession, MacVMBase.Step, Wait and Failure). Nothing sent names a person or an
// account; the base's error is the one free text, and it is scrubbed (scrubError).

// versionPattern is the only form of macOS version the caps carry (host_os_version, image_version).
var versionPattern = regexp.MustCompile(`^\d+(\.\d+){0,2}$`)

// maxVersionLen bounds a version in the caps, in bytes: a macOS version is a few digits.
const maxVersionLen = 32

// validVersion says v may go in the caps.
func validVersion(v string) bool { return len(v) <= maxVersionLen && versionPattern.MatchString(v) }

// maxBaseError bounds the base's error in the caps, in bytes.
const maxBaseError = 512

// macOSVMReprobeEvery is how many recompute ticks (15 s each) pass between the probe's thermal re-reads
// while the base is ready: 60 s. macOSVMReprobeWait bounds one.
const (
	macOSVMReprobeEvery = 4
	macOSVMReprobeWait  = 5 * time.Second
)

// Seams for tests: the Mac's own tools.
var (
	swVersPath    = "/usr/bin/sw_vers"
	launchctlPath = "/bin/launchctl"
)

// macProductVersion is this Mac's macOS version (sw_vers -productVersion), or "" when it cannot say.
func macProductVersion() string {
	out, err := exec.Command(swVersPath, "-productVersion").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// prepareStatus is what prepare.log says of the prepare it last logged: one pass over its event lines,
// started over at each check event (a prepare's first).
type prepareStatus struct {
	step, wait    string
	percent       int
	downloadBytes int64
	version       string
	rate          int64
	sawBytes      bool // a download progress event with bytes came since the check
}

// stepOf is the step the drawer shows for a helper phase, or "" for one it does not show.
func stepOf(phase string) string {
	switch phase {
	case macvm.PhaseCheck:
		return runnerwire.MacVMStepCheck
	case macvm.PhaseDownload, macvm.PhaseValidate:
		return runnerwire.MacVMStepDownload
	case macvm.PhaseCreate, macvm.PhaseInstall:
		return runnerwire.MacVMStepInstall
	case macvm.PhaseProvision, macvm.PhaseSetupByHand, macvm.PhaseBootstrap, macvm.PhaseShutdown:
		return runnerwire.MacVMStepSetup
	case macvm.PhaseTools:
		return runnerwire.MacVMStepTools
	}
	return ""
}

// add folds one event in.
func (s *prepareStatus) add(ev macvm.PrepareEvent) {
	if ev.Phase == macvm.PhaseCheck {
		*s = prepareStatus{}
	}
	step := stepOf(ev.Phase)
	if step == "" {
		return
	}
	if step != s.step {
		s.percent = 0
	}
	s.step = step
	// A pause, its end and a retry carry no fraction; only an event that has one moves the percent.
	if (ev.Phase == macvm.PhaseDownload || ev.Phase == macvm.PhaseInstall) && ev.Fraction > 0 {
		s.percent = min(int(ev.Fraction*100), 100)
	}
	if (ev.Phase == macvm.PhaseDownload || ev.Phase == macvm.PhaseValidate) && ev.Total > 0 {
		s.downloadBytes = ev.Total
	}
	if validVersion(ev.Version) {
		s.version = ev.Version
	}
	switch {
	case ev.Paused:
		// Only the check resets sawBytes: the event that ends a pause reads the same as the one that
		// starts the download, so neither can tell a pause on battery from one before the first byte.
		s.wait = runnerwire.MacVMWaitACPower
		if ev.Phase == macvm.PhaseDownload && s.sawBytes {
			s.wait = runnerwire.MacVMWaitBattery
		}
	case ev.Retry:
		s.wait = runnerwire.MacVMWaitDownloadRetry
	default:
		s.wait = ""
	}
	if ev.Phase == macvm.PhaseDownload && !ev.Retry && !ev.Paused && ev.Bytes > 0 {
		s.sawBytes = true
		s.rate = int64(ev.BytesPerSec)
	} else if ev.Phase == macvm.PhaseDownload && (ev.Paused || ev.Retry) {
		s.rate = 0 // the rate from before a wait is stale when the download resumes
	}
}

// scanPrepare reads prepare.log. A line that is not a progress event (the helper's final error, say) is
// skipped.
func scanPrepare(log string) prepareStatus {
	var s prepareStatus
	f, err := os.Open(log)
	if err != nil {
		return s
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, prepareLogLineMax)
	for sc.Scan() {
		var ev macvm.PrepareEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			s.add(ev)
		}
	}
	return s
}

// apply puts the status on b. The wait is for a base that is preparing.
func (s prepareStatus) apply(b *runnerwire.MacVMBase, preparing bool) {
	b.Step, b.DownloadBytes, b.ImageVersion = s.step, s.downloadBytes, s.version
	if s.step == runnerwire.MacVMStepDownload || s.step == runnerwire.MacVMStepInstall {
		b.StepPercent = s.percent
	}
	if preparing {
		b.Wait = s.wait
	}
	if preparing && s.step == runnerwire.MacVMStepDownload && b.Wait == "" {
		b.BytesPerSec = s.rate
	}
}

// splitFailure is the failure a helper error ends with and its cause: the error with the helper's
// wrapping stripped, however many times it is wrapped. Any other error has no failure and is its own
// cause.
func splitFailure(msg string) (failure, cause string) {
	for _, p := range []struct{ prefix, failure string }{
		{macvm.InstallFailurePrefix, runnerwire.MacVMFailureInstall},
		{macvm.DownloadFailurePrefix, runnerwire.MacVMFailureDownload},
		{macvm.NoDesktopFailurePrefix, runnerwire.MacVMFailureNoDesktop},
	} {
		if !strings.HasPrefix(msg, p.prefix) {
			continue
		}
		for strings.HasPrefix(msg, p.prefix) {
			msg = strings.TrimPrefix(msg, p.prefix)
		}
		return p.failure, msg
	}
	return "", msg
}

// toolsFailure is why the Command Line Tools did not install on the ready base in baseDir, or "".
func toolsFailure(baseDir string) string {
	rec, err := macvm.ReadToolsRecord(baseDir)
	if err != nil {
		return ""
	}
	ids := make([]string, 0, len(rec.Withheld))
	for id := range rec.Withheld {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if cause, ok := strings.CutPrefix(rec.Withheld[id].Reason, macvm.CLTFailurePrefix); ok {
			return cause
		}
	}
	return ""
}

// selftestWithheld is a status line for each harness the ready base in baseDir withheld because the
// launcher's self-test failed for it. The end of the self-test's output is in the base's tools.json,
// which the line names.
func selftestWithheld(baseDir string) []string {
	rec, err := macvm.ReadToolsRecord(baseDir)
	if err != nil {
		return nil
	}
	var lines []string
	for id, w := range rec.Withheld {
		if strings.HasPrefix(w.Reason, macvm.SelftestFailurePrefix) {
			lines = append(lines, "withheld: "+id+" (its self-test failed; see "+filepath.Join(baseDir, macvm.ToolsRecordFile)+")")
		}
	}
	slices.Sort(lines)
	return lines
}

// scrubError is err as the caps carry it: this login's home as "~", the uid in a socket path as <uid>,
// and at most maxBaseError bytes. The full text stays on this Mac, in the log and prepare.log.
func (m *macOSVMs) scrubError(err string) string {
	root := strings.TrimRight(cmp.Or(m.sockRoot, admission.DefaultSockDir), "/")
	err = regexp.MustCompile(regexp.QuoteMeta(root)+`/u\d+`).ReplaceAllLiteralString(err, root+"/u<uid>")
	if home, herr := os.UserHomeDir(); herr == nil && home != "" && home != "/" {
		err = strings.ReplaceAll(strings.ReplaceAll(err, "/System/Volumes/Data"+home, "~"), home, "~")
	}
	if len(err) > maxBaseError {
		err = err[:maxBaseError]
		for !utf8.ValidString(err) {
			err = err[:len(err)-1]
		}
	}
	return err
}

// explainProbe sets why a probe that failed turned macOS VMs off, as codes: macOS older than the helper's
// minimum, else a Mac or build that cannot run them. A probe that said nothing sets none.
func (m *macOSVMs) explainProbe(r probeReport) {
	if v := macProductVersion(); v != "" {
		if major, _, _ := strings.Cut(v, "."); atoi(major) > 0 && atoi(major) < macvm.MinHostMajor {
			m.reason = runnerwire.MacVMReasonHostOSTooOld
			if validVersion(v) {
				m.hostOSVersion = v
			}
			return
		}
	}
	if r.Host == "" {
		return // no report: the helper did not run
	}
	switch {
	case r.Reason == runnerwire.MacVMHostNoVirtualization || r.Reason == runnerwire.MacVMHostNoProvisioning:
		m.reason, m.hostReason = runnerwire.MacVMReasonHostUnsupported, r.Reason
	case !r.Cgo || !r.VZ || r.Provisioning != "ok":
		m.reason, m.hostReason = runnerwire.MacVMReasonHostUnsupported, runnerwire.MacVMHostHelperBuild
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// setThermal keeps the probe's thermal state.
func (m *macOSVMs) setThermal(t string) {
	m.mu.Lock()
	m.thermal = t
	m.mu.Unlock()
}

// tick counts one recompute tick and, on every macOSVMReprobeEvery-th while the base is ready, re-reads
// the probe's thermal state. A probe that fails changes nothing.
func (m *macOSVMs) tick(ctx context.Context) {
	m.ticks++
	if m.ticks%macOSVMReprobeEvery != 0 || m.helper == "" {
		return
	}
	m.mu.Lock()
	ready := m.caps != nil && m.caps.Base.State == runnerwire.MacVMBaseReady
	m.mu.Unlock()
	if !ready {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, macOSVMReprobeWait)
	defer cancel()
	if r, err := probeHelper(ctx, m.helper); err == nil && r.Thermal != "" {
		m.setThermal(r.Thermal)
	}
}

// checkSession sets noSession when this runner is not in a GUI login session (launchctl's manager is not
// Aqua), where Apple's framework runs. It keeps the answer as a bool and throws the output away; a
// launchctl that cannot be run says nothing.
func (m *macOSVMs) checkSession(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, macOSVMReprobeWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, launchctlPath, "managername").Output()
	if err != nil {
		return
	}
	m.noSession = strings.TrimSpace(string(out)) != "Aqua"
}

// hostStatus is what build reads of the fields the gate, the session check and the thermal re-probe set.
func (m *macOSVMs) hostStatus() (reason, hostOS, hostReason, thermal string, noSession bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reason, m.hostOSVersion, m.hostReason, m.thermal, m.noSession
}

// reasonsWhenReady is the reasons a ready base can carry beyond its minimum and a leaked slot: low memory,
// then too hot.
func (m *macOSVMs) reasonsWhenReady(c *runnerwire.MacVMCaps, man macvm.Manifest, thermal string) {
	if c.Reason != "" {
		return
	}
	need := m.guestMemory(man) + admission.LiveMemoryMarginBytes
	if avail, err := m.host.AvailableMemoryBytes(); err == nil && avail < need {
		c.Reason, c.FreeGB, c.NeedGB = runnerwire.MacVMReasonLowMemory, int(avail>>30), int((need+1<<30-1)>>30)
		return
	}
	if thermal == "serious" || thermal == "critical" {
		c.Reason = runnerwire.MacVMReasonTooHot
	}
}

// reasonSlotLeaked says the ledger holds a slot the framework keeps and nothing accounts for (a leaked
// entry, recorded by a helper whose start Apple refused with its VM limit), until the Mac restarts. It
// yields to an earlier reason.
func (m *macOSVMs) reasonSlotLeaked(c *runnerwire.MacVMCaps) {
	if c.Reason != "" {
		return
	}
	counted, err := m.ledger.Counted()
	if err == nil && slices.ContainsFunc(counted, func(e admission.Entry) bool { return e.Kind == admission.KindLeaked }) {
		c.Reason = runnerwire.MacVMReasonSlotLeaked
	}
}

// reasonDiskShort says a Mac with no base (or a failed one) has too little free disk to prepare one.
func (m *macOSVMs) reasonDiskShort(c *runnerwire.MacVMCaps) {
	if c.Base.State != runnerwire.MacVMBaseNone && c.Base.State != runnerwire.MacVMBaseFailed {
		return
	}
	need := admission.PrepareDiskReservation(macvm.BaseDiskBytes) + admission.DiskFloorBytes
	if free, err := m.host.FreeDiskBytes(m.layout.Root); err == nil && free < need {
		c.Reason, c.FreeGB = runnerwire.MacVMReasonDiskShort, int(free>>30)
	}
}
