package tokenizer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The pinned tokenizer, as recorded in data/codebook-de2.json.
const (
	pinnedRepo     = "shisa-ai/shisa-de-2"
	pinnedSnapshot = "8f21734b27b3462471c4662d5638e52195b005a3"
)

// loadPinned finds the tokenizer.json the parity vectors were produced with.
//
// The file is 32 MB, so it is not checked in. The test looks in the places a
// developer or CI would have it, then skips. A skip is not a pass: the caller
// has to notice.
func loadPinned(t *testing.T) *Tokenizer {
	t.Helper()
	candidates := []string{}
	if path := os.Getenv("SHISA_DE_TOKENIZER_JSON"); path != "" {
		candidates = append(candidates, path)
	}
	candidates = append(candidates, filepath.Join("testdata", "tokenizer.json"))
	for _, revision := range []string{pinnedSnapshot, "main"} {
		if path, ok := findInHubCache(pinnedRepo, revision); ok {
			candidates = append(candidates, path)
		}
	}
	for _, path := range candidates {
		if !exists(path) {
			continue
		}
		tokenizer, err := FromFile(path)
		if err != nil {
			t.Fatalf("loading %s: %v", path, err)
		}
		return tokenizer
	}
	if os.Getenv("SHISA_DE_TOKENIZER_DOWNLOAD") == "1" {
		tokenizer, err := Load(pinnedRepo + "@" + pinnedSnapshot)
		if err != nil {
			t.Fatalf("downloading the pinned tokenizer: %v", err)
		}
		return tokenizer
	}
	t.Skipf("no tokenizer.json found; set SHISA_DE_TOKENIZER_JSON, or "+
		"SHISA_DE_TOKENIZER_DOWNLOAD=1 to fetch %s@%s", pinnedRepo, pinnedSnapshot)
	return nil
}

type parityVector struct {
	Text string `json:"text"`
	IDs  []int  `json:"ids"`
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
	if len(vectors) == 0 {
		t.Fatal("the parity vectors are empty")
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

func TestDecode(t *testing.T) {
	tokenizer := loadPinned(t)
	cases := []string{"A", "IW", "<channel|>", "hello world", "日本語", "🙂", "a\tb\nc", ""}
	for _, text := range cases {
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

func TestFromBytesRejectsWhatItCannotRead(t *testing.T) {
	cases := map[string]string{
		"model type":         `{"model": {"type": "WordPiece", "vocab": {"a": 0}}}`,
		"regex normalizer":   `{"normalizer": {"type": "Replace", "pattern": {"Regex": "\\s+"}}, "model": {"type": "BPE", "vocab": {"a": 0}}}`,
		"unknown normalizer": `{"normalizer": {"type": "NFKC"}, "model": {"type": "BPE", "vocab": {"a": 0}}}`,
		"empty vocab":        `{"model": {"type": "BPE", "vocab": {}}}`,
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
	for _, revision := range []string{pinnedSnapshot, "main"} {
		if path, ok := findInHubCache(pinnedRepo, revision); ok {
			tokenizer, err := FromFile(path)
			if err != nil {
				b.Fatalf("loading %s: %v", path, err)
			}
			return tokenizer
		}
	}
	if path := os.Getenv("SHISA_DE_TOKENIZER_JSON"); path != "" && exists(path) {
		tokenizer, err := FromFile(path)
		if err != nil {
			b.Fatalf("loading %s: %v", path, err)
		}
		return tokenizer
	}
	b.Skip("no tokenizer.json found")
	return nil
}
