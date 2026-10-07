# Claude Design / DesignSync access evidence

Status: **remote verification blocked by an unavailable DesignSync connection**. No remote project was opened, listed, changed, uploaded to or verified for this mock round. No concrete Claude Design prototype has been created. There is no owner-approval claim.

Recorded 2026-10-07 for lane M, tracker #1916 under #1914, repository baseline `7b08fd722ca4dcfd9d2d59e6f1cb8ecab54d8dcf`.

## Evidence

| Check | Result |
|---|---|
| Current session tool metadata searched for `Claude`, `DesignSync`, `finalize_plan`, `write_files` and design capability | No callable Claude Design or DesignSync provider. Root additionally searched the plugin directory and found no relevant provider. |
| `command -v claude` | Exit 0; `/home/cjohn/.local/bin/claude`. |
| `claude --version` | Exit 0; `2.1.292 (Claude Code)`. |
| `claude mcp list` | Exit 0. Account-linked Claude Docs was connected; configured browser/database/other connectors were listed. **No DesignSync or Claude Design server was listed.** Unrelated connectors' health is not evidence about DesignSync. |
| `timeout 15s claude mcp get DesignSync` | Exit 1; `No MCP server named "DesignSync".` |
| Local capability-path inventory under `~/.claude/commands`, `~/.claude/skills`, `~/.claude/plugins` | No `design-sync`, `design-login`, `designsync` or `claude-design` skill found. No private credential values were printed. |
| Main checkout `.design-sync/project.local.json` | Present; its account-specific `projectId` was read from that untracked file and is deliberately not copied into these shareable docs. It is local input, **not** evidence that this account can read/write it or that it is a design-system project. |
| Lane worktree `.design-sync/project.local.json` | Absent, as expected for untracked account-specific state. The ID was not copied into tracked content in the repository. |
| Main checkout `.ds-sync/` | Staged local converter exists (`resync.mjs`, package build/validate/capture and dependencies). Its driver expects the agent to obtain the remote anchor through DesignSync and upload through DesignSync; the converter is not a remote transport or sign-in tool. |

The local cached anchor and earlier account IDs in `.design-sync/NOTES.md` were not treated as current remote proof. Claude Docs, Figma and Playwright are not a substitute for the owner-specified provider. No authentication token, session cookie or credential file contents were printed or copied into the artifacts.

## What is needed to clear this gate

The execution environment must expose Claude Design's DesignSync tools under the intended account. `docs/design/SYNC.md` says: “`/design-login` in the session. The design tool needs its own authorization even when the session is already signed in.” Once connected, list writable projects and check the current ID against that list; inspect the project type and remote files before calling it the console design system. If the account cannot access that ID, select or create an account-accessible **design-system** project and update only the account-local file.

Then follow `docs/design/SYNC.md` and `.design-sync/NOTES.md`: build current UI CSS; copy the stable stylesheet; apply the dark-card override; build/validate the bundle; fetch the current remote anchor before a differential resync; grade the generated previews; call `finalize_plan` on the exact writes/deletes; write `_ds_needs_recompile` first, content in chunks of at most 256 paths, deletes as planned, and `_ds_sync.json` last. A first sync has no trusted remote anchor and requires a full upload. Verify the resulting remote project and create the three independently reviewable, driveable prototypes M-F, M-R and M-O from it.

Record each real prototype URL, immutable revision or content hash, design-system project verification, keyboard/state walkthrough and owner decision before clearing its visual gate. The completed local packets and copy/state inventories reduce the remaining authoring work; they do not stand in for this verification.

No request to install a different provider was made. No remote approval request was sent to anyone.
