# M5 round 2 — consolidation

Requested reviewers: `astra`, `ds4`, `glm`. `astra` failed to execute for the
second time with `[Error: Our servers are currently overloaded. Please try again
later.]`, which `pi_review_cycle` records as a successful execution because a
file was written. `ds4` and `glm` both produced real reports. Zero HIGH, zero
CRITICAL; one MEDIUM-confidence and eight LOW-confidence findings, deduplicated
below.

## Fixed

- **The `%q`-for-`!r` sweep stopped at `client.go` and `calibration.go`.** The
  readout's three boundary/token-count messages and the code-slot messages are
  otherwise word-for-word Python's and are built with `!r` there; they reach
  `doctor`'s `boundary_check` and `read_probe` fields in both output forms and
  `explain`'s stderr, which is exactly the failing run the parity diff could not
  see. All of them now use `pythonQuote`, and
  `TestBoundaryMessagesQuoteLikePython` asserts the whole sentence.
- **`APIKeyFromEnv` stripped with `strings.TrimSpace`.** Python's
  `_api_key_from_env` uses `str.strip()`, and this value is the bearer token, so
  a key padded with U+001C-U+001F was sent verbatim here and stripped there: a
  different `Authorization` header. Now `TrimPythonSpace`, pinned in
  `TestAPIKeyFromEnv`.
- `TestReportOrderCoversHealth`'s `family_note` guard compared a three-element
  argv to two, so the assertion never ran. Fixed to `len(argv) == 3`.
- An explicitly empty `--state` was still untested, which was the third of
  round 1's three untested output paths. `TestAskState` now pins that
  `--state ''` reaches the prompt as `"evidence": ""` rather than falling back to
  the built-in example.

## Recorded rather than fixed

`docs/PORT.md` now records four things the reviewers found unrecorded: `Health`'s
`slots` is an `Object` (a public shape change, and its `encoding/json` form is an
array of members); the usage, `--help` and error text is this port's own layout
and wording, with only the flag set, exit statuses and streams matching
`argparse`; `ResolveCalibration("")` means "auto" where Python raises, because a
Go `string` cannot carry `None` and the command line refuses an explicit `""`
first; and the DE-2 readout rewords the messages it shares with DE-1
(letter/letter slots to code/codebook) plus the two checks that have no Python
counterpart.

## Not changed

`Object` was left without a `MarshalJSON`, which the reviewer offered as an
alternative to recording the shape. Adding one would change what
`encoding/json` does to every `Object` in the library, and the two candidate
spellings (Python's `json.dumps` separators, or `encoding/json`'s) would each
surprise a caller in a different way; the milestone is the command line, and
`PythonJSON` already writes the object Python writes.

## Correction (found in round 3)

The paragraph below claims `PORT.md` now records the usage, `--help` and error
text. It did not: the edit that was meant to add it did not match the file and
silently did nothing. Round 3 caught it and the sentence is in `PORT.md` now.

## Round 3

Two of the three requested reviewers executed and reported no HIGH or CRITICAL
finding. `astra` did not execute, and this was the second attempt at it; the
provider returned the same overload error both times. Round 3 re-runs the three
over the full updated scope, per the round-2 rule, and is the last round before
`maxCycles`.
