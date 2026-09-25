-- Link a run to the onboarded workspace it SCANS. Set only on a governed repo
-- scan run (handleScanWorkspace); NULL for every ordinary run. The scan-result
-- endpoint reads this to persist a repo's scanned profile onto the correct
-- workspace from TRUSTED server-side state — never from sandbox-supplied input
-- (the proxy deliberately strips the sandbox's query string on the brokered
-- scan-result route). A non-NULL workspace_id also marks the run as scan-only:
-- the driver runs wardyn-scan after cloning instead of the agent.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS workspace_id UUID;
