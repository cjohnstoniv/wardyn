// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package posture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

const (
	// deviceCertInterval is how often a connected Mac presents its device identities again.
	deviceCertInterval = 6 * time.Hour
	// deviceHelperTimeout bounds one run of the helper, which may wait on the keychain.
	deviceHelperTimeout = 60 * time.Second
	// maxDeviceHelperOutput bounds what the helper may print: 8 proofs of 4 certificates of 8 KiB, base64 coded, with room.
	maxDeviceHelperOutput = 1 << 20
)

// DeviceIdentityHelper returns the proofs the Mac's MDM device identities make for this runner and organisation.
type DeviceIdentityHelper func(ctx context.Context, stateDir, orgSHA256 string, signedAt time.Time) (runnerwire.DeviceCertReport, error)

// DeviceCertSender presents a Mac's MDM device identities to the organisation (the device_cert event).
type DeviceCertSender struct {
	helper    DeviceIdentityHelper
	stateDir  string
	orgSHA256 string
	now       func() time.Time
	every     time.Duration
	// ack sends the event and waits for the organisation's answer: peer.EventAck, replaced in tests.
	ack func(ctx context.Context, peer *runnerwire.Peer, ev runnerwire.Event) error

	mu       sync.Mutex
	watching *runnerwire.Peer
	refused  *runnerwire.Peer
}

// NewDeviceCertSender returns the sender that runs the wardyn-macvm helper at helperPath, or nil when there is
// none there: a Mac with no macOS VM support has no helper and presents nothing.
func NewDeviceCertSender(helperPath, stateDir, orgSHA256 string) *DeviceCertSender {
	if fi, err := os.Stat(helperPath); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	return newDeviceCertSender(execHelper(helperPath), stateDir, orgSHA256, time.Now, deviceCertInterval)
}

func newDeviceCertSender(h DeviceIdentityHelper, stateDir, orgSHA256 string, now func() time.Time, every time.Duration) *DeviceCertSender {
	return &DeviceCertSender{helper: h, stateDir: stateDir, orgSHA256: orgSHA256, now: now, every: every,
		ack: func(ctx context.Context, peer *runnerwire.Peer, ev runnerwire.Event) error {
			return peer.EventAck(ctx, ev)
		}}
}

func (s *DeviceCertSender) refusedFor(peer *runnerwire.Peer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused == peer
}

// Send runs the helper and reports its proofs on peer, waiting for the organisation's answer. An organisation
// that refuses the event for good is not asked again on this session.
func (s *DeviceCertSender) Send(ctx context.Context, peer *runnerwire.Peer) error {
	if s.refusedFor(peer) {
		return nil
	}
	hctx, cancel := context.WithTimeout(ctx, deviceHelperTimeout)
	defer cancel()
	report, err := s.helper(hctx, s.stateDir, s.orgSHA256, s.now())
	if err != nil {
		return err
	}
	data, err := runnerwire.Marshal(report)
	if err != nil {
		return err
	}
	err = s.ack(ctx, peer, runnerwire.Event{Kind: runnerwire.EventDeviceCert, Data: data})
	var refusal *runnerwire.EventRefusal
	if errors.As(err, &refusal) {
		s.mu.Lock()
		s.refused = peer
		s.mu.Unlock()
		slog.InfoContext(ctx, "runner device certificate not accepted by the organisation; not sending again on this session", "code", refusal.Code)
		return nil
	}
	return err
}

// Watch presents the identities again every interval until peer ends or the organisation refuses the event.
func (s *DeviceCertSender) Watch(peer *runnerwire.Peer) {
	s.mu.Lock()
	if s.watching == peer {
		s.mu.Unlock()
		return
	}
	s.watching = peer
	s.mu.Unlock()
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		select {
		case <-peer.Done():
			return
		case <-t.C:
		}
		if s.refusedFor(peer) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*deviceHelperTimeout)
		if err := s.Send(ctx, peer); err != nil {
			slog.Warn("runner device certificate report failed", "error", err)
		}
		cancel()
	}
}

// execHelper runs `<helper> device-identity` with typed fields and no environment. The helper builds the signed
// message itself; nothing here picks the bytes it signs.
func execHelper(path string) DeviceIdentityHelper {
	return func(ctx context.Context, stateDir, orgSHA256 string, signedAt time.Time) (runnerwire.DeviceCertReport, error) {
		cmd := exec.CommandContext(ctx, path, "device-identity", "--state-dir", stateDir, "--org-sha256", orgSHA256,
			"--signed-at", signedAt.UTC().Format(time.RFC3339))
		cmd.Env = []string{}
		var out, errb bytes.Buffer
		cmd.Stdout = &limitedWriter{w: &out, n: maxDeviceHelperOutput}
		cmd.Stderr = &limitedWriter{w: &errb, n: 4096}
		if err := cmd.Run(); err != nil {
			return runnerwire.DeviceCertReport{}, fmt.Errorf("device identity helper: %w: %s", err, lastLine(errb.String()))
		}
		var rep runnerwire.DeviceCertReport
		dec := json.NewDecoder(&out)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rep); err != nil {
			return runnerwire.DeviceCertReport{}, fmt.Errorf("device identity helper printed no report: %w", err)
		}
		return rep, nil
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// limitedWriter stops a runaway helper: the write that would pass n fails, which ends the command.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, errors.New("the helper printed too much")
	}
	l.n -= len(p)
	return l.w.Write(p)
}
