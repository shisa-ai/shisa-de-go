package tokenizer

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The pinned tokenizer is the one the parity vectors were generated from:
// scripts/gen_tokenizer_parity.py, model shisa-ai/shisa-de-2, revision
// 8f21734b27b3462471c4662d5638e52195b005a3 (PinnedRepo/PinnedSnapshot in
// hub.go). The codebook's token_ids were audited on
// google/gemma-4-26B-A4B-it@4d7ae4984b7db7de8f8457170b3f1a419ee76d52
// (data/codebook-de2.json); TestParityCoversEveryCodebookCode checks that the
// two agree.

// loadPinned loads the tokenizer the parity vectors were produced with and
// checks that it is the pinned one.
//
// The file is 32 MB, so it is not checked in. The test looks in the places a
// developer or CI would have it, then skips, unless SHISA_DE_TOKENIZER_REQUIRE
// is set, which turns the skip into a failure so CI can require the run.
func loadPinned(t *testing.T) *Tokenizer {
	t.Helper()
	tokenizer, path, err := LoadPinned(PinnedRepo, PinnedSnapshot)
	if errors.Is(err, ErrNoPinnedTokenizer) {
		if os.Getenv(DownloadEnv) == "1" {
			tokenizer, err = Load(PinnedRepo + "@" + PinnedSnapshot)
			if err != nil {
				t.Fatalf("downloading the pinned tokenizer: %v", err)
			}
			return checkPinned(t, tokenizer, PinnedRepo+"@"+PinnedSnapshot)
		}
		if os.Getenv(RequireEnv) == "1" {
			t.Fatalf("no tokenizer.json found and %s=1; set %s or %s=1",
				RequireEnv, TokenizerJSONEnv, DownloadEnv)
		}
		t.Skipf("no tokenizer.json found; set %s, or %s=1 to require it", TokenizerJSONEnv, RequireEnv)
	}
	if err != nil {
		t.Fatalf("loading the pinned tokenizer: %v", err)
	}
	return checkPinned(t, tokenizer, path)
}

// checkPinned fails when the loaded tokenizer is not the pinned one.
func checkPinned(t *testing.T, tokenizer *Tokenizer, path string) *Tokenizer {
	t.Helper()
	if err := VerifyPinned(tokenizer); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return tokenizer
}

type parityVector struct {
	Text string `json:"text"`
	IDs  []int  `json:"ids"`
}

type parityMeta struct {
	Model        string `json:"model"`
	Revision     string `json:"revision"`
	Strings      int    `json:"strings"`
	IDs          int    `json:"ids"`
	DecodeCases  int    `json:"decode_cases"`
	Sha256       string `json:"sha256"`
	DecodeSha256 string `json:"decode_sha256"`
}

// loadParityMeta reads the generator's provenance file and checks that it names
// the pinned model and revision.
func loadParityMeta(t *testing.T) parityMeta {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "tokenizer_parity.meta.json"))
	if err != nil {
		t.Fatalf("reading the parity provenance: %v", err)
	}
	var meta parityMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("parsing the parity provenance: %v", err)
	}
	if meta.Revision != PinnedSnapshot || meta.Model != PinnedRepo {
		t.Fatalf("the vectors were generated from %s@%s, want %s@%s",
			meta.Model, meta.Revision, PinnedRepo, PinnedSnapshot)
	}
	return meta
}

// TestParityVectors is the test the whole tokenizer rests on: every id this
// package produces must equal the id Hugging Face produced for the same string.
func TestParityVectors(t *testing.T) {
	tokenizer := loadPinned(t)
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "tokenizer_parity.json"))
	if err != nil {
		t.Fatalf("reading the parity vectors: %v", err)
	}
	var vectors []parityVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parsing the parity vectors: %v", err)
	}
	// The counts and digest come from the generator's provenance file, so a
	// corpus that silently shrank is a failure rather than a smaller green run.
	meta := loadParityMeta(t)
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != meta.Sha256 {
		t.Fatalf("the parity fixture hashes to %s, the provenance records %s", got, meta.Sha256)
	}
	if len(vectors) != meta.Strings {
		t.Fatalf("the fixture has %d vectors, the provenance records %d", len(vectors), meta.Strings)
	}
	total := 0
	for _, vector := range vectors {
		total += len(vector.IDs)
	}
	if total != meta.IDs {
		t.Fatalf("the fixture has %d ids, the provenance records %d", total, meta.IDs)
	}

	mismatches := 0
	for _, vector := range vectors {
		got := tokenizer.Encode(vector.Text)
		if equalInts(got, vector.IDs) {
			continue
		}
		mismatches++
		if mismatches > 5 {
			continue
		}
		at, want, have := firstDifference(got, vector.IDs)
		text := vector.Text
		if len(text) > 90 {
			text = text[:90] + "..."
		}
		t.Errorf("Encode(%q): first difference at %d, got %d, want %d (got %d ids, want %d)",
			text, at, have, want, len(got), len(vector.IDs))
	}
	if mismatches > 0 {
		t.Fatalf("%d of %d vectors disagree with Hugging Face", mismatches, len(vectors))
	}
}

