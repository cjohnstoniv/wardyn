# 0.8.9 mock review packets

The three written packets are reviewable. Design prototypes now exist and the owner approved all three (2026-10-08): M-F at prototype version `1791421176-82e4`, M-O at `1791419190-5f01`, and M-R at Version 7 (`1791427781-730a`), which is what shipped in 0.8.9. Each approval covers only its own packet and only what that version shows. M-O's refused-viewer server answer stayed open at approval. For M-R, the Access rows are approved as direction only (their rules are superseded by the owner's 2026-10-07 component decisions), and any backend component design is not approved.

| Packet | Independent scope | Status |
|---|---|---|
| [M-F](M-F.md) | #1901 saved/default extra-workspace refusal; #1906 terminal link token; #1908 sign-in reconciliation | Owner-approved 2026-10-08 at prototype version `1791421176-82e4` |
| [M-R](M-R.md) | Four New Run panels, full rail, shared PolicyDocument, YAML-default editing, existing Access controls and strict-parser diagnostic states | Owner-approved 2026-10-08 at prototype Version 7 (`1791427781-730a`); shipped in 0.8.9 |
| [M-O](M-O.md) | #1831 honest recording-recovered output, explicit HTTP-200 gaps and genuine refusal precedence | Owner-approved 2026-10-08 at prototype version `1791419190-5f01`; refused-viewer answer open |

Each packet has the established five parts: what it unblocks, surfaces, exact strings and homes, states with keyboard/screen-reader behavior, and decisions. Each was approved independently against its recorded prototype revision.

Supporting documents:

- [Canon inventory](canon-inventory.md) identifies the canonical source files and hashes at baseline. Existing strings keep their homes; additions/amendments are explicit in each packet.
- [Regression inventory](regression-inventory.md) records all 39 New Run e2e census matches, 27 New Run unit files, 20 Spec (JSON) test anchors, four invalid-JSON sentence sites, preserved controls, and the Segmented/Policy-tab provenance check.

The source baseline is `7b08fd722ca4dcfd9d2d59e6f1cb8ecab54d8dcf`. No source or rendering file was changed for these packets.

The prototypes were built on the console design-system project through the process in `docs/design/SYNC.md`: sync and grade the current components, create driveable state/focus prototypes, then record the owner's decision. A Markdown packet or a local cached anchor is not remote verification.

Browser, visual and assistive-technology walkthroughs of the shipped console remain part of the visual gate; the prototype approvals do not replace them.

M-O's correction closed the capture-gap case with the empty `stdout` frame, the partial recording frame, exact Copy/focus behavior and a proposed cause-neutral `captureGap` sentence. This covers written proposals only and does not approve the proposed copy.

The M-R amendment incorporates the policy-document parser's diagnostic inventory, adds a proposed position label and specifies invalid-source interaction. Its field-semantics correction aligns Access and acceptance criteria with the existing server contract; it changes no API behavior or visible copy. No parser or product-rendering source changed.
