# Canon source inventory

Baseline source inventory for the 089 mock round. These hashes identify the existing canonical bytes read while preparing the packets; they do not establish remote synchronization or prototype approval. Existing strings are imported from these homes, not copied into a second rulebook. New/changed strings are explicitly listed in M-F, M-R and M-O.

| Repository path | Owns | SHA-256 at baseline |
|---|---|---|
| `ui/src/app/components/wardyn/copy/new-run-rail.ts` | RAIL, RAIL_CHECK, RAIL_SETUP, RAIL_PROVIDER, RAIL_CREDENTIAL, recording strings | `15bcd91d1d63ec6798644f2c6f50fa4890ec75976cd61a6b24b1fa56673a8805` |
| `ui/src/app/components/wardyn/copy/policy-templates.ts` | POLICY_TEMPLATE_COPY default mode and templates; M-F adds saved hold | `1bbd6139510abd68671b69ce34d9e12a2d4bc5d1e53b68b06ac789c99edd0cb5` |
| `ui/src/app/components/wardyn/copy/run-clone.ts` | RUN clone/gone-policy copy; NO_BARRIER | `458f9edbc7c50c05944e13d3823e92a12fba9bb4d4cecab2d508450c04c9d7cb` |
| `ui/src/app/components/wardyn/copy/shell.ts` | UNSAVED_GUARD and DIRTY_CHIP | `89f1a97892b82e46e517b041d5d49d0f8b18021df159e8d95492c2c1e96f253a` |
| `ui/src/app/components/wardyn/model-access-copy.ts` | Existing model access and launch/door sentences | `0d377bfbc37e3b8e48f248f57d9b800ad4a6a64a3599bb109a1b076f15431e79` |
| `ui/src/app/components/wardyn/copy/door.ts` | CONNECTIONS actions and existing doors | `f04c8bc47d96795892af5818800ece2c899625735d45c92dc1144595a0a372d3` |
| `ui/src/app/components/screens/run-detail/policy-tab-copy.ts` | POLICY_TAB, CHANGE_HEADING, SUMMARY; unchanged Summary/used/redaction/marks | `ae8b411abf6edb10a23537809b2b80397a4c9857b585c13934a036b6a1748bf6` |
| `ui/src/app/components/wardyn/policy-panel.tsx` | Existing Spec (JSON), JSON validity, saved/custom mode literals | `99da245d17259c1738b442efd1094168a9fe9db1cd2d25b38b37cfb07f818a05` |
| `ui/src/app/lib/governance-copy.ts` | Ceiling/member/autonomy canon | `03195ef00d490dd796e2e43f18ad4fe0e22978ad625e86825677eeaab71e5dc1` |
| `ui/src/app/lib/workspace-copy.ts` | Workspace canon | `4caf3fd4f81d92e1846706df89d74eb4edc86fd44e3fcbb0d6f3306d71507490` |
| `ui/src/app/lib/permissions-copy.ts` | Permissions canon shared with Segmented consumers | `77ca8d2e10afc3a7c549175be1ce674eb55163d874861e9bb3e9af8cb64a5dc0` |
| `ui/src/app/lib/ado-access-copy.ts` | ADO capability names and access summary | `74d837104b57e164bafd5676ffa0f43a1c63ae10c33a9629c773881aac8c5399` |
| `ui/src/app/lib/ado-entra-copy.ts` | ADO connection/launch refusal canon | `6534ca4d3f59c1e164c3593782ba72cbed10f57346f978ce10a8fa6ac89e557e` |
| `ui/src/app/lib/ado-pat-copy.ts` | ADO per-run token/narrowing/connection facts | `7af85b5aa94b23766aff87b36575236aeb060e45fd7f099ee16b05a82d1921f4` |
| `ui/src/app/lib/workspace-providers-copy.ts` | Provider/ADO editor vocabulary | `2ced605e0bccee7f5ea0f179c562460da51d9255d738627ece756cb1c5f5d44e` |
| `ui/src/app/components/wardyn/copy/run-sign-in.ts` | RUN_SIGN_IN, no new M-F strings | `a819005783aa2fda8081ab4121adf1ea08cbb5e9a589206300d33b8ee16d130f` |
| `ui/src/app/components/wardyn/copy/runs-landing.ts` | Waiting row word/action | `e67745c3753c04e65bd6d4d164ef11799efca2addccd90a8434ffd321667ef99` |
| `ui/src/app/lib/reauth-copy.ts` | REAUTH_DIALOG, REAUTH_RENEW, role change | `04f811de8d5d22716bce08f11e01ec026d2b39f06ff8c089552ca8f99d24e6f0` |
| `ui/src/app/lib/session-renew-copy.ts` | Eager Sign in again constant only | `9956ed73f8b8efa6860affdb9cbfd6598bd68d9d2e5e302b58f0495fab1b048b` |
| `ui/src/app/components/wardyn/copy/run-output.ts` | RUN_OUTPUT; M-O proposes two additions and an explicit captureGap copy amendment | `ab5a2b6fac61c2a603baed928b8fd5f4264a7e558c6a67af9352dcc1a6451dfe` |
| `ui/src/app/components/screens/new-run/new-run-launch-gates.ts` | Exact local gate ordering and literals | `871945986c32b93dcbe13a7a3d2707b2f0edaaece227cc6c8d73a3aa60e1889b` |
| `ui/src/app/components/screens/new-run/new-run-launch-panel.tsx` | Existing backend/pin/model/launch gate composition | `4ebbc414ff2dd73bd4e6446ebdd5e32a49804885962a1c674645bf64bc302a96` |
| `ui/src/styles/theme.css` | Token and reduced-motion authority | `57d13af9da0482085bba7cdab70c4453385bbfd9aeb562af640a0e5fba6c1954` |
| `docs/design/CONSOLE-RULES.md` | Binding token/component/copy/mock rubric | `546630f9b3fc62d01d24b23acc044109824d7ccd9c515b892d159dc0b2e97052` |
| `docs/design/SYNC.md` | Claude Design / DesignSync and driveable prototype process | `5e7fd4f7ce8881b20d2953e2e52445f1861c7792c20e0a3840b3944038fe19c8` |
| `.design-sync/config.json` | Tracked design-system package configuration | `c2b3d0ccefda88bb9f27a85bdac2a147f9546caf1922888aea1b646dfa46edfb` |
| `.design-sync/NOTES.md` | Build/grade/upload ordering gotchas | `11fea2e85ce8bafbcbea8c8eee18fe6795ed2c146c4db8dd0740a922e786d0b6` |
| `.design-sync/conventions.md` | Design-system library conventions | `5a9bd255158dfa6287a9cf05b4b20235792b4e3d6968b19c56dc7e0dd54e5731` |

