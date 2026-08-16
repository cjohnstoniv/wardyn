// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Sandbox resource usage — CPU, memory, disk written, process count — for the
// Sandbox widget on the run-detail cockpit.
//
// WHY NOT a Runner.Resources interface method: that buys a moby ContainerStats
// path, a k8s metrics-API path (which needs metrics-server installed — often it
// is not), and two more fakes, all to read numbers the kernel already publishes
// INSIDE the sandbox. One ExecStream over cgroup v2 + procfs is substrate-
// agnostic and works wherever the SSH gateway's exec channels already work.
//
// ponytail: cgroup v2 + procfs via one exec, not a per-substrate Runner method.
// Upgrade path is a Runner.Resources seam if some substrate ever needs a
// non-exec read (a substrate with no exec at all, say).
package api

import "net/http"

// handleRunResources serves GET /api/v1/runs/{id}/resources.
//
// CONTRACT (lane A2 — fill this in):
//
//   - Gate: parseIDParam + s.getRunAuthorized (owner-or-admin; a foreign run
//     404s). Follow handleAttachTicket (attach_ticket.go:113) exactly.
//
//   - Read: ONE ExecStream running /bin/sh -c with a script that prints
//     `key=value` lines, parsed in Go. Precedent: sshgateway_channels.go:527.
//     Drain Stderr concurrently with Stdout (see ExecSession's doc — an
//     undrained stderr blocks the demux goroutine, Stdout, AND Wait).
//
//     cpu    — /sys/fs/cgroup/cpu.stat usage_usec, sampled TWICE ~200ms apart
//              inside the same script, divided by the delta and by `nproc`.
//              One sample cannot yield a percentage; do not pretend it can.
//     memory — /sys/fs/cgroup/memory.current over memory.max; a literal "max"
//              means unlimited -> fall back to MemTotal from /proc/meminfo.
//     disk   — /sys/fs/cgroup/io.stat, wbytes summed across devices.
//     procs  — count of /proc/[0-9]* entries.
//
//   - HONESTY, and this one reaches the UI: gVisor (CC3 / Vault) presents a
//     synthetic procfs/sysfs and may not expose these cgroup files at all. A
//     key the sandbox did not report comes back ABSENT (a nil/omitted field),
//     never 0 — the widget renders "not available on this barrier" for it. A
//     zero here would read as "this sandbox is using no memory", which is a lie
//     about a governed workload. Use pointer/omitempty fields, not bare ints.
//
//   - errors.Is(err, runner.ErrExecStreamUnsupported) -> 501 with that reason.
//
//   - Bound it with a context deadline. Audit on FAILURE only — the console
//     polls this.
func (s *Server) handleRunResources(w http.ResponseWriter, r *http.Request) {
	// ponytail: honest 501 until lane A2 lands — the widget already renders an
	// unsupported state, so an unbuilt endpoint degrades instead of lying.
	writeError(w, http.StatusNotImplemented, "sandbox resource usage is not implemented on this build")
}
