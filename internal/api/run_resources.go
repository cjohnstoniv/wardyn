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

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runResourcesExecTimeout bounds the whole read: launch + the script's own
// ~200ms CPU sampling window + parse. A stuck sandbox (a hung shell, a
// gVisor gvisor bug, whatever) must resolve to a clean error, not a
// handler that never returns — this is the console's poll endpoint.
const runResourcesExecTimeout = 5 * time.Second

// runResourcesScript runs INSIDE the sandbox via ExecStream and prints one
// `key=value` line per metric it could actually read. Every read is
// individually `2>/dev/null`-guarded and presence-checked before it is
// echoed: a file cgroup v2/procfs does not expose (gVisor/CC3 — see the
// honesty comment on runResourcesResponse) simply produces no line for that
// key, never a line claiming a zero it isn't in a position to attest to.
//
// Tooling kept to awk/grep/cat/ls/wc/sleep — present on both coreutils
// (node:*-bookworm-slim, the shipped agent images) and busybox (the
// conformance-agent image, and any BYOI image). Deliberately NOT used:
// `nproc` (absent on some minimal images; /proc/cpuinfo's `processor` lines
// are the same count via a tool every image has) and `date +%s%N`
// (busybox's `date` has no sub-second resolution). The CPU sample's
// wall-clock delta instead comes from /proc/uptime's first field
// (centisecond resolution, and procfs is universal where cgroup files may
// not be).
const runResourcesScript = `
n=$(grep -c ^processor /proc/cpuinfo 2>/dev/null)
case "$n" in ''|0) n=1 ;; esac
echo "nproc=$n"

u1=$(awk '$1=="usage_usec"{print $2}' /sys/fs/cgroup/cpu.stat 2>/dev/null)
p1=$(awk '{print $1}' /proc/uptime 2>/dev/null)
[ -n "$u1" ] && echo "cpu_usage_usec_1=$u1"
[ -n "$p1" ] && echo "uptime_1=$p1"

sleep 0.2

u2=$(awk '$1=="usage_usec"{print $2}' /sys/fs/cgroup/cpu.stat 2>/dev/null)
p2=$(awk '{print $1}' /proc/uptime 2>/dev/null)
[ -n "$u2" ] && echo "cpu_usage_usec_2=$u2"
[ -n "$p2" ] && echo "uptime_2=$p2"

mc=$(cat /sys/fs/cgroup/memory.current 2>/dev/null)
[ -n "$mc" ] && echo "memory_current=$mc"

mm=$(cat /sys/fs/cgroup/memory.max 2>/dev/null)
[ -n "$mm" ] && echo "memory_max=$mm"

mt=$(awk '$1=="MemTotal:"{print $2}' /proc/meminfo 2>/dev/null)
[ -n "$mt" ] && echo "mem_total_kb=$mt"

wb=$(awk '{for(i=1;i<=NF;i++) if($i ~ /^wbytes=/){split($i,a,"="); s+=a[2]}} END{print s+0}' /sys/fs/cgroup/io.stat 2>/dev/null)
[ -n "$wb" ] && echo "disk_wbytes=$wb"

pc=$(ls -d /proc/[0-9]* 2>/dev/null | wc -l)
echo "proc_count=$pc"
`

// runResourcesResponse is the Sandbox widget's payload.
//
// HONESTY (the whole point of this file): every field is a POINTER, never a
// bare number. gVisor (CC3/Vault) presents a synthetic procfs/sysfs and may
// not expose some or all of these cgroup files at all — a key the sandbox
// did not report comes back ABSENT (nil, omitted from the JSON) so the UI
// renders "not available on this barrier" for it. A bare int would have to
// pick some zero value for "did not report", and 0 reads as "this governed
// workload is using no memory/CPU/disk" — a lie the operator would act on.
// A pointer to a genuine 0 (the sandbox DID report zero bytes written, say)
// still serializes as 0: encoding/json's omitempty on a pointer looks only
// at nilness, never at the pointed-to value.
type runResourcesResponse struct {
	CPUPercent       *float64 `json:"cpu_percent,omitempty"`
	MemoryUsedBytes  *int64   `json:"memory_used_bytes,omitempty"`
	MemoryLimitBytes *int64   `json:"memory_limit_bytes,omitempty"`
	DiskWrittenBytes *int64   `json:"disk_written_bytes,omitempty"`
	ProcessCount     *int     `json:"process_count,omitempty"`
}

// runResourcesUnsupportedMsg is the honest 501 reason, shared by BOTH the
// no-runner-configured guard and the ErrExecStreamUnsupported case: reading
// cgroup/procfs needs an exec channel into the sandbox, and a runner without
// one cannot be inspected at all. Deliberately NOT phrased as a confinement-
// tier limit — ErrExecStreamUnsupported is a property of the RUNNER, and
// blaming the barrier would send an operator to change a security setting that
// has nothing to do with it.
const runResourcesUnsupportedMsg = "sandbox resource usage needs sandbox exec, which this runner does not support"

