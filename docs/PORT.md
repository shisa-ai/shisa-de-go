# How this port maps onto the Python reference

`github.com/shisa-ai/shisa-de` is the reference implementation. `docs/READOUT-DE2.md`
and `docs/READOUT.md` are copied from it verbatim and remain the specification;
they are not edited here. This page records the mapping and every place the Go
client deliberately behaves differently.

## File mapping

| Python | Go |
| --- | --- |
| `shisa_de/readout.py` | `readout.go`, `render.go`, `endpoint.go` |
| `shisa_de/policy.py` | `policy.go` |
| `shisa_de/calibration.py` | `calibration.go` |
| `shisa_de/family.py` | `family.go` |
| `shisa_de/questions.py` | `questions.go` |
| `shisa_de/client.py` | `client.go` |
| `json.dumps` / `json.loads` | `pyjson.go` |
| `httpx` | `net/http` in `endpoint.go` |
| `shisa_de/overflow.py` | not ported; DE-1 only, so nothing here needs it |
| `shisa_de/images.py` | `images.go` |
| the exception classes in `questions.py`, `readout.py` and `images.py` | `errors.go` |
| the codebook load in `readout.py` | `codebook.go` |
| `repr()` of a `str`, and `str.isprintable()` | `pythonprintable.go`, and `pythonQuote` in `client.go` |
| `shisa_de/data/*.json` | `data/*.json`, embedded with `go:embed` |
| `shisa_de/cli.py` | `cmd/shisa-de/` |
| `tests/test_offline.py` | `*_test.go` with a stub tokenizer and `httptest` |
| `tests/test_live.py` | `live_test.go` behind `SHISA_DE_LIVE=1`, plus the parity gate |

The tokenizer has no Python counterpart as a module: the Python client calls
`transformers.AutoTokenizer`. This port implements the tokenizer in
`tokenizer/` so the library needs neither cgo nor a Python runtime.

Three rows cross a module boundary on the Python side. `errors.go` holds the four
classes Python defines in three places -- `QuestionError` in `questions.py`,
`ReadoutError` and `ReadoutHTTPError` in `readout.py`, `ImageError` in
`images.py` -- because a caller catching them here does so with `errors.As` and
the package is one namespace. `codebook.go` is the load of
`data/codebook-de2.json` that Python does inside `readout.py`, split out because
this port reads the codes from an embedded copy, lazily on first use rather than
at import. `pythonprintable.go` is the `str.isprintable()` half of `repr()`:
Python spells a label with `{value!r}` in most of the messages this port has to
reproduce byte for byte, Go's `strconv.Quote` is a different predicate against a
different Unicode table, so the port embeds the interpreter's predicate over
every code point and `pythonQuote` in `client.go` does the escaping around it.

The file mapping is the target layout, not a description of the tree: M1 landed
the codebook and the tokenizer; M2 landed the renderer, family resolution, the
question types and the Python-JSON writer; M3 landed the DE-2 readout and its
HTTP layer; M4 landed the policy, the calibration records and the friendly layer;
M5 landed the command line; M6 landed the live suite and its parity gate; M7
landed the image read, which is the last read the DE-2 page specifies. The port
is complete.

**The image read is part of that mapping.** DE-2 is not a text-only model: it has
an image read, specified in `docs/READOUT-DE2.md` section 6 -- a page this
repository ships -- and this port implements it. `images.go` builds the
`image_url` content part from a local path, an HTTP(S) URL or a data URL;
`Readout.ReadImage` posts to `/v1/chat/completions` and reads the option letters
from the returned top-k; `Classify` and `Decide` take the image on `CallOptions`; `CodeRead.SystemRender`
records what the server did with the system turn, and the client copies it to
`Decision.Raw[head]["system_render"]`; `Health` reports it as
`chat_system_render`; and `ask` takes `--image` and `--image-top-logprobs`. The
readout version stays `de2-codebook-v1`, because the image read is part of that
contract rather than a new one.

It is a third read beside the text one, not a variant of it: an image cannot
travel through the text readout at all, because the client renders that prompt
itself with the checkpoint's chat template and only the server can expand an
image placeholder into the image tokens the checkpoint expects. So the image
question posts to the chat endpoint, lets the server render, and takes one direct
read: no repeat, no thinking step, at most 26 options, no fallback for a letter
outside the returned top-k, and no calibration.

The system turn is the one place the read has to make a judgement rather than
follow a rule. vLLM hands Gemma 4's chat template every message of an image
request as content parts, and the template writes a space after a system message
that arrives that way, so the image prompt carries one token the text prompt does
not. DE-2 was trained on the string rendering, without images. This port records
the difference in `CodeRead.SystemRender` (`string`, `differs` or `unverified`),
which the client copies to `Decision.Raw[head]["system_render"]`, and answers
anyway, which is what the Python client does; it exposes
`ImageReadOptions.RequireStringSystem` for a server whose template has been
fixed. Section 6 of the DE-2 page has the measurement the judgement rests on.

The reference API is otherwise Python's, message for message:
`testdata/images_parity.json` replays 55 references through both clients and
compares the verdicts and the error text, including the four rooted-`~` cases
that pin pathlib's `not (self.drive or self.root)` guard. Two differences are
language rather than behaviour: Python's `prepare_image` also accepts a
`pathlib.Path` and rejects other types by name, which a Go `string` parameter
cannot express, and `ImageError` is its own type here where Python's subclasses
`ValueError` (Python's `~user` lookup raises a bare `RuntimeError`, which is not
an error the image API promises to return).

## Deliberate differences

**DE-2 only.** This port implements `de2-codebook-v1`. A model that resolves to
DE-1 is an error. The Python client reads DE-1 correctly, so this is a missing
feature rather than a contract change: it must fail loudly, never fall back to
DE-2 reads. The Python client makes that decision in `DecisionModel`; this port
makes it in `NewReadout` as well, because a `Readout` can be held directly and
it is the thing that spends requests. Resolution is `ResolveFamily`'s order --
a declaration, then a slug in the model id, then a slug in the tokenizer source,
otherwise assumed DE-2 -- and the source is kept on the readout so an assumption
is reported as one.

**Calibration is selected per released checkpoint, in two functions.** Python's
`calibration_for(family, model=, tokenizer=)` takes the served model through
keyword arguments. Go has none, and changing `CalibrationFor(family)` would break
a caller that only wants a family's record, so the port keeps it and adds
`CalibrationForCheckpoint(family, model, tokenizer)`; the client calls the
second. `CalibrationFor` is what a model whose id names no released checkpoint
gets, which is Python's `CALIBRATION_FILES` case.

**`CheckpointCalibrationFiles` has no DE-1 entry.** Python's table lists
`shisa-ai/shisa-de-1` because it ships that record. This port ships none and does
not read the family, so `ReleasedCheckpoint` does not recognise a DE-1 name and
the table lists only what can be loaded. A caller who needs a DE-1 name matched
needs the Python client.

**`ReleasedCheckpoint` tests its boundaries directly.** Python writes the rule as
the lookaround pair `(?<![a-z0-9.])shisa-de-2.1(?![a-z0-9.])`. Go's `regexp` is
RE2 and has no lookaround, so the port scans for each slug and tests the bytes on
either side against the same ASCII class. The class matters: `.` and a digit
continue a name, so `shisa-de-2.10`, `shisa-de-2.2` and `shisa-de-21` are not
DE-2, and `TestReleasedCheckpointRecognisesReleasedNames` pins the same table the
Python test does. A name that extends a release with a suffix --
`shisa-de-2-FP8-dynamic`, and equally `shisa-de-2-preview` -- is that release.

