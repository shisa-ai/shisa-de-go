# Review Report

## Summary

Round 2 of M5. I re-read the whole M5 surface (`cmd/shisa-de/`, the `pyjson.go` options, the Python-compatibility exports, and the docs) against `shisa_de/cli.py` at the pinned revision, with the round-1 fix set as the diff to verify.

The fix set closes the substance of what it claims. I confirmed by reading both sides that: `explain` now prints sections 1–2 before the read, which is `_explain`'s order, and `TestFailedRead` genuinely proves it (a 500 on the completion leaves both headings on stdout and no section 3); the think gate is printed with `PythonStr`, so an integral gate is `1.0`; a null logprob prints `None` and is carried as a null; `family_note`, the three `Applicability` reasons and both warnings are built with `pythonQuote`, which is Python's `repr` for the model and family ids in play; `Health`'s `slots` is an `Object` whose order is `count, A, B`, which is what `json.dumps(report, indent=2)` prints; the four choice flags are set-aware and an explicitly empty one is a usage error with the same wording Python uses; `--family` is an exact `de1`/`de2` match; the bare word `help` is an unknown command; `stateFrom`, `labelsFrom` and `apiKeyFromEnv` use `TrimPythonSpace`; `orderedSlots` and `stateFrom`'s always-nil error return are gone; `--compound` is in the hand-written help; and `reportOrder` still matches `client.py`'s insertion order key for key. I found no new divergence introduced by any of the fixes.

Two round-1 findings are only partly closed, and both are the same kind of gap the round-1 reports were about: the `%q`-for-`!r` sweep stopped at `client.go` and `calibration.go`, leaving the readout's tokenizer-mismatch and boundary messages (which reach `doctor`'s `boundary_check`/`read_probe` fields, in both output forms, and `explain`'s stderr) double-quoted; and of the three untested output paths round 1 listed, `--state ''` is still untested. I also found one dead assertion in the new coupling test, one unrecorded public-shape change (`Health`'s `slots` type), and one unrecorded deliberate difference (the help/usage text is this port's own layout, not `argparse`'s). All five are LOW.

## Findings

### LOW: the `%q`-for-`!r` sweep did not reach the readout messages that land in `doctor`'s report
- Severity: LOW
- Confidence: HIGH
- Location: `readout.go:522-524`, `readout.go:612-615`, `readout.go:625-628`, `readout.go:643-646`, `readout.go:795-798`; printed at `cmd/shisa-de/doctor.go:112` (`boundary check`) and `cmd/shisa-de/doctor.go:131-133` (`read probe`), and to `explain`'s stderr at `cmd/shisa-de/explain.go:33-35`
- Problem: round 1's MEDIUM was "Go's `%q` where Python writes `!r`", and the fix converted `client.go`'s `family_note`/warnings and `calibration.go`'s three reasons. The same substitution is still in the messages a failing read produces, and those messages are otherwise word-for-word Python's:
  - `readout.go:612-615` — `the prompt does not end on %q (token %d), so the answer is not at the first generated position. The tokenizer (%q) does not match the served model (%q).` versus `readout.py:490-494`'s `!r` on the terminator, the tokenizer source and the model;
  - `readout.go:625-628` — `the answer boundary moves for %q: appended to %q it does not tokenize as token %d. The tokenizer (%q) does not match the served model (%q).` versus `readout.py:497-501`;
  - `readout.go:643-646` — `the server counted %d prompt tokens where the client tokenized %d: the tokenizer (%q) does not match the served model (%q)` versus `readout.py:511-514`;
  - `readout.go:795-798` — `code %q was not returned for its token id and the prompt_logprobs fallback returned nothing for it` versus `readout.py:576-578`'s `code {slot.letter!r} ...`.
  Each reaches the command line: `boundary_check` is `"failed: " + err` in both `doctor` forms, `read_probe` is `"failed: " + err`, and `explain` prints `shisa-de explain: <err>`. `PORT.md:588-590` claims `doctor`'s text report and `doctor --json` are "identical apart from the numbers a fresh request changes and the one `chat system` line"; that holds for the run that was measured, where the boundary check passed, but not for the failing run the boundary check exists to produce — which is exactly the case round 1's MEDIUM was raised against. `readout.go:522-524` also differs beyond quoting (`answer code %q ... read with the codebook` where `readout.py:289-292` says `answer letter ... read with letter slots`), so that one is either a deliberate DE-2 rewording or another accident, and neither is recorded.
