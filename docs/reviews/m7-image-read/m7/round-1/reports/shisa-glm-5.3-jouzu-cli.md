# Review Report

## Summary

Reviewed the M7 image read in the Go port (`/home/morpheus/data/dev/shisa/shisa-de-go`, working tree) against the Python reference (`shisa_de/images.py`, `shisa_de/readout.py`, `shisa_de/client.py`, `shisa_de/cli.py`) and `docs/READOUT-DE2.md` section 6.

The read itself is faithful. `ReadImage` matches `read_image` on the message order (image part before text), the request body (`logprobs`, `top_logprobs`, `prompt_logprobs: 0`, `chat_template_kwargs`), the 26-option limit, the absence of a letter fallback, and the refusal arithmetic word for word. `checkImageBoundary` requires the terminator by id *and* decoding, so a response not read at the boundary cannot pass it; the by-id resolution of an ambiguous position is the recorded difference and is strictly more deterministic than Python's insertion-order first key. `systemRender`'s arithmetic is a correct transcription of Python's (the explicit `min` matches Python's forgiving slice; the `want[:-1] != through_system` check is reproduced, including the truncation cases). The client branch prepares the image before validating anything, defaults `Calibrated` off with an image, refuses a wide choice rather than overflowing, and builds the flat `Raw` shape. The CLI matches `cli.py`: `--image`/`--image-top-logprobs` on `ask` alone, the `getattr(..., 20)` default, the `{} if args.image and args.state is None` rule, and `doctor`'s `chat system` line placement. The fixtures are load-bearing: the images fixture replays 51 verdicts with exact error text plus a provenance check that can be shown to fail, and the readout fixture records the missing-code, too-many-options, malformed-top, `differs` and `unverified` image scenarios; the boundary refusals are pinned by Go-only tests instead (acceptable, since the boundary message wording is the one recorded wording difference anyway).

The findings are one caller-visible raw-shape divergence on the debug image path, and a cluster of stale or inaccurate documentation/comments left over from the "no image path" era, plus one path-handling divergence in `pythonPath`.

## Findings

### MEDIUM: The image `Raw` block writes an empty prompt where Python writes None
- Severity: MEDIUM
- Confidence: HIGH
- Location: client.go:1252 (`imageRaw`)
- Problem: With `Debug: true`, `imageRaw` sets `raw["prompt"] = read.Prompt`. An image read never sets `CodeRead.Prompt`, so this is always `""`. Python's `read_image` builds a `LetterRead` with no `prompt`, and `_run`'s image branch writes `decision.raw[head]["prompt"] = read.prompt`, which is `None`. So a caller comparing debug output reads `"prompt": ""` here and `"prompt": null` in Python. This also contradicts the recorded difference PORT.md makes for exactly this field: "The raw block writes nil for it, so a caller sees Python's None rather than empty prompt" — `PolicyRead.Raw` honours that (policy.go:345-349 writes `nil` for an empty prompt), but `imageRaw` does not. No test covers it: the client image tests do not run with debug, and the client parity fixture has no image cases, so the replay gate cannot catch it.
- Recommendation: In `imageRaw`'s debug block, write `nil` for the prompt (an image read has no local prompt, ever), mirroring `PolicyRead.Raw`, and add a debug-mode assertion to the client image tests. Alternatively, record the divergence in PORT.md — but the existing PORT.md paragraph already promises the opposite, so fixing the code is the smaller coherent change.

### LOW: client.go's file header still says the image path is not ported
- Severity: LOW
- Confidence: HIGH
- Location: client.go:29-33
- Problem: The package comment says "The Python client also reads images ... Neither is ported: this client's readout has no image path ... so `Classify` and `Decide` have no image argument". M7 added `CallOptions.Image`, `ImageTopLogprobs` and the image branch of `run` in this same file. A reader of the header is told the opposite of what the file below it does.
- Recommendation: Update the header to state that the image read is ported and that DE-1 overflow is the one thing still absent.

### LOW: Docs name a nonexistent `Answer.SystemRender` field
- Severity: LOW
- Confidence: HIGH
- Location: README.md:208, docs/PORT.md:45, docs/PORT.md:63
- Problem: README and PORT.md say the system-render verdict is recorded in `Answer.SystemRender`. `Answer` has no such field; the verdict lives in `CodeRead.SystemRender` and reaches callers as `Decision.Raw[head]["system_render"]` — which is what `docs/READOUT-DE2.md` section 6 and the Python contract say. The documentation-rot test only checks `shisade.X` inside Go code fences, so a prose reference to a wrong field passes it.
- Recommendation: Change both docs to `CodeRead.SystemRender` / `Raw[head]["system_render"]`, and consider having `readme_test` also resolve prose references of the shape `` `X.Y` `` against exported fields.

