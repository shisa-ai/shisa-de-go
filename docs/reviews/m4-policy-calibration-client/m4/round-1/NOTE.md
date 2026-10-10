# Round 1 coverage and disposition

Reviewers: `astra`, `ds4`, `glm`. All three produced usable reports.

`glm` ran through its fallback (`glm-zai-glm-5.3-jouzu-cli`) after one failed
attempt on its primary candidate, so its report is `glm-5.3`'s through a
different provider route rather than the alias's primary. `astra` and `ds4` ran
on their primaries.

No HIGH or CRITICAL findings. Two MEDIUMs and eleven LOWs, all of which are
addressed in `5d901e2`.

The MEDIUMs:

- `astra`: `NewDecisionModel` resolved `config.Family` for its own check but
  never passed it to `NewReadout`, so a declaration the client accepted could be
  refused by the readout, or the two layers could report different family
  sources. Fixed by forwarding the declaration, and the regression test fails
  when the forward is removed.
- `astra`: JSON `null` in a temperature table decoded into a `map[string]float64`
  as zero, which `TemperDistribution` passes through, so a malformed record
  produced raw probabilities reported as calibrated. `CalibrationFromJSON` now
  requires the Python loader's keys and refuses a null entry.
- `astra`: duplicate option keys were accepted and then collapsed into one
  probability, so the returned distribution no longer summed to one. Python's
  criteria are a dict and cannot reach this state; this port takes a slice, so
  the key is now checked in the validation pass.
- `astra` and `ds4` independently: `pythonStr` returned this package's JSON
  spellings rather than Python's `str()`, so `inf` came back as `Infinity` and a
  container as `['low', None]`'s JSON form. The value reaches `Answer.Level`, the
  legend, the classify value and the TypeSafe legend.
- `astra` and `ds4` independently: the replay recorded neither the legend nor
  `to_wire`, and the answer comparison covered selected fields rather than the
  whole map, so a missing legend or a wrong wire field passed. The fixture now
  records both and the replay compares `to_dict` and `to_wire` whole, per answer
  and for the decision.
- `astra` and `ds4` independently: the generator wrote a fixed date, a fixed
  endpoint label in the `serving_fingerprint` field, and a literal readout
  version. It now records the date it ran, the served `system_fingerprint`, the
  readout version from the reference implementation, and the tokenizer revision
  read from the hub cache.

The LOWs: the debug `prompt` was `""` where Python writes `null` for the
id-form read after a thought (fixed, and the new `decide-debug-think` case fails
without the fix); the `covered` rule had no test that failed without it (fixed);
`PORT.md` credited a `CalibrationForRead` that does not exist and overstated
which fields the replay compares (both corrected); the recorder's documented
API-key variable did not work (fixed, and the generator now reads the same three
variables the client does); `pythonStr`'s float branch was dead code (folded into
the `str()` fix); the multi-label template was not Python's `str.format` (fixed
for `{{`, `}}` and unknown fields, and the remaining format-spec gap is recorded
in `PORT.md`); `Decide` does not accept the label-set shorthand Python's does
(recorded in `PORT.md` and the doc comment, and `QuestionFromHead` is exported);
`MaxWorkers: 0` means the default here where Python's explicit `0` means one
worker (recorded); `CalibrationFromJSON` was more permissive than the Python
loader (now matches it); a multi-label head with described options read a
different thing here than in Python (now refused); and the parity transport
collapsed two exchanges with identical request bodies into one (now a queue).

Two findings were checked rather than assumed:

- `astra` reported that the recorded `decide-noul-near-gate` case thinks. It does
  not: the recorded strategy is `repeat2` at a top probability of 0.59, below the
  0.7 gate. The `PORT.md` sentence was wrong, not the fixture.
- `ds4` reported that the pointer values in caller-visible maps are invisible to
  the parity gate. They are: the gate compares through JSON, and a `*float64`
  marshals to the same number as a `float64`. The maps were dereferenced anyway,
  because a caller sees the difference, and a separate test now pins the Go type
  rather than leaving it to the gate.

The fixture was regenerated in the same commit: 24 cases, 35 exchanges, served
`system_fingerprint: vllm-0.30.0-tp2-ca69195e`, tokenizer pinned to
`8f21734b27b3462471c4662d5638e52195b005a3`. The previous fixture was 23 cases
and 32 exchanges with the same fingerprint.
