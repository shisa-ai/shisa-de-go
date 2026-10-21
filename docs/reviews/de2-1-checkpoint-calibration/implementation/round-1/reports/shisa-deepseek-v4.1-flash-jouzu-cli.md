# Review Report

## Summary

The change does what the scope says: a second embedded record, a per-checkpoint selector that mirrors Python's `released_checkpoint`/`calibration_for`, normalisation in `Applicability`, tests, and docs. I found one HIGH issue: the documented workaround for the alias case does not work. When `Model` names one released checkpoint and `Tokenizer` names another — exactly the configuration README.md:317-320 tells a user to write for a DE-2.1 deployment behind the `shisa-ai/shisa-de-2` alias — the selector takes the model's record, `Applicability` then reports `checkpoint`, and DE-2's temperatures are applied to DE-2.1 answers with no warning. The new live test passes in that configuration too. Six LOW findings follow, mostly around tests that assert less than they appear to and name-matching edges.

## Findings

### HIGH: The documented `Tokenizer` escape hatch selects the alias's record, not the real checkpoint's
- Severity: HIGH
- Confidence: HIGH
- Location: `calibration.go:516` (`CalibrationForCheckpoint`), `client.go:688`, with the promise in `README.md:310-320`, `AGENTS.md:43-46`, `docs/READOUT-DE2.md:390-392`
- Problem: README tells a caller whose server serves DE-2.1 behind `shisa-ai/shisa-de-2` to write

  ```go
  Model:     "shisa-ai/shisa-de-2",   // the alias the server serves
  Tokenizer: "shisa-ai/shisa-de-2.1", // the checkpoint behind it
  ```

  That configuration applies DE-2's fit. Trace: `client.go:688` calls `CalibrationForCheckpoint(de2, "shisa-ai/shisa-de-2", "shisa-ai/shisa-de-2.1")`; the loop at `calibration.go:517` tries the model first, `ReleasedCheckpoint("shisa-ai/shisa-de-2")` is a table hit, its record's family is `de2`, so it returns DE-2's record and never reads the tokenizer. `Applicability` (`calibration.go:338-357`) then builds names `["shisa-ai/shisa-de-2", "shisa-ai/shisa-de-2.1"]`, matches the record's model on the first, and returns `ApplicabilityCheckpoint` with no reasons, so `CalibrationApplied` is true (`client.go:699`) and no warning is recorded. The caller is told the record is the checkpoint's, gets the over-correction the table in README.md:296-300 exists to warn about, and `doctor` reports `calibration_level: checkpoint`. `TestLiveCalibrationMatchesTheServedCheckpoint` also passes in this configuration: `ReleasedCheckpoint(liveModelID())` is DE-2 and `model.Calibration.Model` is DE-2, so the check the change added confirms the wrong record. The `Calibration:` half of the same paragraph does work, because a supplied record is applied as given.
- Recommendation: Decide the rule for two names that disagree, and make the docs match it. The tokenizer source is the caller's only way to say "the weights are not what the id says", so preferring it when it names a released checkpoint (or refusing the contradiction with an error) is the reading that makes the documented example true; add a table case for `model=shisa-ai/shisa-de-2, tokenizer=shisa-ai/shisa-de-2.1` to `TestTheClientAppliesTheRecordOfTheCheckpointItServes`. If the order stays as it is, then README.md:317-320, AGENTS.md:44-46 and `docs/READOUT-DE2.md:390-392` must stop offering `Tokenizer` as the way to name the weights behind an alias, and `--calibration`'s help (`cmd/shisa-de/main.go:241-244`, "Default: chosen from the served model id") should say what actually happens. Python's `calibration_for` has the same model-first order, so a fix here is a divergence to record in `docs/PORT.md` (and, per AGENTS.md, likely belongs upstream too). A separate explicit field (`Checkpoint`, `--checkpoint`) would also remove the ambiguity without changing what `Tokenizer` means.

### LOW: The live test's per-read temperature loop cannot fail
- Severity: LOW
- Confidence: HIGH
- Location: `live_test.go:1319-1328`
- Problem: `want := model.Calibration.Reads[read]` then `model.Calibration.TemperatureFor(questionType, read) != temperature` compares the record against itself; `TemperatureFor` reads the same map (`calibration.go:288-308`), so the assertion is a tautology. If the selected record had no `reads` at all, the range over the nil map iterates zero times and the loop silently asserts nothing. The comment above it ("The temperatures are the record's, not the other checkpoint's") describes a check that is not there; the real check is the `model.Calibration.Model != checkpoint` line above, which does work for a non-aliased id.
- Recommendation: either delete the loop, or assert something the record does not already contain — e.g. that the applied `TemperatureFor` differs from the other DE-2 checkpoint's for the same read, which is the claim the comment makes and is available offline anyway (`TestDE21IsTemperedWithItsOwnFit` already pins it). Also assert `len(model.Calibration.Reads) != 0` so an empty table fails rather than passing vacuously.

