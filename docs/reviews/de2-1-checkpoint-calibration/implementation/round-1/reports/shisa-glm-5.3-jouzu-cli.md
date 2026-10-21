I have completed the review. I verified the Go port's working tree at `shisa-de-go` against the Python reference implementation in `shisa-de`, comparing the name-matching rule, record selection, applicability, the embedded record, the client call site, the CLI, the live test, and all four documentation files.

# Review Report

## Summary

The per-checkpoint record selection is correctly implemented and at parity with Python's `calibration_for` for every input class I could construct: `ReleasedCheckpoint` reimplements Python's lookaround rule faithfully (byte-boundary test is equivalent to the ASCII class, longest-slug-first ordering matches), `CalibrationForCheckpoint` keeps Python's model-then-tokenizer order and family guard, `Applicability` normalises served names through `ReleasedCheckpoint`, the client call site and the applied/supplied logic match `client.py`, and the embedded `data/calibration-de2.1.json` is content-identical to the Python client's record (I compared both files; I could not run a byte-level `diff` because no shell tool is available in this session). Tests cover the name table, per-checkpoint selection, applicability normalisation, six client-level served ids, and that the two records temper differently.

One high-severity problem: the README documents `Tokenizer` as the remedy for the planned alias deployment, but `CalibrationForCheckpoint` returns at the model id and never consults the tokenizer whenever the alias is itself a released checkpoint id — which is exactly the deployment the README paragraph and the scope describe. The documented escape hatch silently does nothing in that case. The Python client shares this flaw, so this is parity, but the docs promise behavior the code does not have, and the failure mode is the one this change exists to prevent (DE-2's fit applied to DE-2.1, reported as calibrated).

## Findings

### HIGH: The documented `Tokenizer` alias remedy does not change the record when the alias id is a released checkpoint
- Severity: HIGH
- Confidence: HIGH
- Location: README.md:310-326 (Go repo, `shisa-de-go`), README.md:413-415, calibration.go `CalibrationForCheckpoint`
- Problem: The README's alias paragraph says: if a server serves DE-2.1 behind `shisa-ai/shisa-de-2`, "say which checkpoint is really behind the alias" by passing `Model: "shisa-ai/shisa-de-2"` and `Tokenizer: "shisa-ai/shisa-de-2.1"`. But `CalibrationForCheckpoint` iterates `[]string{model, tokenizer}` and returns as soon as the model id resolves to a released checkpoint of the right family. `shisa-ai/shisa-de-2` is in `CheckpointCalibrationFiles`, so DE-2's record is returned and the tokenizer is never consulted. `Applicability` then reports `checkpoint` (the model name matches the record's fitted model) and the record is applied. The caller believes they opted into DE-2.1's fit; they get DE-2's fit, reported as calibrated. This is exactly the planned deployment (the scope says `shisa-ai/shisa-de-2` will be retained as an alias for DE-2.1's weights), and the scope's own context paragraph assumes "unless the caller passes Tokenizer or Calibration" — the Tokenizer half of that is false here. The `Tokenizer` route works only when the alias is *not* itself a released checkpoint id (e.g. `alias`, a local name), which the offline test pins; the released-id case is untested and undocumented. Python's `calibration_for` has the same model-first precedence, so this is a shared upstream flaw, but the Go README turns it into a false instruction. The `Calibration: &record` route does work (a supplied record is applied as given, verified in `client.go` and pinned by tests).
- Recommendation: Either fix the docs or fix the behavior, and they must agree. Minimum: change the README (both the alias paragraph and the local-server bullet) and the `live_test.go` guidance to say that `Tokenizer` identifies the checkpoint only when the alias id names no released checkpoint, and that a released-id alias requires `Calibration`. Better: when the model and the tokenizer name *different* released checkpoints, that disagreement is detectable — emit a warning (or refuse) instead of silently preferring the model, since it is either the alias case or a mistake; coordinate with the Python client so the two stay at parity. Add a test for `Model: "shisa-ai/shisa-de-2", Tokenizer: "shisa-ai/shisa-de-2.1"` pinning whichever behavior is chosen; the README snippet is not run by `example_test.go`, so nothing currently catches this.

### MEDIUM: The new live test's guidance recommends a configuration in which the test fails or passes vacuously
- Severity: MEDIUM
- Confidence: HIGH
- Location: live_test.go:1286-1328 (Go repo)
- Problem: The test's doc comment says to run it "at the alias with `SHISA_DE_TOKENIZER` set to the checkpoint behind it". Two problems. (1) If the alias is not a released id, `liveModel` builds the model with the tokenizer, the client correctly selects and applies DE-2.1's record through it, but the test's `checkpoint == ""` branch asserts `!model.CalibrationApplied` and a warning — the test fails in exactly the configuration its comment recommends. (2) If the alias is a released id (the actual planned deployment), the tokenizer is ignored (finding 1), DE-2's record is applied, and the id-derived expectation matches — the test passes while the wrong record is applied, which is the failure this change exists to prevent. The test does assert something that can fail against a regression in selection (serving `shisa-ai/shisa-de-2.1` while the code picks the family record fails it), so it is not vacuous in general; it is the alias guidance that is wrong.
- Recommendation: Correct the guidance to "point it at the checkpoint's own id" only, or derive the expectation from both the model id and the tokenizer the way `CalibrationForCheckpoint` does, so the recommended configuration is one the test accepts and still checks.

### LOW: The ported DE-2.1 section breaks the readout page's internal spelling and its "verbatim" claim
- Severity: LOW
- Confidence: HIGH
- Location: docs/READOUT-DE2.md:394 (Go repo) vs lines 58, 304, 345; README.md:17, AGENTS.md:22
- Problem: The new DE-2.1 calibration table writes `Answer.Strategy` while the same page's other three tables and the Python source write `Answer.strategy` (the page elsewhere keeps Python spellings such as `calibrated=False`). The section also rewrites the record path (`data/calibration-de2.1.json` for Python's `shisa_de/data/calibration-de2.1.json`), and the "Other checkpoints" section uses the Go API spelling `Calibration: &record` where Python writes `DecisionModel(calibration=...)`. Both README and AGENTS claim the page is "copied verbatim from the Python repository", and AGENTS makes the page the spec that the code is judged against, so mixing Go-isms into it invites drift between the two clients' contracts.
- Recommendation: Restore `Answer.strategy` in that table so the page stays Python-spelled as its other sections are. For the path and the caller-facing spellings, either keep the Python forms and record the Go-repo mapping in `docs/PORT.md`, or drop the "verbatim" wording in README/AGENTS to "adapted" — pick one and make the page internally consistent.

