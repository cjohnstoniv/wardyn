// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"

	"golang.org/x/sys/unix"
)

// AgentProcesses ends the agent account's processes in a guest (Server.EndAgent).
type AgentProcesses struct {
	Run CmdRunner
	UID uint32
}

// End signs the agent account out and kills every process left of it (endAccount), and says how many
// were running.
func (a AgentProcesses) End(ctx context.Context) (int, error) {
	list := func() ([]int, error) { return accountProcesses(a.UID) }
	return endAccount(ctx, a.Run, a.UID, list, func(pid int) error { return unix.Kill(pid, unix.SIGKILL) })
}

// accountProcesses lists the live processes of the account uid (ofAccount). A table that reads empty is
// an error: this process is always in it, and an empty read would pass for an ended account.
func accountProcesses(uid uint32) ([]int, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	if len(kps) == 0 {
		return nil, errors.New("the process table read empty")
	}
	var pids []int
	for i := range kps {
		p, e := &kps[i].Proc, &kps[i].Eproc
		if ofAccount(p.P_stat, e.Pcred.P_ruid, e.Ucred.Uid, e.Pcred.P_svuid, uid) {
			pids = append(pids, int(p.P_pid))
		}
	}
	return pids, nil
}
