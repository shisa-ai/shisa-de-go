# M7 round 1 — disposition

All three requested reviewers reported, each on its primary candidate with no
failover: `astra` (`shisa-gpt-6-astra-jouzu-cli`), `ds4`
(`shisa-deepseek-v4.1-flash-jouzu-cli`) and `glm` (`shisa-glm-5.3-jouzu-cli`).
The three reports, the run summary and `results.json` sit beside this note; the
cycle's prompts, stderr and failover summaries stayed in the Python checkout the
session was working in, which is where the harness wrote them.

Round 1: 0 HIGH, 0 CRITICAL. No rerun, which is the rule.

All three said the read itself is faithful — the request body key for key, the
message order, the option limit, the no-fallback rule, the refusal arithmetic,
the system-render comparison, and `images.go` against `images.py`. Every finding
was in the layer around the read, and they converged: the live suite, the
package doc, the `Answer.SystemRender` naming, and the debug raw block were each
found by two of the three.

## Fixed

| Finding | Reporters | What changed |
| --- | --- | --- |
| `TestLiveHealthProbeServesARead` asserted the absence of `chat_system_render`, which `Health` sets whenever it probes, so the live suite failed by construction | ds4 M | asserts the verdict instead, in Python's own set (`string`, `differs`) |
| `client.go`'s package doc said this client has no image path, as did two `live_test.go` comments and one in `cmd/shisa-de/main_test.go` | ds4 M, glm L | the doc names images as ported and DE-1 overflow as the gap; the comments corrected |
| `README.md` and `docs/PORT.md` named `Answer.SystemRender`, which neither client has | ds4 M, glm L | both name `shisa.CodeRead.SystemRender` and `Decision.Raw[head]["system_render"]` |
| `imageRaw` wrote `""` for the prompt and a `map[string]*float64` for the top-k under debug, where the policy path writes `nil` and a map of numbers | ds4 L, glm M | `nil` and `optionalFloatMap`, with a debug image decision added to the pointer walker, which fails on either if reverted |
| `ReadImage` resolved its slots, its terminator and its system-render comparison from separate tokenizer lookups, so a concurrent `SetTokenizer` could split one read across two | astra M | one `tokenizerSnapshot` for the read, `codeSlotsFor`/`answerPrefixIDFor`/`systemRenderFor` against it, and no caching when the generation moved |
| `docs/PORT.md`'s readout fixture counts were stale (22/62 against the metadata's 38/77) and the cover list omitted the image read and the probe | ds4 L | updated, with a per-operation count in the metadata that `TestReadoutFixtureProvenance` asserts |
| The image boundary refusal was not Python's text for a missing `decoded_token` (`''` against `None`) or a key that is not a token id (`(id 0)` against the key) | ds4 L | `DecodedToken` is a `*string`, `boundaryEntry` returns the raw key, and the messages are Python's |
| `firstChatChoice` said `... no choices for the image read` where Python says `... no choices`, contradicting the page's "verbatim" claim, and the branch had no test | glm L | Python's message, with a test that pins it exactly |
| `pythonPath` expanded a rooted `~` where pathlib's `not (self.drive or self.root)` guard does not, turning `/~/x.png` into a `//home/...` path | glm L | the guard, four rooted-`~` cases in the fixture, and the guard named in `docs/PORT.md` |
| `ask --image ""` was a silent text read here and `image is empty` in Python | ds4 L | `--image` is recorded through a `flag.Value`, and an explicitly empty one is refused |
| `ReadImage`'s result has no counterpart to `LetterRead.elapsed_ms` | glm L | recorded in `docs/PORT.md`: nothing reads one, and `PolicyRead.ElapsedMS` is the text path's |
| `ProbeChatRender` prefixes a `QuestionError` with `unavailable:` where Python lets it propagate | glm L | recorded in `docs/PORT.md`, with why the case cannot arise from `Health` |
| The prose field check could not see a backticked `Type.Field`, which is how the `Answer.SystemRender` claim survived | glm L (suggestion) | `TestReadmeProseNamesRealFields` resolves a backticked `Type.Field` whenever `Type` is an exported type here; it fails on the pre-fix text |

## Not changed, with the reason

- **`chat_system_render` accepting `unverified` (ds4's recommendation).** ds4
  suggested asserting the verdict "as Python's live test does" and listed
  `unverified` among the valid values. Python's test does not:
  `tests/test_live.py:159` asserts `in {"string", "differs"}`. The stricter set
  is what was applied, so the two suites agree on what a healthy endpoint
  reports. `unverified` from a text-only probe is a finding worth failing on,
  not a verdict to accept.
- **Adding `CodeRead.ElapsedMS` (glm's alternative).** Nothing consumes it: not
  `Raw`, not the fixture's `read_record`, not `Usage`, not the client. An unused
  exported field is a promise to keep it accurate; the page records the omission
  instead.
- **The client-level image `Raw` block has no recorded fixture (ds4 and glm
  residual).** It cannot gain one from the endpoint the fixture was recorded
  against, because the image read needs a vision-enabled server. `docs/PORT.md`
  now says so, names what covers the surface instead, and records that this is
  where the raw shapes went unnoticed until this round.
- **The docs' `system_render` measurement does not show whether the extra
  system-turn token changes accuracy (astra residual).** The page cites the
  Python side's measurement and does not claim more than it shows.
- **`~nonexistentuser` mapping Python's `RuntimeError` onto `ImageError` (glm
  residual).** `docs/PORT.md` records it in the same paragraph as the rooted-`~`
  guard.
- **The live image path is unverified against a real vision server (all three
  residual).** It needs a vision-enabled endpoint and an image file; the page
  records it as a gap rather than a check.
