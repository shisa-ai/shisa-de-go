# Review Report

## Summary

The milestone's two goals are met. The live gate now sends a real image question through both clients against the same endpoint, the capability is measured by a probe rather than assumed, the client parity fixture has an image case from both entry points that compares the client-level `Raw` block whole (the residual the two M7 reviewers raised), and `TestPortDocFixtureCounts` reads four stated counts out of `docs/PORT.md` prose and compares them with `testdata/*.meta.json`. The `api_url` fix is correct for every base-URL shape the Python client accepts, and I verified the four counts the new test covers, the regenerated client fixture's coverage (the strategies in `testdata/client_parity.json` still match what `docs/PORT.md` claims for each case), and that the probe image is the same 32×32 PNG the recorded question carries.

The two substantive problems are both about what a missing thing reads as. `serves_images()` collapses "the server refused an image request" with "the probe could not be measured" (transport error, 5xx, 429, or a probe-shape bug), and the Go side turns the second into a skip whose message asserts the deployment cannot serve images. Separately, `liveSystemRenderProblem` infers "this is a text question, nothing to compare" from an empty recorded verdict, so a recording that carries `null` for an image question is compared against nothing — the mirror of the case the author explicitly guarded against. The rest are documentation drift of exactly the kind this milestone exists to catch, in the one paragraph `TestPortDocFixtureCounts` does not read.

## Findings

### MEDIUM: The vision probe reports every failure as "the endpoint does not serve images"
- Severity: MEDIUM
- Confidence: HIGH
- Location: `scripts/live_parity.py:139-175` (`serves_images`, branches at 171 and 174), `live_test.go:528-535` (the skip)
- Problem: `serves_images()` returns `(False, reason)` for a transport error, any non-200, and any status other than 200 — 401, 429, a 502 from a proxy, a 500, or a 400 caused by the probe's own body being wrong. The Go test then skips `image-color` with `"the endpoint does not serve images: %s"`. So a transient rate limit or a probe-shape regression silently removes the image case from the gate while the message asserts a property of the deployment. The probe is the only thing that decides whether the milestone's one new live check runs, and nothing distinguishes "the server refused an image" from "I could not ask". The scope's own question — "can the skip be reached by a real port defect or a real request-shape bug rather than by a server that lacks vision" — is answerable yes for request-shape bugs in the probe and for any non-capability failure; the `api_url` fix closed the one instance found by hand, not the class.
- Recommendation: Split the two verdicts. Only a well-formed HTTP response that is a refusal (4xx) should mean "this deployment will not serve images"; a transport error, a 5xx, or a 429 should make the recorder exit non-zero (or record a third state such as `images: null` / `images_reason: "could not measure: ..."`) so the gate fails or reports inconclusive rather than skipping. On the Go side, either fail when `images_reason` is not a capability refusal, or word the skip so it does not claim a deployment property it did not establish ("the image question was not recorded: %s").

### MEDIUM: A recording with no `system_render` for an image question is compared against nothing
- Severity: MEDIUM
- Confidence: HIGH
- Location: `live_test.go:575-596` (`liveSystemRenderProblem`), `scripts/live_parity.py:298`
- Problem: `liveSystemRenderProblem` decides "nothing to compare" from `want == ""`, where `want` is the recorded `system_render`. `livePythonAnswer.SystemRender` is a plain `string`, so a recorded `null` — which the recorder writes whenever the Python client's `raw` has no `system_render` for an image question (`result.raw[head].get("system_render")`, and the Python client only writes the key when `read.system_render is not None`) — is indistinguishable from a text question, and the function returns no problem. The table test covers the case the author named (a recorded verdict against an answer carrying none: `"no verdict recorded at all"`), but not this direction. The Go side already knows which questions are image questions (`question.Image != ""`), so the information needed to close it is at the call site.
- Recommendation: Pass image-ness explicitly (`liveSystemRenderProblem(isImage bool, want string, raw map[string]any)`) or require a non-empty recorded verdict for every image question in the question set, so an image question whose verdict is missing on either side fails rather than passes. Add the case to the table test.

