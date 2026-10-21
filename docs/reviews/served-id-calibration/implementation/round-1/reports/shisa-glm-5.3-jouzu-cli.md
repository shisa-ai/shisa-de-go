# Review Report

## Summary

The commit's core mechanism is sound and carefully built: `completionResponse`/`chatResponse` carry the served id, `readCodes` records the first response that names one via the `noteServed` closure, `ReadImage` records it directly, and `servedRecord` swaps in the shipped record of the served checkpoint without mutating `DecisionModel.Calibration` (so a shared model stays race-free), never overrules a caller-supplied record, and only swaps onto a fitted record whose `Model` is exactly the canonical checkpoint. The three new tests pin the three branches, and the parity fixtures are unaffected because the new `Answer` fields are excluded from `ToDict`/`ToWire`.

The main problems are at the edges: the two live gates were not updated for the new behaviour and, as written, both fail against the aliased hosted endpoint this commit itself measured — while `docs/PORT.md` claims one of them passes. Second, the served-id signal is consulted only for swapping between *covered* checkpoints: a served id that names a different, uncovered checkpoint leaves the configured record silently tempering weights the response says are not its own. I verified everything against the working tree of `shisa-de-go` (no shell tool is configured in this session, so no `git`/`go` commands were run; the scope's rules were followed).

## Findings

### HIGH: The live calibration check contradicts the new served-id behaviour, and PORT.md claims it passes
- Severity: HIGH
- Confidence: HIGH
- Location: live_test.go:1298-1357; docs/PORT.md:1031-1039
- Problem: `TestLiveCalibrationMatchesTheServedCheckpoint` still derives its expectation from `model.Calibration` — the configured record — at live_test.go:1355 (`want := model.Calibration.TemperatureFor(...)`), and compares `answer.Temperature` against it exactly (live_test.go:1356-1357). Against the aliased hosted endpoint (configured `shisa-ai/shisa-de-2`, served `shisa-ai/shisa-de-2.1`, per the commit's own 2026-10-21 measurement), the client now tempers at DE-2.1's fit (e.g. repeat2 choice 1.64, direct noul 1.98) while the test expects the configured DE-2 record's fit (1.89, 2.69) — the test fails on the default live configuration. Its doc comment (live_test.go:1304-1309, "It cannot see through an alias… this check passes with that id's record applied") states the pre-change behaviour and is now false. `docs/PORT.md:1036-1037` was updated in this commit to claim the opposite — "`SHISA_DE_MODEL=shisa-ai/shisa-de-2` served by DE-2.1 passes with DE-2.1's record applied" — which the unchanged test does not do. The commit message records only that offline suites and two manual live reads were run, not the live gates.
- Recommendation: Update the test to derive the expectation from what actually tempered the answer — e.g. resolve the served record via `answer.ServedModel`/`answer.Calibration` (falling back to the configured record when the served id is empty or echoing) — rewrite the stale doc comment, and make the PORT.md claim match, or run the gate and record the corrected claim with its fingerprint.

### MEDIUM: The M6 live parity gate fails by design on an aliased endpoint, and its docs were not updated
- Severity: MEDIUM
- Confidence: HIGH
- Location: live_test.go:443-448, 652-654; docs/PORT.md:416-419, 992-1010
- Problem: `TestLiveParityAgainstPython` compares the temperature exactly (live_test.go:652-654, with the comment that the temperature is the direct signal for "the same per-read fit was selected") and every probability within 1e-9. On an aliased endpoint the Python client tempers with the configured DE-2 record while this client now tempers with the served DE-2.1 record — PORT.md:416-417 says the two "can differ in probability on an aliased endpoint" — so the gate fails against the hosted default endpoint, by design, whenever the Python repo is present. The live-gate paragraph (PORT.md:992-1010) still describes the gate as requiring exact temperature agreement and does not say it now needs a non-aliased model id (`SHISA_DE_MODEL=shisa-ai/shisa-de-2.1`, or an endpoint whose response echoes the request).
- Recommendation: Either document the gate's new precondition explicitly (point both clients at a checkpoint's own id, or at an echoing server), or make the comparison conditional on the served id agreeing with the configured one. Record which choice was made and why in PORT.md.

### MEDIUM: A served id that names a different, uncovered checkpoint leaves the configured record tempering
- Severity: MEDIUM
- Confidence: MEDIUM
- Location: client.go:1414-1433
- Problem: `servedRecord` consults the served id only to swap between checkpoints that have shipped records. Two cases treat the same evidence asymmetrically. (a) Configured model `shisa-ai/shisa-de-2` (record applied) against a server that honestly reports an uncovered id such as `de2-v4-lr5e5-e3-s7`: `ReleasedCheckpoint(served)` is empty, so the configured DE-2 record keeps tempering weights the response says are not DE-2's — the same failure this commit exists to close, with the answer marked `Calibrated: true` and the only visible signal being `Answer.ServedModel`. (b) Configured model an uncovered alias (record held, not applied) against a server that reports `shisa-ai/shisa-de-2` — exactly the checkpoint of the held family record: the `served == m.Calibration.Model` guard returns early and the answers stay raw, while a server reporting `shisa-ai/shisa-de-2.1` in the same position would have its record swapped in and applied. Neither case is stated in README, AGENTS.md or PORT.md; README:327-328 ("the answer is tempered by the record for the id the server named") reads as unconditional.
- Recommendation: Decide the rule for a served id that disagrees with the configured record and names no covered checkpoint: at minimum document that the configured record stays applied (and that `Answer.ServedModel` is how a caller spots it); better, treat a disagreement that is not an echo as invalidating the configured applicability (answer raw) or emit a warning. Also document case (b), where confirming evidence does not lift a held record.

### LOW: The served-record swap skips the readout-version compatibility check
- Severity: LOW
- Confidence: HIGH
- Location: client.go:1426-1431
- Problem: The configured record is gated by `Calibration.Applicability`, which refuses a record fitted against a readout whose answers the current one does not reproduce. The swap branch of `servedRecord` bypasses that: it returns the served checkpoint's record with `applied=true` without checking `CalibrationCompatibleWith(record.ReadoutVersion, m.ReadoutVersion)`. It cannot fire today — `CalibrationForCheckpoint` only returns shipped DE-2 records, all `de2-codebook-v1` — but a future readout bump with an empty compatibility list would leave the configured record correctly withheld while the served-id swap still applied the stale fit, violating the repo's own rule that a record is applied only under a readout its fit describes.
- Recommendation: Add `CalibrationCompatibleWith(record.ReadoutVersion, m.ReadoutVersion)` to the swap guard, so an incompatible served record falls back to the configured one (which is already applicability-gated).

### LOW: The measured probability is transcribed two ways
- Severity: LOW
- Confidence: HIGH
- Location: docs/PORT.md:425
- Problem: The same measurement — the raw direct `noul` read tempered by DE-2.1's 1.98 fit — is `0.961568819` in the commit message (and the review scope) and `0.961568822` in PORT.md:425. One is a transcription error. I could not recompute which is correct from the digits PORT.md publishes (the raw probability is given as `0.998299278` "to every digit the client reports", so an exact reproduction needs the full client output), but the repo's evidence rules say a number carries its measurement.
- Recommendation: Re-derive the value from the recorded raw read (or re-run the read) and make PORT.md, the commit message's claim and any future citation agree.

### LOW: README's local-server bullet still says a released-id alias needs Calibration unconditionally
- Severity: LOW
- Confidence: MEDIUM
- Location: README.md:435-441
- Problem: The bullet ends "An alias that *is* a released id — `shisa-ai/shisa-de-2` serving DE-2.1's weights — needs `Calibration`, as described under [Probabilities and calibration]". The section it links to now says the alias is caught from the response when the server reports the canonical id, and `Calibration` is needed only for a server that echoes the request. A reader of the bullet alone would pass a record unnecessarily (harmless), but the bullet contradicts the section it cites for any server that does report the canonical id.
- Recommendation: Add the qualifier to the bullet — needs `Calibration` only when the response echoes the request and reveals nothing — or fold the bullet's case into the alias paragraph outright.

### LOW: Decision.Meta and doctor still describe only the configured record
- Severity: LOW
- Confidence: HIGH
- Location: client.go:1330 (Meta["calibration"]); cmd/shisa-de/doctor.go:84-99
- Problem: On a swap, `Decision.Meta["calibration"]` still names the configured record while `Meta["calibrated"]`/`calibration_by_head` reflect what actually tempered each answer, so the Meta block is internally inconsistent about which record applied. `Health`/`doctor` likewise report only the configured record's id, model and `applied` verdict; against the aliased hosted endpoint doctor prints the DE-2 record as applied while every answer will be tempered by DE-2.1's fit. The parity-fixture reason for keeping Meta's bytes is sound and PORT.md:416-419 states it, but no doc tells a caller that Meta describes configuration while `Answer.Calibration` describes what tempered the answer.
- Recommendation: Keep the wire shapes, but state the split in the README alias paragraph (one sentence: Meta and doctor name the configured record; the answer names the one that tempered it). Consider having doctor print the served id its probe already receives.

### LOW: The image-path served-id wiring has no test, and only one of the three new tests fails on revert
- Severity: LOW
- Confidence: HIGH
- Location: client_test.go:2066-2180; client.go:1259
- Problem: The three new tests cover the text staged path only. The image call site (chatResponse.Model → `ReadImage` → `CodeRead.ServedModel` → `servedRecord` → `answerFromRead` with read name "image", client.go:1259) is untested; a regression that stops plumbing `chatResponse.Model` would silently keep the wrong record on image questions. Answering the scope's question directly: only `TestAServedIdMovesTheRecordOntoTheWeightsItNames` fails if `servedRecord` is reverted; the supplied-record and echoed-id tests still pass under a revert, which is fine — they guard against a different wrong implementation (overruling a supplied record), but the report should not claim revert-coverage it does not have. No test covers a served id naming a different, uncovered checkpoint (the MEDIUM finding above) or a suffixed served id (`shisa-ai/shisa-de-2.1-FP8-dynamic`).
- Recommendation: Add an image-path test with a served id in the stub chat response, and tests for the uncovered-served-id case once its intended behaviour is decided.

### LOW: servedRecord re-loads and re-parses the calibration record for every answer
- Severity: LOW
- Confidence: HIGH
- Location: client.go:1426-1431
- Problem: On any swap — which is now the hosted default — `CalibrationForCheckpoint` re-reads and `json.Unmarshal`s the embedded record once per answer, per call. Correct, but repeated work on the hot path for multi-head calls.
- Recommendation: Memoize per served checkpoint outside `DecisionModel` (e.g. a package-level cache keyed by family and checkpoint) so the model stays immutable and shared models stay race-free.

## Things Checked

- `servedRecord` (client.go:1414-1433): all four early-return guards, the swap guard (`record.Model == checkpoint`, `record.Fitted()`), no mutation of `DecisionModel` fields, read-only access to construction-immutable state — safe under the concurrent head pool (it runs in the sequential results loop after `group.Wait()` anyway).
- `noteServed` in `readCodes` (readout.go): first-non-empty rule across blocks; the `disableCandidateIDs` retry path (`continue` without advancing `start`) re-asks and then notes the served id; fallback requests (`letterLogprob`) never note one — consistent with the documented "first response that reports an id", though a server that reports a model only on fallback responses would surface "". A later block reporting a *different* id than the first is silently ignored; all requests name the same model so this is acceptable, and it is documented in the code comment.
- The staged path recomputes `covered` from the chosen record's `Reads`, so a single-fit swapped record would leave a post-thought answer raw; the image path's read name ("image") hits `TemperatureFor`'s 1.0 default on the per-read DE-2 records, so image answers stay raw — matching the "fitted for the text path" rule.
- `Answer.Calibration`/`ServedModel` are excluded from `ToDict`/`ToWire`, so `TestClientParity` and the CLI parity fixtures are unaffected; `answerFixture` and `compareAnswer` name fields explicitly, so the new fields cannot leak into the comparison.
- Case handling: a served id in odd casing or suffixed (`…-FP8-dynamic`) normalises through `ReleasedCheckpoint`'s ASCII fold and suffix rule; `shisa-de-2.2`/`shisa-de-2.10` correctly name nothing; a DE-1 id names nothing (no DE-1 entry in this port's table).
- A supplied record is never replaced (guard ordering is correct: `m.calibrationSupplied` returns before any swap logic); the silence on a supplied-record/served-id disagreement is detectable by callers via `Answer.ServedModel`, and AGENTS.md states the rule — no per-call warning channel exists, so silence is a defensible trade-off.
- The three new tests exercise the alias swap (record id, served model, temperature, and that `model.Calibration` does not move), the supplied-record guard, and the echo case.
- README/AGENTS.md/PORT.md alias paragraphs against the code for the echoing server, the supplied record, and the released-id alias; verified the doc/code contradiction in the live-check paragraph and the README bullet above.