- Recommendation: build these five messages with `pythonQuote` (the same one-line change as round 1), or add one sentence to `PORT.md`'s flag/readout sections saying error text is not part of the byte-parity claim and that the DE-2 slot message is reworded from the shared DE-1 helper. Note the round-1 recommendation named only `family_note` and the reasons, so this is a scope gap rather than a regression.

### LOW: `Health`'s `slots` changed type, which is a public shape change `PORT.md` does not record
- Severity: LOW
- Confidence: MEDIUM
- Location: `client.go:818-823`; `docs/PORT.md:302-310` (the caller-visible-map paragraph)
- Problem: round 1's fix replaced `report["slots"] = map[string]any{...}` with a `shisade.Object` so `doctor --json` keeps the codebook's order by construction. That is the right fix for the CLI, but the value is part of `Health`'s public return: a caller who read it as `report["slots"].(map[string]any)` now fails the assertion, and `Object` has no `MarshalJSON`, so `json.Marshal(model.Health(...))` renders that one field as `[{"Key":"count","Value":256},{"Key":"A",...}]` instead of a JSON object. The CLI is unaffected (`PythonJSONWith` handles `Object` explicitly, and `TestDoctorJSON`/`TestHealthOffline` pin the shape), and no in-tree caller is broken, but `PORT.md`'s map-order paragraph still says the order is restored only in `doctor --json`, which is no longer the whole story.
- Recommendation: add the `slots` entry to `PORT.md`'s caller-visible-map paragraph (an `Object`, ordered by construction, and its `encoding/json` shape), or give `Object` a `MarshalJSON` that writes a JSON object so a caller who marshals the report gets an object.

### LOW: `TestReportOrderCoversHealth`'s `family_note` assertion is unreachable
- Severity: LOW
- Confidence: HIGH
- Location: `cmd/shisa-de/main_test.go:956-960`
- Problem: the guard is `if len(argv) == 2 && argv[0] == "--model" && argv[1] == "my-adapter"`, but the `my-adapter` case in the loop above is `{"--model", "my-adapter", "--probe"}` (`main_test.go:924`), a three-element slice, so the guard is never true and the "an assumed family reported no family_note" check never runs. The test's real value — that every key `Health` produces is one `orderedReport` prints — is unaffected and load-bearing (`len(ordered) != len(report)` plus the `known` sweep), so the test passes for the right reason on its main claim, but the conditional-key coverage the comment describes is not actually exercised for `family_note`.
- Recommendation: drop the guard and assert `report["family_note"]` for the `my-adapter` iteration by matching on the argv contents (or `len(argv) == 3`), so the intended assertion runs.

### LOW: `--state ''` versus an absent `--state` is still untested
- Severity: LOW
- Confidence: HIGH
- Location: `cmd/shisa-de/main_test.go:593-625` (`TestAskState`); `cmd/shisa-de/main.go:210-213`, `cmd/shisa-de/state.go:37-52`
- Problem: round 1's glm report listed three untested output paths: `doctor`'s not-ok exit, `ask`'s classify failure, and `--state ''` producing an empty state rather than the built-in example. The fix set added `TestDoctorNotOKExitsOne` and `TestFailedRead` (which covers `ask`'s failure), so two of the three are closed; `--state ''` is not. It is exactly the distinction `optionalStringValue` exists to make: `stateFrom("", true)` returns the empty string, which is Python's `_state_from("")`, while `stateFrom("", false)` returns the built-in example. `TestStateAndLabels` exercises only the absent form (`stateFrom("", false)`), and `TestAskState`'s four cases are `{"a": 1}`, plain text, malformed `{`, and no flag at all. I traced the code and the behaviour is correct on both sides; nothing pins it.
- Recommendation: add one case that runs `ask --state ''` against the stub and asserts the prompt carries the empty evidence (`""`) rather than `Order 4812`, or add a direct `stateFrom("", true)` assertion next to the existing one.

