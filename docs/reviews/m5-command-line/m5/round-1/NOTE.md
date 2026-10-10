# M5 round 1 — consolidation

Four reviewer slots ran, three of them requested (`astra`, `ds4`, `glm`) plus the
config default `gpt-5.5`. Two of the four produced no report: `astra` and
`gpt-5.5` both answered `[Error: Our servers are currently overloaded. Please try
again later.]`, which `pi_review_cycle` recorded as a successful execution because
a file was written. The reports here are `ds4` and `glm`; the two error strings
were deleted rather than kept as reviews.

Zero HIGH, zero CRITICAL. One MEDIUM and fourteen LOW, deduplicated below.

## Fixed

- **MEDIUM — `%q` where Python writes `!r`.** `family_note`, the three
  `Calibration.Applicability` reasons, the assumed-family warning and the
  not-applied warning were built with Go's `%q`, which is double-quoted. Python
  interpolates all of them with `!r`. They reach `doctor`'s `note` line, the
  indented reason lines under `calibration`, and `doctor --json`. Now built with
  the package's own `pythonQuote`, and the exact lines are asserted in
  `TestDoctorText` and `TestDoctorQuotesLikePython`.
- `explain` printed the think gate with `%v`, so an integral gate printed `1`
  where Python prints `1.0`. Now `PythonStr`, in the output and in the
  `--reasoning-prob` help default.
- An explicitly empty `--family`, `--policy`, `--reads` or `--calibration` was
  read as "not given". `argparse` validates `choices` at parse time, so all four
  exit 2 there; `--policy "$POLICY"` with `POLICY` unset silently took the
  default here. All four now record that they were given.
- `--family` went through `NormalizeFamily`, so `--family DE-2` was accepted
  where `argparse`'s `choices` refuses it. Now an exact `de1`/`de2` match.
- `stateFrom`, `labelsFrom` and `apiKeyFromEnv` trimmed with
  `strings.TrimSpace`, which does not strip U+001C-U+001F. Python's `str.strip()`
  does, so `--state $'\x1c{"a": 1}'` was JSON to Python and plain text here. The
  library now exports `TrimPythonSpace` and the command line uses it.
- `explain` took the read before printing anything, so a failed read printed
  nothing where Python leaves sections 1-2 on stdout. The read now comes after
  section 2, which is Python's order; `TestFailedRead` pins both halves.
- `orderedSlots` sorted the code summary, which matched Python's codebook order
  only while the first two codes happened to sort that way. `Health` now builds
  the summary as an `Object`, so the order is the codebook's by construction and
  the sort is gone.
- A null logprob printed as `0.0000`, a number the server never sent. It now
  prints `None`; `topLogprob` carries the null.
- `--compound` was registered but missing from the hand-written `--help`.
- The bare word `help` exited 0; `argparse` has no such subcommand, so it exits 2
  as an unknown word. Only `-h`/`--help` print the usage.
- `stateFrom`'s error return was always nil, so two exit-2 branches were dead.
  Dropped.
- A `state.go` comment named a `defaultState` identifier that does not exist, and
  `orderedSlots`'s comment promised an order the code did not produce.

## Tests added

`TestDoctorNotOKExitsOne` (the exit contract in both output forms),
`TestReportOrderCoversHealth` (every report key is printed, over the four report
shapes that carry the conditional keys), `TestChoiceFlagsRejectAnEmptyValue`,
`TestFailedRead`, `TestStateFromTrimsPythonsWhitespace`,
`TestDoctorQuotesLikePython`, `TestExplainPrintsTheGateLikePython`, plus the
exact-quoting assertions and the `null` flag in `TestSortedTopLogprobs`.

## Recorded rather than fixed

`docs/PORT.md` now records the flag layer's remaining grammar differences from
`argparse` (no prefix abbreviation, `--reasoning=true` accepted, `--timeout 0`
read as the library default) and `explain`'s null-logprob and tie-break
handling.

## Re-run

`astra` did not execute, so round 2 re-runs the same three reviewers over the
full updated scope.
