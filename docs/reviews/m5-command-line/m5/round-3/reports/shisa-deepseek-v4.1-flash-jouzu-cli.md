# Review Report

## Summary

Round 3 of M5. I re-read the full updated M5 surface — `cmd/shisa-de/` against `shisa_de/cli.py`, the readout's messages against `shisa_de/readout.py`, `pyjson.go`'s two options, the Python-compatibility exports, `docs/PORT.md`, and both round NOTES — with the round-2 fix set as the diff to verify.

The round-2 fix set closes the substance of four of its five claims, and I verified each against both sides rather than against the NOTE: the five Python-worded readout messages (`readout.go:525-527`, `617-621`, `631-635`, `650-653`, `803-805`) now use `pythonQuote`, and I compared every one word for word with `readout.py:289-292`, `490-494`, `499-503`, `510-514`, `576-579` — they match, so the `%q`-for-`!r` sweep is complete for messages that are Python's sentence. `APIKeyFromEnv` uses `TrimPythonSpace` and matches `_api_key_from_env` (`client.py:889-894`), including the blank-first-variable fall-through and the U+001C–U+001F strip that `TestAPIKeyFromEnv` pins. `TestReportOrderCoversHealth`'s guard is now `len(argv) == 3 && argv[0] == "--model" && argv[1] == "my-adapter"`, which is true for the `{"--model","my-adapter","--probe"}` iteration, so the `family_note` assertion runs. `TestAskState` pins `--state ''` as `"evidence": ""` and explicitly rejects the `Order 4812` fallback. `TestBoundaryMessagesQuoteLikePython` asserts two whole sentences that are, character for character, `readout.py:490-494` and `readout.py:499-503` with `!r` quoting.

I found no new divergence introduced by the fix set: nothing in it touches rendering, request bodies, code handling, scoring, the readout version (`de2-codebook-v1`, `readout.go:50`) or the calibration record, so no served answer moves and no version bump was owed. The changes are message text, one bearer-token trim, tests, and documentation.

The gaps that remain are all in the record rather than the code, which is where the round-2 findings were too. `PORT.md`'s DE-2 rewording paragraph now makes a claim that is false for two messages, one of which is reachable through `doctor`'s `boundary_check` and `explain`'s stderr on exactly the tokenizer-mismatch run the paragraph is about; the round-2 NOTE claims a `PORT.md` entry for the usage/`--help`/error text that is not in the file; three of the five messages the fix re-quoted have no whole-sentence assertion anywhere; and two smaller unrecorded differences (the warnings' wording, and the CLI-level consequences of the absent DE-1 record) round it out. All five are documentation or test-text gaps; none changes behavior.

## Findings

### MEDIUM: `PORT.md`'s DE-2 rewording paragraph claims everything else is Python's sentence, and two more messages are not
- Severity: MEDIUM
- Confidence: HIGH
- Location: `docs/PORT.md:328-338`; `readout.go:576-579`, `readout.go:116`
- Problem: the paragraph ends "Everything else the readout raises is Python's sentence with Python's `!r` quoting, including the boundary check's three messages, which reach `doctor`'s `boundary_check` field and `explain`'s stderr." Two messages it shares with the Python readout are reworded, not just re-quoted:
  - `readout.go:576-579` — `the generation-prompt terminator '<channel|>' is not one token for tokenizer 'x', so the answer boundary cannot be confirmed with this tokenizer`. Python (`readout.py:644-648`) says `..., so the image readout cannot confirm the answer boundary with this tokenizer`. The Go comment at `readout.go:574-575` says the change is deliberate ("this port has no image readout, so the sentence stops at the tokenizer"), but `PORT.md` does not record it. This is reachable: `answerPrefixIDFor` is called from `checkCodeBoundaryFor`, so a tokenizer under which `<channel|>` is not one token produces it through `doctor`'s `boundary_check` (`doctor.go:109`, both output forms) and `explain`'s stderr (`explain.go:25-27`) — the failing run the whole round-2 discussion was about, and the one path on which the two command lines now differ in wording as well as in the `chat system` line.
  - `readout.go:116` — `no code logprobs to normalize`. Python's shared `softmax` (`readout.py:898`) raises `no letter logprobs to normalize`. Library-only (a read errors before `Softmax` sees an empty map), but it is the same DE-2 rewording as the slot message the paragraph does name.
  `PORT.md`'s rule is that a difference not in it is an accident; a paragraph that asserts there are no others makes the omission worse than an omission, because a later maintainer reading it could "restore" Python's sentence (reintroducing "the image readout", which this port does not have) or trust error text to be byte-comparable on a mismatch run.
