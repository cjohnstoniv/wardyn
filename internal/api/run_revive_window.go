// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"strconv"
	"strings"
)

// The proxy-window arithmetic GET /admin/runs/proxy-window lists runs by (run_revive.go), moved out
// of run_revive.go to keep it under scripts/check-file-size.sh's 1000-line gate.

// inProxyWindow: release is the current minor or the one before it. An
// unparseable release ("" before migration 0084) is outside.
func inProxyWindow(current, release string) bool {
	cm, cn, ok := majorMinor(current)
	rm, rn, rok := majorMinor(release)
	return ok && rok && cm == rm && (rn == cn || rn == cn-1)
}

// proxyWindow names the supported minors, newest first.
func proxyWindow(current string) []string {
	m, n, ok := majorMinor(current)
	if !ok {
		return nil
	}
	w := []string{fmt.Sprintf("%d.%d", m, n)}
	if n > 0 {
		w = append(w, fmt.Sprintf("%d.%d", m, n-1))
	}
	return w
}

func majorMinor(v string) (int, int, bool) {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	m, err1 := strconv.Atoi(parts[0])
	n, err2 := strconv.Atoi(parts[1])
	return m, n, err1 == nil && err2 == nil
}
