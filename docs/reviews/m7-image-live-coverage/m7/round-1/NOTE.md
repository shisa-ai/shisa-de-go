# M7 round 1 — disposition

All three requested reviewers produced a report. `ds4`
(`shisa-deepseek-v4.1-flash-jouzu-cli`) and `glm` (`shisa-glm-5.3-jouzu-cli`) ran
on their primaries; `astra`'s primary (`shisa-gpt-6-astra-jouzu-cli`) exited 143
with `timed out: true`, and the harness covered it with
`openai-codex-gpt-6-astra-jouzu-cli`. Both files are beside this note: the failed
attempt's report holds only the harness's error block, and the review is the
`openai-codex-` one. The summary, `results.json` and the manifest are copied
here; the cycle's prompts, stderr and the failover summaries stayed in the Python
checkout the session was working in, which is where the harness wrote them, so
the paths inside them name that tree.

Round 1: 0 HIGH, 0 CRITICAL, 0 uncovered reviewer failures. No rerun, which is
the rule for a round that found nothing above MEDIUM.

The three converged unusually tightly. All three independently found the same
two MEDIUMs, and both are the same mistake in two places: a missing thing read as
a verdict. `ds4` and `glm` also both found the live-gate paragraph's stale
nine-question set, and all three found a gap in the doc-count check.

## Fixed

| Finding | Reporters | What changed |
| --- | --- | --- |
| `serves_images()` reported a transport error, a 401/403/429, a 5xx and a probe-shape 4xx as "the endpoint does not serve images", so any of them silently dropped the gate's image coverage behind a message that read as a deployment fact | all three M | `probe_images()` returns `served`/`refused`/`unknown`; only a sub-500 answer that names images, vision or modality is a refusal, and anything else exits 4 without writing a fixture, so the run fails rather than recording an unmeasured capability as an absent one. Nine offline tests over an injected client |
| `liveSystemRenderProblem` inferred "this is a text question" from an empty recorded verdict, so a recording that carried `null` for an image question was compared against nothing — the mirror of the case its table test pinned | all three M | image-ness comes from `question.Image != ""`, and an image question must carry `string`/`differs`/`unverified` on both sides. Three table cases added (a text answer that recorded a verdict, a null verdict, an unreadable one) |
| `docs/PORT.md`'s live-gate paragraph enumerated nine questions and cited only the 2026-10-10 measurement, so the ten-question run this milestone produced was in no durable document, and `AGENTS.md`'s `image-color` claim had no date or fingerprint beside it | ds4 M, glm M | the enumeration names the image question, the paragraph records the ten-question range (2.2e-19 to 5.6e-16 against `vllm-0.30.0-tp2-ca69195e` on 2026-10-12) beside the nine-question one, and `AGENTS.md`'s measurement sentence and image-coverage rule move with it |
| The gate's image question was only ever compared against the Python recording, so an endpoint that accepted the content part and ignored it would have passed | ds4 L | `TestLiveImageReadUsesTheImage` asks a red and a blue image the same question and requires the answer to follow, which is the Python suite's own image test. It is the check a two-client comparison cannot make |
| `AGENTS.md` attributed the live recorder's image-dropping to the client parity recorder, which has no probe and fails instead of dropping | ds4 L | both are named, with the difference stated: `live_parity.py` skips on a measured refusal, `gen_client_parity.py` fails |
| `TestLiveRecorderExitClassification`'s doc comment sat above `TestLiveSystemRenderComparison`, so godoc attached it to the wrong function | ds4 L | moved back, and the exit table gained 4 |
| "the passes disagree on the chosen value" was also the message for a flipped `system_render` or a changed request count | ds4 L, glm L | both messages now say "disagree for %v" |
| `TestPortDocFixtureCounts` guarded four counts; the tokenizer's strings/ids/decode outputs, the Python-JSON figure, the `repr` figure and the command line's were unguarded, and the render count is stated twice with only one read | ds4 L, glm L, astra L | a row per claim rather than per paragraph: the three tokenizer counts are read (commas dropped on both sides) and the second render statement is read. Both new rows shown to fail against a mutated `docs/PORT.md` |

## Not changed, with the reason

- **Building the probe from the client's own machinery (glm's alternative).**
  `Readout.read_image` needs the tokenizer, so the probe would load one to spend
  one request, and the recorder already loads one per model. The classification
  is what was wrong, not the probe's shape: a 4xx that does not name images is
  `unknown` and fails the run, so a shape regression cannot pass as a verdict.
- **Recording a third `images` state (ds4's alternative).** `images: null` would
  need a third branch on the Go side and a rule for what a caller does with it.
  Exiting 4 leaves the fixture unwritten, which is the same information with no
  new schema.
- **Asserting the colour inside the parity gate (ds4's first alternative).** The
  gate's claim is agreement between two clients; folding a correctness assertion
  into it would make a parity failure and a read failure indistinguishable in the
  same `t.Run`. The separate test keeps both claims and each one's failure means
  one thing.
- **The client fixture's 24 pre-existing cases were not compared against the
  previous recording (ds4 residual, also listed as a scope non-goal).** The
  checkpoint is non-deterministic across sessions, so the old numbers are not a
  reference; the replay against the new recording is the check that matters.
- **The probe spends one request `meta.requests` does not count (ds4 residual).**
  `Recorder` counts model requests and the probe is not one. Nothing depends on
  the figure, and the probe is one request per run.