### LOW: `--calibration <family>` applies DE-2's fit to a DE-2.1 endpoint without a warning
- Severity: LOW
- Confidence: HIGH
- Location: `calibration.go:539-556` (`ResolveCalibration`), `client.go:699-704`
- Problem: `ResolveCalibration("de2")` returns the family record, whose model is `shisa-ai/shisa-de-2`. The CLI passes it as a supplied `Calibration` (`cmd/shisa-de/main.go:169-173`), so `CalibrationApplied` is true regardless of level, the warning at `client.go:700` is skipped because the record *is* applied, and a DE-2.1 endpoint is tempered with DE-2's temperatures. `doctor` does print a `calibration_note` for the family level, but the library caller and `ask` get no signal. Now that a family has two records, the family spelling no longer identifies one fit.
- Recommendation: when a supplied record's level is `ApplicabilityFamily` and a record for the served checkpoint exists, record a warning (or refuse the family spelling when the served model names a released checkpoint, and require the checkpoint id). At minimum say in README.md:323-326 that the family spelling means DE-2's record.

### LOW: A local fine-tune named after its base checkpoint gets the base's record at `checkpoint` level
- Severity: LOW
- Confidence: HIGH
- Location: `calibration.go:73-110` (`ReleasedCheckpoint`), `README.md:302-308`
- Problem: "a name that contains a released checkpoint's name as its own token is that checkpoint" is what makes `/models/shisa-de-2.1` and `shisa-ai/shisa-de-2.1-FP8-dynamic` work, but the same rule makes `/models/shisa-de-2.1-mine-lr5e5` DE-2.1, so a fine-tune whose directory is named after its base is calibrated with the base's fit, at `checkpoint` level, with no warning. That is the opposite direction from the alias case the README warns about, and it is the one where the name rule applies a fit that nobody vouched for. `docs/PORT.md:128-129` states the rule ("a name that extends a release with a suffix ... is that release"), but the user-facing README only warns about an id outliving its weights.
- Recommendation: add a sentence to README.md:302-308 that a fine-tune or adapter named after its base is treated as the base and should pass its own `Calibration` (or a `Tokenizer` that names no release, which leaves the record held rather than applied). The behaviour can stay; the surprise is the part worth documenting.

### LOW: `Applicability` normalises the served names but not the record's own model
- Severity: LOW
- Confidence: HIGH
- Location: `calibration.go:337-357`
- Problem: `names` maps `model` and `Tokenizer` through `ReleasedCheckpoint`, but the comparison at `calibration.go:353` lowercases `c.Model` raw. A caller who fits a record on `shisa-ai/shisa-de-2.1-FP8-dynamic` and serves that same id gets `ApplicabilityFamily` with "same family, another checkpoint" for the exact name the record was fitted on. The shipped records are unaffected because their `model` values are canonical, and a supplied record is applied anyway, so this only misreports the level and the reason — but `doctor` prints both.
- Recommendation: compare `ReleasedCheckpoint(c.Model)` (falling back to the lowercased raw name) against the normalised names, so both sides of the comparison use one rule.

