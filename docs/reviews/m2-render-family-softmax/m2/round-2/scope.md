# Review Scope: m2-render-family-softmax

- Mode: implementation
- Stage: m2
- Target: HEAD (e0a4baf) of the Go port /home/morpheus/data/dev/shisa/shisa-de-go; the fix commit under review is a76057a, on top of the reviewed revision 12a1a29

## Summary

M2 of the Go port of the shisa-de Python client: the pinned DE-2 renderer, the Python-compatible JSON writer and reader, question types and option limits, family resolution, and softmax over option codes. Round 1 (revision 12a1a29) produced two MEDIUM and eleven LOW findings and no HIGH or CRITICAL. Commit a76057a addresses all of them; this round checks the fixes and re-runs astra, whose round-1 report was only an overload error.

Files changed in a76057a (478 insertions, 52 deletions):
- pyjson.go: decodeValue replaces a repeated object key's value in place (last value, first position, matching json.loads); new isJSONNumberLiteral validates a json.Number against JSON's number grammar before writing it; appendJSONNumber normalizes "-0" to "0" and treats strconv.ErrRange as Python's infinity/zero; appendPythonString now returns an error and rejects a string that is not valid UTF-8 instead of writing U+FFFD; callers updated (string case, reflect.String case, appendPythonObject key).
- questions.go: isPythonSpace/trimPythonSpace implement Python's str.strip() set (unicode.IsSpace plus U+001C-U+001F), used by RenderOption and validateQuestion; validateQuestion defaults optionLimit <= 0 to MaxOptions.
- readout.go: Softmax now iterates keys sorted by (len, lexicographic) -- the codebook's own order -- for the peak, the sum and the division, so the result is bit-reproducible; comment notes the DE-1 calibration entry is inert.
- family.go, render.go: comment-only. ResolveFamily documents that an empty declared string means "not declared"; the template pin documents that <bos> is a tokenizer-config value.
- render_test.go: new TestRenderFixtureProvenance checks testdata/render_parity.meta.json (model, revision, counts, sha256); loadRenderFixture now also fails on an empty serialization list; TestRenderMatchesTheReadoutPage's 216-token pin is re-attributed to the fixture rather than to the readout page.
- pyjson_test.go: new TestDecodeJSONKeepsTheLastOfARepeatedKey, TestDecodeJSONStaysStricterThanPythonOnBareNonFiniteLiterals, TestPythonSpaceIsPythonsStrip; TestSoftmaxDoesNotDependOnMapOrder replaced by TestSoftmaxIsDeterministic (exact equality, 50 runs) and TestSoftmaxSumsInCodebookOrder; cases added for -0, 1e400, -1e400, 1e-400 and invalid UTF-8 in a string, an object key and a json.Number.
- scripts/gen_render_parity.py: pins MODEL/REVISION and passes tokenizer_revision to Readout, writes testdata/render_parity.meta.json. Regenerating reproduced render_parity.json byte-identically, which confirms the fixture already came from the pinned snapshot.
- docs/PORT.md, AGENTS.md: M1/M2 attribution corrected, a "Python's JSON, not Go's" difference added, render and JSON parity gates recorded, the project overview marked as the target layout.

## Review questions

- Does the repeated-key handling in DecodeJSON match json.loads in both the surviving value and its position, and does Object.Get now agree with what PythonJSON writes for the same document?
- Is isJSONNumberLiteral exactly JSON's number grammar (no leading zeros, no bare '.', no trailing 'e'), and does appendJSONNumber reproduce CPython's json.loads+json.dumps bytes for every literal it accepts, including -0, 1e400, 1e-400 and values beyond 2^64?
- Is the UTF-8 rejection complete? Can any path still write a replacement character or invalid bytes into the output (json.RawMessage, map keys, Object keys, nested values)?
- Is the softmax sort exactly the order CodesFor returns for every n from 2 to 256, and does the accumulation now match the Python client's for the same logprobs?
- Is isPythonSpace exactly Python's str.strip() set, with no rune that Go strips and Python does not, or the reverse?
- Does validateQuestion's non-positive-limit default now agree with Payload and Render in every case, including which limit the error message names?
- Is the render-fixture provenance check sound: does the sha256 cover the checked-in bytes, and does the meta's revision pin actually constrain what the generator can produce?
- Does any of the new strictness reject an input the readout legitimately produces, or change the bytes for a case the 33-render and 37-fixed-point gates do not cover?

## Known non-goals

- DE-1 is not implemented; the DE-1 readout identity and calibration-compatibility entry stay only to keep the identity table faithful to the Python source of truth.
- Softmax still returns an unordered map; carrying the code order to the caller is M3's job and is documented rather than changed.
- The tokenizer package, the codebook and the tokenizer parity fixtures are M1 and out of scope for this round.
- No requests were made to a served model; the render and JSON gates are offline against Python-generated fixtures.
- DecodeJSON stays stricter than json.loads on the bare NaN/Infinity/-Infinity literals; that asymmetry is documented rather than fixed.