// handleRunResources serves GET /api/v1/runs/{id}/resources.
func (s *Server) handleRunResources(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}
	// A runner must be wired to exec into anything (headless API mode cannot).
	if s.cfg.Runner == nil {
		// 501, matching handleRunFiles' identical guard — NOT 503. The two
		// widgets sit beside each other in the same rail reading the same
		// deployment, so a split verdict renders as "this runner can't be
		// inspected" next to "couldn't load right now": one a permanent
		// capability fact, the other a transient blip the operator will retry
		// forever. No runner configured is the former for both.
		writeError(w, http.StatusNotImplemented, runResourcesUnsupportedMsg)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), runResourcesExecTimeout)
	defer cancel()

	kv, err := s.execRunResourcesScript(ctx, run)
	if err != nil {
		// Audit on FAILURE only — the console polls this endpoint, so an audit
		// row per tick (the success path) would flood the trail. A failure is
		// the rare, interesting case an operator would want in the log.
		at, principal := actorFromRequest(r)
		s.recordAudit(r.Context(), s.auditEvent(&id, at, principal, "run.resources", id.String(), "failure",
			mustJSON(map[string]any{"error": err.Error()})))
		if errors.Is(err, runner.ErrExecStreamUnsupported) {
			// The human sentence AND the sentinel: the UI shows the first, an
			// operator reading a response body needs the second to tell this
			// apart from the no-runner-configured guard above, which produces
			// the same status and the same first half.
			writeError(w, http.StatusNotImplemented, runResourcesUnsupportedMsg+": "+err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "sandbox resource usage read failed")
		return
	}

	writeJSON(w, http.StatusOK, parseRunResourcesKV(kv))
}

// execRunResourcesScript launches runResourcesScript in run's sandbox and
// returns its parsed `key=value` output. The returned error is either the
// ExecStream launch failure (possibly runner.ErrExecStreamUnsupported) or a
// Stdout read failure (e.g. ctx's deadline killing the exec mid-read) —
// never a script exit code, see the Wait comment below.
func (s *Server) execRunResourcesScript(ctx context.Context, run types.AgentRun) (map[string]string, error) {
	sess, err := s.cfg.Runner.ExecStream(ctx, run.SandboxRef, runner.ExecSpec{
		Argv: []string{"/bin/sh", "-c", runResourcesScript},
	})
	if err != nil {
		return nil, err
	}
	// STREAMING CONTRACT (runner.go ExecSession doc): Stdout and Stderr are
	// unbuffered io.Pipes fed by ONE demux goroutine — a single undrained
	// stderr byte blocks that goroutine, Stdout, AND Wait. The script's own
	// reads are all individually `2>/dev/null`-guarded, but a shell-launch
	// failure itself (a BYOI image with no /bin/sh) writes ITS error to
	// stderr — drain it concurrently with the Stdout read below regardless,
	// or exactly that image hangs this handler forever.
	if sess.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	}
	var out []byte
	if sess.Stdout != nil {
		out, err = io.ReadAll(sess.Stdout)
	}
	if sess.Wait != nil {
		// Exit code unexamined: every read in the script is individually
		// presence-checked and guarded, so a partial sandbox (missing cgroup
		// files) still exits 0 with a partial key set — there's no failure
		// mode this script can signal via exit code that isn't already
		// visible as an absent key in its own output.
		_, _ = sess.Wait()
	}
	if sess.Close != nil {
		_ = sess.Close()
	}
	if err != nil {
		return nil, err
	}
	return parseKVLines(out), nil
}

// parseKVLines splits the script's stdout into a `key=value` map, ignoring
// blank and malformed lines rather than erroring on them — the script is the
// only writer, so this is defensive, not a real expected input shape.
func parseKVLines(out []byte) map[string]string {
	kv := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		kv[k] = v
	}
	return kv
}

// kvInt64/kvFloat64 look up and parse key from kv, reporting ok=false for
// both a missing key AND an unparsable value — either way the caller must
// treat the field as absent, never substitute a zero.
func kvInt64(kv map[string]string, key string) (int64, bool) {
	s, ok := kv[key]
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

func kvFloat64(kv map[string]string, key string) (float64, bool) {
	s, ok := kv[key]
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

// parseRunResourcesKV turns the script's raw key/value output into the
// response, leaving every field absent (nil) whose inputs weren't ALL
// present — see runResourcesResponse's honesty comment.
func parseRunResourcesKV(kv map[string]string) runResourcesResponse {
	var resp runResourcesResponse

	// CPU: needs both usage_usec samples, both uptime samples, and nproc — a
	// single sample cannot yield a percentage, so any one missing input
	// leaves CPUPercent absent rather than reporting a bogus rate.
	u1, ok1 := kvInt64(kv, "cpu_usage_usec_1")
	u2, ok2 := kvInt64(kv, "cpu_usage_usec_2")
	p1, ok3 := kvFloat64(kv, "uptime_1")
	p2, ok4 := kvFloat64(kv, "uptime_2")
	n, ok5 := kvInt64(kv, "nproc")
	if ok1 && ok2 && ok3 && ok4 && ok5 && n > 0 {
		if wallSec := p2 - p1; wallSec > 0 {
			pct := float64(u2-u1) / (wallSec * 1e6) / float64(n) * 100
			resp.CPUPercent = &pct
		}
	}

	if v, ok := kvInt64(kv, "memory_current"); ok {
		resp.MemoryUsedBytes = &v
	}
	if raw, ok := kv["memory_max"]; ok {
		if raw == "max" {
			// Unlimited cgroup: fall back to the sandbox host's own MemTotal —
			// only when that fallback is ITSELF available, never a fabricated
			// number.
			if mt, ok := kvInt64(kv, "mem_total_kb"); ok {
				v := mt * 1024
				resp.MemoryLimitBytes = &v
			}
		} else if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
			resp.MemoryLimitBytes = &v
		}
	}

	if v, ok := kvInt64(kv, "disk_wbytes"); ok {
		resp.DiskWrittenBytes = &v
	}

	if v, ok := kvInt64(kv, "proc_count"); ok {
		procCount := int(v)
		resp.ProcessCount = &procCount
	}

	return resp
}
