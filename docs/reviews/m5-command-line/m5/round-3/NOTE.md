# M5 round 3 — consolidation

Requested reviewers: `astra`, `ds4`, `glm`. `astra` failed for the third time
with `[Error: Our servers are currently overloaded. Please try again later.]`,
and `glm` failed this round too — both of its candidates exited with only a Node
`ExperimentalWarning` on stderr and an empty report. `ds4` produced the one real
report. Zero HIGH; one MEDIUM and four LOW, all of them record or test-text gaps
rather than behaviour.

This was the last round before `maxCycles`, so the findings below are fixed and
the review stops here.

## Fixed

- **MEDIUM — `PORT.md`'s DE-2 rewording paragraph overclaimed.** It ended
  "everything else the readout raises is Python's sentence", but two more
  messages are reworded: the generation-prompt-terminator message drops Python's
  "the image readout cannot confirm" (this port has no image readout), and
  `Softmax` says "no code logprobs" where the shared Python helper says "no
  letter logprobs". The terminator one is reachable through `doctor`'s
  `boundary_check` in both output forms and `explain`'s stderr, so it was the
  one failing run where the two command lines' text differed. Both are named
  now, and the paragraph says a boundary-check failure is outside the parity
  diff.
- **The round-2 NOTE claimed a `PORT.md` entry that was not there.** The edit
  meant to add the usage/`--help`/error-text sentence did not match the file and
  silently did nothing. The sentence is in `PORT.md` now, the round-2 NOTE
  carries a correction, and this is the second time in M5 that a
  `str.replace`-shaped edit that did not match went unnoticed — the lesson is
  the one the round-1/2 NOTES already record.
- Three of the five re-quoted readout messages had no whole-sentence assertion.
  `TestReadoutMessagesQuoteLikePython` pins the code-slot and prompt-token
  sentences, and `TestFallbackMessageQuotesLikePython` drives a real read whose
  top-k omits a code and whose `prompt_logprobs` fallback returns nothing, so
  the fallback sentence is pinned too. A revert to `%q` in any of them now
  fails.
- The two library warnings reword Python's advice to name the Go fields, and
  `PORT.md`'s warnings paragraph said only that they are collected as strings.
  It now says the tails differ (`Family`/`Calibration` against Python's
  `family=`/`calibration=`).
- `--family de1` and `--calibration de1` are accepted by the parser, because
  `argparse`'s `choices` has both families, and then always exit 2 here, because
  this port ships no DE-1 read and no DE-1 record. `PORT.md` recorded the
  library refusal but not this CLI-level consequence; it does now.

## Round accounting

Three rounds of three requested reviewers, `maxCycles` reached. Per reviewer:
`ds4` executed all three rounds; `glm` executed rounds 1 and 2 and failed in
round 3; `astra` failed all three with the same provider overload error. No
round reported a HIGH or CRITICAL finding, so no round was re-run on that
ground; rounds 2 and 3 were run because a required reviewer had failed to
execute, which is the other condition the review rules allow.
