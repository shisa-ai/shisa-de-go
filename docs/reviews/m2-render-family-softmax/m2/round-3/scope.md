# Review Scope: m2-render-family-softmax

- Mode: implementation
- Stage: m2
- Target: HEAD (782d78e) of the Go port /home/morpheus/data/dev/shisa/shisa-de-go; the fix commit under review is 044542e

## Summary

M2 of the Go port of the shisa-de Python client, round 3: the fixes in 044542e for the round-2 findings, and the state of the M2 surface as a whole.

Round 1 (12a1a29) found two MEDIUM and eleven LOW and no HIGH or CRITICAL; a76057a addressed them. Round 2 (a76057a) cleared those fixes and found two MEDIUM and ten LOW: PythonJSON wrote a repeated object key (which json.dumps cannot produce), and json.RawMessage bypassed the round-1 UTF-8 check because json.Valid does not validate UTF-8. 044542e addresses both plus the LOW items.

This round is also the third attempt to get astra's coverage: it returned only "[Error: Our servers are currently overloaded. Please try again later.]" in rounds 1 and 2, which the harness counted as a successful execution because the CLI exited 0 with a report file.

Changed files in 044542e: pyjson.go (DecodeJSON UTF-8 pre-check, linear repeated-key decode, RawMessage UTF-8 check, byte-array rejection, repeated-key rejection in appendPythonObject, doc updates), pyjson_test.go (new TestDecodeJSONRejectsInvalidUTF8, TestDecodeJSONCoercesALoneSurrogate, TestRenderOptionTreatsATypedNilAsNone; new rejection cases for a RawMessage with invalid UTF-8, a repeated Object key, and byte arrays), questions.go (isNilDescription, used by RenderOption), readout.go (softmax comment), render.go (guard comment), render_test.go (vocab_size/close_id provenance check), scripts/gen_render_parity.py (identity check at generation, newline="", identity in the meta), docs/PORT.md, README.md, AGENTS.md.

## Review questions

- Is DecodeJSON's utf8.Valid(data) pre-check the right place, and does it reject exactly what CPython's json.loads on bytes rejects, without rejecting input json.loads accepts?
- Is the json.RawMessage UTF-8 check complete, and does the RawMessage case now behave consistently with the string case for every input?
- Does appendPythonObject's repeated-key rejection break any caller in this repository, and is rejecting the right choice rather than deduplicating to match json.loads?
- Is the linear repeated-key decode in decodeValue exactly equivalent to the previous rescan (last value, first position), including for a key repeated three or more times and for nested objects?
- Does the byte-element check now cover every Go type that Python's json.dumps would reject as bytes, without rejecting a type Python accepts (a [3]int, a []any of ints)?
- Does isNilDescription treat exactly the values that should be absent, without turning a nil map or slice into a fallback to the key?
- Is the softened softmax comment accurate about CPython 3.12+ sum() and about the size of the difference?
- Do the doc changes now describe the guards, the provenance and the parity evidence accurately, with no number or fingerprint that was not measured?

## Known non-goals

- DE-1 is not implemented; the DE-1 readout identity and calibration entry stay for fidelity to the Python source of truth.
- Softmax still returns an unordered map; carrying the code order is M3's job.
- The lone-surrogate coercion in DecodeJSON is documented and pinned, not detected: a literal U+FFFD in the input is indistinguishable from a coerced one.
- A json.Number with more digits than CPython's int-string limit is accepted and written verbatim; that is documented rather than enforced.
- The tokenizer package and the codebook are M1 and out of scope.
- No requests were made to a served model.

## Validation already run

- gofmt -l . (no output)
- go build ./...
- go vet ./...
- go test ./... -count=1 (all pass)
- SHISA_DE_TOKENIZER_REQUIRE=1 go test ./... -count=1 -v (TestRenderTokenCounts and the tokenizer parity suite run rather than skip, all pass)
- python3 scripts/gen_render_parity.py (exit 0; render_parity.json reproduced byte-identically; the identity check on vocab 262144 and <channel|> 101 passed)

## Diff summary / excerpts

commit 044542e "fix: close the M2 round-2 review findings" (on top of a76057a, which was reviewed in round 2)

 AGENTS.md                        |   9 ++-
 README.md                        |  13 +++-
 docs/PORT.md                     |  58 +++++++++-------
 pyjson.go                        |  76 ++++++++++++++------
 pyjson_test.go                   | 112 +++++++++++++++++++++++++++++-
 questions.go                     |  22 +++++-
 readout.go                       |   8 +-
 render.go                        |  12 ++-
 render_test.go                   |   8 +-
 scripts/gen_render_parity.py     |  26 +++++--
 testdata/render_parity.meta.json |   2 +
 11 files changed, 279 insertions(+), 65 deletions(-)

Code changes, abbreviated (read the files at HEAD):

pyjson.go
- DecodeJSON now begins with: if !utf8.Valid(data) { return nil, fmt.Errorf("shisade: decoding JSON: the input is not valid UTF-8, which Python's json.loads cannot decode either") }
- decodeValue's object case uses positions := make(map[string]int) and updates object[at].Value in place, instead of rescanning the members per key.
- case json.RawMessage: after json.Valid it now also requires utf8.Valid(typed), with the comment that json.Valid checks syntax only.
- reflect.Slice, reflect.Array: the byte-element check is no longer guarded by Kind() == reflect.Slice, so [32]byte is rejected like []byte.
- appendPythonObject keeps a seen map[string]struct{} and returns an error naming the key when a key repeats.
- DecodeJSON's doc now lists three places it is stricter than json.loads (bare NaN/Infinity, invalid UTF-8, and CPython's int-string digit limit) and one place it is looser (a lone surrogate escape is coerced to U+FFFD, documented rather than detected).
- Object's doc says a repeated key is an error.

questions.go
- new isNilDescription(description any) bool: true for nil and for a nil pointer (reflect.Pointer + IsNil); a nil map or slice is NOT nil here.
- RenderOption uses isNilDescription instead of `description == nil`.

readout.go
- Softmax's comment now says the order matches the Python client's but that CPython 3.12+ sums with Neumaier compensation, so the totals are not bit-identical, and the difference cannot move an argmax or the gate.

render.go
- The template-pin comment now says TestRenderParity is the guard on the render, and that the runtime checks (AnswerPrefix, usage.prompt_tokens) only catch a client/server tokenizer mismatch because both sides tokenize the same bytes.

render_test.go
- renderMeta gains vocab_size and close_id; TestRenderFixtureProvenance checks them against tokenizer.PinnedVocabSize/PinnedCloseID, with a comment that the generator's own MODEL/REVISION constants cannot catch a mis-pin on their own.

scripts/gen_render_parity.py
- Adds VOCAB_SIZE = 262144 and CLOSE_ID = 101, checks len(tokenizer) and convert_tokens_to_ids("<channel|>") against them and exits if they differ, records both in the meta, and writes both files with newline="" so the digest does not depend on the platform.

docs/PORT.md
- The "No Jinja" paragraph now says what the runtime checks catch and names TestRenderParity.
- The verified-parity header now says every check recorded is offline, produced from the pinned snapshot and the Python client, with no requests to a served model, and that the port has produced no measurements against a served model.
- The codebook bullet now cites the research audit and the Gemma snapshot its token_ids came from, instead of claiming a hosted-endpoint read.

README.md / AGENTS.md
- README's status paragraph names M1/M2 as landed; its live-test section is marked planned and notes that -run Live matches nothing.
- AGENTS.md's verification table marks the live-suite, doctor and explain rows as planned.
