// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// task is one thread from /proc.
type task struct {
	pid, tid   int
	ruid, suid int
	state      byte
}

// listTasks reads every thread in the container's pid namespace. A task that
// exits mid-read is skipped.
func listTasks(proc string) []task {
	var out []task
	dirs, _ := os.ReadDir(proc)
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		tids, _ := os.ReadDir(filepath.Join(proc, d.Name(), "task"))
		for _, t := range tids {
			tid, err := strconv.Atoi(t.Name())
			if err != nil {
				continue
			}
			base := filepath.Join(proc, d.Name(), "task", t.Name())
			ruid, suid, ok := readUIDs(filepath.Join(base, "status"))
			st, ok2 := readState(filepath.Join(base, "stat"))
			if ok && ok2 {
				out = append(out, task{pid: pid, tid: tid, ruid: ruid, suid: suid, state: st})
			}
		}
	}
	return out
}

func readUIDs(path string) (ruid, suid int, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 4 && f[0] == "Uid:" {
			r, e1 := strconv.Atoi(f[1])
			s, e2 := strconv.Atoi(f[3])
			return r, s, e1 == nil && e2 == nil
		}
	}
	return 0, 0, false
}

// readState returns the state letter of a /proc stat line ("pid (comm) S ...").
func readState(path string) (byte, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 || i+2 >= len(b) {
		return 0, false
	}
	return b[i+2], true
}

// signalled reports whether kill(-1) from uid reaches t: the sender's uid
// matches the target's real or saved uid.
func signalled(t task, uid int) bool { return t.ruid == uid || t.suid == uid }

// running lists the pids of uid's tasks that are not stopped, other than self.
// Zombies and dead tasks are not running.
func running(tasks []task, uid, self int) []int {
	var out []int
	for _, t := range tasks {
		if t.pid == self || !signalled(t, uid) {
			continue
		}
		switch t.state {
		case 'T', 't', 'Z', 'X', 'x':
			continue
		}
		if !slices.Contains(out, t.pid) {
			out = append(out, t.pid)
		}
	}
	return out
}

// stoppedPIDs lists the pids of uid's tasks already stopped before a pause, so
// a resume leaves them as it found them.
func stoppedPIDs(tasks []task, uid, self int) []int {
	var out []int
	for _, t := range tasks {
		if t.pid != self && t.tid == t.pid && signalled(t, uid) && (t.state == 'T' || t.state == 't') {
			out = append(out, t.pid)
		}
	}
	return out
}

// pauseResult is pause-agent's stdout, handed back to resume-agent by wardynd.
type pauseResult struct {
	AlreadyStopped []int `json:"already_stopped"`
}

// pauseVerifyFor bounds how long a pause waits for every task to stop (a task
// in uninterruptible sleep stops when it leaves it).
const pauseVerifyFor = 3 * time.Second

// cmdPauseAgent stops every process of the agent's uid but itself, then
// proves it. kill(-1) cannot be caught and races no fork; the container's
// pid 1 is stopped by wardynd from outside the namespace before this runs.
// If the proof fails the agent is resumed and the pause refused.
func cmdPauseAgent(out io.Writer) error {
	if err := checkAgentUID(); err != nil {
		return err
	}
	uid, self := os.Getuid(), os.Getpid()
	res := pauseResult{AlreadyStopped: stoppedPIDs(listTasks("/proc"), uid, self)}
	if err := syscall.Kill(-1, syscall.SIGSTOP); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("stop the agent: %w", err)
	}
	deadline := time.Now().Add(pauseVerifyFor)
	for {
		left := running(listTasks("/proc"), uid, self)
		if len(left) == 0 {
			return json.NewEncoder(out).Encode(res)
		}
		if time.Now().After(deadline) {
			resume(uid, self, res.AlreadyStopped)
			return fmt.Errorf("the agent did not stop: %d process(es) still running; the agent was resumed", len(left))
		}
		for _, pid := range left {
			_ = syscall.Kill(pid, syscall.SIGSTOP)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// cmdResumeAgent continues every stopped process of the agent's uid except
// the ones pause-agent found already stopped (its JSON, on stdin).
func cmdResumeAgent(in io.Reader) error {
	if err := checkAgentUID(); err != nil {
		return err
	}
	var res pauseResult
	if err := json.NewDecoder(io.LimitReader(in, 1<<20)).Decode(&res); err != nil && err != io.EOF {
		return fmt.Errorf("read the pause record: %w", err)
	}
	resume(os.Getuid(), os.Getpid(), res.AlreadyStopped)
	return nil
}

func resume(uid, self int, keep []int) {
	seen := map[int]bool{}
	for _, t := range listTasks("/proc") {
		if t.pid == self || seen[t.pid] || !signalled(t, uid) || slices.Contains(keep, t.pid) {
			continue
		}
		seen[t.pid] = true
		_ = syscall.Kill(t.pid, syscall.SIGCONT)
	}
}
