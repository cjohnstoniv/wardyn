-- Operator-owned per-workspace egress approvals (workspace-scanner "needs"
-- feature): hosts an operator explicitly promoted from the scanner's
-- content-derived SuggestedEgress. OWNED BY THE OPERATOR — never written by a
-- scan (scans rebuild the profile blob only, and a hostile repo's content must
-- never widen an allowlist without an explicit human approval), and cleared
-- when the workspace's source/kind changes (the approval was reviewed against
-- the old content). NULL/empty = nothing approved.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS approved_egress JSONB;