### LOW: the help and usage text is this port's own layout, and `PORT.md` does not say so
- Severity: LOW
- Confidence: HIGH
- Location: `cmd/shisa-de/main.go:310-345` (`topUsage`, `commandUsage`), `cmd/shisa-de/flags.go:154-186` (`commonFlags`); `docs/PORT.md:278-291`
- Problem: the port writes its usage and `--help` by hand (`main.go:311-313` says why), and the result is not `argparse`'s: no `usage: shisa-de doctor [-h] [--base-url BASE_URL] ...` line, no metavars or `positional arguments`/`options` sections, shortened help strings (`--family NAME  the contract the served model is read through` where Python prints the resolution order; `--probe  also send one question to check the served read` where Python says "one question and one chat request"; `--calibration SPEC  a family (de2) ...` where Python lists `de1, de2`), and a differently ordered error block (`shisa-de doctor: <msg>` then the usage, where `argparse` prints the usage then `shisa-de doctor: error: <msg>`). `PORT.md:289-291` records only that `-h`/`--help`/`<command> --help` print the usage and exit 0 and that bad invocations exit 2. The repo's own rule (`AGENTS.md`: a difference that is not in `PORT.md` is an accident) makes this an unrecorded deliberate difference; round 1 flagged it as out of scope, but this round's scope asks whether `PORT.md` describes every remaining one.
- Recommendation: add one sentence to `PORT.md`'s flag paragraph — the usage/help text is this port's own layout, so `--help` output and the error block are not byte-comparable to `argparse`'s, though the flag set, the exit statuses and the streams are.

## Things Checked