## Residual Risks

- The client trusts the response's `model` field. A proxy that rewrites ids can move answers onto a record that was not fitted on the weights that answered; the failure is bounded — temperature scaling preserves the argmax and the per-read choice, so labels and scores do not flip, only probabilities/confidence shift — but nothing cross-checks the reported id against `/v1/models` or the request.
- I could not run `git` (no shell tool configured) or the offline/live suites (the scope forbids broad test runs and did not ask for them), so the working tree rather than the `954adba..de33223` diff was reviewed, and the two live-gate failures are derived from the code plus the commit's own endpoint measurement rather than from executing the tests. The commit message in `.git/COMMIT_EDITMSG` matches the scope's description of the change, which is the evidence the tree is the reviewed state.
- The live parity fixtures cover only the echoing case by construction (Python has no served-id logic), so no offline test can catch a regression in the swap; until the two live-gate findings are fixed, the served-id path is pinned only by the three stub tests.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "HIGH",
      "title": "Live calibration check contradicts the served-id behaviour; PORT.md claims it passes",
      "location": "live_test.go:1355; docs/PORT.md:1036",
      "recommendation": "Derive the live test's expectation from the record that actually tempered the answer (Answer.ServedModel/Answer.Calibration), fix the stale doc comment, and correct the PORT.md claim"
    },
    {
      "severity": "MEDIUM",
      "title": "M6 live parity gate fails by design on an aliased endpoint; docs not updated",
      "location": "live_test.go:652; docs/PORT.md:992",
      "recommendation": "Document that the gate needs a non-aliased model id or an echoing server, or make the comparison conditional on the served id agreeing"
    },
    {
      "severity": "MEDIUM",
      "title": "Served id naming a different uncovered checkpoint leaves the configured record tempering",
      "location": "client.go:1414",
      "recommendation": "Decide and document the rule for a disagreeing, uncovered served id; consider answering raw or warning, and document that a confirming served id does not lift a held record"
    },
    {
      "severity": "LOW",
      "title": "Served-record swap skips the readout-version compatibility check",
      "location": "client.go:1426",
      "recommendation": "Gate the swap on CalibrationCompatibleWith(record.ReadoutVersion, m.ReadoutVersion)"
    },
    {
      "severity": "LOW",
      "title": "Measured probability transcribed two ways: 0.961568819 vs 0.961568822",
      "location": "docs/PORT.md:425",
      "recommendation": "Re-derive from the recorded raw read and make the documents agree"
    },
    {
      "severity": "LOW",
      "title": "README local-server bullet says a released-id alias needs Calibration unconditionally",
      "location": "README.md:435",
      "recommendation": "Qualify it: needed only when the response echoes the request and reveals nothing"
    },
    {
      "severity": "LOW",
      "title": "Decision.Meta and doctor describe only the configured record",
      "location": "client.go:1330; cmd/shisa-de/doctor.go:84",
      "recommendation": "Document the split between Meta/doctor (configuration) and Answer.Calibration (what tempered the answer); consider printing the probe's served id in doctor"
    },
    {
      "severity": "LOW",
      "title": "Image-path served-id wiring untested; only one of three new tests fails on revert",
      "location": "client_test.go:2066; client.go:1259",
      "recommendation": "Add an image-path test with a served id in the stub chat response, and tests for the uncovered-served-id case"
    },
    {
      "severity": "LOW",
      "title": "servedRecord re-loads and re-parses the calibration record per answer",
      "location": "client.go:1426",
      "recommendation": "Memoize the record per family and checkpoint outside DecisionModel"
    }
  ]
}
END_REVIEW_JSON
