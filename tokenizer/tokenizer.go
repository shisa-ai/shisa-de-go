// Package tokenizer implements the Hugging Face BPE tokenizer the DE-2 readout
// needs, in pure Go.
//
// It implements exactly what the readout uses: encoding a rendered prompt to
// token ids, and decoding ids back to text for the answer-boundary check.
// Truncation, padding, offsets, post-processing and training are not
// implemented. A tokenizer file that needs one of those, or a pattern type this
// package does not implement, is rejected rather than approximated.
//
// The pipeline is Hugging Face's: the raw text is split on the added tokens,
// each remaining piece is normalized, pre-tokenized, and merged with the BPE
// merge table. Nothing is cached across calls, so a Tokenizer is safe for
// concurrent use once loaded.
package tokenizer

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrUnsupported reports a tokenizer file this package cannot read faithfully.
type ErrUnsupported struct {
	What string
}

func (e *ErrUnsupported) Error() string { return "tokenizer: unsupported: " + e.What }

// pair is two adjacent symbol ids, the key of the merge table.
type pair struct{ a, b int32 }

// merge is one entry of the merge table: its rank, and the id it produces.
type merge struct {
	rank int32
	id   int32
}

// Tokenizer is a loaded Hugging Face BPE tokenizer.
type Tokenizer struct {
	vocab  map[string]int32
	byID   []string
	merges map[pair]merge
	added  *addedVocab

	unkID        int32
	hasUnk       bool
	byteFallback bool
	fuseUnk      bool
	ignoreMerges bool

	byteID [256]int32
	byteOK [256]bool
}

// -- the tokenizer.json shapes we read ---------------------------------------

type tokenizerFile struct {
	Version      string          `json:"version"`
	AddedTokens  []addedToken    `json:"added_tokens"`
	Normalizer   stageSpec       `json:"normalizer"`
	PreTokenizer stageSpec       `json:"pre_tokenizer"`
	Model        modelSpec       `json:"model"`
	PostProcess  json.RawMessage `json:"post_processor"`
}

// stageSpec covers the tagged-union stages. Only the literal-string pattern the
// pinned tokenizer uses is understood.
type stageSpec struct {
	Type    string `json:"type"`
	Pattern struct {
		String string `json:"String"`
		Regex  string `json:"Regex"`
	} `json:"pattern"`
	Content  string `json:"content"`
	Behavior string `json:"behavior"`
	Invert   bool   `json:"invert"`
}

type modelSpec struct {
	Type                    string            `json:"type"`
	UnkToken                *string           `json:"unk_token"`
	ByteFallback            bool              `json:"byte_fallback"`
	FuseUnk                 bool              `json:"fuse_unk"`
	IgnoreMerges            bool              `json:"ignore_merges"`
	ContinuingSubwordPrefix *string           `json:"continuing_subword_prefix"`
	EndOfWordSuffix         *string           `json:"end_of_word_suffix"`
	Vocab                   map[string]int32  `json:"vocab"`
	Merges                  []json.RawMessage `json:"merges"`
}

type addedToken struct {
	ID         int32  `json:"id"`
	Content    string `json:"content"`
	SingleWord bool   `json:"single_word"`
	Lstrip     bool   `json:"lstrip"`
	Rstrip     bool   `json:"rstrip"`
	Normalized bool   `json:"normalized"`
	Special    bool   `json:"special"`
}

// FromFile reads a tokenizer.json from disk.
func FromFile(path string) (*Tokenizer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tokenizer: reading %s: %w", path, err)
	}
	return FromBytes(data)
}

// FromBytes parses a tokenizer.json.
func FromBytes(data []byte) (*Tokenizer, error) {
	var file tokenizerFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("tokenizer: parsing tokenizer.json: %w", err)
	}
	if file.Model.Type != "BPE" {
		return nil, &ErrUnsupported{What: fmt.Sprintf(
			"model type %q; this package implements BPE only", file.Model.Type)}
	}
	if err := checkNormalizer(file.Normalizer); err != nil {
		return nil, err
	}
	if err := checkPreTokenizer(file.PreTokenizer); err != nil {
		return nil, err
	}
	if file.Model.ContinuingSubwordPrefix != nil || file.Model.EndOfWordSuffix != nil {
		return nil, &ErrUnsupported{What: "continuing_subword_prefix or end_of_word_suffix"}
	}
	if len(file.Model.Vocab) == 0 {
		return nil, &ErrUnsupported{What: "an empty vocab"}
	}

	t := &Tokenizer{
		vocab:        file.Model.Vocab,
		merges:       make(map[pair]merge, len(file.Model.Merges)),
		fuseUnk:      file.Model.FuseUnk,
		ignoreMerges: file.Model.IgnoreMerges,
		byteFallback: file.Model.ByteFallback,
	}

	if file.Model.UnkToken != nil {
		if id, ok := t.vocab[*file.Model.UnkToken]; ok {
			t.unkID, t.hasUnk = id, true
		}
	}
	if t.byteFallback {
		for b := 0; b < 256; b++ {
			token := fmt.Sprintf("<0x%02X>", b)
			if id, ok := t.vocab[token]; ok {
				t.byteID[b], t.byteOK[b] = id, true
			}
		}
	}

	maxID := int32(-1)
	for _, id := range t.vocab {
		if id > maxID {
			maxID = id
		}
	}
	for _, a := range file.AddedTokens {
		if a.ID > maxID {
			maxID = a.ID
		}
	}
	t.byID = make([]string, maxID+1)
	for token, id := range t.vocab {
		if id >= 0 {
			t.byID[id] = token
		}
	}

	// Merges are ranked by position in the file. A merge whose concatenation is
	// not in the vocab cannot be produced, so it is dropped rather than guessed.
	for rank, raw := range file.Model.Merges {
		a, b, err := parseMerge(raw)
		if err != nil {
			return nil, fmt.Errorf("tokenizer: merge %d: %w", rank, err)
		}
		idA, okA := t.vocab[a]
		idB, okB := t.vocab[b]
		if !okA || !okB {
			continue
		}
		merged, ok := t.vocab[a+b]
		if !ok {
			continue
		}
		t.merges[pair{idA, idB}] = merge{rank: int32(rank), id: merged}
	}

	added, err := newAddedVocab(file.AddedTokens, t.vocab)
	if err != nil {
		return nil, err
	}
	t.added = added
	return t, nil
}