### LOW: `ReleasedCheckpoint`'s tie-break is lexicographic where Python's is table order
- Severity: LOW
- Confidence: MEDIUM
- Location: `calibration.go:80-95`
- Problem: Python sorts by `key=len, reverse=True`, which is stable, so two checkpoints of equal length are tried in `CHECKPOINT_CALIBRATION_FILES` insertion order. Go sorts by length then lexicographically, which is the only total order available over a Go map. Today the two keys differ in length (19 vs 21), so the results agree; adding a third checkpoint whose id has the same length as an existing one can order differently between the clients, and the winner decides which record is applied when a name contains both slugs.
- Recommendation: keep an explicit ordered slice (longest first, then the table's order) as the source of truth and derive the map from it, or say in `docs/PORT.md:122-129` that ties break lexicographically rather than by table order. An ordered slice also removes the per-call allocation and sort.

### LOW: `ReleasedCheckpoint` is not input-for-input equivalent to Python's regex for non-ASCII case
- Severity: LOW
- Confidence: HIGH (mechanism), LOW (impact)
- Location: `calibration.go:74` (`strings.ToLower`)
- Problem: the ASCII boundary class is equivalent — a byte at or above 0x80 is a boundary in both, so multi-byte runes behave the same, and the "slug appears twice" case agrees because both prefer checkpoint order (longest first) over the earliest match position. The lowercasing is not equivalent: Python's `str.lower()` applies the full case mapping, so `İ` (U+0130) becomes `i` + U+0307, while Go's `strings.ToLower` applies the simple mapping and yields `i`. `SHİSA-DE-2.1` therefore matches in Go and not in Python. Separately, an invalid UTF-8 byte becomes U+FFFD under `strings.ToLower` but survives as a lone surrogate in a Python `str`.
- Recommendation: not worth code for a name nobody serves, but if the parity claim matters, say in `docs/PORT.md` that the rule is equivalent over valid UTF-8 apart from Unicode case mapping, or compare against a `strings.Map`-free lowercasing that only folds ASCII (which is all the slugs need and is closer to what the boundaries assume).

### LOW: The DE-2.1 record's provenance is not pinned the way DE-2's is
- Severity: LOW
- Confidence: HIGH
- Location: `calibration_test.go:TestShippedCalibration`, `calibration_test.go:TestEachReleasedCheckpointGetsItsOwnRecord`
- Problem: `TestShippedCalibration` checks `FittedOn`, `ServingShape`, `Source` and the ECE/withheld tables, but only for `CalibrationFor(FamilyDE2)`. For DE-2.1 the tests check the model, family, level and a couple of temperatures; `source` is not required by the loader, so a truncated DE-2.1 record (empty `source`, empty `test_ece`, empty `note`) would load and pass. The record is the one artifact this change adds, and its provenance is what a reader needs to judge the fit.
- Recommendation: run the same provenance assertions over every entry in `CheckpointCalibrationFiles`, including `Source`, a non-empty `TestECE` and the `think` row, so a truncated copy fails the build.

## Things Checked

- HEAD is `ecc90ff2abb73bd38653f6681423bea8f68dd272` per `.git/refs/heads/main`, matching the scope's target; reviewed the working tree.
- `data/calibration-de2.1.json` against the Python client's `shisa_de/data/calibration-de2.1.json`: read both in full, identical text (I could not run a byte diff). Its temperatures and every ECE/accuracy row match the tables in `docs/READOUT-DE2.md:394-419` and the `test_ece` figures in the record.
- `ReleasedCheckpoint` against Python's `released_checkpoint`: exact id; slug-token match with ASCII boundaries; longest-first precedence over earliest position when a name holds both slugs; empty name; `shisa-de-2.10`, `shisa-de-2.2`, `shisa-de-21`, `my-shisa-de-2.1x`; training arms; `/models/...`; quantized suffixes; a leading or trailing `.`; DE-1 names absent by design. Equivalent for ASCII and multi-byte input apart from the case-mapping note above.
- Record selection order matches Python's `calibration_for` (model, then tokenizer, first family match wins). Python's DE-1 branch in that loop is unreachable here: the table has no DE-1 entry and `RequireDE2` refuses a DE-1 model before construction, so `CalibrationForCheckpoint(FamilyDE1, ...)` only errors for direct callers.
- `client.go:688` passes the same `tokenizerSource` used for family resolution, so family and record selection cannot disagree about which name is the tokenizer.
- `Applicability`: normalisation of both served names, `checkpoint`/`family`/`mismatch`/`unfitted` precedence, both reasons on a double mismatch, supplied records applied as given, and the level/reason the client records for each of the six served ids in the test table.
- No readout version bump: `calibrationCompatible` still lists no DE-2 predecessors and both records carry `de2-codebook-v1`, so `CalibrationCompatibleWith` accepts both. The answer-producing contract is unchanged; only calibration selection and data changed, which is consistent with AGENTS.md's rule. Answers for a DE-2.1 caller do change (held-then-raw becomes applied), and the `Calibration` id on each answer is what distinguishes them.
- No regression for DE-2 callers: `CalibrationForCheckpoint(de2, "shisa-ai/shisa-de-2", "")` returns the same record, the same `checkpoint` level and the same empty warning list as `CalibrationFor` plus `Applicability` did before.
- `ResolveCalibration`: checkpoint ids and suffixed spellings, family spellings, `""`/`"auto"`, a real file, a missing file, and a version with no record (errors, naming the shipped checkpoints). The CLI passes the resolved record as a supplied `Calibration`, so `--calibration shisa-ai/shisa-de-2.1` does work.
- Tests reviewed for what they can and cannot fail: the name table, per-checkpoint record identity and differing fits, the six client-level ids, the DE-2.1-vs-DE-2 tempering comparison, and the live test's gating and its two branches.
- Docs: README's "Everything else in the Python client's API has a Go counterpart" holds — every `calibration.py` export (`CALIBRATION_FILES`, `CHECKPOINT_CALIBRATION_FILES`, `Calibration`, `released_checkpoint`, `calibration_for`, `calibration_from_dict`, `confidence`, `load_calibration`, `load_calibration_file`, `resolve_calibration`, `temper_binary`, `temper_distribution`) has a Go counterpart, and the one table difference (no DE-1 entry) is recorded in `docs/PORT.md:116-120`. README's DE-2 temperature table matches `data/calibration-de2.json`.
- `docs/PORT.md` records three paragraphs about this change (two functions, no DE-1 entry, boundary testing); the scope said four, and the `--calibration de1` paragraph at `PORT.md:445-450` reads as the fourth.
- Did not run `gofmt`, `go vet` or `go test`: no shell is available in this session and the scope does not ask for them. The scope's validation results are taken as reported.

## Residual Risks

- No live run against a DE-2.1 endpoint. The shipped DE-2.1 temperatures were fitted on the release candidate's merged weights, so whether they describe the uploaded checkpoint is unverified, as the record's own `note` says.
- The alias case stays undetectable from a response. Whatever mechanism is chosen, a caller who passes neither `Tokenizer` nor `Calibration` gets DE-2's fit against a DE-2.1 deployment, and `doctor` will report `checkpoint`.
- The DE-2.1 `think` fit (5.23) covers a read where 936 of 1,298 thoughts were cut by the 1,024-token budget, and it still leaves a cross-validated ECE of 0.086. Probabilities read after a thought on another serving shape are outside the fit.
- If the selection order is changed to prefer the tokenizer, this port and the Python client will disagree on the alias configuration until the Python side changes; that divergence needs `docs/PORT.md` and probably an upstream change.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "HIGH",
      "title": "Documented Tokenizer escape hatch selects the alias's record, not the real checkpoint's",
      "location": "calibration.go:516",
      "recommendation": "Prefer the tokenizer's released checkpoint when model and tokenizer name different releases (or refuse the contradiction), add a test for model=shisa-ai/shisa-de-2 with tokenizer=shisa-ai/shisa-de-2.1, and correct README.md:317-320, AGENTS.md:44-46 and docs/READOUT-DE2.md:390-392 if the order stays model-first."
    },
    {
      "severity": "LOW",
      "title": "Live test's per-read temperature loop compares the record with itself",
      "location": "live_test.go:1319",
      "recommendation": "Delete the loop or assert a value the record does not already hold (e.g. that it differs from the other checkpoint's for the same read), and require a non-empty Reads table so an empty record fails instead of passing vacuously."
    },
    {
      "severity": "LOW",
      "title": "--calibration <family> applies DE-2's fit to a DE-2.1 endpoint with no warning",
      "location": "calibration.go:539",
      "recommendation": "Warn when a supplied record's level is family and the served checkpoint has its own record, or refuse the family spelling when the model names a released checkpoint; document what the family spelling means."
    },
    {
      "severity": "LOW",
      "title": "A fine-tune named after its base checkpoint gets the base's record at checkpoint level",
      "location": "calibration.go:73",
      "recommendation": "Document in README.md:302-308 that a fine-tune or adapter named after a release is treated as that release and should pass its own Calibration."
    },
    {
      "severity": "LOW",
      "title": "Applicability normalises the served names but not the record's own model",
      "location": "calibration.go:353",
      "recommendation": "Compare ReleasedCheckpoint(c.Model) against the normalised names so a record fitted on a suffixed repo name reaches checkpoint level for that name."
    },
    {
      "severity": "LOW",
      "title": "ReleasedCheckpoint tie-break is lexicographic where Python's is table order",
      "location": "calibration.go:80",
      "recommendation": "Use an explicit ordered slice as the source of truth (longest first, then table order) or record the lexicographic tie-break in docs/PORT.md."
    },
    {
      "severity": "LOW",
      "title": "ReleasedCheckpoint lowercasing is not equivalent to Python's for non-ASCII case",
      "location": "calibration.go:74",
      "recommendation": "Note the case-mapping difference in docs/PORT.md, or fold only ASCII when lowercasing, which is all the slugs need."
    },
    {
      "severity": "LOW",
      "title": "DE-2.1 record's provenance is not pinned by a test the way DE-2's is",
      "location": "calibration_test.go:TestShippedCalibration",
      "recommendation": "Run the same provenance assertions (Source, TestECE, think row) over every entry in CheckpointCalibrationFiles."
    }
  ]
}
END_REVIEW_JSON