- Recommendation: extend the paragraph to name the terminator message and the `Softmax` message as the other two rewordings (letter/letter slots → code/codebook, image readout → the answer boundary, letter logprobs → code logprobs), or replace the final sentence with one that says which messages are Python's verbatim and which are the port's own.

### LOW: the round-2 record of the usage, `--help` and error text is not in `PORT.md`
- Severity: LOW
- Confidence: HIGH
- Location: `docs/PORT.md:278-291` (the flag-layer paragraph), `docs/PORT.md:289-291`; `cmd/shisa-de/main.go:311-345`, `cmd/shisa-de/flags.go:154-186`
- Problem: round 2's consolidation (`docs/reviews/m5-command-line/m5/round-2/NOTE.md:35`) states that `PORT.md` now records "the usage, `--help` and error text is this port's own layout and wording, with only the flag set, exit statuses and streams matching `argparse`". That sentence is not in the file: the flag-layer paragraph covers three grammar differences, the `-h`/`--help` exit statuses and the streams, and nothing about the text itself. The difference is real and visible: `topUsage`/`commandUsage`/`commonFlags` are hand-written, so there is no `usage: shisa-de doctor [-h] [--base-url BASE_URL] ...` line, no metavars or `positional arguments`/`options` sections, shortened help strings, and an error block ordered `shisa-de doctor: <msg>` then the usage where `argparse` prints the usage then `shisa-de doctor: error: <msg>`. So one of the four items round 2 claims to have recorded was not recorded, and this is the round in which the record is supposed to be complete.
- Recommendation: add the sentence to the flag-layer paragraph, or to the "command line's own wording" place the round-2 NOTE assumed existed.

### LOW: three of the five re-quoted readout messages have no whole-sentence assertion
- Severity: LOW
- Confidence: HIGH
- Location: `readout_test.go:1255-1296` (`TestBoundaryMessagesQuoteLikePython`), `readout_test.go:1190-1213` (`TestReadoutSlot`); `scripts/gen_readout_parity.py:391,394`
- Problem: the fix converted five messages to `pythonQuote`; the new test pins two sentences. The other three are asserted only weakly:
  - the code-slot message (`readout.go:525-527`, Python `readout.py:289-292`) is reached by `TestReadoutSlot` via `readout.Slot("not-a-code")`, which asserts `err != nil` and nothing about the text;
  - the prompt-tokens message (`readout.go:650-653`) is matched by the parity fixture's `error_match=r"counted \d+ prompt tokens where the client tokenized \d+"`, which stops before the tokenizer and model ids, so a revert to `%q` on either would still pass;
  - the fallback-nothing message (`readout.go:803-805`) is matched by `error_match=r"prompt_logprobs fallback returned nothing for it"`, which likewise excludes the quoted code.
  A regression back to `%q` in any of the three would leave the suite green, which is the same class of gap round 1 and round 2 were raised about. Also worth noting: the test's comment calls its first case "The terminator message", but that case is `the prompt does not end on ...`; the terminator message proper (`the generation-prompt terminator ... is not one token`) is the untested one from the finding above.
- Recommendation: extend `TestBoundaryMessagesQuoteLikePython` with the code-slot, prompt-tokens and fallback-nothing sentences, or add the quoted ids to the two fixture `error_match` patterns.

### LOW: the two library warnings reword Python's advice to name the Go API, unrecorded
- Severity: LOW
- Confidence: HIGH
- Location: `client.go:597-600`, `client.go:625-628`; `docs/PORT.md:351-354`
- Problem: the warnings are built with `pythonQuote`, so the ids match Python's `!r`, but their tails do not: Go says `pass Family to say which contract it was trained for` where Python (`client.py:332-336`) says `pass family='de1' or family='de2' ...`, and `pass Calibration to apply a record you vouch for` where Python (`client.py:354-359`) says `pass calibration= to apply a record you vouch for`. The rewording is reasonable — Go has no keyword arguments — but `PORT.md`'s warnings paragraph says only that the warnings are "collected as strings", so this difference is not in the record either. It is not printed by the command line, so it does not affect M5's parity claims.
- Recommendation: add a clause to the warnings paragraph noting that the two messages name the Go fields (`Family`, `Calibration`) where Python names its keyword arguments, so the text is not identical.