### LOW: `docs/PORT.md`'s live-gate paragraph still describes a nine-question set
- Severity: LOW
- Confidence: HIGH
- Location: `docs/PORT.md:851-860` (enumeration and "over nine questions"), `testdata/live_questions.json`
- Problem: The paragraph enumerates the fixed set as "a noul, a three-option choice, a choice with unnamed options, an ordered score, a 27-option choice, a 256-option choice, the same choice read under `direct`, the same score asked for the one-read probability view, and a choice whose gate is 1.0 so it thinks" and then says "The measured deltas are ... over nine questions against `vllm-0.30.0-tp2-ca69195e` on 2026-10-10". The file now holds ten questions and the gate ran ten (including `image-color`). The paragraph mentions the image case 35 lines later ("The Python suite's image case is the gate's `image-color` question"), but the enumeration and the count do not include it, so a reader who counts the list gets nine. This is the same drift `TestPortDocFixtureCounts` was added to stop, in a number the table does not cover. The paragraph also does not record the new behaviour: the capability probe, the `images`/`images_reason` fields, the skip, or that the gate now compares the image's `system_render`.
- Recommendation: Add the image question to the enumeration, correct the question count (and, if the sentence is meant to describe the current run, its date), and state the probe/skip and the `system_render` comparison. If the nine-question figure is meant as a historical measurement, say so explicitly rather than presenting it as the current set.

### LOW: The gate's image question never checks that the answer is the colour the image shows
- Severity: LOW
- Confidence: HIGH
- Location: `live_test.go:448-573`, `testdata/live_questions.json` (`image-color`), `docs/PORT.md:896-898`
- Problem: The image case is compared against the Python client's recording and nothing else. An endpoint that accepts the content part and ignores it would still return 200 to the probe, produce a `system_render` both clients agree on, and be compared answer-for-answer, so the gate would pass while nothing in the suite established that the image was read. The Python suite's own image test asserts `result["color"] == color` for a red and a blue image (`tests/test_live.py:126-148`); the port has no equivalent, by design ("so that the two clients are compared on it rather than each being checked alone"). The recorded evidence (red at 0.999998 on the hosted endpoint) is strong but nothing asserts it.
- Recommendation: Assert that the gate's `image-color` answer is `red` (and that its probability is high), or say in `docs/PORT.md` that the gate establishes client parity on an image request and not that the server used the image. Adding the assertion keeps the parity comparison and makes the coverage a check of the read.

### LOW: `AGENTS.md` attributes the live recorder's image-dropping to the client parity recorder
- Severity: LOW
- Confidence: HIGH
- Location: `AGENTS.md:309-316`
- Problem: The sentence is about the client parity fixture ("the two image cases in the client parity fixture") and continues "...and the recorder leaves the image questions out of the fixture for the same reason, recording `images` and `images_reason`". `scripts/gen_client_parity.py` has no capability probe and no drop path: against a text-only endpoint it raises `ReadoutHTTPError` and writes nothing, so the two image cases cannot be left out of that fixture. `scripts/live_parity.py` is the script that drops them, and its output (`testdata/live_python.json`) is a run artifact that `.gitignore` excludes, not a fixture.
- Recommendation: Name the script and the artifact ("`scripts/live_parity.py` leaves the image questions out of its recording"), and state separately that the client parity generator fails rather than skipping, so the difference between the two is not lost.

### LOW: `TestLiveRecorderExitClassification`'s doc comment is attached to the wrong function
- Severity: LOW
- Confidence: HIGH
- Location: `live_test.go:1037-1044` and `live_test.go:1103`
- Problem: The comment block for `TestLiveRecorderExitClassification` ("is the branch that decides whether a recorder run whose own passes disagreed...") sits directly above `TestLiveSystemRenderComparison`, and its last three lines are the new test's comment, so the block reads as one comment that starts describing one test and ends describing another. `TestLiveRecorderExitClassification` itself has no doc comment. `go vet` does not flag this and godoc attaches the whole block to the wrong function.
- Recommendation: Move the first four lines back above `TestLiveRecorderExitClassification` (or place the new test after it).

### LOW: "disagree on the chosen value" now also covers strategy, request count and the system-render verdict
- Severity: LOW
- Confidence: HIGH
- Location: `live_test.go:496-499`, `scripts/live_parity.py:419-421` and `:460`
- Problem: `unstable` is built from `same_value`, `same_strategy`, `same_requests` and `same_system_render`, but both messages say the passes "disagree on the chosen value". A run whose only instability is a flipped `system_render` (the new field) or a changed request count is reported with a wrong cause, in the one message an operator uses to decide whether the run means anything.
- Recommendation: Reword both to "the Python client's own passes disagree for %v" (or name the field that moved, which the drift report already carries).

