# Review Report

## Summary

Reviewed the working tree of the Go port (`../shisa-de-go`, target of commits ecc90ff..12faffa) against the round-2 scope. The round-1 HIGH is genuinely fixed: `CalibrationForCheckpoint` selects by model id, the tokenizer only identifies a checkpoint the id does not name, the disagreement is reported, the README/AGENTS.md describe the alias case as needing an explicit `Calibration`, and tests pin both directions. `ResolveCalibration` now loads a path before matching a served name, and a misspelt `.json` path is an error. The readout page matches the Python reference on every spot check, and the DE-2.1 record is byte-identical to the Python repo's.

I found two places where the case-folding story is still not the safe subset it claims to be: the ASCII-only fold fixes U+0130 but introduces a new divergence at the boundary for U+212A (Kelvin sign), and `Applicability`'s non-checkpoint fallback still uses `strings.ToLower`, which invents matches Python does not make — both in the dangerous direction (a fit applied where Python leaves answers raw). I also found the new supplied-record warning firing outside the case it was written for, including with a false statement and on the README's own documented alias remedy, and a live-test expectation that uses the strategy name as the read name.

## Findings

### MEDIUM: Applicability's fallback fold invents matches Python does not make (U+0130)
- Severity: MEDIUM
- Confidence: MEDIUM
- Location: shisa-de-go/calibration.go:378-386 (`Applicability`)
- Problem: For a name that `ReleasedCheckpoint` does not recognise, the port falls back to `strings.ToLower(name)` on both sides (served names at line 378, `recordModel` at 382-386), matching Python's `released_checkpoint(name) or str(name).lower()` (shisa-de/calibration.py:196-197). But Go's `strings.ToLower` is the simple mapping — U+0130 (İ) becomes `i` — while Python's `str.lower()` is the full mapping — U+0130 becomes `i` + U+0307. For a served id such as `shisa-ai/sh\u0130sa-de-2`, `ReleasedCheckpoint` returns "" on both sides, but the Go fallback produces `shisa-ai/shisa-de-2`, which equals the DE-2 record's model, so the level is `checkpoint` and the fit is applied. Python gets `shisa-ai/shi\u0307sa-de-2`, reports `family`, and leaves the answers raw with a warning. This is the exact hazard the round-1 fix removed from `ReleasedCheckpoint`, surviving on the `Applicability` path, in the dangerous direction: a bundled fit applied to a name the reference client treats as uncovered. The `recordModel` side has the mirror-image divergence for a record whose model contains U+0130.
- Recommendation: Treat the exact-string fallback as ASCII-only as well (a name or record model containing non-ASCII never reaches `checkpoint` level through this path, and gets the safe `family` level), or implement Python's full lowercase mapping for the handful of code points where full and simple differ (U+0130 is the only one producing ASCII). Then the "recognises a subset and never a name Python does not" claim holds for `Applicability` too, not just `ReleasedCheckpoint`.

### MEDIUM: The ASCII fold's byte boundary recognises a name Python does not (U+212A), so the "never a match Python would not make" claim is false
- Severity: MEDIUM
- Confidence: MEDIUM
- Location: shisa-de-go/calibration.go:95-152 (`ReleasedCheckpoint`, `asciiLower`, `continuesCheckpointName`)
- Problem: The port scans for the slug in the ASCII-folded text and treats any byte ≥ 0x80 as a boundary. Python applies its regex to the *fully-lowercased* text, where some code points lower into the ASCII class `[a-z0-9.]`: U+212A (KELVIN SIGN) lowercases to `k` in Python. So for `shisa-de-2\u212a`, Go sees boundary byte 0xE2 after the slug and names DE-2, while Python's `(?![a-z0-9.])` sees `k` and returns "" — the family record, reported and not applied. Same on the left boundary (`\u212ashisa-de-2`). The consequence is a bundled fit applied to a name the Python client holds raw, which is the opposite of the documented invariant. That invariant is stated in three places that are all therefore false: the `asciiLower` comment ("never a name Python does not"), `docs/PORT.md` ("never a name Python does not; a name that needs non-ASCII folding to match gets raw probabilities"), and the scope summary. The pre-fix `strings.ToLower` behaviour matched Python here, so this is a corner-case regression introduced by the round-2 fix. The input is contrived (a Kelvin sign abutting the slug), which is why this is MEDIUM and not higher.
- Recommendation: Make the boundary check rune-aware: decode the rune adjacent to the slug and treat it as continuing when the last code point of its *full* lowercase is in `[a-z0-9.]` (U+212A → `k` → continuing; U+0130 → `i`+U+0307 → the combining mark is a boundary, matching Python). Alternatively, if the exact rule is not worth the code, keep the implementation and correct the claim in the `asciiLower` comment and `docs/PORT.md`, and pin the U+212A case in `TestReleasedCheckpointIsConservativeAboutCase` either way.

