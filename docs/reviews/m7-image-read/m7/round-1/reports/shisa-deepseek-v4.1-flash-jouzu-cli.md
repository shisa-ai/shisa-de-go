# Review Report

## Summary

Scope reviewed: M7 (`877cd6c..HEAD`, six commits) — `images.go`, `Readout.ReadImage`, `AnswerPrefixID`, `checkImageBoundary`/`boundaryEntry`, `systemRender`/`ProbeChatRender`, the chat request/response shapes in `endpoint.go`, the client's image branch and `imageRaw`, `Health`'s `chat_system_render`, the CLI flags, the fixtures, and the rewritten docs. I read the M7 range against the Python reference (`shisa_de/images.py`, `Readout.read_image`, `_check_image_boundary`, `_system_render`, `DecisionModel._run`'s image branch, `cli.py`), and re-checked the pre-existing files M7 touches (`live_test.go`, `client.go`'s package doc, `readout_parity.meta.json`).

The read itself is faithful. I could not find a defect in `ReadImage`'s request body, message order, refusal arithmetic, or the no-fallback rule; `PrepareImage`/`ValidateImageURL` match `images.py` including the media-type sniffing, the error text and the pathlib semantics; the client's image branch does what `_run` does (prepare before validating, `Calibrated` off by default, wide choice refused at 26, flat `Raw` with `system_render`); and the two recorded differences are real and correctly described.