**No `strip_state_quotes`.** Python's `DecisionModel(strip_state_quotes=True)`
and `--strip-state-quotes` remove delimiter quote characters from the edges of a
state string before rendering. This port renders the state verbatim, as the
contract specifies, and has no counterpart, so the normalisation is the caller's
to apply. It is off by default in Python, so the two clients read the same prompt
unless a caller asks for the other behaviour.

**No Jinja.** The Python client renders with
`tokenizer.apply_chat_template(...)`, which runs the checkpoint's own Jinja
template. This port renders the two message shapes the readout uses with a
pinned function. It is byte-identical for those shapes and for no others, which
is the whole of what the readout needs. The system line is a parameter, as it is
in the Python client (`Readout(system_prompt=...)`); the thinking render always
uses the thinking line whatever the parameter holds, because that line is part
of the read rather than a preference.

What guards the pin: `TestRenderParity` compares 35 rendered prompts byte for
byte against the Python client, and that is the check on the render itself. The
runtime checks catch a different failure -- a client/server tokenizer mismatch.
Every read checks that the prompt ends on `AnswerPrefix`, and the first read
checks the server's `usage.prompt_tokens` against this client's; both compare
two tokenizations of the same bytes the client sent, so neither can see a render
that disagrees with the checkpoint's Jinja template.

**Python's JSON, not Go's.** The user turn is
`json.dumps(payload, ensure_ascii=False)`, and its bytes are the prompt, so
`pyjson.go` reimplements that writer rather than using `encoding/json`. Go's
encoder writes `{"a":1}` where Python writes `{"a": 1}`, escapes `<`, `>` and
`&`, and formats floats over a different range; each difference would move the
prompt away from the one the calibration was fitted on. `DecodeJSON` keeps
Python's int-or-float distinction, which an `any` from `json.Unmarshal` loses,
and keeps an object's key order in `Object` because the order reaches the prompt;
a Go map is accepted and has its keys sorted. `PythonJSONWith` carries the two
`json.dumps` options that change the bytes, for the callers that are not writing
a prompt: `indent` (`nil` is the compact form, and `""` is the indented form with
no indentation, which is a different output) and `ensure_ascii` (`true` escapes
DEL and the C1 block and writes an astral code point as the surrogate pair UTF-16
would). `PythonJSON` is the compact `ensure_ascii=False` form, which is what the
prompt needs.

`PythonJSON` is deliberately
stricter than Python in two places, both of which have no Python counterpart: a
string that is not valid UTF-8 is an error rather than U+FFFD, and a
`json.Number` must be a JSON number literal. `DecodeJSON` rejects the bare
`NaN`/`Infinity` literals that `json.loads` accepts by default, while
`PythonJSON` still writes them for a NaN or infinite float.