// parseMerge accepts both merge encodings: the older "a b" string and the newer
// two-element array.
func parseMerge(raw json.RawMessage) (string, string, error) {
	var parts []string
	if err := json.Unmarshal(raw, &parts); err == nil {
		if len(parts) != 2 {
			return "", "", fmt.Errorf("expected a pair, got %d parts", len(parts))
		}
		return parts[0], parts[1], nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", "", fmt.Errorf("not a pair or a string: %s", raw)
	}
	a, b, ok := strings.Cut(s, " ")
	if !ok {
		return "", "", fmt.Errorf("expected two space-separated tokens, got %q", s)
	}
	return a, b, nil
}

func checkNormalizer(spec stageSpec) error {
	if spec.Type == "" {
		return nil
	}
	if spec.Type != "Replace" {
		return &ErrUnsupported{What: fmt.Sprintf("normalizer type %q", spec.Type)}
	}
	if spec.Pattern.Regex != "" {
		return &ErrUnsupported{What: "a regex normalizer pattern"}
	}
	if spec.Pattern.String == "" {
		return &ErrUnsupported{What: "a normalizer with no literal pattern"}
	}
	return nil
}

func checkPreTokenizer(spec stageSpec) error {
	if spec.Type == "" {
		return nil
	}
	if spec.Type != "Split" {
		return &ErrUnsupported{What: fmt.Sprintf("pre-tokenizer type %q", spec.Type)}
	}
	if spec.Pattern.Regex != "" {
		return &ErrUnsupported{What: "a regex pre-tokenizer pattern"}
	}
	if spec.Pattern.String == "" {
		return &ErrUnsupported{What: "a pre-tokenizer with no literal pattern"}
	}
	if spec.Behavior != "MergedWithPrevious" && spec.Behavior != "Isolated" &&
		spec.Behavior != "Removed" && spec.Behavior != "Contiguous" {
		return &ErrUnsupported{What: fmt.Sprintf("pre-tokenizer behavior %q", spec.Behavior)}
	}
	return nil
}

// -- the public surface ------------------------------------------------------

// Len is the number of entries in the vocabulary.
func (t *Tokenizer) Len() int { return len(t.vocab) }

// TokenID resolves a token's text to its id.
func (t *Tokenizer) TokenID(token string) (int, bool) {
	id, ok := t.vocab[token]
	return int(id), ok
}

// Encode turns text into token ids, without adding special tokens. The
// tokenizer's post-processor is not applied, which is what the readout wants:
// the prompt is rendered in full, and no template token is added here.
func (t *Tokenizer) Encode(text string) []int {
	ids := t.encodeToIDs(text)
	out := make([]int, len(ids))
	for i, id := range ids {
		out[i] = int(id)
	}
	return out
}

// Decode turns token ids back into text.
//
// It applies the decoder the pinned tokenizer uses: the word-boundary marker
// becomes a space, and a byte-fallback token becomes its byte. Ids outside the
// vocabulary are skipped.
func (t *Tokenizer) Decode(ids []int) string {
	var b []byte
	for _, id := range ids {
		if id < 0 || id >= len(t.byID) {
			continue
		}
		token := t.byID[id]
		if len(token) == 6 && token[0] == '<' && token[1] == '0' && token[2] == 'x' && token[5] == '>' {
			if v, err := strconv.ParseUint(token[3:5], 16, 8); err == nil {
				b = append(b, byte(v))
				continue
			}
		}
		b = append(b, strings.ReplaceAll(token, wordBoundary, " ")...)
	}
	return string(b)
}

const wordBoundary = "\u2581"

// -- the pipeline ------------------------------------------------------------