### LOW: `AGENTS.md` overstates which fixture counts `TestPortDocFixtureCounts` checks
- Severity: LOW
- Confidence: HIGH
- Location: `AGENTS.md:180-185`, `readme_test.go:313-355`
- Problem: The rule says "The fixture counts in `docs/PORT.md` come from `testdata/*.meta.json`, and `TestPortDocFixtureCounts` fails when they disagree". The table covers four paragraphs — readout scenarios/exchanges, client cases/exchanges, image references, render prompts. The tokenizer's "1,056 strings, 48,392 token ids" and "83 `tokenizer.decode` outputs", the Python-JSON "264 more", the `repr` "32,509 sampled code points" and the command line's "eleven invocations" are all stated in `docs/PORT.md` and are not read by any test. The table's comment names only the command-line exclusion, so the remaining gap is invisible.
- Recommendation: Either narrow the wording to the counts the table covers, or add the tokenizer/JSON/repr counts to the table (their meta files already carry them: `strings`, `ids`, `decode_cases`, `cases`, and the repr count). Narrowing the wording is the cheaper honest fix.

## Things Checked

- `scripts/live_parity.py` in full: `endpoint()`/`model_id()` resolution, `api_url` against the Python client's own `(base_url).rstrip("/")` + `endswith("/v1")` normalization (correct for base with and without `/v1`, with and without a trailing slash; consistent with `httpx` joining), `serves_images` (the probe body is the same 32×32 PNG data URL as the recorded question, so the probe and the read carry identical bytes), `record` (policy/probability/image pass-through matching `DecisionModel.decide`'s keyword semantics), `drift`/`_movement` (worst pair over every consecutive pass, `same_system_render` included, `None` deltas for non-numeric values), the `--passes < 2` guard, the exit-3 path, the sha256-before-meta ordering, and the `images`/`images_reason` fields.
- `live_test.go`: the image question's `call()` (image on the call, no policy/probability), the gate's `asked` count arithmetic against `meta.Questions`, the `images`/`images_reason` consistency checks, the skip placement inside the subtest, the endpoint/model/readout-version/self-delta/unstable gates, the fingerprint check over every recorder (including the gate client's), `liveSystemRenderProblem` and its seven-case table, `TestLiveQuestionSetHasAnImageCase`, and the offline harness tests that run without `SHISA_DE_LIVE`.
- Image-read selection: `liveQuestion.Image` → `CallOptions.Image` → `PrepareImage` → `imageURL != ""` selects `ReadImage` for every head; `policy` and `probability` are ignored on that path in both clients (`client.go:1146-1160`, `shisa_de/client.py:718-724`), and calibration is off for an image in both (`calibrated = image_url == ""` / `calibrated = image_url is None`). The image question in the set carries neither override, so no interaction is exercised live, and none is broken.
- `TestPortDocFixtureCounts`: patterns against the actual prose, and the meta values (`readout 38/77`, `client 26/37`, `images 55`, `render 35`) — all four agree; the fail-on-no-match is `t.Fatalf` inside the subtest, which is the right behaviour for a check whose failure mode is silence; `strconv.Itoa(int(want))` on a `float64` from JSON is correct for integer counts.
- The regenerated client fixture: `testdata/client_parity.json` still covers what `docs/PORT.md` claims case by case (`decide-noul-sure` and `decide-noul-near-gate` both `repeat2`, `decide-choice-think` and `decide-choice-3-think` `repeat2-think`, `decide-choice-27-above-cap` `repeat2`, `decide-direct` `direct`, `decide-probability` `repeat2`, both image cases `direct` with `system_render: "differs"`), so re-recording did not quietly drop a branch. The image `Raw` blocks carry `prompt: null`, the full `top_logprobs`, `messages` with the data URL and `system_render`, and `compareDecision` compares `raw`, `values`, `usage`, `meta` and `wire` whole, which is the coverage the M7 residual asked for. `client_parity.meta.json` has one endpoint, one model, one fingerprint (`vllm-0.30.0-tp2-ca69195e`), one date (2026-10-12), one tokenizer revision and a digest; `AGENTS.md` now records that the numbers moved between sessions under an unchanged fingerprint, which is the honest description. Re-recording the whole file was the right call: hand-splicing two cases into the old file would give one provenance covering two sessions, and a Go regression is still caught because the replay compares the Go answer against the Python answer for the recorded responses regardless of which session they came from.
- `docs/PORT.md`, `AGENTS.md`, `README.md`, `live_test.go` for statements implying the image read has no live coverage or that the endpoint cannot serve images: none left. The README's server-requirements bullet ("An image question also needs `/v1/chat/completions` with vision enabled") is a deployment requirement, not a claim about this endpoint, and the live-gate paragraph in the README does not mention the image question (it is silent, not stale).
- `scripts/test_live_parity.py`: the new `ApiUrlCase` (four cases, including the default endpoint), the question-set tests (`test_the_set_covers_the_image_read` decodes the data URL and checks the PNG signature and the two-option shape; `test_the_image_probe_is_a_png`), and the drift cases including `same_system_render`.
- `testdata/live_python.json`/`.meta.json` are gitignored run artifacts; the stale local copy (nine questions, no `images` key, 2026-10-10) is not read by any test and is not a repository state issue.
- I did not run any test or build command; the prompt forbids broad commands and no shell tool is configured. Everything above is read from the tree.

## Residual Risks

- The gate proves both clients send the same image request and read the same answer; it does not prove the server used the image. A deployment that accepts and discards the content part would pass (see the LOW finding above), and the probe cannot distinguish that case either.
- The probe spends one request that `meta.requests` does not count (the `Recorder` counts only model requests), and its own `system_fingerprint` is not checked. Nothing depends on either today.
- The four fixture counts are now checked; the tokenizer, Python-JSON, `repr` and command-line counts stated in `docs/PORT.md` are still hand-maintained.
- The regenerated client fixture's pre-existing numbers were not compared with the previous recording, so a *server-side* change between 2026-10-10 and 2026-10-12 that altered which branch a case exercises would be invisible unless it changed a strategy (it did not, per the check above). The Go side's answers are still checked against the new recording.
- `liveSystemRenderProblem` compares one string; the rest of the image `Raw` block is only compared by the offline replay, so a Go-side change to `imageRaw` that the fixture's two image cases do not exercise stays invisible to the live gate.
- The live suite is non-deterministic by design: the recorded deltas (2.2e-19 to 5.6e-16 over ten questions) are one session's, and the doc's figures are from an earlier session and set.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "Vision probe reports every failure as 'the endpoint does not serve images'",
      "location": "scripts/live_parity.py:139-175; live_test.go:528-535",
      "recommendation": "Treat only a well-formed 4xx refusal as a capability verdict; make a transport error, 5xx or 429 fail the recorder (or record a third 'could not measure' state) so the gate fails or reports inconclusive instead of skipping, and stop the skip message asserting a deployment property it did not establish."
    },
    {
      "severity": "MEDIUM",
      "title": "A recording with no system_render for an image question is compared against nothing",
      "location": "live_test.go:575-596; scripts/live_parity.py:298",
      "recommendation": "Decide 'nothing to compare' from whether the question is an image question (question.Image != \"\") rather than from an empty recorded verdict, so a null verdict for an image question fails, and add that case to the table test."
    },
    {
      "severity": "LOW",
      "title": "docs/PORT.md's live-gate paragraph still describes a nine-question set",
      "location": "docs/PORT.md:851-860",
      "recommendation": "Add the image question to the enumeration, correct the question count and date (or mark the figure as a historical measurement), and record the capability probe, the images/images_reason fields, the skip and the system_render comparison."
    },
    {
      "severity": "LOW",
      "title": "The gate's image question never checks that the answer is the colour the image shows",
      "location": "live_test.go:448-573; testdata/live_questions.json (image-color); docs/PORT.md:896-898",
      "recommendation": "Assert that the image-color answer is red with a high probability, keeping the parity comparison, or state in docs/PORT.md that the gate establishes client parity on an image request and not that the server used the image."
    },
    {
      "severity": "LOW",
      "title": "AGENTS.md attributes the live recorder's image-dropping to the client parity recorder",
      "location": "AGENTS.md:309-316",
      "recommendation": "Name scripts/live_parity.py and its recording, and say that scripts/gen_client_parity.py fails rather than dropping the image cases, so the two recorders' different behaviour is not conflated."
    },
    {
      "severity": "LOW",
      "title": "TestLiveRecorderExitClassification's doc comment is attached to TestLiveSystemRenderComparison",
      "location": "live_test.go:1037-1044, live_test.go:1103",
      "recommendation": "Move the exit-classification comment back above its function, leaving the new test's comment with the new test."
    },
    {
      "severity": "LOW",
      "title": "'disagree on the chosen value' now also covers strategy, request count and the system-render verdict",
      "location": "live_test.go:496-499; scripts/live_parity.py:419-421, 460",
      "recommendation": "Reword both messages to 'the Python client's own passes disagree for %v', or name the field that moved."
    },
    {
      "severity": "LOW",
      "title": "AGENTS.md overstates which fixture counts TestPortDocFixtureCounts checks",
      "location": "AGENTS.md:180-185; readme_test.go:313-355",
      "recommendation": "Narrow the wording to the four paragraphs the table covers, or extend the table with the tokenizer, Python-JSON, repr and command-line counts their meta files already carry."
    }
  ]
}
END_REVIEW_JSON