// TestParityCoversEveryCodebookCode checks the codes the readout scores, which
// is the property the answer boundary depends on: each is exactly one token.
func TestParityCoversEveryCodebookCode(t *testing.T) {
	tokenizer := loadPinned(t)
	raw, err := os.ReadFile(filepath.Join("..", "data", "codebook-de2.json"))
	if err != nil {
		t.Fatalf("reading the codebook: %v", err)
	}
	var file struct {
		Codes    []string       `json:"codes"`
		TokenIDs map[string]int `json:"token_ids"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parsing the codebook: %v", err)
	}
	closeID, ok := tokenizer.TokenID("<channel|>")
	if !ok {
		t.Fatal("<channel|> is not in the vocab")
	}
	if closeID != 101 {
		t.Errorf("<channel|> is token %d, want 101", closeID)
	}
	for _, code := range file.Codes {
		ids := tokenizer.Encode(code)
		if len(ids) != 1 {
			t.Fatalf("code %q is %d tokens, want 1: %v", code, len(ids), ids)
		}
		if want, ok := file.TokenIDs[code]; ok && ids[0] != want {
			t.Errorf("code %q is token %d, want %d", code, ids[0], want)
		}
		combined := tokenizer.Encode("<channel|>" + code)
		if len(combined) != 2 || combined[0] != closeID || combined[1] != ids[0] {
			t.Errorf("<channel|>%s is %v, want [%d %d]", code, combined, closeID, ids[0])
		}
	}
}

// TestDecode replays decode vectors from Hugging Face, including byte-fallback
// runs that Encode cannot produce.
func TestDecode(t *testing.T) {
	tokenizer := loadPinned(t)
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "tokenizer_decode_parity.json"))
	if err != nil {
		t.Fatalf("reading the decode vectors: %v", err)
	}
	var cases []struct {
		IDs  []int  `json:"ids"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parsing the decode vectors: %v", err)
	}
	// The digest and the case count come from the generator's provenance file,
	// so the decode corpus cannot shrink silently either.
	meta := loadParityMeta(t)
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != meta.DecodeSha256 {
		t.Fatalf("the decode fixture hashes to %s, the provenance records %s", got, meta.DecodeSha256)
	}
	if len(cases) != meta.DecodeCases {
		t.Fatalf("the decode fixture has %d cases, the provenance records %d", len(cases), meta.DecodeCases)
	}
	for _, vector := range cases {
		if got := tokenizer.Decode(vector.IDs); got != vector.Text {
			t.Errorf("Decode(%v) = %q, want %q", vector.IDs, got, vector.Text)
		}
	}
	// Round-tripping valid text is a separate property from matching the
	// decoder on arbitrary ids.
	for _, text := range []string{"A", "IW", "<channel|>", "hello world", "日本語", "🙂", "a\tb\nc", ""} {
		ids := tokenizer.Encode(text)
		if got := tokenizer.Decode(ids); got != text {
			t.Errorf("Decode(Encode(%q)) = %q", text, got)
		}
	}
}

// TestWidePromptIsNotQuadratic guards the merge implementation. The widest
// prompt the readout can send normalizes into one pre-token of tens of
// thousands of symbols, which a naive merge loop cannot handle.
func TestWidePromptIsNotQuadratic(t *testing.T) {
	tokenizer := loadPinned(t)
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "tokenizer_parity.json"))
	if err != nil {
		t.Fatalf("reading the parity vectors: %v", err)
	}
	var vectors []parityVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parsing the parity vectors: %v", err)
	}
	var widest parityVector
	for _, vector := range vectors {
		if len(vector.IDs) > len(widest.IDs) {
			widest = vector
		}
	}
	if len(widest.IDs) < 8000 {
		t.Fatalf("the widest vector has only %d ids; the fixture lost its widest prompt", len(widest.IDs))
	}
	started := time.Now()
	got := tokenizer.Encode(widest.Text)
	elapsed := time.Since(started)
	if !equalInts(got, widest.IDs) {
		t.Fatalf("the widest prompt does not match: got %d ids, want %d", len(got), len(widest.IDs))
	}
	if elapsed > 5*time.Second {
		t.Errorf("encoding %d ids took %v, which is too slow to be a linear-ish merge", len(got), elapsed)
	}
	t.Logf("encoded %d ids in %v", len(got), elapsed)
}