### LOW: `firstChatChoice`'s message differs from Python's and its branch is untested, against PORT.md's claim
- Severity: LOW
- Confidence: HIGH
- Location: endpoint.go:226
- Problem: Go raises "the chat response carries no choices for the image read"; Python's `_first_choice` raises "the chat response carries no choices". PORT.md claims the boundary refusal "is the one image-path message that differs in wording" and "Everything else there is Python's text verbatim", which this message disproves. No test exercises a chat response with no choices at all (grep finds no test asserting this message), so neither the wording nor the branch is pinned anywhere.
- Recommendation: Either match Python's message verbatim (simplest, and it restores PORT.md's claim), or record the difference and add a test for the empty-`choices` chat response.

### LOW: `pythonPath` expands a rooted `~` where pathlib does not, and builds a `//home/...` path
- Severity: LOW
- Confidence: MEDIUM
- Location: images.go:218
- Problem: Go expands a first component starting with `~` unconditionally. Python 3.14's `Path.expanduser` (verified in `/usr/lib/python3.14/pathlib/__init__.py`) guards with `not (self.drive or self.root)`, so `/~/photo.png` and `//~/photo.png` are never expanded. Go turns `/~/photo.png` into `"/" + "/home/<user>/photo.png"` — a malformed `//home/...` path (its own test pins `//` as a distinct POSIX root). On Linux the OS collapses the slashes, so Go finds `$HOME/photo.png` and sends the image where Python refuses with "image file not found: /~/photo.png". The verdict and the error text both diverge; the fixture has no rooted-`~` case and `TestPythonPathMatchesPathlib` has none either. Not recorded in PORT.md.
- Recommendation: Only expand when `drive` and `root` are both empty, matching pathlib; add `/~/x.png` and `//~/x.png` cases to `TestPythonPathMatchesPathlib` and, if the fixture generator can express them, to `images_parity.json`; mention the guard in PORT.md's path paragraph.

### LOW: Stale failure text in the CLI doctor test
- Severity: LOW
- Confidence: HIGH
- Location: cmd/shisa-de/main_test.go:523-524
- Problem: The assertion (a doctor run without `--probe` must not print a chat-system line) is still correct, but the failure message says "this port has no chat probe" — stale since M7 added `ProbeChatRender` and the `chat system` line.
- Recommendation: Reword to say the line only appears under `--probe`.

### LOW: `ReadImage`'s result has no counterpart to `LetterRead.elapsed_ms`
- Severity: LOW
- Confidence: HIGH
- Location: readout.go:1099-1112 (`ReadImage` result construction)
- Problem: Python's `read_image` sets `elapsed_ms` on the returned `LetterRead`; the Go `CodeRead` an image read returns has no elapsed field, and the difference is not in PORT.md. Provenance only: nothing in `Raw`, the fixtures (`read_record` does not record it), or the client reads it — which is also why nothing catches it.
- Recommendation: Either add `CodeRead.ElapsedMS` filled by `ReadImage` (the policy layer already has `ElapsedMS` precedent), or record the omission in PORT.md. Recording it is enough given nothing consumes the value.

### LOW: `ProbeChatRender`'s QuestionError handling is documented in code but not in PORT.md
- Severity: LOW
- Confidence: HIGH
- Location: readout.go:1148-1160
- Problem: Python's `probe_chat_render` lets a `QuestionError` from `_payload` propagate and converts only `ReadoutError` to `unavailable: ...`. Go returns the `unavailable:` prefix for both, because its signature is `(string)` with no error channel. The reasoning is in the code comment and the case cannot arise from `Health` (which passes a Noul), but the repository's rule is that a difference not in PORT.md is an accident, and PORT.md says nothing about the probe's error surface.
- Recommendation: Add one sentence to PORT.md's `doctor --probe` / image paragraph recording this.

## Things Checked

