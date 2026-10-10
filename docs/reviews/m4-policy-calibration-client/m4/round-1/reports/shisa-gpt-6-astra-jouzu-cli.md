# Review Report

## Summary

Reviewed the M4 implementation in `/home/morpheus/data/dev/shisa/shisa-de-go` against the Python reference, both readout contracts, and `docs/PORT.md`.

Found eight issues: an explicit family declaration is dropped during readout construction; malformed calibration values can silently disable tempering while reporting calibration; duplicate option keys corrupt returned distributions; some score labels differ from Python; debug output loses a null value; the parity test omits the TypeSafe output and legends; fixture provenance is hard-coded; and the documented fixture-generation command does not accept its advertised API-key variable.

The policy sequencing, raw-probability gate, thinking cap, final-read temperature selection, and single-fit exclusion after a thought otherwise match the reference on inspection.

## Findings

### MEDIUM: The constructor drops the explicit family declaration
- Severity: MEDIUM
- Confidence: HIGH
- Location: `/home/morpheus/data/dev/shisa/shisa-de-go/client.go:578`
- Problem: `NewDecisionModel` resolves `config.Family`, but does not pass it into `NewReadout`. The readout then resolves the family again from the model and tokenizer names. For example, a DE-2 checkpoint served under a neutral alias with the shared `shisa-ai/shisa-de-1` tokenizer is accepted by the outer resolver when `Family: FamilyDE2` is supplied, but rejected by `NewReadout`. Even when construction succeeds, the two layers can report different family sources. Existing declaration tests inject a readout, bypassing this path.
- Recommendation: Forward the explicit declaration into `ReadoutConfig.Family`. Add constructor tests without an injected readout, including a declared DE-2 family overriding a DE-1 tokenizer slug.

### MEDIUM: Null calibration temperatures become zero and are reported as applied
- Severity: MEDIUM
- Confidence: HIGH
- Location: `/home/morpheus/data/dev/shisa/shisa-de-go/calibration.go:309`
- Problem: Decoding temperature tables directly into `map[string]float64` converts JSON `null` values to zero. A supplied record containing `"temperatures":{"choice":null}` therefore loads successfully, reports `Fitted() == true`, and produces answers with `Calibrated == true` and `Temperature == 0`. However, `TemperDistribution` returns the raw distribution for zero temperature. The Python reference fails when converting `None` to a float; it does not silently accept this record.
- Recommendation: Reject null temperature entries in both `temperatures` and `reads` before converting them to floats. Add tests proving malformed records cannot produce raw probabilities marked as calibrated.

### MEDIUM: Duplicate option keys silently discard probability mass
- Severity: MEDIUM
- Confidence: HIGH
- Location: `/home/morpheus/data/dev/shisa/shisa-de-go/client.go:1161`
- Problem: `HeadSpec.Options` and `NewChoice` accept duplicate keys, but answer construction stores probabilities in a map keyed by those keys. Two options named `"a"` are assigned separate codes and scored separately, then the later probability overwrites the earlier one. The returned distribution no longer sums to one, and confidence is calculated from a different distribution than the caller receives. Python choice criteria are dictionaries, so duplicate keys cannot survive into this read.
- Recommendation: Reject duplicate choice keys during the pre-request validation pass. Cover both direct `Decide` questions and `Classify` label sets, asserting that rejection spends no requests.

### MEDIUM: Score labels use JSON formatting instead of Python string formatting
- Severity: MEDIUM
- Confidence: HIGH
- Location: `/home/morpheus/data/dev/shisa/shisa-de-go/client.go:1195`
- Problem: `pythonStr` falls back to `PythonJSON` for composite levels and uses the JSON float formatter for nonfinite numbers. These are not Python’s `str()` representations. A level equivalent to `["low", None]` becomes `["low", null]` instead of `['low', None]`; positive infinity becomes `"Infinity"` instead of `"inf"`. Both clients accept non-string score levels, so this changes `Answer.Level`, `Legend`, the `Classify` value, and the TypeSafe legend for the same question. `TestPythonStr` currently enshrines the incorrect infinity spelling.
- Recommendation: Implement Python-compatible string representations for supported level values, including nested containers and nonfinite floats. Derive regression expectations from Python rather than the JSON writer.

### MEDIUM: The client parity test does not check legends or TypeSafe output
- Severity: MEDIUM
- Confidence: HIGH
- Location: `/home/morpheus/data/dev/shisa/shisa-de-go/client_parity_test.go:438`
- Problem: The generator records `answer.to_dict()` plus `level`, but that representation excludes `legend`. `answerFixture` likewise has no legend, and the replay never compares `Decision.ToWire()` with Python’s `result.to_wire()`. Consequently, a missing or incorrect legend—or a regression confined to `ToWire()`—passes this parity test. This contradicts the field-for-field legend claim in `docs/PORT.md` and leaves a requested M4 compatibility surface unchecked.
- Recommendation: Record Python’s complete `to_wire()` result and compare it recursively with Go’s `ToWire()`. Also compare the serialized `ToDict()` maps rather than only selected decoded fields. Add a negative test demonstrating that an altered legend or omitted wire field fails.

### MEDIUM: Fixture regeneration writes fixed provenance instead of observed provenance
- Severity: MEDIUM
- Confidence: HIGH
- Location: `/home/morpheus/data/dev/shisa/shisa-de-go/scripts/gen_client_parity.py:331`
- Problem: Every regeneration writes the fixed date `2026-10-10`, the label `shisa-hosted-2026-10-10`, and a literal readout version. The recorded responses actually identify the server as `vllm-0.30.0-tp2-ca69195e`. The generator also does not pass a pinned tokenizer revision despite describing the tokenizer as pinned. A later run can therefore replace the measurements while preserving stale provenance, and the replay only checks that the date and fingerprint are nonempty.
- Recommendation: Record the actual recording date, collect the response `system_fingerprint` values, obtain the readout version from the reference implementation, and explicitly pin and record the tokenizer revision. Validate those fields against the fixture during replay.

