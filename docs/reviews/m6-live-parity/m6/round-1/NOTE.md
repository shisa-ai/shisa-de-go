# M6 round 1 — disposition

Two of the three requested reviewers reported. `astra`
(`shisa-gpt-6-astra-jouzu-cli`) returned a single line, "Our servers are
currently overloaded. Please try again later.", which the harness recorded as a
successful execution because a report file was written. It is not a review;
nothing below is attributed to it. `ds4` and `glm` each produced a full report
and converged independently on the two MEDIUM findings that mattered.

Round 1: 0 HIGH, 0 CRITICAL. No rerun (the rule is HIGH/CRITICAL, and 2 of 3
reviewers executed).

## Fixed

| Finding | Reporters | What changed |
| --- | --- | --- |
| The recorder hardcoded the hosted endpoint and model, so the gate could compare two servers and only log it | ds4 M, glm M | `live_parity.py` resolves `SHISA_DE_ENDPOINT`/`SHISA_DE_MODEL` the way the client does; the gate now fails on a mismatch, normalized the way the client normalizes. Both verified by patching the recorder to ignore the variable |
| Exit 3 made the "inconclusive, the server moved" branch dead code | ds4 L, glm M | classified as a recording verdict by `liveRecorderExitIsUnstable`, with a test that re-executes the test binary at each status; verified by perturbing the second pass |
| `compareLiveAnswer` compared neither temperature nor confidence | ds4 L, glm M | temperature compared exactly (it is the only signal that the same per-read fit was selected on a peaked answer), confidence within tolerance, `thought_closed` recorded and compared |
| The thinking case hardcoded 6.8, which is wrong against an uncovered checkpoint | ds4 M, glm L | the expectation comes from `model.CalibrationApplied` and `Calibration.TemperatureFor` |
| `liveTolerance = 1e-6` | ds4 L | 1e-9, with the sum-to-one check on its own constant |
| The recorder was found through the Python checkout's sibling layout | ds4 L, glm L | resolved from the package directory; a missing recorder is a failure |
| The recording's sha256 was never verified | glm L | verified before the answers are read |
| The gate checked only the default client's fingerprint | glm L | every recorder the test creates is checked |
| The wide-choice case asserted a key count | ds4 L | asserts the four key names |
| `--passes` edge cases, and drift over only the first pair | glm L, ds4 (drift) | `--passes` below 2 is refused; every consecutive pair is compared |
| Ambiguous tokenizer cache silently dropped the pin | glm L | reports the ambiguity instead of returning `""` |
| The live set only exercised the default read | ds4 L | added a `direct` policy question and a probability-view question |
| `README.md` said the live suite was unbuilt | ds4 M | updated, with the actual invocation and the parity gate's dependency |
| `PORT.md` said the port had no served-model measurements while reporting them, and the numbers had no date or fingerprint | ds4 M, glm L | corrected, with the date and `vllm-0.30.0-tp2-ca69195e` |

## Not changed, with the reason

- **`liveTolerance` at 1e-6 was "defensible" (glm) versus "nine orders too
  loose" (ds4).** ds4's argument is the stronger one and is what was applied:
  the server was measured bit-identical within a session, so the only expected
  difference is summation order, and 1e-6 let a delta a million times the
  observed noise pass. The measured worst delta over nine questions is 2.22e-16.
- **`self_delta` guard masking a port difference (scope question).** It cannot:
  it aborts rather than relaxes. The only way it could was the exit-3 path,
  which is fixed.
- **A same-shape restart mid-run is invisible (glm residual risk).** Nothing in
  the harness can rule it out; the fingerprint narrows it to "the same serving
  shape" and the failure message already says to re-run.
- **The 128-option single-block boundary is not in the gate set.** It is in the
  ported wide-choice test, and the offline `TestClientParity` covers the request
  shapes exactly.
- **The `chat_system_render` probe assertion is absent.** The image path is not
  ported; `PORT.md` records it.
