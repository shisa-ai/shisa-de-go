# Review Report

## Summary

I re-reviewed the M5 command line in `shisa-de-go` (the scope's file paths resolve there; the review records live in `shisa-de`) against `shisa_de/cli.py` and the round-1 fix set. I verified every fix the consolidation claims, checked whether any fix introduced a new divergence, and walked the CLI output paths line by line against the reference.

The round-1 fix set holds up:

- The MEDIUM `%q`-vs-`!r` finding is closed: `family_note` (client.go:769-771), the three `Applicability` reasons (calibration.go:250, 255, 275) and both warnings use `pythonQuote`, and `TestDoctorText`/`TestDoctorQuotesLikePython` assert the exact single-quoted lines. `TestPythonQuoteMatchesPythonRepr` still pins `pythonQuote` against the generated `repr` table, so the quoting is the interpreter's, not merely single-vs-double.
- The gate spelling is closed: `PythonStr` in `explain`'s footer and in the `--reasoning-prob` help default, pinned by `TestExplainPrintsTheGateLikePython` including the integral `1.0` case.
- The set-aware choice flags are correct: all four record set-ness, an explicit empty is a usage error (exit 2), `--family` is an exact `de1`/`de2` match matching `argparse`'s `choices=FAMILIES` (no legitimate spelling is broken — argparse accepts exactly those two), and `--calibration ''` reproduces Python's own `ValueError` text. `--calibration auto` matches `resolve_calibration`'s "auto" branch.
- The read order is Python's again: `explain` prints sections 1-2, then reads, then prints section 3. `TestFailedRead` is load-bearing — a 500 from the stub leaves sections 1-2 on stdout, no section 3, and the error on stderr, which is exactly the ordering claim. `TestExplainReportsAnUnreadableQuestion`'s comment now correctly attributes the boundary path to both implementations.
- `Health`'s `slots` is an `Object` with `count` first and the first two codes in codebook order, which is `{"count": ..., **{letter: id}}` by construction; `doctor --json` renders it through `PythonJSONWith(indent="  ", EnsureASCII: true)`, which is `json.dumps(report, indent=2)`. `reportOrder` still matches Python's insertion order including the conditional keys. `orderedSlots` and its wrong comment are gone.
- The null logprob prints `None` right-aligned in the 9-wide field, `topLogprob` carries the null, and PORT.md records both the null handling and the tie-break.
- `--compound` is listed in `commonFlags`; I checked every registered flag is described for all three commands and none extra.
- The bare word `help` is an unknown command, exit 2, pinned in `TestRunDispatch`.
- `TrimPythonSpace` is the right export (Python's `str.strip()` set, including U+001C-U+001F) and the three CLI strips use it: `stateFrom`, `labelsFrom`, `apiKeyFromEnv`, pinned by `TestStateFromTrimsPythonsWhitespace`.
- `stateFrom`'s dead error return is gone, the `defaultState` comment is fixed, and `ask.go` lost its dead branch.
- PORT.md records the remaining `argparse` grammar differences (no prefix abbreviation, `--reasoning=true`, `--timeout 0`) and the "Verified parity" paragraph now says the manual diffs pin the measured run while the offline suite covers the rest — an accurate narrowing.
- `TestReportOrderCoversHealth` is load-bearing: the three invocations plus the hand-built DE-1 record cover the union of conditional keys, and the length + unknown-key checks catch a `Health` key missing from `reportOrder` in both directions. `TestDoctorNotOKExitsOne` pins exit 1 in both output forms on an unlisted model.

No HIGH or MEDIUM findings. The four LOW findings below are: one place the round-1 whitespace fix stopped short of the library, one round-1 test gap that was not closed, one library-level `resolve_calibration` divergence the CLI paper-broader, and one unrecorded class of stderr difference.

## Findings

### LOW: The library's `APIKeyFromEnv` still strips with Go's whitespace set, not Python's
- Severity: LOW
- Confidence: HIGH
- Location: client.go:680 (`APIKeyFromEnv`)
- Problem: The round-1 fix exported `TrimPythonSpace` and applied it to the CLI's three strips, but only the CLI's display-side copy (`explain.go`'s `apiKeyFromEnv`, which decides whether the curl line shows the Authorization header). The library's `APIKeyFromEnv` — the one whose result is sent as the bearer token on every request — still uses `strings.TrimSpace`. Python's `_api_key_from_env` returns `value.strip()` (`client.py:889-895`), so a key environment variable padded with U+001C-U+001F is stripped by Python and sent verbatim by this port: a different `Authorization` header and potentially a rejected request. `AGENTS.md` now states the port's rule as "`TrimPythonSpace` for a strip", and this is the one strip that did not get it. PORT.md does not record the difference.
- Recommendation: Change `client.go:680` to `TrimPythonSpace`. The readout parity gate compares the `Authorization` header, and an ASCII key is unaffected, so the fix is safe and the divergence disappears rather than being documented.

### LOW: An explicitly empty `--state` still has no test, which was one third of a round-1 finding
- Severity: LOW
- Confidence: HIGH
- Location: cmd/shisa-de/state.go:32 (`stateFrom`), cmd/shisa-de/main_test.go (TestStateAndLabels, TestAskState)
- Problem: Round 1 asked for three untested paths: `doctor`'s not-ok exit (now `TestDoctorNotOKExitsOne`), `ask`'s server failure (now the second half of `TestFailedRead`), and `--state ''` producing an empty-text state rather than the built-in example. The third was not added and is not in NOTE.md's test list. The `stateSet` machinery in `parse` exists precisely to distinguish `--state ''` from an absent `--state`, and `stateFrom("", true)` is correct by inspection (the empty string is neither `{`- nor `[`-prefixed, so it is returned as the state), but nothing pins it: a regression that "simplified" `stateFrom` to test `text == ""` would silently restore the round-1 bug and no test would notice, because the only empty-text call is `stateFrom("", false)` (the built-in example).
- Recommendation: Add one case — either `stateFrom("", true)` returning `""` in `TestStateAndLabels`, or an end-to-end `ask --model ... --state ''` asserting the recorded prompt does not contain `Order 4812`.

### LOW: `ResolveCalibration("")` reads an empty spec as "auto" where Python's `resolve_calibration("")` raises
- Severity: LOW
- Confidence: MEDIUM
- Location: calibration.go:433-434
- Problem: Python's `resolve_calibration("")` falls through `calibration_for("")` (a `ValueError`) into `load_calibration_file("")` (a `FileNotFoundError`) and raises `ValueError: --calibration '' is neither a family (...) nor a record file`. The Go library's `ResolveCalibration` returns `ok=false` for `""`, treating it as "auto". The command line compensates — `modelConfig` refuses `--calibration ''` before `ResolveCalibration` is reached, with a message identical to Python's — so CLI parity holds. But the library is the port's other consumer surface, and a direct caller passing `""` gets a silent default where Python errors. The behavior is documented on the Go function's own doc comment but not in PORT.md, which is where the repo's rule puts deliberate differences from the reference.
- Recommendation: Either make `ResolveCalibration` refuse an empty spec (the CLI already refuses it first, so no command-line behavior changes and the two implementations agree everywhere), or add one sentence to PORT.md's calibration section recording that the library treats `""` and `"auto"` alike where Python raises on `""`.

### LOW: The failure-path stderr form and the usage-error wording are the port's own and unrecorded
- Severity: LOW
- Confidence: MEDIUM
- Location: cmd/shisa-de/main.go:59, 152-156, 174-189; cmd/shisa-de/explain.go:29-33; cmd/shisa-de/ask.go:28-31
- Problem: On every failure path the exit status matches Python (2 for usage, 1 for a read or report failure) and the stdout content now matches too, but the stderr text does not: Python dies with an uncaught-exception traceback (or `argparse`'s `invalid choice` / `error:` wording) where this port prints one `shisa-de <command>: <message>` line plus its own usage block. The uniform error line is clearly deliberate and is the right design for a Go command; PORT.md's command-line paragraphs record the flag grammar and output differences but not this one, and the repo's own rule is that a difference not in PORT.md is an accident. Round 1 declined to flag the hand-written `--help` text on the grounds that help text was out of scope; the error form is the same class, which is why this is LOW and a documentation-only recommendation.
- Recommendation: Add one sentence to PORT.md's flag-layer paragraph, e.g. "error and usage messages are this port's own wording; the exit statuses and streams are what match". If the lead classes it with help text as out of scope, close it as wontfix.

## Things Checked

- **The round-1 fix set, one by one**, against both round-1 reports and NOTE.md: `pythonQuote` at all six call sites (family_note, the two warnings, three reasons); the gate via `PythonStr` in output and help; the four set-aware choice flags including the `--calibration ''` message text (byte-identical to Python's `ValueError`); exact `--family` match against `FAMILIES = ("de1", "de2")`; the bare `help` word; the null logprob and its 9-wide `None` spelling; the `Object` slot report and its order; the deleted `orderedSlots` and `optionalString`; the corrected comments in `state.go` and `TestExplainReportsAnUnreadableQuestion`.
- **`doctor`**: `reportOrder` against `client.py:456-519`'s insertion order key by key, including `family_note` after `ok`, the mismatch/note exclusivity, `slots` before `boundary_check`, and `read_probe` last; the text block against the `cli.py` print sequence (labels, padding, `NO`, `yes`/`no`, `True`/`False`, the conditional `model listed` and `family_note` lines); exit 0/1 in both output forms; the absent `chat system` line as the recorded non-goal.
- **`ask`**: `Decision.Values` as `dict(result)` under `indent=2, ensure_ascii=False`; the `--prompt` handling matching Python's truthiness (`if args.prompt:`); `--state`'s three readings including the unstripped return of non-JSON text; exit 1 with stderr on a failed classify (pinned).
- **`explain`**: the print-then-read order against `_explain`; all five sections byte-for-byte against the f-strings (headings, 72-dash rule, `%6d` slot ids, `sampled!r` via `reprOptionalString`, the marks map, `%18s`+`PythonRepr`, `%9.4f` and the null, the fallback line, `%7.4f` distribution with `RenderOption`'s key-for-blank-description, the answer line, the curl body's seven-key order and `ensure_ascii=True`, `shlexQuote`, the footer's three conditional lines and the `<= 26` cap); `bestCode` against `max(probabilities, key=...)` given slot-ordered insertion; `sortedTopLogprobs` ordering, null-last and tie-by-token against Python's stable `-kv[1]` sort.
- **Flag layer**: `optionalBool`/`Float`/`Int`/`StringValue` shapes; `BoolFunc`'s bare and `=value` forms; `contains` generic over `~string`; `familyNames`/`calibrationFamilies`; `commonFlags` describing exactly the registered flags per command; `-h`/`--help`/`<command> --help` exit 0, everything else exit 2 with usage on stderr; `--calibration`'s "auto", any-spelling family, and path branches against `resolve_calibration`; `--family de1` refusing at the DE-2-only boundary (the recorded non-goal).
- **Library seams in scope**: `TrimPythonSpace` (the U+001C-U+001F difference against `unicode.IsSpace` and Python's Zs+WS/B/S set), `PythonStr`/`PythonRepr` (integral floats, `None`, containers), `Readout.MaxLogprobs`, `PythonJSONWith` options (`Indent` pointer for `None` vs `""`, `EnsureASCII` default matching `ensure_ascii=False`, item-separator switch, inline empty containers, surrogate pairs).
- **Tests**: the new cases are load-bearing — `TestFailedRead` proves the read follows sections 1-2 (the stub's `completionStatus` knob reaches only completions, so the listing still succeeds); `TestReportOrderCoversHealth`'s three invocations plus the hand-built DE-1 record cover the union of conditional keys and check both drop directions; `TestDoctorNotOKExitsOne` drives both output forms; `TestExplainReportsAnUnreadableQuestion` still proves the boundary failure prints nothing (the stub tokenizer is swapped before `invoke`, and `stubModel` re-reads `server.tokenizer` at construction, so the empty word list is the one the readout holds); the `newModel` seam with `t.Cleanup` and no `t.Parallel`.
- **Docs**: PORT.md's flag-grammar, null-logprob and tie-break paragraphs match the code; the "Verified parity" command-line paragraph accurately separates the measured hosted diffs from the offline suite's coverage; AGENTS.md's `cmd/shisa-de` entry lists the compatibility surface including `TrimPythonSpace`; NOTE.md matches what the working tree actually contains (with the `--state ''` test as the one omission).

## Residual Risks

- I had no shell tool in this session, so I did not run `gofmt`, `go vet` or the test suites; the scope's validation claims (offline suite, `-race`, the hosted-endpoint diffs) are taken as reported. All file-level claims above were verified by reading the working tree.
- The command-line tests run over an invented stub tokenizer, so they pin control flow and text, not the bytes a real tokenizer produces; `explain`'s actual bytes remain pinned only by the manual hosted diff, which PORT.md now correctly says is not re-run at test time.
- The live-endpoint parity is measured, not fixture-gated: a future change to `explain`'s layout would regress silently until the next manual diff.
- `newModel` is process-global and mutable; a `t.Parallel()` added to any test in `cmd/shisa-de` would race with `installCLIModel`.
- Error and usage message text is not asserted against the reference anywhere, which is consistent with treating it as this port's own wording — but it means the LOW finding above (recording that class in PORT.md) is the only check on it.
- `--probabilities` on `ask` inherits the documented map-order rule for the distribution; it is covered by PORT.md's general map-order paragraph but not called out for the distribution specifically.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "LOW",
      "title": "The library's APIKeyFromEnv still strips with Go's whitespace set, not Python's",
      "location": "client.go:680",
      "recommendation": "Use TrimPythonSpace in APIKeyFromEnv so the bearer token Python strips is stripped here too; the parity gate's header comparison is unaffected for ASCII keys."
    },
    {
      "severity": "LOW",
      "title": "An explicitly empty --state still has no test",
      "location": "cmd/shisa-de/state.go:32",
      "recommendation": "Pin stateFrom(\"\", true) returning the empty string (or an end-to-end ask --state '' whose prompt is not the built-in example), which was the untested third of a round-1 finding."
    },
    {
      "severity": "LOW",
      "title": "ResolveCalibration(\"\") reads an empty spec as auto where Python raises",
      "location": "calibration.go:433",
      "recommendation": "Make ResolveCalibration refuse an empty spec (the CLI already refuses it first, so nothing changes on the command line), or record the library-level difference in PORT.md."
    },
    {
      "severity": "LOW",
      "title": "The failure-path stderr form and usage-error wording are unrecorded",
      "location": "cmd/shisa-de/main.go:59",
      "recommendation": "Add one PORT.md sentence recording that error and usage messages are this port's own wording while exit statuses and streams are what match; otherwise close as out of scope like the help text."
    }
  ]
}
END_REVIEW_JSON
