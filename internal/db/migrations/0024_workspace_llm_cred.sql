-- Workspace/container as the execution environment with a bound model/harness
-- credential (Stage 5). Two additive changes, both riding the existing
-- opaque-blob / widen-CHECK precedent (0010 approved_egress, 0011 status) — no
-- new tables.
--
-- 1) A workspace can now be a CONTAINER: a bring-your-own base image as a named,
--    reusable execution environment (Source is the image ref, no mount). The
--    inline kind CHECK from 0008 is auto-named workspaces_kind_check; drop-if-
--    exists then re-add widened to include 'container'.
ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_kind_check;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_kind_check
    CHECK (kind IN ('local_dir', 'repo', 'container'));

-- 2) llm_cred: the OPERATOR-owned model/harness credential BINDING on a
--    workspace/container — {mode, api_key_secret?, bedrock?} — refs/NAMES only,
--    never secret values (the SiteConfig precedent). A run that picks this
--    workspace inherits it (applyWorkspaceCreds folds it into the run policy at
--    create). NULL => no binding; the run uses the global provider config, or is
--    a plain governed command. Written only via SetWorkspaceLLMCred, mirroring
--    approved_egress's operator-owned discipline.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS llm_cred JSONB;
