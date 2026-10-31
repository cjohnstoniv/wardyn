# Runs filters on a phone: canon strings

The strings the Runs list shows under the `sm` breakpoint (#3041): the Filters (n) button, its bottom sheet and the removable chips. They live once in this file and once in `RUNS_FILTERS_SHEET` and `RUNS_FILTERS` in `ui/src/app/components/wardyn/copy/runs-landing.ts`. `ui/src/app/lib/runs-filters-copy.test.ts` parses the table back and compares each row with its constant, byte for byte, in both directions. The control labels and option texts the sheet reuses (`Status · Needs you`, `Save view`, and the rest) are the Runs filter bar's own and are not repeated here.

The source is the approved R6 Run phone board (frames 1 to 3). A key with arguments is a function; the test calls it with each argument written as `{name}`.

## 1. Frozen strings

| Key | Source | Frozen string |
|---|---|---|
| `TITLE` | mock | Filters |
| `BUTTON_COUNT(n)` | mock | Filters ({n}) |
| `CLEAR` | mock | Clear filters |
| `SHOW(n)` | mock, plural other | Show {n} runs |
| `SHOW_ONE` | derived: plural one of `SHOW` | Show 1 run |
| `SHOW_MORE(n)` | derived: the page header's N+ for a list cut at its limit | Show {n}+ runs |
| `CHIP_REMOVE_ARIA(chip)` | derived: the chip's remove button name, not drawn | Remove {chip} |
| `WORKSPACE_CHIP(w)` | derived: board chip pattern (R6-Run-Phone) | Workspace · {w} |

| `SEGMENT_ALL` | mock | All |
| `SEGMENT_AGENT_TASKS` | mock | Agent tasks |
| `SEGMENT_ENVIRONMENTS` | mock | Environments |
| `SEGMENT_APPS` | mock | Apps & scripts |
| `SEGMENT_SERVICES` | mock | Services |
| `SEGMENT_SERVICES_EMPTY` | mock | No services yet. Start one with Keep it running on an Automation / script run. |
| `SEGMENT_LABEL` | derived: the segment group's accessible name, not drawn | Run shape |

`TITLE` is the sheet's title and the button's label when no filter is active. `WORKSPACE_CHIP` is the one key that lives in `RUNS_FILTERS`. The `SEGMENT_*` keys live in `RUNS_SEGMENTS` (the shape segments above the filter bar, not a phone-only surface).