## Validation already run

- gofmt -l . (no output)
- go build ./...
- go vet ./...
- go test ./... -count=1 (all pass, including 33 render fixtures, 37 JSON fixed points, provenance check)
- SHISA_DE_TOKENIZER_REQUIRE=1 go test ./... -count=1 (TestRenderTokenCounts and the tokenizer parity suite run rather than skip, all pass)
- python3 -c 'json.loads/json.dumps' cross-check of -0, 1e400, -1e400, 1e-400, duplicate keys, str.strip of U+001C and U+200B (Go expectations match CPython)
- python3 scripts/gen_render_parity.py (render_parity.json reproduced byte-identically; meta written)

## Diff summary / excerpts

commit a76057a "fix: close the M2 render and JSON findings"

 AGENTS.md                        |  15 +++--
 docs/PORT.md                     |  42 ++++++++++--
 family.go                        |   6 ++
 pyjson.go                        | 115 ++++++++++++++++++++++++++++---
 pyjson_test.go                   | 142 +++++++++++++++++++++++++++++++++++++--
 questions.go                     |  28 +++++++-
 readout.go                       |  34 ++++++++--
 render.go                        |  13 ++--
 render_test.go                   |  84 ++++++++++++++++++++---
 scripts/gen_render_parity.py     |  41 +++++++++--
 testdata/render_parity.meta.json |  10 +++
 11 files changed, 478 insertions(+), 52 deletions(-)

Code hunks, abbreviated (read the files at HEAD for the rest):

pyjson.go decodeValue object case:
    value, err := decodeValue(decoder)
    ...
    // A repeated key keeps the last value, at the first occurrence's position.
    replaced := false
    for i := range object {
        if object[i].Key == key { object[i].Value = value; replaced = true; break }
    }
    if !replaced { object = append(object, Member{Key: key, Value: value}) }

pyjson.go appendJSONNumber:
    if !isJSONNumberLiteral(text) { return fmt.Errorf("shisade: json.Number %q is not a JSON number literal", text) }
    if !strings.ContainsAny(text, ".eE") {
        if text == "-0" { text = "0" }
        buffer.WriteString(text); return nil
    }
    value, err := strconv.ParseFloat(text, 64)
    if err != nil {
        var numberError *strconv.NumError
        if errors.As(err, &numberError) && errors.Is(numberError.Err, strconv.ErrRange) {
            appendPythonFloat(buffer, value); return nil
        }
        return fmt.Errorf("shisade: json.Number %q is not a number: %w", text, err)
    }
    appendPythonFloat(buffer, value); return nil

plus isJSONNumberLiteral(text string) bool implementing -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)? and isDigit(b byte) bool.

pyjson.go appendPythonString now returns error and begins:
    if !utf8.ValidString(text) {
        return fmt.Errorf("shisade: a string is not valid UTF-8, and Python cannot hold one")
    }
Callers: case string: return appendPythonString(buffer, typed); case reflect.String: return appendPythonString(buffer, reflected.String()); appendPythonObject returns the key error.

questions.go:
    func isPythonSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1C && r <= 0x1F) }
    func trimPythonSpace(text string) string { return strings.TrimFunc(text, isPythonSpace) }
RenderOption and validateQuestion use trimPythonSpace; validateQuestion adds:
    if optionLimit <= 0 { optionLimit = MaxOptions }

readout.go Softmax:
    keys := make([]string, 0, len(logprobs))
    for key := range logprobs { keys = append(keys, key) }
    sort.Slice(keys, func(i, j int) bool {
        if len(keys[i]) != len(keys[j]) { return len(keys[i]) < len(keys[j]) }
        return keys[i] < keys[j]
    })
    // then peak, weights/total and probabilities all iterate keys in that order

scripts/gen_render_parity.py adds MODEL = "shisa-ai/shisa-de-2", REVISION = "8f21734b27b3462471c4662d5638e52195b005a3", passes tokenizer_revision=REVISION to Readout, and writes testdata/render_parity.meta.json:
    {"model": MODEL, "revision": REVISION, "transformers": ..., "tokenizers": ...,
     "renders": 33, "serialization": 37, "tokens": 22948, "sha256": <sha256 of the fixture bytes>}

render_test.go adds TestRenderFixtureProvenance (revision must equal tokenizer.PinnedSnapshot, model must equal tokenizer.PinnedRepo, sha256 must match, and the renders/serialization/token counts must match the fixture) and makes loadRenderFixture fatal when the serialization list is empty.