**No cgo.** The tokenizer is pure Go. `sugarme/tokenizer` was evaluated and
rejected: it does not match added tokens inside a rendered prompt, and it took
14 to 35 seconds on an 8,580-token prompt. `daulet/tokenizers` (cgo to
Hugging Face's Rust implementation) is exact and fast but needs a 49 MB
platform-specific static library.

**The tokenizer pipeline is pinned, not general.** The package implements the
pinned checkpoint's normalizer, pre-tokenizer and decoder and checks the file
for exactly those, rejecting anything else with `ErrUnsupported`. Hugging Face
accepts a file with no normalizer, a different `Split` behavior, a `Strip`
decoder or BPE dropout and reads it with that pipeline; this port refuses
rather than reading it as if it were the pinned one. It also refuses some files
Hugging Face reads for reasons of its own: a duplicate vocab id (Go's reverse
lookup would otherwise depend on map iteration order) and an added token whose
declared id disagrees with the vocab. Finally, it refuses what Hugging Face
refuses too: a negative vocab id, a merge whose operands or concatenation are
missing, and a file with neither a resolvable `unk_token` nor a complete byte
vocabulary, where an unknown character would silently produce no token.
`Lookup` also resolves only the revision asked for, never another snapshot that
happens to be cached.

**Resolution has an explicit local-only policy.** `ResolveWith` and `LoadWith`
take a `ResolveOptions{LocalFilesOnly: true}`, which returns `ErrNotCached` for a
source that is neither a readable file nor already in the hub cache, instead of
fetching it. `Resolve` and `Load` are the same functions with that policy off,
so the M1 call sites are unchanged.

**`Decode` reproduces the pinned decoder, including its invalid-byte
handling.** A run of consecutive byte-fallback tokens decodes to the run's text
when the bytes are valid UTF-8, and otherwise to one replacement character per
byte. Hugging Face's `ByteFallback` does the same; a plain concatenation of the
bytes would not, and neither would `from_utf8_lossy`, which consumes maximal
invalid subsequences. `Encode` never produces such a run for the pinned
vocabulary, so this only matters for hand-built or model-generated id lists.

**Errors instead of exceptions.** `ReadoutError` and `QuestionError` replace the
Python exception classes; callers use `errors.As`. `ReadoutHTTPError` keeps its
`status` and `detail` fields and is a `ReadoutError` as well, which is what the
Python hierarchy already is (`ReadoutHTTPError(ReadoutError)`): a handler for a
failed read catches a rejected request without naming both types. One place the
Go version is stricter is a response whose `choices` is missing or empty: the
Python client indexes `data["choices"][0]` and raises `KeyError` or
`IndexError`, while this port returns a `ReadoutError` saying which read came
back without a choice.

**The readout is stricter about three things the Python client tolerates.** Each
is a case where the Python client picks something and this port refuses to:

- The `prompt_logprobs` fallback reads the first entry of the last prompt
  position, whatever the map holds. `prompt_logprobs: 0` asks for the prompt's
  own token and nothing else, so a response with more than one entry there is a
  server answering a different question; this port errors rather than scoring a
  token nobody asked about. A null *value* in that position's map is the same
  error here, where the Python client reaches `None.get("logprob")` and dies with
  an `AttributeError` that is not a `ReadoutError` at all. A null *position*
  (`[null, null]`) is not a difference: both clients report the code as
  unreturned.
- `CodeRead.Sampled` is a `string`, not `str | None`. A response that omits
  `choices[0].text` gives `""` here and `None` there. It is a plain struct field,
  not a caller-visible map entry, and nothing reads it as anything but
  provenance; Go's zero value carries the same meaning. The same field appears
  in the raw block as a *string or nil*, not as `""`, because that map is what
  a caller reads and Python's is `None`.
- `CodeRead.Prompt` is the same shape with a different spelling. The Python
  client types the field `str | None` and it is `None` for a read sent as token
  ids after a thought, which is every read in a `repeat-think` policy that
  thought. This port's field is `""` there. The raw block writes nil for it, so
  a caller sees Python's `None` rather than an empty prompt; the field itself
  keeps Go's zero value.
- One response body is capped at 64 MB before it is read. The Python client
  reads whatever arrives. The cap has to clear the largest honest response,
  which is the `prompt_logprobs: 0` fallback's one entry per prompt token rather
  than one position of top-logprobs, so it is sized for a prompt far longer than
  any DE-2 prompt is.

A fourth difference is a Go decoding hazard rather than a divergence, and both
clients refuse it: a `token_ids` element that is not a JSON integer, including
`null`. Go's decoder turns a null element into token 0 and leaves it, so a plain
`[]int` would continue a thought with a token the server never produced; the
Python client's `isinstance` check rejects the same shapes. The Go type
(`TokenIDList`) exists to make the decoder do what the Python client does, and
is not a licence to accept less.

**Go's zero values cannot express two of the Python constructor arguments.**
`ReadoutConfig.SystemPrompt` of `""` renders `DirectSystem`, so Python's explicit
empty system line cannot be asked for; and `ReadoutConfig.MaxLogprobs` of 0 sends
`logprobs: 20`, where Python's `max_logprobs=0` sends `logprobs: 0` and makes
every code fall back. Both mappings are documented on the fields; neither is
reachable from a normal caller.

**Field-level decoding is typed where Python coerces.** `usage.prompt_tokens`,
`rank`, `choices[0].text`, a model entry's `id`, and every value in the
`top_logprobs` and `prompt_logprobs` maps are decoded into Go types, so a string
where a number belongs, or a number where a string belongs, fails the response
rather than being coerced. The Python client's `int(... or 0)`,
`entry.get("rank")` and `entry.get("id")` accept the coercion and yield `None`
for a missing id where this port yields `""`; its logprob maps hold any JSON
value and fail later, inside `math.exp`, where this port refuses the response.
No OpenAI-compatible server sends any of these shapes.

**`Softmax` sums in a fixed key order, not the caller's.** Python's `softmax`
sums `weights.values()` in the dict order it was given, which `read_codes` sets
to option order, and on CPython 3.12+ it sums with Neumaier compensation. This
port sorts the keys (shortest first, then lexicographic, which is the codebook's
own order) and sums left to right, so the result is reproducible under Go's
randomized map iteration and uses the same order Python does for the
codebook-ordered prefix the readout always passes. It is still a relative 1e-16
or so from Python's number, because the arithmetic differs even when the order
does not, and the replay gate compares the probabilities with a 1e-12 relative
tolerance rather than exactly for that reason.

**The boundary-approval cache is keyed by code and token id, not by code.**
Python's `_boundary_checked` is a set of letters, so a hand-built slot carrying
an approved code and an id the tokenizer never produced passes its
`check_code_boundary`. This port records the approved id with the code and
re-checks when a slot's id differs. It also refuses to record an approval from a
read that lost a `SetTokenizer` race, so a later read under the new tokenizer
cannot inherit one.

**`ListModels` reports its failures as readout errors.** Python's `list_models`
calls `raise_for_status()`, which raises an `httpx.HTTPStatusError` outside the
`ReadoutError` hierarchy, and lets a `response.json()` failure escape uncaught.
This port returns `ReadoutHTTPError` for a rejected request and `ReadoutError`
for a body that is not the expected object, so one handler covers the read paths
and the model listing.

**`SetTokenizer` has no Python counterpart.** It is a Go-only test hook that
replaces the tokenizer and discards everything derived from the old one: the
resolved slots, the boundary approvals, and the generation counter a concurrent
read uses to tell whether its approvals are still current. Python's readout
resolves its tokenizer once and has no way to swap it.

**A mid-read capability downgrade re-reads every code, as Python does.** When a
server rejects `logprob_token_ids` after an earlier block was accepted, both
clients drop the field and ask again for the whole option list in one request,
so the retry's top-k replaces the codes the earlier blocks already read. The
replay fixture's `codes-late-downgrade` scenario pins this with a server whose
top-k scores differ from its candidate-id scores.

**`LocalFilesOnly` is threaded into tokenizer resolution.** The Python client
passes `local_files_only=True` to `from_pretrained`; this port passes the same
flag to `tokenizer.LoadWith`, which returns `tokenizer.ErrNotCached` instead of
fetching when the source is neither a readable file nor already in the hub
cache.

The Python client also accepts any JSON value from the endpoint and rejects a
non-object after parsing; this port decodes straight into the object type, so a
JSON array fails with the same "did not return a JSON object" error.

**Lazy loading.** The Python client loads the tokenizer inside
`Readout.ensure_tokenizer`. The Go client does the same, holding the tokenizer
and the resolved slots behind one mutex, so concurrent first calls serialize on
one load and a failed load leaves nothing behind to be retried.

**Concurrency.** `ThreadPoolExecutor` becomes goroutines with a bounded worker
pool, and the heads are sorted before they are dispatched, so the work order is
fixed rather than dependent on Go's map iteration.

**The default model id is DE-2, and DE-1 is retired.** `DefaultModel` is
`shisa-ai/shisa-de-2`, so `NewDecisionModel(DecisionModelConfig{})` returns a
working DE-2 client against the hosted endpoint. It used to be
`shisa-ai/shisa-de-1`, matching the Python client's `DEFAULT_MODEL`, which made a
zero config an `UnsupportedFamilyError` here: this port cannot read DE-1, and
quietly substituting a different checkpoint was a worse difference than an error.

DE-1 was retired as a model after that. Measured 2026-10-13 against
`https://api.shisa.ai/openai`: both `shisa-ai/shisa-de-1` and
`shisa-ai/shisa-de-2` are listed by `/v1/models`, and a chat request naming either
comes back with `"model": "shisa-ai/shisa-de-2"` from the same
`system_fingerprint`, `vllm-0.30.0-tp2-ca69195e`. So the DE-1 name is an alias for
the DE-2 checkpoint and there is no DE-1 deployment left to default to, which is
why the two clients' defaults now name the same model again. A zero config was
re-run against that endpoint after the change: `shisa-de doctor --probe` reports
`shisa-ai/shisa-de-2`, `de2-codebook-v1`, `boundary check passed` and `read probe
passed`, and `shisa-de ask` with no flags answers the built-in support ticket with
`order_status` at 0.977.

The family *resolution* rule is unchanged, and deliberately: a model id or
tokenizer naming `de-1` still resolves to the DE-1 contract and is still refused.
The client cannot tell the retired alias from a DE-1 fine-tune of someone's own at
resolution time, and resolving the latter to DE-2 would be the unmeasured read the
refusal exists to prevent. What changed is that the refusal now says what became
of the name -- it is an alias for `shisa-ai/shisa-de-2` -- rather than telling the
caller to pass a DE-2 checkpoint because the default is DE-1, which stopped being
true.

**The image path resolves an ambiguous answer boundary by token id.** The image
readout cannot render its prompt locally, so it confirms the answer position from
the server's side: the last prompt token must be the terminator's own id.
`prompt_logprobs: 0` returns one token per position, so a well-formed response has
one entry at the boundary; a position carrying several is ambiguous and both
clients refuse it. Before refusing, the Python client takes the dict's first key,
which is insertion order there. A Go map has none, so `boundaryEntry` looks for
the terminator by id and otherwise reports the lowest id: the same answer twice
rather than a different one each run. The refusal text is Python's, including
`None` for an entry with no `decoded_token` and the raw key for one that is not a
token id; only the token it names can differ, and only when the boundary position
is ambiguous.

**An image read reports no elapsed time.** Python's `read_image` stamps
`LetterRead.elapsed_ms`; the `CodeRead` an image read returns has no such field,
and nothing reads one: it is not in `Raw`, not in the fixture, and not in
`Usage`. The policy layer's `PolicyRead.ElapsedMS` is the counterpart that does
exist, for the text path.

**The chat probe prefixes every failure with `unavailable:`.** Python's
`probe_chat_render` converts a `ReadoutError` to `unavailable: ...` and lets a
`QuestionError` from `_payload` propagate. Go's returns a `string` with no error
channel, so both get the prefix. The case cannot arise from `Health`, which asks
a Noul, and the same prefix is what the Python command line prints for the other
one.

**The image path reads the extension the way Python 3.14 reads it.**
`pathlib.Path.suffix` changed in 3.14, and the two rules differ on two shapes: a
name with two or more leading dots and a dot-free body (`..png`, `..a`), and a
name that ends with a dot without being all dots (`a.`, `x.`). Neither shape can
name a supported image, so both interpreters accept and reject the same set of
files. The fixture's `suffix_divergence` block names one input per shape and
records both rules' answers, `TestPrepareImageSuffixDivergence` checks the port
against the column the recording was made under and that both shapes are covered,
and `testdata/images_parity.meta.json` records the interpreter. A regeneration
under 3.10 or 3.12 therefore fails on the rule, naming the shape, rather than as
two unexplained rows in the replay. `gen_images_parity.py` enumerates every name
over `{.}` and a letter up to six characters to check that there is no third
shape; the difference is a shape and not a fixed pair of names.

**The command line's image flags.** `ask` takes `--image` and
`--image-top-logprobs`, matching the Python command line's, including the rule
that an image question with no `--state` asks about the image alone rather than
falling back to the built-in ticket. `--image-top-logprobs` is registered on
`ask` only, the way `argparse` registers it; the other commands still carry the
default, because the Python command line reads it with
`getattr(args, "image_top_logprobs", 20)`. `doctor --probe` prints the
`chat system` line after the read probe, from the same `chat_system_render`
entry. `explain` still prints the DE-2 walkthrough and no DE-1 branch, because
there is no DE-1 read. The `explain` output is otherwise byte-identical to the
Python command line's for the same question, checked against the hosted
endpoint.

**The command line's `--model` defaults to DE-2.** The library keeps
`DefaultModel` at the Python client's DE-1 id so that a caller who asks for
nothing gets an error rather than a different checkpoint than they asked for. A
command whose zero-config form can only fail is not a command, so
`cmd/shisa-de` defaults `--model` to `shisa-ai/shisa-de-2` and says so in
`--help`. The two command lines therefore query different checkpoints when
`--model` is omitted; `doctor` prints which one it read.

**The command line's flag layer is Go's `flag`, not `argparse`.** Values are
checked after parsing rather than by `choices`, so the grammar differs in three
ways. A flag is written out in full: `--fam` is a unique prefix `argparse`
accepts and this one does not. A boolean flag takes no argument, as
`BooleanOptionalAction` does, but Go's `flag` also accepts `--reasoning=true`
and `--no-reasoning=false`, which `argparse` refuses because its action has
`nargs=0`. And a zero or negative `--timeout` is read as "use the library's
default" rather than being passed through, because that is what a zero
`time.Duration` means to `NewReadout`. What does match: an explicitly empty
`--family`, `--policy`, `--reads` or `--calibration` is a bad invocation rather
than the default value, `--family` accepts exactly `de1` and `de2`, and a
leftover positional argument is refused. `-h`, `--help` and `<command> --help`
print the usage and exit 0; every other bad invocation exits 2 with the usage on
stderr. What is not `argparse`'s is the text itself: the usage block and
`--help` output are this port's own layout rather than `argparse`'s
`usage: ... [-h]` line with metavars and a positional section, and a failure
prints one `shisa-de <command>: <message>` line followed by that usage where
`argparse` prints the usage followed by `shisa-de <command>: error: <message>`.
The flag set, the exit statuses and which stream each goes to are what match.

Two of the values the parser accepts can only fail here. `--family de1` and
`--calibration de1` are legal spellings, because `argparse`'s `choices` has
both families, but this port ships no DE-1 read and no DE-1 record, so the
first reaches `NewDecisionModel`'s DE-2-only refusal and the second
`CalibrationFor(FamilyDE1)`, and both exit 2 with the usage. The Python command
line reads DE-1 for the same invocations.

**`explain` prints `None` for a null logprob.** The Python walkthrough sorts
`read.top_logprobs` by `-kv[1]`, which raises `TypeError` on a null a server can
send, so the reference crashes there. This port sorts a null last and prints
`None` for it rather than the `0.0000` a collapsed zero would show, because the
walkthrough is read next to the server's own response. Equal logprobs are the
other case Python orders and Go cannot: `sorted` is stable over the dict's
insertion order, which is the server's response order, and a Go map has none, so
a tie sorts by token instead.

**A caller-visible map prints its keys sorted, and the views are `Object`s.**
The Python client's `dict` is ordered and `json.dumps` writes it in insertion
order. A Go map has none, so `PythonJSON` sorts the keys. That reaches
`Decision.Values`, whose `--confidence` and `--probabilities` views are
`shisade.Object` rather than `map[string]any`: `label` then `confidence` then
`probabilities`, and the distribution's own keys in the caller's option order
(`yes` then `no` for a noul, ascending positions for a score). The recording
above is what pins it -- `ask --image --labels outage,billing --probabilities`
prints the distribution in that order, and a map would print it sorted.

This is the one place the port's public API differs from a plain Go shape, and it
is deliberate: `Object` is the ordered map `PythonJSONWith` renders, so the type
that carries the order is the type the writer needs. `Object.Get` reads a key and
`OrderedProbabilities` builds the distribution's `Object` from `Answer`. An
`Answer` a caller builds itself has no order to reproduce and its keys are
sorted.

`doctor --json` restores the order from a fixed key list instead, because the
report's key set is the command line's own. `Health`'s nested slot summary is the
third case and it is ordered at the source: Python writes
`{"count": len(slots), **{letter: token_id}}`, so `report["slots"]` is a
`shisade.Object` (`count`, then the codebook's first two codes) rather than a
`map[string]any`. A caller who reads either one gets an `Object` and not a map,
and `json.Marshal` on it writes an array of members rather than a JSON object;
`PythonJSON` and `PythonJSONWith` write it as the object Python writes.

**The library's `ResolveCalibration("")` means "auto".** Python's
`resolve_calibration(None)` means "let the client pick from the served model" and
`resolve_calibration("")` falls through to `load_calibration_file("")` and
raises. A Go `string` cannot carry the `None`/`""` distinction, and the zero
value is the natural spelling of "nothing was passed", so the empty spec and
`"auto"` are the same here. The command line does not depend on that: it records
whether `--calibration` was given and refuses an explicit `""` with Python's own
message, so the two command lines agree.

**The DE-2 readout rewords the messages it shares with DE-1.** The Python
readout has one slot resolver for both families, so a DE-2 code that is not one
token is reported as `answer letter 'A' ... read with letter slots`. This port
says `answer code 'A' ... read with the codebook`, because the DE-2 contract's
slots are codes. Two other messages have no Python counterpart at all and are
Go's own: the wrapper around a tokenizer that will not load, and the two checks
on the `prompt_logprobs` fallback (more than one entry, or a null one) that keep a
server this client cannot read that way from scoring a token nobody asked about.
`explain` has no DE-1 branch, so a tokenizer under which `<channel|>` is not one
token is the one failing run where the two command lines' text can differ.

On the image path the messages are Python's verbatim, including the terminator
message in `AnswerPrefixID` and all four `checkImageBoundary` refusals; the one
case where the text can differ is an ambiguous boundary position, where the token
named is the one `boundaryEntry` resolved. And `Softmax` says `no code logprobs
to normalize` where the shared Python helper says `no letter logprobs`, the same
DE-2 rewording as the slot message.

The rest of what the readout raises is Python's sentence with Python's `!r`
quoting, including the boundary check's three messages, which reach `doctor`'s
`boundary_check` field and `explain`'s stderr. A run that fails the boundary
check is not part of the parity diff, which passed it.

**`Decision` is a struct, not a dict subclass.** Python's `result["route"]`
becomes `decision.Values["route"]`, and `result.answers` becomes
`decision.Answers`. There is no indexing operator to overload in Go, and one
field is clearer than an accessor pair.

**`Answer.Value` and `Answer.Label` hand back the value, not a pointer to it.**
The `Answer` struct keeps `*float64` and `*string` fields, because those are the
places the Python client has `None`; the two accessors and `ToWire` dereference
them, so a caller reads `answer.Value().(float64)` rather than a pointer, which
is what the Python client returns and what JSON already erases.

**Warnings are recorded, not raised.** The Python client calls `warnings.warn`
for an assumed family and for a calibration record that is held but not applied.
Go has no warning channel, so `DecisionModel.Warnings` collects them as strings.
Nothing is printed. The ids inside them are Python's `!r`, but their advice is
not identical: where Python writes `pass family='de1' or family='de2'` and `pass
calibration= to apply a record you vouch for`, this port names the Go fields,
`pass Family to say which contract it was trained for` and `pass Calibration to
apply a record you vouch for`, because Go has no keyword arguments.

**A repeated option key is refused.** Python's choice criteria are a dict, so a
repeated key cannot reach a read. This port takes a slice, because a Go map has
no order and the order is what assigns the answer codes; two options with one key
would be scored from two codes and then written into one probability, so the
returned distribution would not sum to one. `Question.Validate` refuses the
repeated key before any request goes out.

**`HeadSpec.MultiLabel` with described options is refused.** Python's
`{"labels": {"a": "A", "b": "B"}, "multi_label": True}` reaches its mapping
branch, which reads a single `Choice` over the described labels and ignores
`multi_label`. There is no way to read that input a second way here without
dropping the descriptions, so this port refuses the combination and says so. A
multi-label head takes bare keys.

**The multi-label template is Python's `str.format`, within limits.** Python
calls `template.format(label=label)` when the template contains `{label}`, and
appends the label when it does not. This port does the same, including `{{` and
`}}` as literal braces, and refuses a replacement field that names anything else
rather than passing it through. Format specs (`{label!r}`, `{label:>10}`) are
refused too, which Python would honour.

**A score's level and legend are spelled the way Python spells them.** Both go
through `str()`: `inf` and `nan`, not this package's JSON `Infinity` and `NaN`,
and a container is spelled with `repr` for its elements (`['low', None]`). The
one difference is a container's key order: Python keeps insertion order and a Go
map has none, so `pythonStr` sorts the keys of a `map[string]any`.

**`HeadSpec` replaces the duck-typed label spec, and the order is explicit.**
Python's `classify` accepts a list of labels, a label-to-description mapping, a
`{"levels": [...]}` mapping, a `{"labels": [...], "multi_label": True}`
mapping, or a question object, and tells them apart by shape. A Go map does not
preserve insertion order, and the order of a label set decides which answer code
each label is read as, so this port takes a `HeadSpec` whose `Options` is a slice.
That makes the presentation order part of the API instead of an accident of the
caller's map, and it makes the three shorthands separate fields rather than
shapes to be sniffed. `Question` still wins over every other field.

**`ResolveOptions` takes an overrides struct, because Go has no keyword
arguments.** Python distinguishes "not passed" from "passed as None" for
`policy`, `reads`, `reasoning` and the rest. A Go `bool` or `float64` cannot, so
`OptionOverrides` holds pointers and nil is Python's `None`. The two names of
one setting (`think_gate`/`reasoning_prob`, `think_budget`/`reasoning_len`,
`overflow`/`compound`) are both fields, and passing both with different values is
the same error it is in Python.

**`Decide` takes typed questions, not label sets.** Python's `decide` accepts
"a `Noul`, `Choice`, or `Score`, or the label-set shorthand `classify` accepts".
This port keeps the two apart: `Decide` takes `map[string]Question`, and
`QuestionFromHead` builds the question a `HeadSpec` describes for a caller who
wants the shorthand. The difference is a compile error rather than a silent one.

**`ReadPolicy` takes resolved `ReadOptions`.** Python's `read_policy` has
defaults for `policy`, `think_gate` and `think_budget` in its signature; this
port takes the `ReadOptions` the client already resolved, so the defaults live in
one place (`ResolveOptions`) rather than two. `ResolveOptions(family, nil, {})`
produces the same defaults for a caller that wants them.

**`Health` reports a nil readout rather than panicking.** A `DecisionModel`
built by `NewDecisionModel` always has a readout; the zero value does not, and a
nil `*Readout` would panic inside `net/http`. Every entry point that reads checks
and reports.

**`MaxWorkers: 0` means the default, not one worker.** Python's
`max(1, int(max_workers))` has a default of 8, so an explicit `max_workers=0`
means serial execution there. Go's zero value cannot express "explicitly zero",
and `MaxWorkers: 0` is this port's default of 8; a negative value clamps to 1,
which is what Python does with a negative too. A caller who wants one worker asks
for one.

**A calibration record needs its provenance and its table.** `CalibrationFromJSON`
requires `model`, `readout_version`, `serving_shape`, `fitted_on` and
`temperatures`, and refuses a `null` anywhere it expects a number or a table.
That is the Python loader's set, not a superset: it reads
`record["temperatures"]` unconditionally, so a record without one raises there
and is refused here. A `null` temperature raises `float(None)`'s `TypeError`
there; here it is refused explicitly, because a `map[string]float64` would have
read the null as zero, which `TemperDistribution` passes through, and the answer
would have been a raw probability reported as calibrated. `reads` is optional
and a `null` one is refused rather than read as absent.

**`pythonQuote` is the interpreter's `repr`, from a generated table.** A level's
text and a legend's values are option keys, spelled with `repr` when they sit
inside a container. `repr` escapes everything `str.isprintable()` rejects, and
Go's `strconv.IsPrint` is that rule against a different Unicode version: Go
1.25's tables are Unicode 15.0.0 and Python 3.14.6's are 16.0.0, so 5,812 code
points assigned since are printable to Python and unprintable to Go, and
`strconv.IsPrint` escaped all of them. `data/python-printable.json` is
`str.isprintable()` over every code point, compacted to 736 inclusive ranges and
generated by `scripts/gen_python_printable.py` with the interpreter's version
recorded; `testdata/python_repr.json` holds `repr()` for 32,509 sampled code
points, covering every range boundary and the BMP's first three pages, and the
test replays it. Neither direction of the old disagreement is left.

**`DecisionModel.Close` drops idle connections.** The Python client's `close`
closes its httpx client. Go's `http.Client` is shared and reusable, so `Close`
calls `CloseIdleConnections` and leaves the readout usable.

## Performance

Measured on an 8-core Intel Xeon E5-2690 v4 against the hosted
`shisa-ai/shisa-de-2` (`vllm-0.30.0-tp2-ca69195e`), 2026-10-10. Both clients
answered the same question against the same endpoint in one session.

| | Go | Python |
| --- | ---: | ---: |
| Client construction | 0.6 ms | 91 ms |
| Cold process to first answer | 2,220 ms | 5,041 ms |
| Warm decision, median of 10 | 86.9 ms | 87.8 ms |
| Warm decision, minimum of 10 | 85.2 ms | 85.2 ms |
| Warm decision, mean of 10 | 90.1 ms | 91.2 ms |

**Per-decision latency is the same, and that is the finding.** The server round
trip is about 85 ms and the client's own work is a few milliseconds on both
sides, so it does not surface; the minimums agree to the tenth of a millisecond.
The difference between the two is entirely in startup, and it is one process's
worth:

- **Construction is free here and not in Python.** `NewDecisionModel` resolves
  and returns in 0.6 ms because it defers the tokenizer to the first call, which
  then costs 1.87 s. Python's construction is 91 ms on top of a 267 ms cold
  `import shisa_de`, and its first call costs 5.0 s, of which 1.15 s is
  `import transformers` and about 3.2 s is `AutoTokenizer.from_pretrained`.
- **The Go 1.87 s is parse and structure, not I/O.** The tokenizer is already
  cached. About 0.85 s is `encoding/json` over the 32 MB `tokenizer.json`
  (measured directly: 801 ms into a `map[string]any`, 875 ms into the typed
  fields), and the rest is building 514,906 merge ranks and 262,144 vocabulary
  entries. A streaming or generated parser is the obvious place to look if this
  ever matters, and it has not mattered yet: one load per process.

Both clients skip the tokenizer entirely until the first call, so a client that
is constructed and never used costs nothing in either language.

This is one machine, one endpoint and one day. The warm-latency comparison is
robust because the minimums coincide exactly and the client-side work is two
orders of magnitude below the network; the absolute startup figures are
machine-dependent, though the ratio is not.

## Verified parity

Every check recorded here is offline at test time: it was produced by running the
Python client and `transformers` over the pinned snapshot
(`shisa-ai/shisa-de-2@8f21734b27b3462471c4662d5638e52195b005a3`) or against the
hosted endpoint, and the results are checked in as fixtures. The Python client is
`shisa-de` `0.4.0` at `d172d4977d133fefb9b8c82ffcc0e3d007554551`.

The tokenizer, render, JSON and readout fixtures were produced with no served
model, so they carry no serving fingerprint. The client fixture was recorded
against the hosted endpoint and does: the responses carried
`system_fingerprint: vllm-0.30.0-tp2-ca69195e`, on 2026-10-12, with the tokenizer
pinned to `8f21734b27b3462471c4662d5638e52195b005a3`. Where a number below is a
measurement of a served checkpoint, it says so.

The client and command-line fixtures below are the Python client's results,
replayed. The live gate is this port's own measurement against a served
checkpoint, and says so where it appears.

- **Tokenizer.** 1,056 strings, 48,392 token ids: this package's ids equal
  `transformers`' ids on every one. The corpus covers all 24 added tokens and
  their prefix pairs, adjacent explicit `<unk>` tokens (the case that catches
  `fuse_unk` applied to added tokens), all 256 codes, the rendered scaffold,
  JSON payloads, Japanese, emoji, combining marks, control characters, 60 KB of
  real text from both repositories, and 400 random strings. A second fixture
  replays 83 `tokenizer.decode` outputs, including byte-fallback runs that
  `Encode` cannot produce and a run split by an id outside the vocabulary.
  `testdata/tokenizer_parity.meta.json` records the model, the revision, the
  library versions, the counts and a sha256 of each fixture; the test fails if
  either fixture disagrees with it. Regenerate with
  `scripts/gen_tokenizer_parity.py`, which pins the revision it passes to
  `from_pretrained`.
- **Render.** All 35 fixture renders are byte-identical to what
  `Readout._payload` and `Readout.render` produced for the same case: both
  template shapes (the repeated/direct read and the thinking read), a
  caller-supplied system line for each shape, the empty-description rule,
  non-string descriptions, 17 evidence shapes, widths 2/26/27/77/256, and long
  evidence. Each fixture prompt also re-tokenizes to the same ids the Python
  client produced. The readout page's 238 and 137 token counts are pinned; the
  fixture's third pin (216, the Japanese choice) is a regression check and is
  not a published value.
  `testdata/render_parity.meta.json` records the model, the revision, the
  library versions, the counts and a sha256 of the fixture, and the test fails
  if the fixture disagrees with it. Regenerate with
  `scripts/gen_render_parity.py`, which pins the revision it passes to
  `Readout` as `tokenizer_revision`.
- **Python JSON.** 39 fixed points, produced by Python's own `json.dumps`: the
  35 payloads above replayed through `DecodeJSON` then `PythonJSON`, plus
  scalar, float, string and nested values. The fixtures constrain the encoder
  rather than being shaped by it. Duplicate object keys, `-0`, out-of-range
  exponents, invalid UTF-8 and Python's `str.strip()` whitespace set have their
  own unit tests; each was checked against CPython's `json` module directly.
  `testdata/pyjson_dumps.json` holds 264 more, generated by
  `scripts/gen_pyjson_dumps.py`: every value crossed with `indent` in
  `None, "", 0, 1, 2, 4` and both `ensure_ascii` settings, which is what pins the
  indented form the command line prints. The values carry an empty and a nested
  container, both number kinds, the escapes the encoder special-cases, and the
  code point ranges `ensure_ascii` treats differently.
- **Python `repr`.** 32,509 sampled code points, each replayed against
  `repr()` from Python 3.14.6 (Unicode 16.0.0): every printable range's first
  and last code point with a neighbour on each side, all of the BMP's first
  three pages, the tag characters and the variation selectors, and a stride
  through the rest. The sampling is what a generated table needs, because a
  wrong range shows up at its edges; `data/python-printable.json` is
  `str.isprintable()` over every code point, and `testdata/python_repr.json`
  records the interpreter that produced it so a regeneration on a different
  Unicode version is a diff rather than a mystery. Regenerate both with
  `scripts/gen_python_printable.py`.
- **Readout.** 38 scenarios, 77 recorded exchanges, replayed request for request
  and field for field. Each exchange records the method, the path and the
  headers the client set itself -- `Content-Type`, and `Authorization` for the
  one scenario that configures a key -- and the replay server compares all of
  them, so a readout that posted to the wrong path or dropped a header fails the
  gate. The scenarios cover: widths 2,
  26, 27, 128, 129 and 256 (the last two crossing the 128-candidate block
  boundary), a server that honours `logprob_token_ids`, one that ignores it, one
  that rejects it with a 400 and one that accepts the first block and rejects the
  second (so the downgrade happens mid-read and the retry's top-k replaces the
  codes already read), a server that keys the logprobs by decoded text rather
  than `token_id:<id>`, a server that sends no `choices[0].text`, the debug read,
  the thinking read closed by the model and cut by the budget, the read after a
  thought, the fallback after a thought (where the code is appended as a token id
  rather than as text), and four failures: a server whose `usage.prompt_tokens`
  disagrees with the client's tokenization, a fallback that returns nothing, a
  thinking response with no `token_ids`, and a prompt that does not end on the
  terminator (which sends no request at all).

  Thirteen of the scenarios are the image read and three are the chat probe. The
  image read has no local prompt to tokenize, so its scenarios are what pins the
  server-rendered side: the plain read, the debug read, a stock vLLM whose system
  turn carries one extra space (answered, with `differs` recorded), the same
  server under `require_string_system` (refused), a response too short to compare
  (so `unverified`), one with no assistant message, one whose top-k is all
  options, and four failures: a code missing from the top-k, more options than
  the returned top-k can hold, a malformed top-logprob entry, and the three ways
  the answer boundary can fail -- a last prompt token that is not the terminator,
  one with no `decoded_token` at all, and a response with no `prompt_logprobs`.
  The probe scenarios record the three verdicts it can return. Every request body
  the Go client
  sends is compared to the Python client's after JSON decoding, and every result
  -- logprobs, probabilities, request and prompt-token counts, missing codes,
  ranks, sampled text, debug top-logprobs, messages, system render, thought ids
  and closure -- is compared
  to the Python client's. The stub tokenizer is rebuilt from the fixture's
  recorded word list, so a divergence in the two implementations surfaces as a
  wrong token count rather than passing. `testdata/readout_parity.meta.json`
  records the Python revision, the readout version, the limits, the counts, a
  per-operation step count and a sha256 of the fixture; the test fails if the
  fixture disagrees with any of them, so a regeneration that silently dropped the
  image read's refusals fails rather than only shrinking a total.
- **Readout behaviour outside the replay.** The response shapes the fixture does
  not carry have their own tests: a completion with no choices, a server that
  reports no `usage`, a null or fractional `token_ids` element, a null
  `prompt_logprobs` entry (which used to panic), a body past the response cap,
  the DE-1 refusals for a model id, a tokenizer source and a declaration, the
  `ReadoutHTTPError`-is-a-`ReadoutError` taxonomy (and a `ReadoutHTTPError`
  built without its embed, which must not panic or hand back a nil
  `*ReadoutError`), the bearer header and the model listing, a tokenizer swap
  discarding both the ids and the boundary approvals resolved from the old one,
  the exported `Slot`, `Render`, `AnswerPrefixID` and `CheckCodeBoundary` entry
  points, a zero-value `Readout` reporting itself, the `closeEnough` helper
  directly, and eight concurrent readers. `go test -race` is what decides the
  concurrency claim.
- **Policy and calibration.** `ResolveOptions` is tested against expected values
  produced by running the Python resolver for both families and for each layer of
  overrides; the expectations are inline rather than a fixture, so they are not
  re-checked against Python at test time. `ReadPolicy` is tested over a stub
  server that answers with read bodies of the same shape the readout fixture
  pins. The gate is checked from both sides, as is the read after a thought, the
  close the client writes when the model does not, the option cap of 26
  (`ThinkOptionCap`) and the 27-option case that trips it, the aliased names
  (`reasoning_prob`, `reasoning_len`, `compound`) and the error for passing both,
  a policy name the resolver does not know, a `reads` value that is not 1 or 2,
  and `direct` spending one request where the default spends two. The calibration
  tests cover the two arithmetic helpers against hand-computed values, the
  shipped record's temperatures and provenance, the applicability levels for each
  family and checkpoint, `Calibration.TemperatureFor` returning the default for a
  read the record does not describe, and `ResolveCalibration` choosing between a
  bundled record, a named one and a caller's. The rule that a record with no
  per-read fits leaves an answer read after a thought raw is pinned by a record
  that carries a top-level temperature and no `reads`, which is the only shape
  that can tell whether the rule is there.
- **Friendly layer.** `Classify` and `Decide` are tested against a stub server
  over the same readout the policy tests use, for the shapes the live fixture
  cannot force: a head that is refused before any request goes out, probability
  with reasoning refused in the same way, a repeated option key refused in the
  same way, the multi-label threshold and its three views, the multi-label
  template against Python's `str.format`, the per-head provenance block, a call's
  overrides not leaking into the model's defaults, three concurrent heads through
  a bounded pool, and a `DecisionModel` with no readout reporting rather than
  panicking. A separate test pins the Go type in every map a caller reads,
  because the parity gate compares through JSON and cannot see a `*float64` where
  the Python client returns a number.
- **Client.** 26 cases, 37 recorded exchanges, replayed request for request and
  answer for answer. The fixture was recorded live against the hosted endpoint
  (`vllm-0.30.0-tp2-ca69195e`, 2026-10-12) through a forwarding transport, so the
  responses are the server's and the answers are the Python client's; the Go test
  replays the recorded responses and makes no request of its own. The cases
  cover the gate from both sides (`decide-noul-sure` and
  `decide-noul-near-gate` both stop at the repeated read, one well above the gate
  and one close to it, and `decide-choice-think` and `decide-choice-3-think`
  cross it), a choice, a three-way choice, an ordered
  score, a 27-option head above the think cap, `direct`, an explicit `repeat`,
  `probability`, a checkpoint the shipped record does not cover, an aliased
  checkpoint, several heads in one call, the debug read, a debug read that
  reaches the thought, the image read from both entry points, and `classify` with
  label sets, levels, multi-label, the mapping and levels shorthands, the
  probability view and mixed head types.
  Every request body the Go client sends is matched against a recorded one -- a
  body the Python client never sent fails the case, and every recorded exchange
  must be used -- and each answer is compared whole, three times over: the
  `Answer` fields, `to_dict`, and the TypeSafe `to_wire`, which carries the
  legend. `Decision.ToWire` is compared whole too, and so are `Values`, `Raw`,
  `Usage` and `Meta`. `testdata/client_parity.meta.json` records the endpoint,
  the served `system_fingerprint`, the date, the Python revision, the readout
  version, the tokenizer revision, the counts and a sha256 of the fixture; the
  test fails if the fixture disagrees with it. The fingerprint and the date are
  checked against the responses the fixture actually holds rather than against
  the provenance's own shape, so an endpoint label in the fingerprint field
  fails; `TestFixtureIntegrityChecksCanFail` runs that check against mutated
  copies of the provenance, because a check nobody has shown to fail is not one.

  This fixture now has an image case, from both entry points, recorded through
  `/v1/chat/completions` like the rest. It could not gain one until the endpoint's
  vision support was checked: the read needs a server that will accept an image
  content part, and the assumption that the endpoint here was text-only was wrong.
  What the two image cases add over the offline chat replay is the client-level
  `Raw` block -- the `prompt` and `top_logprobs` shapes under debug, and the
  `system_render` verdict -- compared whole against a real exchange. That is the
  block that went unnoticed until the M7 review found it, and the offline replay
  cannot see it because it compares the read rather than the client's view of it.
- **Command line.** `explain`'s output, `doctor`'s text report and `doctor
  --json` were each compared line for line against the Python command line
  against the hosted endpoint, and are identical apart from the numbers a fresh
  request changes and the one `chat system` line above.

  The image surface has a recording rather than only that live comparison.
  `testdata/cli_parity.json` is the Python command line's own stdout for eleven
  invocations -- `doctor` and `doctor --json`, both with and without `--probe`,
  `ask` over text and over an image, `ask --image-top-logprobs`, an image
  question with an explicit state, the probability view, and `ask --image ''` --
  recorded by `scripts/gen_cli_parity.py` against a stub endpoint and a stub
  tokenizer. Each case carries stdout, stderr, the exit status, and every request
  in order, and `TestCLIParity` replays each through `run` and compares all four.
  The tokenizer is the Go test's own, id for id, so the recorded request bodies
  are replayed byte for byte rather than only structurally: a `top_logprobs`
  that stopped reaching the request, or a message order that changed, fails the
  case. The provenance holds a digest the test checks before anything is
  replayed.

  The one case whose stderr is not compared is `ask --image ''`: the Python
  command line has no handler around the command body, so the `ImageError`
  leaves as a traceback where this port prints `shisa-de ask: image is empty`.
  The status is 1 either way, and only the status is compared.

  The rest of the command line is covered offline. It drives `run` end to end
  over a stub endpoint and a stub tokenizer, which
  is what makes `explain` testable at all: the real tokenizer is a download. It
  covers the flag-to-config mapping for every flag including the `--no-` forms,
  the exit statuses for a bad command, a bad flag and a report that is not ok,
  an explicitly empty choice flag, the three ways `--state` is read and its
  Python whitespace, the walkthrough's five sections and its `curl` line, the
  boundary failure that precedes every print, the read failure that follows the
  first two sections, the `--json` shape and its key set, and the exact `!r`
  quoting of `doctor`'s note and calibration-reason lines.
- **Live gate.** `TestLiveParityAgainstPython` runs the fixed set in
  `testdata/live_questions.json` -- a noul, a three-option choice, a choice with
  unnamed options, an ordered score, a 27-option choice, a 256-option choice, the
  same choice read under `direct`, the same score asked for the one-read
  probability view, a choice whose gate is 1.0 so it thinks, and an image
  question whose answer is a 32x32 solid PNG carried as a data URL -- through
  both clients against the same served checkpoint in one session, and requires
  the chosen value, the label, the strategy, the request count, the level, the
  temperature, the confidence, the thought length and closure to match and every
  probability to agree within 1e-9. The image answer additionally has to carry
  the same `system_render` verdict on both sides, which is the judgement that
  read makes and the one field a text answer has no counterpart for. The measured
  deltas are 2.2e-19 to 5.6e-16, over ten questions against
  `vllm-0.30.0-tp2-ca69195e` on 2026-10-12, which is what a `temperature: 0` read
  against a deterministic server should give. The same nine-question set measured
  1e-19 to a few multiples of 1e-16 against the same fingerprint on 2026-10-10;
  the image question is the tenth and the range is the two sessions' together.
  (The worst key moves between runs -- 2.22e-16 and 5.55e-16 on two of them -- so
  the figure is the range and not one run's maximum.) The tolerance is six orders
  of magnitude above that and not more; the temperature is compared exactly,
  because on a peaked answer two fits can produce the same distribution and only
  the temperature says which one was applied.

  The image question needs `/v1/chat/completions` with vision enabled, so the
  recorder probes for that before it records rather than the gate assuming it:
  `probe_images()` sends one image request and classifies the answer. A server
  that refuses with an error naming images is a capability, and the meta's
  `images: false` plus the server's own words in `images_reason` make the gate
  skip that one case. A transport error, a 401/403/429, a 5xx, or an error that
  says nothing about images is **not** a capability: the recorder writes nothing
  and exits 4, so an unmeasured endpoint fails the run instead of dropping the
  gate's image coverage behind a message that reads as a deployment fact.

  `TestLiveImageReadUsesTheImage` is the check the parity gate cannot make. The
  gate compares two clients on one image request, so a server that accepted the
  content part and ignored it would produce agreement about nothing. That test
  asks the same question of a red and a blue image and requires the answer to
  follow, which is the Python suite's `test_image_classification_reads_pixels_not_filenames`.

  `TestLiveCalibrationMatchesTheServedCheckpoint` is the check to run when an
  endpoint changes what it serves. It requires the applied record to be the one
  fitted on the checkpoint the model id names, and an uncovered checkpoint to be
  reported and left raw. It cannot see through an alias, so it is pointed at the
  checkpoint's own id, or at the alias with `SHISA_DE_TOKENIZER` naming the
  checkpoint behind it.

  The Python side is recorded by `scripts/live_parity.py` during the run rather
  than read from a checked-in file, and the reason is a measurement. The served
  checkpoint is deterministic **within** a session: three back-to-back reads of
  one prompt came back bit-identical, and the recorder's own two passes over the
  whole set differ by 0. It is not deterministic **across** sessions: the same
  prompt a day later answered with the same chosen option and probabilities that
  differed by up to 4e-3, which is far more than the arithmetic and far less than
  a port defect would show. Comparing a live run against a recording made earlier
  therefore measures the endpoint, not the port; the first version of this gate
  did exactly that and reported 4e-3 disagreements that the Python client
  reproduced exactly against the Go client on the same day. The recorder runs the
  set twice and records the difference so the test can say which of the two it is
  looking at: when the Python client's own passes disagree, the run is reported
  as inconclusive rather than as a parity failure. `testdata/live_python.json` is
  a run artifact and is not checked in.

  Both halves resolve `SHISA_DE_ENDPOINT` and `SHISA_DE_MODEL` the same way, and
  the gate fails rather than logs when they disagree: two clients against two
  servers would be the one comparison the design exists to prevent. The
  recorder's own logic -- the endpoint and model resolution, the tokenizer pin,
  the drift arithmetic over every consecutive pass -- is tested offline by
  `scripts/test_live_parity.py`, which needs nothing but `python3`.

  The rest of the live suite is the DE-2 half of the Python `tests/test_live.py`:
  the endpoint report and its probe, an obvious classification, three heads in
  one call with the request accounting, a JSON round trip, the 27/128/256-option
  read at one request and two, and the thinking read. The DE-1 overflow case and
  the model card's 161-token example are absent because this port has no DE-1
  read. The Python suite's image case is ported twice: the gate's `image-color`
  question compares the two clients on the same image request, and
  `TestLiveImageReadUsesTheImage` checks the answer against the image on its own.
  The second is what makes the first mean something, because agreement on an
  answer about nothing is still agreement.

  The Python suite's `test_de2_thinks_when_the_repeated_read_is_unsure` ends with
  `assert answer.calibrated is False`, which was written before the DE-2 record
  gained its `think` fit. Against the hosted `shisa-ai/shisa-de-2`, which is the
  checkpoint that record was fitted on, the Python client returns
  `calibrated=True` at 6.8 today and the assertion is stale. Against another
  served checkpoint it is correct: the record does not apply, nothing is
  tempered, and the answer is uncalibrated at 1.0. The Go test derives both
  expectations from the model's own applicability rather than writing 6.8 down,
  so it holds in either configuration, and the Python test is the one that should
  be made conditional.
- **Tokenizer identity.** The test checks the loaded tokenizer's vocab size
  (262,144) and `<channel|>` id (101) before running the vectors, so a fallback
  to a different revision fails loudly. Set `SHISA_DE_TOKENIZER_REQUIRE=1` to
  make a missing tokenizer.json a failure instead of a skip.
- **Codebook.** The 256 codes and their order are the research repository's
  single-token answer-boundary audit
  (`evals/reports/de2-token-code-audit.json`), whose `token_ids` were resolved on
  `google/gemma-4-26B-A4B-it@4d7ae4984b7db7de8f8457170b3f1a419ee76d52`.
  `data/codebook-de2.json` records that provenance. The code is the contract and
  the ids are not read at runtime; the client resolves each code's id from the
  served tokenizer. What ties the two together is the tokenizer identity check
  above: `shisa-ai/shisa-de-1` and `shisa-ai/shisa-de-2` share the Gemma
  tokenizer (vocab 262,144), so a code's id is not family-specific.
- **Speed.** The 256-option repeated prompt (27,810 bytes, 8,580 ids) encodes
  in 18 ms on an Intel Xeon E5-2690 v4, against 13 ms for `daulet/tokenizers`
  (Hugging Face's Rust implementation through cgo) on the same machine. The
  tokenizer normalizes a whole prompt into a single pre-token, so the merge
  runs over tens of thousands of symbols in one call; that is why the merge is
  heap-based rather than a repeated scan.

  That comparison is against the Rust tokenizer a `daulet/tokenizers` caller
  uses, which is not what the Python client uses. Measured against
  `transformers` 5.19.0 on the same machine, for an 8,879-byte prompt: this
  package 4.67 ms, `transformers`' fast (Rust) tokenizer 5.08 ms, its slow
  (pure-Python) one 3.44 ms. **This port is not faster than `transformers` here**
  and the port docs should not be read as claiming it is; it is in the same band.
  The three agree on the ids.
