# Design notes

- This folder holds the design notes behind the console and the releases. It is not documentation: the guides are in [docs/README.md](../README.md).
- A note is **current** while it still governs a screen or a doc, and a **record** once it is the history of a shipped release or round.
- Read a record for why something was built. Read the guide it points at for how it works now.

## Index

| Group | Notes | Status |
|---|---|---|
| Living rules | [CONSOLE-RULES.md](CONSOLE-RULES.md), [SYNC.md](SYNC.md) | Current |
| Canon: the app's frozen text | Every `*-canon.md` here, `491-default-admin-mock/canon.md` and `542-rail-gaps-mock/canon.md` | Current |
| Approved mock prompts and mocks | The `*-prompt.md` and `*-mock/` pairs for governance, user drives, workspace providers, people access, Azure DevOps Entra, available-to, model providers, permissioning and UI sandboxes, and [model-access-prompt.md](model-access-prompt.md) | Current while a doc or test names the note, otherwise a record |
| Release designs | [0.8/PLAN.md](0.8/PLAN.md) and its `0.8.6-*.md` notes, [0.9/PLAN.md](0.9/PLAN.md) and its notes, [hybrid-0.8.md](hybrid-0.8.md), [member-role-desktop.md](member-role-desktop.md) | Record |
| The organisation runner design | [0.9/k8s-runnerd.md](0.9/k8s-runnerd.md), linked from the Kubernetes runner guide | Current |
| Rounds and prototypes | `089-mock`, [ui-batch2-mock.md](ui-batch2-mock.md), [ui-batch3-mock.md](ui-batch3-mock.md), `console-residue-0.8-prompt.md` with its mock, `first-contact-prototype` | Record |

## Rules for a note

- A `*-canon.md` note holds the app's text of record. The console's strings match it, so a change to the text changes the note in the same commit.
- This folder is exempt from the link gate's must-link rule, because it quotes frozen strings. Its links must still resolve.
