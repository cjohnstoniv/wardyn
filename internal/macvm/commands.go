// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import "path/filepath"

// The helper's job commands runnerd starts, and their flag names (without
// the leading "--"). runnerd builds its argv from these.
const (
	PrepareCommand     = "prepare"
	UpdateToolsCommand = "update-tools"

	FlagRoot        = "root"         // the login's macos-vm.noindex
	FlagCPUs        = "cpus"         // the runner's per-run CPU limit, never raised
	FlagMemoryMiB   = "memory-mib"   // the runner's per-run memory limit in MiB, never raised
	FlagRunnerID    = "runner-id"    // the runner whose ledger entry the job holds
	FlagLedgerEntry = "ledger-entry" // the entry runnerd took; the helper claims it first
	FlagLedgerDir   = "ledger-dir"   // the ledger directory ("": admission.DefaultDir)
	FlagSockRoot    = "sock-root"    // the socket root ("": admission.DefaultSockDir)
	FlagSlot        = "slot"         // the base slot (default 0)
	FlagReprepare   = "reprepare"    // prepare only: build a new base and swap it in
	FlagKeepIPSW    = "keep-ipsw"    // prepare only: keep the restore image
	FlagIPSW        = "ipsw"         // prepare only: build from this restore image instead of downloading Apple's
	FlagSHA256      = "sha256"       // prepare only: the SHA-256 the --ipsw file or the --ipsw-url image must have
	FlagIPSWURL     = "ipsw-url"     // prepare only: download the restore image from this HTTPS mirror instead of Apple
	FlagForce       = "force"        // update-tools only: an update someone asked for
)

// JobControlSock is the ctl.sock of a prepare's or a tools update's stage-2
// boot holding ledger entry entryID: runnerd connects there and sends the
// NIC's descriptor (net_fd) once the helper listens.
func JobControlSock(sockRoot, entryID string) string {
	return filepath.Join(SockDir(sockRoot, JobSockKey(entryID)), ControlSock)
}