Every finding is in the layer around the read, and they fall into two groups. One is a live test that now asserts the opposite of the code M7 added, so the opt-in live suite fails on every run. The others are the residue of the milestone: three files still say this port has no image path, and two documentation claims name `Answer.SystemRender`, which exists on neither client. The two low-fidelity items (the image raw block's value shapes, one boundary message) are the same class of bug the M4 round-2 review already fixed for the text path.

## Findings

### MEDIUM: the live suite's probe test now asserts the absence of `chat_system_render`, which `Health` always sets

- Severity: MEDIUM
- Confidence: HIGH
- Location: `live_test.go:692-704` (the test), `live_test.go:38-40` and `live_test.go:663-665` (comments)
- Problem: `TestLiveHealthProbeServesARead` calls `model.Health(ctx, true)` and then fails if the report carries `chat_system_render`:

  ```go
  if _, ok := report["chat_system_render"]; ok {
      t.Error("the report carries the image path's chat render probe")
  }
  ```

  `Health` sets that key unconditionally whenever `probe` is true (`client.go:860-864`), which is the Python client's rule (`client.py:520`). So this test cannot pass against any working endpoint; it fails by construction, not flakily. M7 updated the offline CLI test (`cmd/shisa-de/main_test.go:1398` asserts `"string"`) and added `client_test.go:900`, but left the live test asserting the old behaviour. Python's own live test asserts the opposite of the Go one (`tests/test_live.py:159`: `report["chat_system_render"] in {"string", "differs"}`). The two comments above it are stale in the same way: "the DE-1 and image cases are absent because this port has neither read" and "this port has no DE-1 read and no image path".
- Why this is in scope: the scope's non-goal covers the live suite's *image case* being a manual check. This is not that: it is an existing live assertion that M7 invalidated, in a file the scope lists nowhere but `AGENTS.md` lists as a verification scope.
- Recommendation: replace the absence assertion with the Python assertion (`ChatSystemString`, `ChatSystemDiffers` or `ChatSystemUnverified` are all valid against a real server), and fix the two comments. Then run `SHISA_DE_LIVE=1 go test . -run Live` once to confirm the gate is green again — this is the one finding I would fix before trusting any other live result.

### MEDIUM: `README.md` and `docs/PORT.md` name `Answer.SystemRender`, a field neither client has

- Severity: MEDIUM
- Confidence: HIGH
- Location: `README.md:208`, `docs/PORT.md:45` and `docs/PORT.md:63`
- Problem: Both docs say the system-render verdict is recorded on the answer: "This port records the difference in `Answer.SystemRender` and answers anyway" and "`Answer.SystemRender` records what the server did with the system turn (`string`, `differs` or `unverified`)". There is no `Answer.SystemRender`:
  - `CodeRead.SystemRender` is the field, on the *read* (`readout.go:211-214`).
  - The client records it in the per-head raw block: `Decision.Raw[head]["system_render"]` (`client.go:1258-1260`), exactly as the Python client does (`client.py:802-803`). Python's `Answer` dataclass has no `system_render` either (`client.py:51-71`).
  
  The one client-level test that touches it asserts the raw key (`client_test.go:1479-1484`), not an answer field, so the code and the test agree and only the prose is wrong. `readme_test.go`'s rot check cannot catch this: its pattern is `\bshisade\.…`, and the prose names the field without the package prefix. Note that `scope.md` repeats the same wrong name, so the claim has already propagated once.
- Recommendation: name the read's field (`shisade.CodeRead.SystemRender`, which the rot check then verifies) and the raw key (`Decision.Raw[head]["system_render"]`) in both files.

### MEDIUM: `client.go`'s package doc still says this client has no image path

- Severity: MEDIUM
- Confidence: HIGH
- Location: `client.go:29-32`; also `live_test.go:38-40`, `live_test.go:663-665`, `cmd/shisa-de/main_test.go:522-525`
- Problem: The package-level doc comment — the first thing on pkg.go.dev for the friendly layer — reads:

  > The Python client also reads images and, on DE-1, overflows a wide text choice through balanced chunks. Neither is ported: this client's readout has no image path and no DE-1 read, so `Classify` and `Decide` have no image argument and a DE-1 checkpoint is refused outright.

  All but the last clause is now false: `CallOptions.Image` (`client.go:295-299`), the image branch of `run` (`client.go:1014-1017`, `1138-1141`) and the CLI's `--image` all exist. The same stale claim appears in `live_test.go` (twice) and, in comment form, in `cmd/shisa-de/main_test.go:522-525` ("this port has no chat probe"). The `main_test.go` assertion itself is still correct — a report with no `--probe` has no chat-system line, as in Python — so only its comment is wrong.
- Recommendation: rewrite `client.go`'s paragraph to the same shape the README now uses (DE-1 and overflow are the gaps), and correct the three comments.

### LOW: `imageRaw` records `prompt` as `""` and `top_logprobs` as `map[string]*float64`, contradicting the policy path and the port's own rule

- Severity: LOW
- Confidence: HIGH
- Location: `client.go:1252-1253`; contrast `policy.go:346` and `policy.go:350`
- Problem: For a debug image read the flat raw block is built as:

  ```go
  raw["prompt"] = read.Prompt          // "" for an image read
  raw["top_logprobs"] = read.TopLogprobs  // map[string]*float64
  ```

  Python records `read.prompt` (`None` for an image read) and a dict of numbers, and the text path already handles both correctly: `policy.go:346-350` writes `nil` for an empty prompt and `optionalFloatMap(...)` for the top-k. So the image path is the only place that emits `""` where Python emits `null` (visible through JSON, i.e. to the parity gate, if an image debug case ever reaches the client fixture) and the only place that hands a caller a map of pointers — the exact regression M4 round-2 fixed, whose fix `TestCallerVisibleMapsHoldValuesNotPointers` (`client_test.go:942-1002`) cannot see because it only walks text decisions. `TestClassifyImageIsOneDirectRead` checks the raw block's keys only, and is not a debug read, so neither shape is covered.
- Recommendation: use `nil` and `optionalFloatMap(read.TopLogprobs)` in `imageRaw`, and add one debug image decision to the pointer walker. The walker fails on both values if reverted, which is what makes the pair load-bearing.

### LOW: `docs/PORT.md`'s readout fixture counts are stale

- Severity: LOW
- Confidence: HIGH
- Location: `docs/PORT.md:644`
- Problem: "**Readout.** 22 scenarios, 62 recorded exchanges" — `testdata/readout_parity.meta.json` records `"scenarios": 35, "exchanges": 74`, and `readout_test.go:190-196` compares the fixture against those numbers, so the metadata is current and the prose is not. The "scenarios cover:" list that follows also omits the 11 image scenarios and 3 chat-probe scenarios M7 added. The neighbouring client figure ("24 cases, 35 recorded exchanges") is still accurate (`client_parity.meta.json`), so this is an isolated miss rather than a pattern.
- Recommendation: update the counts and add the image read and the chat probe to the cover list, in the paragraph that already explains what each exchange records.

### LOW: the image boundary message is not Python's text when `decoded_token` is missing or the key is not an id

- Severity: LOW
- Confidence: HIGH for the code difference, MEDIUM for reachability of the second case
- Location: `readout.go:1171-1178` (`decoded := ""`), `readout.go:1196-1203` (`boundaryEntry`'s non-id fallback)
- Problem: `docs/PORT.md:68` says the boundary messages are "Python's text verbatim". Two cases differ:
  - A `prompt_logprobs` entry without `decoded_token` prints `the last prompt token is '' (id 7)` here and `the last prompt token is None (id 7)` in Python (`readout.py:731-735`, where `decoded` is `None`). The verdict is the same in both clients; only the text differs, and no Go test covers an entry without `decoded_token` (every stub in `readout_test.go` sets it, e.g. `chatBody`).
  - A position keyed by anything that is not a token id prints `(id 0)`, because `boundaryEntry` returns `best = 0` for that key (`readout.go:1203`), where Python prints the key itself (`str(token_id)` from `next(iter(last.items()))`). The refusal is the same; the id in the message is not.
- Recommendation: make `decoded` a `*string` (or a `pythonQuote` of an explicit `None`) and carry the raw key alongside the parsed id so the message prints what the server sent. This matters for the reason the M5 round-3 review gave: a maintainer comparing error text across the two clients should be able to trust "verbatim".

### LOW: `ask --image ""` is silently a text read here and an error in Python

- Severity: LOW
- Confidence: HIGH
- Location: `cmd/shisa-de/main.go:262-263` with `client.go:1017`; Python `cli.py:154-156`, `images.py:112-114`
- Problem: Go's `CallOptions.Image` is documented as "Empty means no image" and the image branch is guarded by `if options.Image != ""`, so `--image ""` runs the text read. Python's CLI passes `args.image` straight through, and `_run` calls `prepare_image("")` whenever `image is not None`, which raises `ImageError("image is empty")`. A caller who types `--image ""` gets an answer in one client and an error in the other. `PORT.md`'s difference list does not record it.
- Recommendation: either record it as a deliberate difference (the Go library's empty-string idiom, which the CLI then inherits) or have `ask` treat an explicitly empty `--image` as an error, which needs the flag to distinguish "not given" from "given empty".

## Things Checked

- **Request body and message order** (`readout.go:1044-1055`, `imageMessages`) against `readout.py:844-859`: `model`, `messages` (system string, then user with the `image_url` part before the `text` part), `max_tokens: 1`, `temperature: 0`, `logprobs: true`, `top_logprobs`, `prompt_logprobs: 0`, `chat_template_kwargs: {"enable_thinking": false}`. Key for key, order for order.
- **Validation order and arithmetic**: `validate_image_url` → `top_logprobs` positivity → payload → slots → the `len(slots) > top_logprobs` refusal, all in Python's order and with Python's text; a negative `TopLogprobs` is refused (not sent) and a zero defaults to 20.
- **No letter fallback**: a code outside the returned top-k raises rather than costing a second request, and the distribution is never renormalized (`readout.go:1078-1088`). `image-error-missing-code` and `image-error-too-many-options` in the fixture pin both.
- **`checkImageBoundary` as a substitute**: the last `prompt_logprobs` entry must be the terminator's id *and* its decoded text; a response with no `prompt_logprobs`, an empty last entry, a wrong id, or a wrong decoding is refused. A server that returned `prompt_logprobs` for a shorter prompt that still ends on the terminator would pass — that is Python's behaviour too (`prompt_logprobs[-1]` is all it looks at), so it is a shared property, not a port regression. `boundaryEntry`'s preference for the terminator's own id removes the map-order nondeterminism Python's `next(iter(...))` has, and the scope records it as a difference; I agree it is an improvement, and the "lowest id" tiebreak makes it deterministic.
- **`systemRender` arithmetic**: `strings.Index(local, systemPrompt)`, `throughSystem`, `want := localIDs[:len(throughSystem)+1]`, and the `unverified` conditions match `readout.py:752-767`; the first prompt position is skipped on both sides. `differs` is returned only when the server's tokens are comparable and unequal, so a `string` verdict cannot come from a server that rendered something else without the tokens disagreeing.
- **`PrepareImage`/`ValidateImageURL`**: media type sniffed from the leading bytes (PNG, JPEG, RIFF/WEBP), the extension only chooses the message and never the type; `data:`/`http(s)` pass through; a bare path, an unsupported scheme, an empty value, an empty file and a non-image file all raise with Python's wording. The 51-case fixture (`images_parity.meta.json`) plus the Go-only tests (`TestSniffImageIgnoresTheExtension`, `TestPrepareImageSuffixDivergence`, `TestPythonPathMatchesPathlib`, `TestValidateImageURLEdgeCases`, `TestPrepareImageSchemeErrorNamesTheScheme`) cover both the accepted set and the refusals a caller hits; the fixture's own digest and case count are checked (`TestImagesFixtureIsNotStale`).
- **The client's image branch**: `prepare_image` before any validation (`client.go:1014-1017`), `Calibrated` defaulting off for an image and the caller's value not consulted, `Meta["input_type"] == "image"`, `Usage["requests"] == 1`, one read and one logical read, the 26-option refusal with Python's message and no request sent, the flat `Raw` with no `stages`, and `Decision.Raw[head]["system_render"]` present.
- **CLI**: `--image` and `--image-top-logprobs` registered on `ask` only, the `getattr(..., 20)` default present on every command, the `{} if image and state is None` rule, `doctor --probe`'s `chat system` line and its absence without `--probe`, and the probe's cost (three requests).
- **Fixture load-bearingness**: the image scenarios include the failure modes the scope asks about — a dropped code, a boundary that is not the terminator, a `differs` system render, a `strict` refusal, an `unverified` render, a response with no assistant message, an options-only top-k, a malformed entry, and the chat probe's three verdicts. The provenance check compares the digest, the scenario count and the exchange count, so a fixture regenerated without its metadata fails.
- **Readout version**: stays `de2-codebook-v1`, which is right — the image read is part of the DE-2 contract, and no text-path rendering, request or scoring semantics changed.

## Residual Risks

- **The client-level image `Raw` block has no recorded fixture.** The read is replayed exhaustively, but the flat raw shape is only asserted by Go-only tests that check its keys. That is why the `prompt`/`top_logprobs` shapes in the finding above went unnoticed, and it means any future edit to `imageRaw` is invisible to the parity gate.
- **`chat_system_render` is only ever exercised against stubs** (offline: `client_test.go:900`, `main_test.go:1398`). No live run has recorded what a real vision-enabled server answers, so the `differs` verdict's frequency on the hosted endpoint is unknown to this port. The docs' claim that the verdict is recorded rather than refused rests on the Python side's measurement in `docs/READOUT-DE2.md` section 6, which I did not reproduce.
- **The image read has no live coverage at all** (a recorded non-goal). Everything here is the recorded replay plus Go-only stubs; the one live gate this milestone could have run (`SHISA_DE_LIVE=1`) currently fails on the stale assertion above, so the live suite is not usable as a check until that is fixed.
- **`docs/PORT.md` and `README.md` are the high-conflict files** in this repo, and both need edits for the `Answer.SystemRender` finding; whoever applies that fix should coordinate, per `AGENTS.md`.
- **I did not run the suite.** Per the prompt's rules I used read-only inspection only; the counts and code paths above are read from the tree and from `testdata/*.meta.json`, and the live-test failure is derived from the code, not observed.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "live probe test asserts the absence of chat_system_render, which Health always sets",
      "location": "live_test.go:692-704",
      "recommendation": "Assert the verdict (string/differs/unverified) as Python's live test does, fix the two stale comments at live_test.go:38-40 and 663-665, and re-run SHISA_DE_LIVE=1 go test . -run Live."
    },
    {
      "severity": "MEDIUM",
      "title": "README.md and docs/PORT.md name Answer.SystemRender, which exists on neither client",
      "location": "README.md:208, docs/PORT.md:45, docs/PORT.md:63",
      "recommendation": "Name shisade.CodeRead.SystemRender and Decision.Raw[head][\"system_render\"], the two places the verdict actually lives."
    },
    {
      "severity": "MEDIUM",
      "title": "client.go's package doc still says this client has no image path",
      "location": "client.go:29-32",
      "recommendation": "Rewrite the paragraph to name DE-1 and overflow as the gaps, and correct the same claim in live_test.go:38-40, live_test.go:663-665 and the comment at cmd/shisa-de/main_test.go:522-525."
    },
    {
      "severity": "LOW",
      "title": "imageRaw records prompt as \"\" and top_logprobs as a map of pointers",
      "location": "client.go:1252-1253",
      "recommendation": "Use nil for an empty prompt and optionalFloatMap(read.TopLogprobs), as policy.go:346-350 does, and add a debug image decision to TestCallerVisibleMapsHoldValuesNotPointers."
    },
    {
      "severity": "LOW",
      "title": "docs/PORT.md's readout fixture counts are stale",
      "location": "docs/PORT.md:644",
      "recommendation": "Change 22 scenarios / 62 exchanges to the metadata's 35 / 74 and add the image read and the chat probe to the cover list."
    },
    {
      "severity": "LOW",
      "title": "the image boundary message is not Python's text for a missing decoded_token or a non-id key",
      "location": "readout.go:1171-1178, readout.go:1196-1203",
      "recommendation": "Print None where Python prints None, and carry the raw key so a non-id key is reported as itself rather than as id 0, so PORT.md's \"verbatim\" claim holds."
    },
    {
      "severity": "LOW",
      "title": "ask --image \"\" is a silent text read here and an error in Python",
      "location": "cmd/shisa-de/main.go:262-263 with client.go:1017",
      "recommendation": "Record the empty-string idiom as a deliberate difference in docs/PORT.md, or refuse an explicitly empty --image in ask."
    }
  ]
}
END_REVIEW_JSON