- `ReadImage` vs `read_image`: validation order (URL, top-logprobs guard, payload, slots, top-k arithmetic), request body fields and their presence/absence (`logprobs`/`top_logprobs`/`prompt_logprobs: 0` always sent by the read, never the probe; `chat_template_kwargs` always present; no `stop_token_ids`), message construction (`image_url` part then text part, system line substitution for a configured prompt), no fallback, missing-letter refusal text, `Sampled`/`Messages`/`TopLogprobs` debug handling, `Requests`/`PromptTokens` accounting.
- `checkImageBoundary` vs `_check_image_boundary`: missing/empty/null `prompt_logprobs` refusal, id-plus-decoding requirement, `boundaryEntry`'s by-id preference, lowest-id fallback, and unparsable-key reporting; whether a response read at another position could pass (it cannot without the terminator's own id and decoding).
- `systemRender` vs `_system_render`: the `min(len(throughSystem)+1, ...)` slice, the length and equality guards (including the truncation cases where Python's slice is forgiving), single-key-per-entry comparison, first position skipped, and the three verdicts; the "string" verdict only when every compared position echoes the local render's token ids.
- `PrepareImage`/`ValidateImageURL` vs `images.py`: sniffing (PNG-under-`.jpg` accepted, fake PNG refused, short RIFF refused), path cleaning (`//` root, `///` collapse, `..` kept, trailing separator dropped), suffix rule vs Python 3.14's `name.lstrip('.').rfind('.')` (checked against the installed interpreter source), strip semantics (`trimPythonSpace`), every error message against the 51-case fixture, the provenance check and its can-fail test, and `~`/`~user` expansion (found the rooted-`~` divergence above).
- Client wiring vs `DecisionModel._run`: image prepared before validation, `Calibrated` defaulting off with an image and explicit override honoured, the 26-option image refusal before any request, `answerFromRead(..., "image")` with the shipped record having no image fit (temperature stays 1.0), flat `Raw` shape and its key set, usage/meta blocks including `input_type`, and `Health`'s `chat_system_render` set whenever `probe` runs regardless of the read probe's outcome.
- CLI vs `cli.py`: flag registration on `ask` alone, the `getattr(args, "image_top_logprobs", 20)` default, the `{} if args.image and args.state is None` state rule (both halves tested), `doctor`'s `chat system` line placement and its absence without `--probe`.
- Fixtures: `readout_parity.json`'s ten image scenarios plus three probe scenarios (35 total, matching the meta), the stub's chat endpoint (system-space simulation, short prompt, dropped code, non-option filler, malformed entry, omitted message), `images_parity.json` coverage of the refusals a caller actually hits, and both metadata/staleness checks.
- Docs: README "Classify images", the probability/calibration table, the serving notes, and PORT.md's image paragraphs — against the code (found the `Answer.SystemRender` naming and the "one message differs" claim problems above).

## Residual Risks

- The client-level image path has no recorded Python fixture: `client_parity.json` predates images, so `imageRaw`, the image usage/meta accounting and the calibrated-default rule are pinned only by Go-only stub tests. The live image check is a manual case by design (scope non-goal), but a regeneration of the client fixture with an image case would close the last unrecorded surface — and would have caught the MEDIUM finding.
- The chat-response failure modes a server can actually produce on the image path (no choices, a bad boundary) are pinned by hand-written expectations rather than a Python recording; the boundary message is a recorded wording difference, but the no-choices message is currently neither recorded nor tested (finding above).
- The `pythonPath` emulation is spot-checked, not exhaustive; the rooted-`~` case shows the class of divergence it can hold. `~nonexistentuser` maps Python's `RuntimeError` text onto an `ImageError`, which is documented in code but also absent from PORT.md's list of differences.
- `git log 877cd6c..HEAD` could not be run (no shell tool in this session); the review inspected the working tree as the prompt directs, so anything living only in an uncommitted-but-unreferenced state was not diffed.
- Validation was not re-run; the scope's recorded runs (gofmt, vet, full test suite) are taken as given and the findings are from reading, not execution.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "imageRaw writes raw['prompt'] as \"\" under debug where Python's image raw carries None",
      "location": "client.go:1252",
      "recommendation": "Write nil for the prompt in imageRaw's debug block, mirroring PolicyRead.Raw, and assert it in a debug-mode client image test"
    },
    {
      "severity": "LOW",
      "title": "client.go file header still says the image path is not ported",
      "location": "client.go:29-33",
      "recommendation": "Update the package comment: the image read is ported; only DE-1 overflow remains absent"
    },
    {
      "severity": "LOW",
      "title": "README and PORT.md name a nonexistent Answer.SystemRender field",
      "location": "README.md:208",
      "recommendation": "Say CodeRead.SystemRender / Decision.Raw[head]['system_render'] instead"
    },
    {
      "severity": "LOW",
      "title": "firstChatChoice's no-choices message differs from Python's, is untested, and contradicts PORT.md's 'one message differs' claim",
      "location": "endpoint.go:226",
      "recommendation": "Match Python's 'the chat response carries no choices' verbatim (or record the difference) and add a test for an empty-choices chat response"
    },
    {
      "severity": "LOW",
      "title": "pythonPath expands a rooted ~ where pathlib does not, producing a //home/... path",
      "location": "images.go:218",
      "recommendation": "Only expand when drive and root are empty; add /~/x.png and //~/x.png test cases and record the guard in PORT.md"
    },
    {
      "severity": "LOW",
      "title": "Stale 'this port has no chat probe' failure text in the doctor CLI test",
      "location": "cmd/shisa-de/main_test.go:523",
      "recommendation": "Reword the failure message; the chat probe exists under --probe"
    },
    {
      "severity": "LOW",
      "title": "ReadImage's result has no counterpart to LetterRead.elapsed_ms",
      "location": "readout.go:1099",
      "recommendation": "Add CodeRead.ElapsedMS filled by ReadImage, or record the omission in PORT.md"
    },
    {
      "severity": "LOW",
      "title": "ProbeChatRender's QuestionError handling is a code-documented difference absent from PORT.md",
      "location": "readout.go:1148",
      "recommendation": "Record in PORT.md that the probe prefixes 'unavailable: ' for failures Python would raise as QuestionError"
    }
  ]
}
END_REVIEW_JSON