// The pinned pipeline stages, as the real tokenizer.json declares them. Tests
// build valid and invalid files from these so the rejection contract is checked
// against something that otherwise loads.
const (
	okNormalizer   = `"normalizer":{"type":"Replace","pattern":{"String":" "},"content":"\u2581"}`
	okPreTokenizer = `"pre_tokenizer":{"type":"Split","pattern":{"String":" "},"behavior":"MergedWithPrevious","invert":false}`
	okDecoder      = `"decoder":{"type":"Sequence","decoders":[{"type":"Replace","pattern":{"String":"\u2581"},"content":" "},{"type":"ByteFallback"},{"type":"Fuse"}]}`
	okModel        = `"model":{"type":"BPE","unk_token":"<unk>","vocab":{"<unk>":0,"a":1,"b":2,"ab":3},"merges":[["a","b"]]}`
)

func TestFromBytesRejectsWhatItCannotRead(t *testing.T) {
	valid := `{` + okNormalizer + `,` + okPreTokenizer + `,` + okDecoder + `,` + okModel + `}`
	if _, err := FromBytes([]byte(valid)); err != nil {
		t.Fatalf("the reference pipeline should load: %v", err)
	}

	cases := map[string]string{
		"model type":                       `{"model": {"type": "WordPiece", "vocab": {"a": 0}}}`,
		"empty vocab":                      `{` + okNormalizer + `,` + okPreTokenizer + `,` + okDecoder + `,"model":{"type":"BPE","vocab":{}}}`,
		"no unk and no byte fallback":      `{` + okNormalizer + `,` + okPreTokenizer + `,` + okDecoder + `,"model":{"type":"BPE","vocab":{"a":0}}}`,
		"byte fallback incomplete, no unk": `{` + okNormalizer + `,` + okPreTokenizer + `,` + okDecoder + `,"model":{"type":"BPE","byte_fallback":true,"vocab":{"a":0,"<0x41>":1}}}`,
		"no normalizer":                    `{` + okPreTokenizer + `,` + okDecoder + `,` + okModel + `}`,
		"regex normalizer":                 `{"normalizer": {"type": "Replace", "pattern": {"Regex": "\\s+"}}, "model": {"type": "BPE", "vocab": {"a": 0}}}`,
		"unknown normalizer":               `{"normalizer": {"type": "NFKC"}, "model": {"type": "BPE", "vocab": {"a": 0}}}`,
		"normalizer pattern":               `{"normalizer":{"type":"Replace","pattern":{"String":"\t"},"content":""},` + okPreTokenizer + `,` + okDecoder + `,` + okModel + `}`,
		"normalizer content":               `{"normalizer":{"type":"Replace","pattern":{"String":" "},"content":""},` + okPreTokenizer + `,` + okDecoder + `,` + okModel + `}`,
		"normalizer inverted":              `{"normalizer":{"type":"Replace","pattern":{"String":" "},"content":"\u2581","invert":true},` + okPreTokenizer + `,` + okDecoder + `,` + okModel + `}`,
		"pre-tokenizer behavior":           `{` + okNormalizer + `,"pre_tokenizer":{"type":"Split","pattern":{"String":" "},"behavior":"Isolated","invert":false},` + okDecoder + `,` + okModel + `}`,
		"pre-tokenizer inverted":           `{` + okNormalizer + `,"pre_tokenizer":{"type":"Split","pattern":{"String":" "},"behavior":"MergedWithPrevious","invert":true},` + okDecoder + `,` + okModel + `}`,
		"no decoder":                       `{` + okNormalizer + `,` + okPreTokenizer + `,` + okModel + `}`,
		"decoder strip":                    `{` + okNormalizer + `,` + okPreTokenizer + `,"decoder":{"type":"Strip","content":" ","start":1,"stop":0},` + okModel + `}`,
		"decoder reordered":                `{` + okNormalizer + `,` + okPreTokenizer + `,"decoder":{"type":"Sequence","decoders":[{"type":"ByteFallback"},{"type":"Fuse"}]},` + okModel + `}`,
		"dropout":                          `{` + okNormalizer + `,` + okPreTokenizer + `,` + okDecoder + `,"model":{"type":"BPE","dropout":0.1,"vocab":{"a":0}}}`,
		"negative id":                      `{` + okNormalizer + `,` + okPreTokenizer + `,` + okDecoder + `,"model":{"type":"BPE","vocab":{"a":-1}}}`,
		"duplicate id":                     `{` + okNormalizer + `,` + okPreTokenizer + `,` + okDecoder + `,"model":{"type":"BPE","vocab":{"a":1,"b":1}}}`,
		"merge missing concat":             `{` + okNormalizer + `,` + okPreTokenizer + `,` + okDecoder + `,"model":{"type":"BPE","vocab":{"a":0,"b":1},"merges":[["a","b"]]}}`,
		"merge missing left":               `{` + okNormalizer + `,` + okPreTokenizer + `,` + okDecoder + `,"model":{"type":"BPE","vocab":{"b":1,"ab":2},"merges":[["a","b"]]}}`,
		"added token id mismatch":          `{"added_tokens":[{"id":9,"content":"<unk>"}],` + okNormalizer + `,` + okPreTokenizer + `,` + okDecoder + `,` + okModel + `}`,
	}
	for name, body := range cases {
		_, err := FromBytes([]byte(body))
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		var unsupported *ErrUnsupported
		if !errors.As(err, &unsupported) {
			t.Errorf("%s: got %v, want *ErrUnsupported", name, err)
		}
	}
}