func (t *Tokenizer) encodeToIDs(text string) []int32 {
	var out []int32
	start := 0
	for i := 0; i < len(text); {
		if tok, ok := t.added.match(text, i); ok {
			if i > start {
				out = t.encodePiece(text[start:i], out)
			}
			out = append(out, tok.ID)
			i += len(tok.Content)
			start = i
			continue
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		i += size
	}
	if start < len(text) {
		out = t.encodePiece(text[start:], out)
	}
	if t.fuseUnk && t.hasUnk {
		out = fuseUnknowns(out, t.unkID)
	}
	return out
}

// encodePiece normalizes, pre-tokenizes and merges one run of text between
// added tokens.
func (t *Tokenizer) encodePiece(piece string, out []int32) []int32 {
	piece = t.normalize(piece)
	for _, part := range t.preTokenize(piece) {
		out = t.encodePart(part, out)
	}
	return out
}

// normalize applies the tokenizer's normalizer: the word-boundary replacement
// the pinned file carries.
func (t *Tokenizer) normalize(piece string) string {
	return strings.ReplaceAll(piece, " ", wordBoundary)
}

// preTokenize splits a normalized piece on the pre-tokenizer's literal pattern.
//
// The pinned tokenizer replaces every space with the word-boundary marker before
// this runs, so nothing is left to split on and the whole piece is returned.
// The split is implemented anyway, because the pinned file declares it.
func (t *Tokenizer) preTokenize(piece string) []string {
	const pattern = " "
	if piece == "" {
		return nil
	}
	if !strings.Contains(piece, pattern) {
		return []string{piece}
	}
	var parts []string
	rest := piece
	for {
		at := strings.Index(rest, pattern)
		if at < 0 {
			parts = append(parts, rest)
			return parts
		}
		// MergedWithPrevious: the delimiter joins the piece before it, and a
		// leading delimiter is a piece of its own.
		if at == 0 {
			parts = append(parts, pattern)
		} else {
			parts = append(parts, rest[:at+len(pattern)])
		}
		rest = rest[at+len(pattern):]
		if rest == "" {
			return parts
		}
	}
}

// encodePart turns one pre-token into ids: characters become symbols, unknown
// characters fall back to their bytes, and the symbols are merged.
func (t *Tokenizer) encodePart(part string, out []int32) []int32 {
	if part == "" {
		return out
	}
	if t.ignoreMerges {
		if id, ok := t.vocab[part]; ok {
			return append(out, id)
		}
	}
	symbols := make([]int32, 0, utf8.RuneCountInString(part))
	for i := 0; i < len(part); {
		_, size := utf8.DecodeRuneInString(part[i:])
		sub := part[i : i+size]
		if id, ok := t.vocab[sub]; ok {
			symbols = append(symbols, id)
		} else if t.byteFallback {
			for b := 0; b < size; b++ {
				if t.byteOK[part[i+b]] {
					symbols = append(symbols, t.byteID[part[i+b]])
				} else if t.hasUnk {
					symbols = append(symbols, t.unkID)
				}
			}
		} else if t.hasUnk {
			symbols = append(symbols, t.unkID)
		}
		i += size
	}
	if len(symbols) == 0 {
		return out
	}
	return append(out, t.merge(symbols)...)
}

// fuseUnknowns collapses a run of unknown tokens into one, which is what the
// tokenizer's fuse_unk asks for.
func fuseUnknowns(ids []int32, unk int32) []int32 {
	out := ids[:0]
	previousUnk := false
	for _, id := range ids {
		if id == unk {
			if previousUnk {
				continue
			}
			previousUnk = true
		} else {
			previousUnk = false
		}
		out = append(out, id)
	}
	return out
}

// -- the added vocabulary ----------------------------------------------------

// addedVocab matches the added tokens in the raw text, before normalization.
// The longest match at a position wins, which is what makes <|tool> not swallow
// <|tool_call>.
type addedVocab struct {
	byFirst [256][]addedToken
}

func newAddedVocab(tokens []addedToken, vocab map[string]int32) (*addedVocab, error) {
	a := &addedVocab{}
	for _, token := range tokens {
		if token.Content == "" {
			return nil, &ErrUnsupported{What: "an added token with empty content"}
		}
		if _, ok := vocab[token.Content]; !ok {
			return nil, fmt.Errorf("tokenizer: added token %q is not in the vocab", token.Content)
		}
		if token.Normalized || token.Lstrip || token.Rstrip || token.SingleWord {
			return nil, &ErrUnsupported{What: fmt.Sprintf(
				"added token %q with normalized, lstrip, rstrip or single_word set", token.Content)}
		}
		a.byFirst[token.Content[0]] = append(a.byFirst[token.Content[0]], token)
	}
	for b := range a.byFirst {
		group := a.byFirst[b]
		sort.SliceStable(group, func(i, j int) bool { return len(group[i].Content) > len(group[j].Content) })
	}
	return a, nil
}

// match returns the longest added token starting at offset i.
func (a *addedVocab) match(text string, i int) (addedToken, bool) {
	for _, token := range a.byFirst[text[i]] {
		if strings.HasPrefix(text[i:], token.Content) {
			return token, true
		}
	}
	return addedToken{}, false
}