### LOW: the CLI-level consequences of the absent DE-1 record are not recorded
- Severity: LOW
- Confidence: HIGH
- Location: `cmd/shisa-de/main.go:150-163`; `calibration.go:438-446`, `calibration.go:417-426`; `docs/PORT.md:39-47`
- Problem: `--family` accepts exactly `de1` and `de2` (`main.go:153-157`) and `--calibration` accepts `de1` through `NormalizeFamily` (`calibration.go:438-441`), matching `argparse`'s choices, but both values always fail here: `--family de1` reaches `NewDecisionModel`'s DE-2-only refusal and exits 2 with the usage, and `--calibration de1` reaches `CalibrationFor(FamilyDE1)` and exits 2 with `no calibration record ships for family "de1"; this package ships de2`. The Python command line reads DE-1 and loads the DE-1 record for the same invocations. `PORT.md` records the library refusal ("DE-2 only") and the `--model` default, but not that two of the flag values the parser advertises can only fail. Round 2 listed this as a residual risk; it is now the last round, and the fix is one clause.
- Recommendation: say in the DE-2-only paragraph (or the flag paragraph) that `--family de1` and `--calibration de1` are accepted spellings that always exit 2 here because this port ships no DE-1 read and no DE-1 record.

## Things Checked

- **The `%q` → `pythonQuote` sweep.** Compared every message in `readout.go` that has a Python counterpart: slot (`525-527` vs `readout.py:289-292`), terminator (`576-579` vs `644-648`), does-not-end (`617-621` vs `490-494`), boundary-moves (`631-635` vs `499-503`), prompt-tokens (`650-653` vs `510-514`), fallback-nothing (`803-805` vs `576-579`). All six now quote with `pythonQuote`, and the four that are Python's sentence are word for word. The three that keep `%q` are the ones with no Python counterpart — `loading the tokenizer %q` (`readout.go:453`), and the two `prompt_logprobs` strictness checks (`856-858`, `865-867`) — which is the right set. I also swept the other non-test files for `%q` that could reach the command line: `endpoint.go` (no quoting), `client.go`'s `formatLabel`/head errors, `calibration.go`'s loader errors, `policy.go`, `codebook.go`, `cmd/shisa-de/main.go`, `cmd/shisa-de/flags.go` — all Go's own text with no Python sentence behind them.
- **The boundary sentences the test pins.** `TestBoundaryMessagesQuoteLikePython`'s two `want` strings equal `readout.py:490-494` and `readout.py:499-503` exactly, with `!r` producing `'<channel|>'`, `'de2-test'`; the fixture tokenizer makes `Encode("<channel|>AA") == [101, 1114113]`, so the foreign slot (id + 9999) genuinely trips the per-code check and not the terminator check.
- **`APIKeyFromEnv` and its call sites.** `client.go:585-596` reads `APIKeyEnvs` in order, strips with `TrimPythonSpace`, and falls through on a blank value, which is `_api_key_from_env` (`client.py:889-894`); `explain.go`'s `apiKeyFromEnv` uses the same rule for the curl line, so the two consumers agree. `NewReadout` sends the key only when non-empty (`readout.go:400-403`). No other `.strip()` counterpart was left on `strings.TrimSpace`: `stateFrom`/`labelsFrom` (`state.go:41,74`) and the question layer's `trimPythonSpace` are all that remain, and all are the Python rule.
- **`TestReportOrderCoversHealth`.** The `family_note` guard now matches the three-element `{"--model","my-adapter","--probe"}` iteration, so the assertion runs; the load-bearing part (`len(ordered) != len(report)` plus the known-key sweep) still holds, and the DE-1 record built by hand still exercises `calibration_mismatch`, which no `--calibration` value can reach in this package.
- **`TestAskState`.** The five cases (`{"a": 1}`, plain text, malformed `{`, absent, explicit `""`) trace through `stateFrom` (`state.go:37-52`): the explicit empty one reaches the prompt as `"evidence": ""`, which is `_state_from("")` in Python (`cli.py:30-38`), not the built-in example. The JSON path goes through `DecodeJSON`, so `3` stays `3` in the prompt bytes.
- **The readout's Python-compatibility exports.** `PythonJSONWith`'s `Indent`/`EnsureASCII` are the two options the CLI uses (`doctor --json` sets both, `ask`/`explain` set `Indent`), and `PythonStr`/`PythonRepr`/`pythonQuote`/`TrimPythonSpace`/`MaxLogprobs` are used exactly where the Python command line uses `str`, `!r`, `str.strip()` and `readout.max_logprobs`. Nothing in the exports changed this round.
- **The command line's flag grammar.** `--fam` is not accepted (Go's `flag` has no abbreviation), `--reasoning=true`/`--no-reasoning=false` and `--json=true` are accepted where `argparse`'s `store_true`/`BooleanOptionalAction` refuse the `=value` form (the paragraph states this for boolean flags generally and gives `--reasoning` as its example), an explicitly empty `--family`/`--policy`/`--reads`/`--calibration` is a usage error, `--family DE-2` is refused, `--calibration de2` and `--calibration auto` work, a leftover positional is refused, `-h`/`--help`/`<cmd> --help` exit 0 on stdout, everything else exits 2 with the usage on stderr. `--timeout 0`/negative becomes `NewReadout`'s `DefaultTimeout` (`readout.go:384-387`).
- **The `explain` and `doctor` outputs against `cli.py`.** Section order (1–2 before the read, then 3–5), the `%6d` slot field, `reprOptionalString` for `sampled`, the `%18s` repr field with `%9.4f` logprobs and the `<- answer slot` marker, the `curl` line built from `shlexQuote` and an `ensure_ascii=True` body with the two code fields appended, the policy/gate/separator trailing lines, `doctor`'s label padding, its `NO`-versus-level rule, `True`/`False` spelling, the mismatch-then-note reason order and the absent `chat system` line. The Python CLI's `_explain` DE-1 branch and `ask --image*` remain the recorded omissions.
- **Whether M5 can move a served answer.** The round-2 set changes no render, no request body, no code handling and no scoring; the only request-path change is the bearer token's strip, which moves toward Python. `ReadoutVersionFor` still returns `de2-codebook-v1` with an empty `calibrationCompatible` entry (`readout.go:74-77`), and `data/calibration-de2.json` is untouched, so no version bump and no re-fit were owed.
- **M5 completeness.** Every Python subcommand exists with the same flag set minus the recorded image/DE-1 omissions, the same exit statuses and the same streams; `doctor`'s probe spends one read where Python also spends a chat request (recorded); `ask` prints `Decision.Values` as `json.dumps(indent=2, ensure_ascii=False)`; `README.md` and `AGENTS.md` describe the command line and the compatibility surface as the code has them.