### LOW: Debug output changes the post-thought prompt from null to an empty string
- Severity: LOW
- Confidence: HIGH
- Location: `/home/morpheus/data/dev/shisa/shisa-de-go/policy.go:341`
- Problem: Python’s token-ID read after a thought leaves `LetterRead.prompt` as `None`, and `PolicyRead.raw(debug=True)` emits `"prompt": null` for that component. Go stores the absent prompt in a string field and emits `"prompt": ""`. This is an undocumented `Decision.Raw` shape difference. The recorded debug case does not think, so it does not exercise the mismatch.
- Recommendation: Preserve the absent-prompt state when constructing debug raw output. Add a debug case that reaches the thinking read and compare its complete raw representation with Python.

### LOW: The documented recorder command rejects its advertised API-key variable
- Severity: LOW
- Confidence: HIGH
- Location: `/home/morpheus/data/dev/shisa/shisa-de-go/scripts/gen_client_parity.py:315`
- Problem: The script documents `SHISA_DE_API_KEY=... python scripts/gen_client_parity.py`, and its error message says that variable is accepted. The implementation checks and reads only `API_KEY`, so the documented command exits before recording.
- Recommendation: Resolve the key once using the documented environment variables and pass it to both readout construction paths. Either support `API_KEY` as an explicit additional fallback or remove it from the interface.

## Things Checked

- Read the scope first, then the Python and Go repository instructions, `docs/PORT.md`, and both readout contracts.
- Compared `policy.go` with `shisa_de/policy.py`: policy resolution, aliases, override conflicts, raw gate, option cap, thought closure, final-read selection, and accounting.
- Compared calibration arithmetic, applicability, bundled temperatures, and per-read selection with `shisa_de/calibration.py`.
- Compared client construction, answer conversion, multi-label views, thresholds, metadata, usage, and health reporting with `shisa_de/client.py`.
- Inspected the policy, calibration, client, and client-parity tests.
- Inspected fixture generation, replay matching, comparison helpers, metadata, and relevant recorded exchanges.
- Checked the `Readout.httpClient` nil-receiver guard and the updated unsupported-family diagnostic.
- Confirmed that the recorded `decide-noul-near-gate` case stops at `repeat2`; it does not think as `docs/PORT.md` claims.

## Residual Risks

- This was a read-only static review. No files were modified, and no test, build, or live-endpoint commands were run.
- The scope’s reported passing checks were not independently reproduced.
- The large client fixture was inspected selectively; its digest was not independently recomputed.
- The parity replay can skip when the pinned tokenizer is unavailable unless the required-tokenizer setting is enabled.
- DE-1 execution, images, the planned CLI, and the planned live suite were excluded as instructed.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "The constructor drops the explicit family declaration",
      "location": "/home/morpheus/data/dev/shisa/shisa-de-go/client.go:578",
      "recommendation": "Forward the explicit family declaration to NewReadout and test constructor paths without an injected readout."
    },
    {
      "severity": "MEDIUM",
      "title": "Null calibration temperatures become zero and are reported as applied",
      "location": "/home/morpheus/data/dev/shisa/shisa-de-go/calibration.go:309",
      "recommendation": "Reject null entries in all temperature tables before float conversion and add malformed-record regression tests."
    },
    {
      "severity": "MEDIUM",
      "title": "Duplicate option keys silently discard probability mass",
      "location": "/home/morpheus/data/dev/shisa/shisa-de-go/client.go:1161",
      "recommendation": "Reject duplicate choice keys before issuing requests and cover both Classify and Decide."
    },
    {
      "severity": "MEDIUM",
      "title": "Score labels use JSON formatting instead of Python string formatting",
      "location": "/home/morpheus/data/dev/shisa/shisa-de-go/client.go:1195",
      "recommendation": "Match Python str() for supported score levels, including nested values and nonfinite floats."
    },
    {
      "severity": "MEDIUM",
      "title": "The client parity test does not check legends or TypeSafe output",
      "location": "/home/morpheus/data/dev/shisa/shisa-de-go/client_parity_test.go:438",
      "recommendation": "Record and compare complete Python and Go wire outputs, including legends, and add negative comparator tests."
    },
    {
      "severity": "MEDIUM",
      "title": "Fixture regeneration writes fixed provenance instead of observed provenance",
      "location": "/home/morpheus/data/dev/shisa/shisa-de-go/scripts/gen_client_parity.py:331",
      "recommendation": "Record actual dates and response fingerprints, derive the readout version, and pin and record the tokenizer revision."
    },
    {
      "severity": "LOW",
      "title": "Debug output changes the post-thought prompt from null to an empty string",
      "location": "/home/morpheus/data/dev/shisa/shisa-de-go/policy.go:341",
      "recommendation": "Preserve null for an absent post-thought text prompt and add a debug-thinking parity case."
    },
    {
      "severity": "LOW",
      "title": "The documented recorder command rejects its advertised API-key variable",
      "location": "/home/morpheus/data/dev/shisa/shisa-de-go/scripts/gen_client_parity.py:315",
      "recommendation": "Resolve and use the documented API-key environment variables consistently."
    }
  ]
}
END_REVIEW_JSON
