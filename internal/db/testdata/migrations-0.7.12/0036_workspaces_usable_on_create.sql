-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A workspace is usable the moment it is created.
--
-- Until 0.5 a new workspace was born 'pending_scan' and a scan run moved it to
-- 'scanned'. The 0.5 simplification replaced the 7-step workspace wizard with a
-- single dialog that does one POST and no scan/build/verify, so nothing
-- advances that status any more: every writer of 'scanned' targets a SOURCE row
-- (SetSourceScanResult), and the workspace's own writer
-- (SetWorkspaceImportState) is called from exactly one place, only ever to
-- write 'error', and only from a `case WorkspaceScanning` branch that nothing
-- can now reach either.
--
-- The result was a workspace pinned at 'pending_scan' forever, which the console
-- rendered as a permanent amber "Setting up" chip — a progress indicator for
-- work that was never going to happen. Nothing about the workspace was actually
-- unfinished: resolveCreateRunImage is explicitly fail-open, so a run attaches a
-- profile-less workspace exactly as it always did.
--
-- Heal every stuck row. 'scanning' is included for completeness: it was only
-- ever set by the deleted scan pipeline, so any row still wearing it was
-- orphaned mid-flight by an upgrade and is equally usable.
UPDATE workspaces
   SET status = 'scanned',
       updated_at = now()
 WHERE status IN ('pending_scan', 'scanning');