- **`explain`'s read order.** Read `_explain` (`cli.py:236-300`) against `runExplain`: Python renders, checks the boundary, prints 1 and 2 (including the blank line after the slot table), builds `body`, then reads, then prints 3; Go renders, checks the boundary, prints 1 and 2 and the blank line, then reads, then builds `body` and prints 3. The only ordering difference left is when `body` is built, which is unobservable. `TestFailedRead` fails the completion with a 500 and asserts sections 1–2 on stdout, no section 3, and the error on stderr, which pins the read's position; `TestExplainReportsAnUnreadableQuestion` pins the boundary path (empty stdout, zero requests), and its corrected comment is accurate.
- **`Health`'s `slots` and `doctor --json`.** Python builds `{"count": len(slots), **{slot.letter: slot.token_id for slot in slots[:2]}}`; Go appends `count` then the first two slots as an `Object`. `appendPythonJSON` has an explicit `Object` case, `appendPythonObject` refuses a repeated key and writes `": "`, so the nested form is `{"count": 256, "A": 1000, "B": 1001}` under `indent="  "`, matching `json.dumps(report, indent=2)` byte for byte, including the empty-container rule. `reportOrder` matches `client.py:456-519`'s insertion order key for key, with the conditional `family_note`, `calibration_match`, `calibration_mismatch`/`calibration_note`, `served_models`, `model_listed`, `slots` and `read_probe` in the same positions and `chat_system_render` the only omission (recorded).
- **`pythonQuote` against `!r`.** The quote choice (single, or double when the string holds `'` and no `"`), the `\n`/`\r`/`\t`/backslash escapes, and the unprintable set from `data/python-printable.json` are Python's `repr`; on the ids in play (`shisa-ai/shisa-de-2`, `my-adapter`, `de2`) it is identical to `!r`. `TestDoctorQuotesLikePython` and the exact-line assertion in `TestDoctorText` pin the note line and the reason line; the calibration reasons match `calibration.py:155-169` word for word, including `same family, another checkpoint`.
- **The set-aware choice flags.** `parse` records `set` for `--family`, `--policy`, `--reads`, `--calibration` and `--state`; `modelConfig` rejects an explicitly empty family/policy/reads and the empty calibration with Python's own wording (`--calibration '' is neither a family (de1, de2) nor a record file`, using `Families`, which is Python's `FAMILIES`), and `--calibration auto` still means "pick from the model". `TestChoiceFlagsRejectAnEmptyValue` covers all four plus `--family DE-2` and the two accepted spellings. `--family` is `contains(Families, ...)`, an exact match, so no spelling `argparse`'s `choices` accepts is now refused.
- **The remaining flag grammar.** `--fam` is not an abbreviation (Go's `flag` has none), `--reasoning=true`/`--no-reasoning=false` are accepted where `BooleanOptionalAction` has `nargs=0`, and a zero or negative `--timeout` becomes `NewReadout`'s default because `readout.go:384-387` substitutes `DefaultTimeout` for `<= 0` — all three recorded at `PORT.md:278-291`, and the recorded text matches the code. `help` is an unknown command (exit 2, usage on stderr); `-h`, `--help`, `<cmd> --help` exit 0 with the usage on stdout.
- **The gate, the null and the tie-break.** `shisade.PythonStr(model.ThinkGate())` routes through `pythonReprFloat`/`appendPythonFloat`, so `--reasoning-prob 1` prints `against the gate 1.0`; the `--reasoning-prob` help default uses the same spelling. A null logprob is carried as `topLogprob.null` and printed as `     None` in the same 9-column field `%9.4f` occupies, with the null sorted last; `PORT.md:293-300` records both that and the token-order tie-break. `bestCode` is `max(probabilities, key=...)` over slot order (I confirmed `readout.py:584` reorders the logprobs into slot order before `softmax`, so the first-max rule is Python's), skips a code the read did not return, and errors where Python's `max` would raise on an empty dict.
- **`TrimPythonSpace` and its call sites.** The export is a one-line wrapper over the unexported `trimPythonSpace`, documented against `strings.TrimSpace` on U+001C–U+001F. The command line uses it in `stateFrom`, `labelsFrom` and `apiKeyFromEnv`, which are exactly the three places `cli.py`/`client.py` call `.strip()` (`cli.py:33`, `cli.py:45`, `client.py:892-893`); the library's own `.strip()` counterparts (`questions.py:54`, `questions.py:131`) already went through the unexported helper. `TestStateFromTrimsPythonsWhitespace` pins the state and the label cases. `AGENTS.md:59-65` lists it in the compatibility surface with the three call-site rules.
- **`stateFrom` and `labelsFrom`.** All three readings match `_state_from` (absent → the built-in `Object` with `ticket` before `account`, which `TestStateAndLabels` serializes to check the prompt bytes; `{`/`[` → `DecodeJSON`, so `3` stays `3`; malformed `{` → the text itself), and `labelsFrom` drops blank parts and defaults to the four labels. The always-nil error return is gone and neither `ask.go` nor `explain.go` has a dead exit-2 branch.
- **`TestDoctorNotOKExitsOne` and `TestReportOrderCoversHealth`.** The first is load-bearing: an endpoint that does not list the served id gives `model listed no`, `ok False` and status 1 in both output forms, with stderr empty, and a regression returning 0 unconditionally would fail it. The second is load-bearing for its main claim (the `len(ordered) != len(report)` comparison plus the known-key sweep would catch a `Health` key missing from `reportOrder`), and it drives all three conditional-key shapes plus a hand-built DE-1 record for `calibration_mismatch`; only the `family_note` sub-assertion is dead (finding above).
- **`ask`.** `Decision.Values` under `indent="  "` with `EnsureASCII` left false is `json.dumps(..., indent=2, ensure_ascii=False)`; `--prompt` sets `HeadSpec.Instructions` and an absent one falls through to `defaultQuestion`, which is `head.replace("_", " ")`; the sorted-key difference for `--confidence` is the recorded one.
- **`doctor`'s text report.** Compared against `cli.py`'s print sequence: the labels and padding, the `NO`-versus-level rule, `yes`/`no`, the `True`/`False` spelling via `pythonBool`, the mismatch reasons then the note reasons, the conditional `model listed` line (a `[]string` assertion where Python tests `is not None`, and `Health` sets the key only on a successful listing), `read probe`, `note`, and the absent `chat system` line.
- **Docs.** `README.md:58-79` matches the code (including the `--model` default rationale and the non-zero exit); `AGENTS.md:59-65` and the `cmd/shisa-de/` key-file entry list `TrimPythonSpace` and the compatibility surface; `PORT.md`'s "Verified parity" paragraph now says the command-line diffs "pin the run that was measured and not every reachable line; the offline test closes the gap where it can", which is narrower than round 1's wording and matches what the tests do; the new `PORT.md:595-601` list of what the offline test covers matches the test file.
- **Nothing else moved.** `orderedSlots` and `optionalString` no longer exist; `doctor.go`'s report order is unchanged; `familyNames` (CLI) and `contains[T ~string]` are used only where intended; `commonFlags` now describes every registered flag, including `--compound`/`--no-compound`, and lists none that is absent.

## Residual Risks

- I had no shell tool, so I did not run `go test`, `go vet` or `gofmt`, and I did not run the Python command line; every claim above is from reading both implementations. The scope's validation list (gofmt clean, vet, tests, race, and the four live diffs) is taken as reported.
- The command-line parity evidence is still one manual diff against the hosted endpoint and is not re-run at test time, so it pins the passing paths of the models that were tried. The `%q` finding lives in exactly that gap, and so does any other error-text difference: nothing in the tree compares error messages between the two clients, and `PORT.md` does not say error text is exempt.
- `PORT.md:588-590`'s "identical apart from ..." wording is about the measured run; a reader can take it as a claim about the report's shape, which now also includes an `Object` for `slots` whose `encoding/json` form differs from Python's dict.
- `reportOrder` remains a second, hand-maintained copy of `Health`'s key set. The new test enforces the direction that matters (a report key missing from the list), but nothing checks the reverse (a stale name in the list), which is harmless today.
- `--family de1` and `--calibration de1` are accepted by the CLI's choices and then refused by the library (`exit 2`), where the Python command line would build a DE-1 model and read it. That follows from the recorded DE-1 absence, but the CLI-level consequence (a legal `--family` value that always fails) is not spelled out in `PORT.md`.
- `sortedTopLogprobs`' null ordering and token tie-break are now recorded in `PORT.md`, but they are still not exercised end to end (the stub never returns a null logprob or equal logprobs through `explain`); `TestSortedTopLogprobs` pins the helper only.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "LOW",
      "title": "readout error messages still use %q where the Python client uses !r, and they reach doctor's report",
      "location": "readout.go:522-524,612-615,625-628,643-646,795-798",
      "recommendation": "Build these with pythonQuote as round 1 did for family_note and the Applicability reasons, or record in PORT.md that error text (and the reworded DE-2 slot message) is outside the byte-parity claim."
    },
    {
      "severity": "LOW",
      "title": "Health's slots changed from map[string]any to shisade.Object, an unrecorded public shape change",
      "location": "client.go:818-823; docs/PORT.md:302-310",
      "recommendation": "Record the Object in PORT.md's caller-visible-map paragraph (and its encoding/json shape), or give Object a MarshalJSON so a caller who marshals the report still gets a JSON object."
    },
    {
      "severity": "LOW",
      "title": "TestReportOrderCoversHealth's family_note assertion never runs",
      "location": "cmd/shisa-de/main_test.go:956",
      "recommendation": "The guard compares a 3-element argv to 2; match on the argv contents (or len == 3) so the assumed-family assertion is exercised."
    },
    {
      "severity": "LOW",
      "title": "--state '' versus an absent --state is still untested",
      "location": "cmd/shisa-de/main_test.go:593-625",
      "recommendation": "Add the case (or a stateFrom(\"\", true) assertion) that pins the empty-state distinction optionalStringValue exists for."
    },
    {
      "severity": "LOW",
      "title": "The hand-written help/usage text differs from argparse's and is not recorded in PORT.md",
      "location": "cmd/shisa-de/main.go:310-345; cmd/shisa-de/flags.go:154-186; docs/PORT.md:278-291",
      "recommendation": "Add one sentence saying the usage and --help text is this port's own layout, so --help output and the error block are not byte-comparable to argparse's."
    }
  ]
}
END_REVIEW_JSON