### LOW: `ReleasedCheckpoint` can diverge from Python on Unicode lowercasing and tie order
- Severity: LOW
- Confidence: MEDIUM
- Location: calibration.go `ReleasedCheckpoint`, `continuesCheckpointName` (Go repo)
- Problem: The byte-boundary test is equivalent to Python's ASCII class, as the comment claims, but the lowercasing step is not: `strings.ToLower` maps U+0130 (`İ`) to `i`, Python's `str.lower()` to `i` + U+0307, so a pathological name like `İshisa-de-2` is a non-boundary in Go (preceded by `i`) and a boundary in Python (preceded by the combining dot) — the two clients would pick different records for such a name. Also, Go breaks length ties lexicographically where Python's stable sort keeps insertion order; unreachable with the current table (no two same-length slugs can both match one name). No realistic model id hits either divergence.
- Recommendation: Extend the comment to say the parity claim covers the boundary class, not the case mapping, and leave it at that; or add the divergence to `docs/PORT.md` next to the other recorded Unicode differences.

## Things Checked

- `ReleasedCheckpoint` against Python's `released_checkpoint`: exact-id path, longest-slug-first order, boundary rule for `shisa-de-2.10/2.2/21`, `my-shisa-de-2.1x`, training arms, multi-byte runes (byte ≥ 0x80 is a boundary in both), a slug appearing twice (first match wins in both), and short names. Equivalent for every input except the U+0130 lowercasing noted above.
- `CalibrationForCheckpoint` against Python's `calibration_for`: same model-then-tokenizer order, same family guard (a DE-1 request cannot borrow a DE-2 record; pinned by test), same family-record fallback.
- `Calibration.Applicability` against Python's: level order (mismatch before unfitted before checkpoint/family), name normalisation through `ReleasedCheckpoint`, reasons text, `Matches`.
- The client call site in `client.go` against `client.py`: supplied-vs-chosen record, `CalibrationApplied` condition, warning text and its Go field names.
- `data/calibration-de2.1.json`: content-identical to the Python client's record (compared field by field; no shell was available for a byte-level diff). `Fitted`, `TemperatureFor`, `ID` semantics match Python's, including the per-read table being the whole of a record's applicability.
- `ResolveCalibration` and the CLI: family, checkpoint-id, path and `auto` spellings; the `--calibration` help and empty-spec error name the checkpoints; `--calibration de1` fails through `CalibrationFor(FamilyDE1)` as `docs/PORT.md` records; doctor prints the record id and the checkpoint it was fitted on (README claim true).
- Tests: the name table mirrors Python's minus the DE-1 row (documented), plus `""` and case-insensitivity rows; per-checkpoint selection, applicability, client-level selection for six served ids, distinct-tempering test, and the gated live test.
- Docs: README calibration section, temperature table (matches the records), alias paragraph, "What this port does not have" (DE-1, overflow, fixed read, quote delimiters — all accurate against the Python API), `docs/PORT.md`'s four recorded differences, `AGENTS.md`'s new source-of-truth lines.

