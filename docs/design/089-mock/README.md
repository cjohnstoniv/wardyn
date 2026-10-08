# 0.8.9 mock review packets

The three written packets are reviewable. **Their design prototypes are not created or verified, and owner approval is pending.** Component synchronization of the console design-system project and the driveable prototypes remain outstanding. No console rendering is authorized by these documents alone.

| Packet | Independent scope | Status |
|---|---|---|
| [M-F](M-F.md) | #1901 saved/default extra-workspace refusal; #1906 terminal link token; #1908 sign-in reconciliation | Written packet reviewed; prototype pending; owner-unapproved |
| [M-R](M-R.md) | Four New Run panels, full rail, shared PolicyDocument, YAML-default editing, existing Access controls and strict-parser diagnostic states | Parser-diagnostic and field-semantics amendment awaits written review; prototype pending; owner-unapproved |
| [M-O](M-O.md) | #1831 honest recording-recovered output, explicit HTTP-200 gaps and genuine refusal precedence | Written correction reviewed; prototype pending; owner-unapproved |

Each packet has the established five parts: what it unblocks, surfaces, exact strings and homes, states with keyboard/screen-reader behavior, and decisions. Approve them independently only after the real prototype URL/revision and design-system verification are recorded. Nonvisual work proceeds meanwhile.

Supporting documents:

- [Canon inventory](canon-inventory.md) identifies the canonical source files and hashes at baseline. Existing strings keep their homes; additions/amendments are explicit in each packet.
- [Regression inventory](regression-inventory.md) records all 39 New Run e2e census matches, 27 New Run unit files, 20 Spec (JSON) test anchors, four invalid-JSON sentence sites, preserved controls, and the Segmented/Policy-tab provenance check.

The source baseline is `7b08fd722ca4dcfd9d2d59e6f1cb8ecab54d8dcf`. No source or rendering file was changed for these packets.

The prototype must be built on the console design-system project through the process in `docs/design/SYNC.md`: sync and grade the current components, create driveable state/focus prototypes, then record the owner's decision. A Markdown packet or a local cached anchor is not remote verification.

Browser, visual, assistive-technology and prototype walkthroughs have not run because no prototype exists; they remain part of the visual gate, not skipped passing checks.

M-O's correction closed the capture-gap case with the empty `stdout` frame, the partial recording frame, exact Copy/focus behavior and a proposed cause-neutral `captureGap` sentence. This covers written proposals only and does not approve the proposed copy.

The M-R amendment incorporates the policy-document parser's diagnostic inventory, adds a proposed position label and specifies invalid-source interaction. Its field-semantics correction aligns Access and acceptance criteria with the existing server contract; it changes no API behavior or visible copy. No parser or product-rendering source changed.