### MEDIUM: The supplied-record warning fires outside its design, with a false statement and advice contrary to the documented alias remedy
- Severity: MEDIUM
- Confidence: HIGH
- Location: shisa-de-go/client.go:719-726 (`NewDecisionModel`)
- Problem: The warning is written for "a family spelling: naming the family no longer names one fit" (the comment) — a caller who passed `--calibration de2` while serving DE-2.1. But the guard is `own.Model != model_.Calibration.Model` where `own` is `CalibrationForCheckpoint(...)`, so it fires whenever *any* supplied record differs from what the served names would select, including two cases it gets wrong:
  1. The served model names no released checkpoint (a training arm or fine-tune), and the caller supplies their own fit. `own` is then the *family fallback* record (DE-2's), not a record the served checkpoint "has", yet the warning says "`de2-some-arm` names `shisa-ai/shisa-de-2`, which has its own record; pass that record to calibrate it" — a false statement advising the caller to apply DE-2's fit to their fine-tune, the exact mistake this feature exists to prevent. The existing `TestDecisionModelCalibration` constructs precisely this configuration (model `de2-candidate-lr5e5`, supplied record fitted on `some-other-checkpoint`), so the warning already fires in-repo with the false text.
  2. The README's documented alias remedy: serving DE-2.1 behind the id `shisa-ai/shisa-de-2` and passing DE-2.1's record via `Calibration`. Here `own` is DE-2's record, the warning fires, and its advice — "pass that record" — tells the caller to apply DE-2's fit to DE-2.1's answers, the over-correction the remedy exists to avoid.
  The warning is recorded in `docs/PORT.md` as "a supplied record is the family's while the served checkpoint has its own", which is not what the guard implements.
- Recommendation: Restrict the warning to the case it was written for: fire only when the supplied record *is* the family's default record (compare against `CalibrationFor(family)`, not `CalibrationForCheckpoint`) while the served checkpoint has its own record — or, keeping the current guard, additionally require that `ReleasedCheckpoint(model)` or the tokenizer name a released checkpoint. `TestAFamilyRecordOnACheckpointWithItsOwnRecordIsReported` still passes under the first form; both false-firing cases stop.

### LOW: The live calibration check derives its expected temperature from `answer.Strategy`, which is not the read name for a thinking answer
- Severity: LOW
- Confidence: HIGH
- Location: shisa-de-go/live_test.go:1342 (and 1356)
- Problem: `TestLiveCalibrationMatchesTheServedCheckpoint` computes `want := model.Calibration.TemperatureFor(answer.Type, answer.Strategy)`. The client tempers with the *last stage* name (`client.go`, `read.Components[len(read.Components)-1].Stage`), which is `"think"` when the strategy is `repeat2-think`. The record has no `repeat2-think` read, so `TemperatureFor` returns 1.0 while the answer carries the think fit (6.8 or 5.23) — the test then fails with "the answer was tempered at 6.8, its record holds 1", a false alarm about wrong-record application. The distinctness check at line 1356 uses the same wrong read name and would additionally report that the two records "share a temperature ... cannot tell them apart" (both yield 1.0). The test passes today only because the obvious spam question stays above the gate; against a deployment whose repeated read is unsure, it fails for the wrong reason. It cannot false-pass: a wrong record or wrong per-read fit still shows up as a temperature mismatch.
- Recommendation: Derive the read name the way the client does — map `repeat2-think` to `think` (or read the last component's stage out of `decision.Raw[head]["components"]`) — as `TestLiveDE2ThinksWhenTheRepeatedReadIsUnsure` already does with the literal `"think"`.

### LOW: `docs/PORT.md` does not record the `Applicability` adaptation that normalises the record's own model
- Severity: LOW
- Confidence: HIGH
- Location: shisa-de-go/docs/PORT.md:108-160 (the calibration adaptations)
- Problem: `Applicability` (calibration.go:382-386) normalises the record's own model through `ReleasedCheckpoint`, so a record fitted on a suffixed repo name (`shisa-ai/shisa-de-2.1-FP8-dynamic`) reaches `checkpoint` level for the base checkpoint. Python compares `self.model.lower()` only (calibration.py:197), so the same record reports `family` there. This is a deliberate, tested divergence (`TestApplicabilityNormalisesTheRecordsOwnModel`), but the port's rule is "the Python client is the reference; report divergences", and `PORT.md` records every other calibration adaptation — including the fold, the boundary test, the tokenizer rule and the path-first resolution — without this one. The scope question "does PORT.md now record every adaptation?" is answerable no.
- Recommendation: Add a paragraph to the calibration adaptations in `PORT.md` stating the divergence and why it is safe (it only widens `checkpoint` level for a record whose own name is the checkpoint plus a suffix; it cannot apply a record to a checkpoint the record's name does not extend).

### LOW: The disagreement warning fires on the standard, correct DE-2.1 deployment
- Severity: LOW
- Confidence: HIGH
- Location: shisa-de-go/client.go:711-715
- Problem: The warning fires whenever the model id and tokenizer name different released checkpoints, including the documented normal configuration — DE-2.1 served with DE-2's tokenizer (`Model: "shisa-ai/shisa-de-2.1"`, `Tokenizer: "shisa-ai/shisa-de-2"`), which is how DE-2.1 is actually served. Nothing it says is false ("the model id chose the record" is true), and the same warning in the other direction is a real signal for the alias case, so the trade-off is defensible and documented in the README. It is still a warning that will fire on every faithful DE-2.1 deployment, which trains users to ignore the channel the alias detection depends on.
- Recommendation: At minimum, note in the README's calibration section that the warning appears on a standard DE-2.1 deployment and is informational there. Suppressing it for the 2.1-id/DE-2-tokenizer direction specifically would hardcode a release fact into the client and may be counterproductive; the docs note is the proportionate fix.

## Things Checked

- The round-1 HIGH fix: `CalibrationForCheckpoint` order (model id, then tokenizer), the README/AGENTS.md alias documentation, the `Calibration`-as-remedy guidance, and the tests pinning both disagreement directions (`client_test.go:1889-1940`).
- `ResolveCalibration` against the scope's three wrong-record questions: a checkpoint id cannot collide with a `.json` path; a missing relative file is an error naming what ships (tested); no family spelling collides with a checkpoint name. An existing regular file named like a family spelling (`de2`) would be loaded as a path, which is the documented file-wins rule and is listed under residual risks.
- `TestResolveCalibrationLoadsAPathBeforeMatchingAName`, the provenance test over every shipped record, the fold tests (U+0130, U+017F, multi-byte boundaries, both-slugs-present ordering), and `TestEachReleasedCheckpointGetsItsOwnRecord`.
- The live check (`live_test.go:1292-1363`): it now reads through the endpoint, derives its expectation by the client's own rule, asserts the answer's own `Temperature` and `Calibrated`, requires the per-read table, and cross-checks the other record's temperature. It cannot false-pass on a wrong record or wrong per-read fit; its remaining defect is the strategy/read-name aliasing above.
- README claims against the code: the tokenizer fallback scope, the fine-tune case, the family-spelling meaning (`--calibration de2` → DE-2's record, matching `ResolveCalibration` and the CLI flag usage in `cmd/shisa-de/main.go:241-244`), and the alias remedy.
- `docs/READOUT-DE2.md` against the Python reference: no Go spellings found (`Answer.Strategy`, `&record`, record paths); the calibration section's DE-2.1 temperatures match on every spot-checked line.
- `data/calibration-de2.1.json` and `data/calibration-de2.json` against the Python repository's records: byte-identical content, `temperatures` equal to the `repeat2` entry in both, provenance fields present.
- `PORT.md` coverage of the round-2 adaptations: per-checkpoint selection, no DE-1 entry, direct boundary test, ASCII fold, tokenizer-not-weights, path-first resolution, measurement attribution — all present, except the one in the LOW finding above.

## Residual Risks

- No live run was possible (the DE-2.1 deployment is still in progress), and this review is read-only with no shell, so the "validation already run" commands in the scope were not re-executed. The fold findings rest on reading the code against Python's `str.lower()`/regex semantics and standard Unicode mappings, not on an executed check.
- The alias case remains undetectable from a response by construction; the port reports the disagreements it can see and documents the one it cannot, which matches the scope's stated non-goal.
- `ResolveCalibration` treats any existing regular file as a path before family or checkpoint spellings, so a file literally named `de2` (or, less plausibly, a record file at a cwd-relative `shisa-ai/shisa-de-2.1` path) shadows the spelling. Contrived, and a non-record file fails loudly rather than silently; noted rather than fixed.
- The U+0130/U+212A analysis assumes Go's `unicode` tables and the CPython interpreter's Unicode data agree on these code points; they are stable across current versions, but the exact-parity fix should pin both cases in `TestReleasedCheckpointIsConservativeAboutCase`.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "Applicability's strings.ToLower fallback invents matches Python does not make (U+0130), applying a fit Python holds raw",
      "location": "shisa-de-go/calibration.go:378-386",
      "recommendation": "Make the non-checkpoint exact-string fallback ASCII-only (non-ASCII names get the safe family level), or implement Python's full lowercase mapping, so Applicability keeps the subset property."
    },
    {
      "severity": "MEDIUM",
      "title": "The ASCII fold's byte boundary recognises U+212A as a boundary where Python sees 'k', so the never-a-name-Python-does-not claim is false",
      "location": "shisa-de-go/calibration.go:95-152",
      "recommendation": "Make the boundary check rune-aware using the full lowercase mapping (U+212A continues a name, U+0130's combining mark does not), or correct the invariant claim in the comment and PORT.md; pin the U+212A case in the fold test either way."
    },
    {
      "severity": "MEDIUM",
      "title": "The supplied-record warning fires on uncovered checkpoints with a false 'has its own record' statement and on the README's documented alias remedy",
      "location": "shisa-de-go/client.go:719-726",
      "recommendation": "Fire only when the supplied record is the family's default record (compare against CalibrationFor) and the served checkpoint has its own record, or require ReleasedCheckpoint to name a checkpoint first."
    },
    {
      "severity": "LOW",
      "title": "Live calibration check uses answer.Strategy as the read name, so a repeat2-think answer fails the test against 1.0 instead of the think fit",
      "location": "shisa-de-go/live_test.go:1342",
      "recommendation": "Derive the read name the way the client does (repeat2-think maps to think, e.g. from the last component's stage) before calling TemperatureFor."
    },
    {
      "severity": "LOW",
      "title": "PORT.md does not record the Applicability adaptation that normalises the record's own model",
      "location": "shisa-de-go/docs/PORT.md:108-160",
      "recommendation": "Add a paragraph recording the divergence from Python's self.model.lower() comparison and why it is safe."
    },
    {
      "severity": "LOW",
      "title": "The disagreement warning fires on the standard correct DE-2.1-with-DE-2-tokenizer deployment",
      "location": "shisa-de-go/client.go:711-715",
      "recommendation": "Document in the README that the warning is informational on a standard DE-2.1 deployment; suppression may be counterproductive."
    }
  ]
}
END_REVIEW_JSON