On the scope's judgment question — is documenting the undetectable alias plus the escape hatch sufficient, or is a stronger mechanism needed: documenting is the right call for the truly undetectable part, because a warning keyed to a known-aliased id would fire for every self-hosted server that legitimately serves DE-2 under that id, and no code can distinguish the two. But the *detectable* part — a caller who explicitly passes a `Tokenizer` naming a different released checkpoint than the model id — is currently resolved silently in favor of the model id, against the documented remedy. That is where a mechanism belongs (finding 1), not in guessing at server-side aliases.

## Residual Risks

- Once the hosted id flip happens, every default user of both clients — including `shisa-de doctor` reporting `applied yes` at `checkpoint` level — gets DE-2's fit on DE-2.1's answers with no signal anywhere. The mitigation is operational (release note, model card, README update at flip time), not code; the README's alias paragraph is the right home for it, once its remedy is true.
- `TestLiveCalibrationMatchesTheServedCheckpoint` has never run (no DE-2.1 endpoint yet); its assertions are sound against the offline tests but unverified against a real server.
- The DE-2.1 record was fitted on the release candidate's merged weights "before upload; the published checkpoint is expected to be these weights" — the record's own note carries that expectation, and it is unchecked as of this change.
- I reviewed the working tree only; no shell tool was configured in this session, so I could not run `git diff ecc90ff` or the test suite, and the validation recorded in the scope is the implementer's report.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "HIGH",
      "title": "Documented Tokenizer alias remedy does not change the record when the alias id is a released checkpoint",
      "location": "shisa-de-go/README.md:310",
      "recommendation": "Make docs and code agree: either document that a released-id alias needs Calibration (Tokenizer works only for non-released alias ids), or warn/refuse when model and tokenizer name different released checkpoints; coordinate with the Python client, which shares the flaw. Add a test pinning Model=shisa-ai/shisa-de-2 with Tokenizer=shisa-ai/shisa-de-2.1."
    },
    {
      "severity": "MEDIUM",
      "title": "Live test's alias guidance recommends a configuration where the test fails or passes vacuously",
      "location": "shisa-de-go/live_test.go:1286",
      "recommendation": "Correct the guidance to the checkpoint's own id only, or derive the expectation from both the model id and the tokenizer as CalibrationForCheckpoint does."
    },
    {
      "severity": "LOW",
      "title": "Ported DE-2.1 section uses Go spellings in a page claimed verbatim from Python",
      "location": "shisa-de-go/docs/READOUT-DE2.md:394",
      "recommendation": "Restore Answer.strategy in the new table; reconcile the record-path and caller spellings with the 'copied verbatim' claim in README.md and AGENTS.md, recording adaptations in docs/PORT.md."
    },
    {
      "severity": "LOW",
      "title": "ReleasedCheckpoint can diverge from Python on Unicode lowercasing and tie order",
      "location": "shisa-de-go/calibration.go:ReleasedCheckpoint",
      "recommendation": "Note in the comment (or docs/PORT.md) that the parity claim covers the boundary class, not the case mapping; no code change needed for the current table."
    }
  ]
}
END_REVIEW_JSON