## Parser candidate for the M-R diagnostic amendment

This is a separate inventory at Y candidate `3b4af5aee746aff0c496bb370001295d50973e25`, not a replacement for the original baseline hashes above. `ui/src/app/lib/policy-document/index.ts` has SHA-256 `dfa5aa6ecbbe52f87f8d8a3f8671dd1943458b3589b9a4f3106ea268553003b2`; `ui/package.json` pins `yaml` exactly to `2.9.1`. The parser file is byte-identical to the earlier supplement's candidate `9656bfdc2f42007925f1704d0f779d1dddca297c`.

M-R's strict-parser table is the single proposed surfaced inventory for `caught`, `documentValue`, `readSource` and `editPolicySource`. Fixed diagnostics retain that lazy parser home; library/thrown messages remain potentially sensitive variable data rendered only as text. M-R explicitly adds `POLICY_DOCUMENT.SOURCE_POSITION(line,column)` in the proposed lazy `copy/policy-document.ts` home and keeps the existing outer validity/gate proposals. Nothing is added to the eager copy barrel. The new inventory and states await independent written review, the real Claude Design/DesignSync prototype and owner approval; the earlier written acceptance at `036dd8081c137268036404110a0c5e235a0a33f8` does not approve this amendment.

## Exact existing mode copy carried into M-R

| Home/key | Exact text |
|---|---|
| POLICY_TEMPLATE_COPY.DEFAULT_TITLE | Use the default policy |
| POLICY_TEMPLATE_COPY.DEFAULT_HINT | Launch under the policy set for this deployment. |
| POLICY_TEMPLATE_COPY.DEFAULT_HINT_PROFILE(name) | Launch under the policy set by your profile, {name}. |
| POLICY_TEMPLATE_COPY.DEFAULT_PREVIEW | Default policy, read-only |
| POLICY_TEMPLATE_COPY.DEFAULT_NOTE | This run launches under this policy as it stands. Your attached workspace mounts into it; nothing else on this page is merged. |
| POLICY_TEMPLATE_COPY.DEFAULT_LOADING | Loading the default policy… |
| POLICY_TEMPLATE_COPY.DEFAULT_UNAVAILABLE | Couldn't load the default policy to show here. The run still launches under it. |
| policy-panel saved title | Reuse a saved policy |
| policy-panel saved hint | One your operators already wrote and named. |
| policy-panel custom title | Custom policy |
| policy-panel custom hint | Start from a template and edit the spec for this run. |
| POLICY_TAB.viewSummary | Summary |
| POLICY_TAB.viewYaml | YAML |
| POLICY_TAB.copyYaml | Copy YAML |
| SUMMARY.used | This run used |
| POLICY_TAB.hidden | Hidden |
| POLICY_TAB.hiddenTip | Only admins can see this. |
| POLICY_TAB.redacted | `Values shown as <redacted> are hidden from you. Fill them in before using this as a policy.` |
| POLICY_TAB.chipAdded | Added at start |
| POLICY_TAB.chipRemoved | Removed at start |

## Rail inventory, in order

| Section | Decision | Existing source / conditional facts |
|---|---|---|
| Ceiling | Keep | GOVERNANCE.CEILING_TITLE and MEMBER ceiling-profile sentence, when applicable |
| Policy | Keep | Saved-source identity and stored-spec note |
| Barrier | Keep | Actual chosen/requested class and existing floor/host facts |
| Autonomy | Keep | AUTONOMY_RAIL, profile/no-profile/derived hold facts; pending stays pending |
| Credentials | Keep summary; move only picker to Run | RAIL_PROVIDER/RAIL_CREDENTIAL, R1–R9 and git credential facts |
| Startup | Keep | Existing startup mode/command facts |
| Tool rules | Keep | Existing source counts/summary |
| Push rules | Keep | Existing PUSH.RAIL_TITLE/BODY/UNATTENDED, hidden when no path rule |
| Recording | Keep | Existing tri-state useRecordingDisabled; unknown does not claim on or off |

Drive summary remains with the existing workspace/files facts; retaining the nine-section inventory does not remove the user-drives rail line. No section removal is proposed.

## Reuse provenance and validation limit

`regression-inventory.md` records Segmented/Policy-tab introducing commits and the local-note search before the proposed future duplicate removal. This lane changes documentation only and deletes no product code. Source and literal checks can verify the packet against this baseline; remote project type/access, actual uploaded assets and keyboard walkthroughs require the unavailable DesignSync transport.