## Residual Risks

- I had no shell tool, so I did not run `go build`, `go vet`, `gofmt`, `go test` or the Python command line; every claim above is from reading both implementations. The scope's validation list is taken as reported.
- The command-line parity evidence is still one manual diff against the hosted endpoint and is not re-run at test time. `PORT.md:615-619` says so, but the measured run passed the boundary check, so the failing-run text — where the MEDIUM finding lives — is outside it.
- `PORT.md`'s "Verified parity" command-line bullet lists what the offline test covers; it does not mention that three of the re-quoted sentences are unpinned, and `reportOrder` remains a second hand-maintained copy of `Health`'s key set (the direction that matters is tested; a stale name in the list is not).
- `Object` still has no `MarshalJSON`, so `json.Marshal` on `Health`'s report writes `slots` as an array of members. Round 2 recorded this deliberately; a caller who marshals the report rather than using `PythonJSON` still gets the Go shape.
- `sortedTopLogprobs`' null-last ordering and token tie-break are recorded and unit-tested but not exercised end to end, because the CLI stub never returns a null logprob or a tie.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "PORT.md claims every other readout message is Python's sentence, but the terminator and Softmax messages are reworded",
      "location": "docs/PORT.md:328-338; readout.go:576-579, readout.go:116",
      "recommendation": "Name the generation-prompt-terminator rewording (image readout -> the answer boundary, reachable via doctor's boundary_check and explain's stderr) and the Softmax rewording (letter -> code logprobs) in the DE-2 rewording paragraph, or narrow its final sentence."
    },
    {
      "severity": "LOW",
      "title": "The usage/--help/error-text difference the round-2 NOTE says PORT.md records is not in PORT.md",
      "location": "docs/PORT.md:278-291; cmd/shisa-de/main.go:311-345",
      "recommendation": "Add the sentence to the flag-layer paragraph: the usage, --help and error text is this port's own layout and wording; only the flag set, exit statuses and streams match argparse."
    },
    {
      "severity": "LOW",
      "title": "Three of the five re-quoted readout messages have no whole-sentence assertion",
      "location": "readout_test.go:1255-1296, readout_test.go:1190-1213; scripts/gen_readout_parity.py:391,394",
      "recommendation": "Extend TestBoundaryMessagesQuoteLikePython with the code-slot, prompt-tokens and fallback-nothing sentences, or include the quoted ids in the two fixture error_match patterns."
    },
    {
      "severity": "LOW",
      "title": "The two library warnings reword Python's advice to name the Go API and PORT.md does not record it",
      "location": "client.go:597-600, client.go:625-628; docs/PORT.md:351-354",
      "recommendation": "Note in the warnings paragraph that the messages name the Go fields Family and Calibration where Python names its keyword arguments, so their text is not identical."
    },
    {
      "severity": "LOW",
      "title": "--family de1 and --calibration de1 are advertised spellings that always exit 2, unrecorded",
      "location": "cmd/shisa-de/main.go:150-163; calibration.go:417-426, calibration.go:438-446; docs/PORT.md:39-47",
      "recommendation": "Record that both flag values are accepted by the parser but always fail here, because this port has no DE-1 read and ships no DE-1 calibration record, where the Python command line succeeds."
    }
  ]
}
END_REVIEW_JSON