// TestFromBytesAcceptsALargeVocabID guards the reverse-vocabulary lookup: a
// sparse id used to size a slice from the largest id in the file.
func TestFromBytesAcceptsALargeVocabID(t *testing.T) {
	body := `{` + okNormalizer + `,` + okPreTokenizer + `,` + okDecoder + `,"model":{"type":"BPE","unk_token":"<unk>","vocab":{"<unk>":0,"a":1,"z":2000000000}}}`
	tokenizer, err := FromBytes([]byte(body))
	if err != nil {
		t.Fatalf("a sparse id should load: %v", err)
	}
	if got := tokenizer.Decode([]int{2000000000}); got != "z" {
		t.Errorf("Decode([2000000000]) = %q, want %q", got, "z")
	}
}

// TestFuseUnkDoesNotTouchAddedTokens is the regression for the round-1 fuse_unk
// bug: fusing over the assembled output collapsed adjacent explicit <unk>
// tokens. The model here has fuse_unk on and no byte fallback, so unk is both
// producible by the model and present as an added token.
func TestFuseUnkDoesNotTouchAddedTokens(t *testing.T) {
	body := `{"added_tokens":[{"id":0,"content":"<unk>"}],` + okNormalizer + `,` + okPreTokenizer + `,` + okDecoder +
		`,"model":{"type":"BPE","unk_token":"<unk>","fuse_unk":true,"vocab":{"<unk>":0,"a":1},"merges":[]}}`
	tokenizer, err := FromBytes([]byte(body))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	cases := []struct {
		text string
		want []int
	}{
		{"aa", []int{1, 1}},         // known characters are untouched
		{"xy", []int{0}},            // two unknown characters fuse to one unk
		{"<unk><unk>", []int{0, 0}}, // added tokens are never fused
		{"x<unk>", []int{0, 0}},     // a fused model unk stays separate from an added one
		{"xy<unk>xy", []int{0, 0, 0}},
	}
	for _, tc := range cases {
		if got := tokenizer.Encode(tc.text); !equalInts(got, tc.want) {
			t.Errorf("Encode(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

// TestByteFallbackIsAllOrNothing checks that a character whose byte tokens are
// incomplete falls back to unk as a whole, which is what Hugging Face does.
func TestByteFallbackIsAllOrNothing(t *testing.T) {
	body := `{` + okNormalizer + `,` + okPreTokenizer + `,` + okDecoder + `,"model":{"type":"BPE","unk_token":"<unk>","byte_fallback":true,"fuse_unk":true,"vocab":{"<unk>":0,"<0xC3>":1,"<0xA9>":2,"a":3},"merges":[]}}`
	tokenizer, err := FromBytes([]byte(body))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if got := tokenizer.Encode("é"); !equalInts(got, []int{1, 2}) {
		t.Errorf("Encode(\"é\") = %v, want [1 2]", got)
	}

	partial := `{` + okNormalizer + `,` + okPreTokenizer + `,` + okDecoder + `,"model":{"type":"BPE","unk_token":"<unk>","byte_fallback":true,"fuse_unk":true,"vocab":{"<unk>":0,"<0xC3>":1,"a":2},"merges":[]}}`
	tokenizer, err = FromBytes([]byte(partial))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if got := tokenizer.Encode("é"); !equalInts(got, []int{0}) {
		t.Errorf("Encode(\"é\") with a missing byte token = %v, want [0]", got)
	}
}

func TestFromBytesRejectsAnAddedTokenOutsideTheVocab(t *testing.T) {
	body := `{"added_tokens": [{"id": 5, "content": "<|nope|>"}],
	          "model": {"type": "BPE", "vocab": {"a": 0}}}`
	if _, err := FromBytes([]byte(body)); err == nil {
		t.Fatal("expected an error for an added token that is not in the vocab")
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// firstDifference reports the first index where got and want differ, and the
// two values there. One slice may be shorter than the other.
func firstDifference(got, want []int) (at, wantValue, gotValue int) {
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			return i, want[i], got[i]
		}
	}
	if len(got) < len(want) {
		return len(got), want[len(got)], -1
	}
	return len(want), -1, got[len(want)]
}

func BenchmarkEncodeWidePrompt(b *testing.B) {
	tokenizer := loadPinnedBench(b)
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "tokenizer_parity.json"))
	if err != nil {
		b.Fatalf("reading the parity vectors: %v", err)
	}
	var vectors []parityVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		b.Fatalf("parsing the parity vectors: %v", err)
	}
	var widest parityVector
	for _, vector := range vectors {
		if len(vector.IDs) > len(widest.IDs) {
			widest = vector
		}
	}
	b.SetBytes(int64(len(widest.Text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tokenizer.Encode(widest.Text)
	}
}

func loadPinnedBench(b *testing.B) *Tokenizer {
	b.Helper()
	tokenizer, path, err := LoadPinned(PinnedRepo, PinnedSnapshot)
	if errors.Is(err, ErrNoPinnedTokenizer) {
		if os.Getenv(RequireEnv) == "1" {
			b.Fatalf("no tokenizer.json found and %s=1", RequireEnv)
		}
		b.Skip("no tokenizer.json found")
	}
	if err != nil {
		b.Fatalf("loading the pinned tokenizer: %v", err)
	}
	if err := VerifyPinned(tokenizer); err != nil {
		b.Fatalf("%s: %v", path, err)
	}
	return tokenizer
}

// TestLocalFilesOnlyNeverFetches checks the policy the readout's
// `LocalFilesOnly` turns on: a source that is not a readable file and not in the
// cache is refused rather than downloaded. The test would need network access to
// fail in the wrong direction, so it asserts the error identity and that the
// repository id is one the hub could not have cached.
func TestLocalFilesOnlyNeverFetches(t *testing.T) {
	missing := "shisa-ai/this-repository-does-not-exist-local-only-test@0000000000000000000000000000000000000000"
	_, err := ResolveWith(missing, ResolveOptions{LocalFilesOnly: true})
	if err == nil {
		t.Fatal("a source that is not cached should not resolve with LocalFilesOnly")
	}
	if !errors.Is(err, ErrNotCached) {
		t.Errorf("the error is %v, want ErrNotCached", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("the error is %v, want one naming the source", err)
	}

	// A file on disk resolves either way, because no fetch is involved.
	path := filepath.Join(t.TempDir(), "tokenizer.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	resolved, err := ResolveWith(path, ResolveOptions{LocalFilesOnly: true})
	if err != nil {
		t.Fatalf("ResolveWith(%s): %v", path, err)
	}
	if resolved != path {
		t.Errorf("resolved to %q, want %q", resolved, path)
	}

	// The pinned repository is in the cache on a machine that has run the
	// parity suite, so local-only resolution must not change what it finds.
	if _, err := ResolveWith(PinnedRepo+"@"+PinnedSnapshot, ResolveOptions{LocalFilesOnly: true}); err != nil {
		if !errors.Is(err, ErrNotCached) {
			t.Errorf("the pinned repository gave %v, want a path or ErrNotCached", err)
		}
	}
}
