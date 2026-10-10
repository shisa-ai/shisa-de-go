// Package tokenizer implements the Hugging Face BPE tokenizer the DE-2 readout
// needs, in pure Go.
//
// It implements exactly the pinned pipeline and nothing else: the pinned
// tokenizer's normalizer, pre-tokenizer and decoder are checked against the
// file, and a file that declares anything different is rejected rather than
// approximated. Truncation, padding, offsets and training are not implemented.
// The post-processor is parsed but never applied, which matches encoding with
// add_special_tokens=False.
//
// The pipeline is Hugging Face's: the raw text is split on the added tokens,
// each remaining piece is normalized, pre-tokenized, and merged with the BPE
// merge table. Nothing is cached across calls, so a Tokenizer is safe for
// concurrent use once loaded.
package tokenizer

import (
	"encoding/json"
	"fmt"
	"math"
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
	byID   map[int32]string
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
	Decoder      json.RawMessage `json:"decoder"`
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

// decoderSpec covers the decoder stage. Only the pinned Sequence is understood.
type decoderSpec struct {
	Type    string `json:"type"`
	Pattern struct {
		String string `json:"String"`
		Regex  string `json:"Regex"`
	} `json:"pattern"`
	Content  string        `json:"content"`
	Decoders []decoderSpec `json:"decoders"`
}

type modelSpec struct {
	Type                    string            `json:"type"`
	UnkToken                *string           `json:"unk_token"`
	ByteFallback            bool              `json:"byte_fallback"`
	FuseUnk                 bool              `json:"fuse_unk"`
	IgnoreMerges            bool              `json:"ignore_merges"`
	Dropout                 *float64          `json:"dropout"`
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
	if file.Model.Dropout != nil && *file.Model.Dropout != 0 {
		return nil, &ErrUnsupported{What: fmt.Sprintf(
			"BPE dropout %v; encoding would not be deterministic", *file.Model.Dropout)}
	}
	if err := checkNormalizer(file.Normalizer); err != nil {
		return nil, err
	}
	if err := checkPreTokenizer(file.PreTokenizer); err != nil {
		return nil, err
	}
	if err := checkDecoder(file.Decoder); err != nil {
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
		byID:         make(map[int32]string, len(file.Model.Vocab)),
		merges:       make(map[pair]merge, len(file.Model.Merges)),
		fuseUnk:      file.Model.FuseUnk,
		ignoreMerges: file.Model.IgnoreMerges,
		byteFallback: file.Model.ByteFallback,
	}

	// Ids are checked before anything is read from them. A negative id is a file
	// Hugging Face refuses, and a shared id would make Decode depend on map
	// iteration order.
	for token, id := range t.vocab {
		if id < 0 {
			return nil, &ErrUnsupported{What: fmt.Sprintf("vocab id %d for %q", id, token)}
		}
		if other, ok := t.byID[id]; ok {
			return nil, &ErrUnsupported{What: fmt.Sprintf(
				"vocab id %d is shared by %q and %q", id, other, token)}
		}
		t.byID[id] = token
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
	// A character that is neither in the vocab nor fully byte-coverable has to
	// become something. Without a resolved unk token it would be dropped, and
	// Encode would silently return fewer ids than the text implies.
	if !t.hasUnk {
		if !t.byteFallback {
			return nil, &ErrUnsupported{What: "neither a resolvable unk_token nor byte fallback; " +
				"an unknown character would produce no token"}
		}
		for b := 0; b < 256; b++ {
			if !t.byteOK[b] {
				return nil, &ErrUnsupported{What: "byte fallback with an incomplete byte " +
					"vocabulary and no resolvable unk_token"}
			}
		}
	}

	// Merges are ranked by position in the file. An entry whose operands or
	// concatenation is missing from the vocab is a malformed file: Hugging Face
	// refuses it, and dropping the rule here would silently read a different
	// tokenizer.
	for rank, raw := range file.Model.Merges {
		a, b, err := parseMerge(raw)
		if err != nil {
			return nil, fmt.Errorf("tokenizer: merge %d: %w", rank, err)
		}
		idA, okA := t.vocab[a]
		if !okA {
			return nil, &ErrUnsupported{What: fmt.Sprintf("merge %d: %q is not in the vocab", rank, a)}
		}
		idB, okB := t.vocab[b]
		if !okB {
			return nil, &ErrUnsupported{What: fmt.Sprintf("merge %d: %q is not in the vocab", rank, b)}
		}
		merged, ok := t.vocab[a+b]
		if !ok {
			return nil, &ErrUnsupported{What: fmt.Sprintf("merge %d: %q is not in the vocab", rank, a+b)}
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

// checkNormalizer requires the pinned normalizer exactly. Anything else is
// rejected: this package hardcodes the replacement below, so accepting a
// different normalizer would read the file as if it were the pinned one.
func checkNormalizer(spec stageSpec) error {
	if spec.Type != "Replace" {
		return &ErrUnsupported{What: fmt.Sprintf("normalizer type %q; want Replace", spec.Type)}
	}
	if spec.Pattern.Regex != "" || spec.Pattern.String != " " {
		return &ErrUnsupported{What: "a normalizer pattern other than the literal \" \""}
	}
	if spec.Content != wordBoundary {
		return &ErrUnsupported{What: fmt.Sprintf("normalizer content %q; want %q", spec.Content, wordBoundary)}
	}
	if spec.Invert {
		return &ErrUnsupported{What: "an inverted normalizer"}
	}
	return nil
}

// checkPreTokenizer requires the pinned Split exactly, for the same reason.
func checkPreTokenizer(spec stageSpec) error {
	if spec.Type != "Split" {
		return &ErrUnsupported{What: fmt.Sprintf("pre-tokenizer type %q; want Split", spec.Type)}
	}
	if spec.Pattern.Regex != "" || spec.Pattern.String != " " {
		return &ErrUnsupported{What: "a pre-tokenizer pattern other than the literal \" \""}
	}
	if spec.Behavior != "MergedWithPrevious" {
		return &ErrUnsupported{What: fmt.Sprintf("pre-tokenizer behavior %q", spec.Behavior)}
	}
	if spec.Invert {
		return &ErrUnsupported{What: "an inverted pre-tokenizer"}
	}
	return nil
}

// checkDecoder requires the pinned decoder exactly. Decode implements that
// chain, so a file declaring another one is rejected rather than decoded wrong.
func checkDecoder(raw json.RawMessage) error {
	if len(raw) == 0 {
		return &ErrUnsupported{What: "a tokenizer with no decoder"}
	}
	var spec decoderSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("tokenizer: parsing the decoder: %w", err)
	}
	if spec.Type != "Sequence" || len(spec.Decoders) != 3 {
		return &ErrUnsupported{What: fmt.Sprintf(
			"decoder %q with %d stages; want Sequence[Replace, ByteFallback, Fuse]",
			spec.Type, len(spec.Decoders))}
	}
	replace, byteFallback, fuse := spec.Decoders[0], spec.Decoders[1], spec.Decoders[2]
	if replace.Type != "Replace" || replace.Pattern.Regex != "" ||
		replace.Pattern.String != wordBoundary || replace.Content != " " {
		return &ErrUnsupported{What: "a decoder whose first stage is not Replace(word-boundary -> space)"}
	}
	if byteFallback.Type != "ByteFallback" || fuse.Type != "Fuse" {
		return &ErrUnsupported{What: "a decoder that is not Sequence[Replace, ByteFallback, Fuse]"}
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
// It applies the pinned decoder: the word-boundary marker becomes a space, and
// a run of consecutive byte-fallback tokens is decoded the way Hugging Face's
// ByteFallback does. That last part is not a plain concatenation: if the whole
// run is valid UTF-8 it becomes that text, and otherwise each byte becomes one
// replacement character. Ids outside the vocabulary are skipped.
func (t *Tokenizer) Decode(ids []int) string {
	var out strings.Builder
	var pending []byte
	flush := func() {
		if len(pending) == 0 {
			return
		}
		if utf8.Valid(pending) {
			out.Write(pending)
		} else {
			for range pending {
				out.WriteRune(utf8.RuneError)
			}
		}
		pending = pending[:0]
	}
	for _, id := range ids {
		if id < 0 || id > math.MaxInt32 {
			continue
		}
		token, ok := t.byID[int32(id)]
		if !ok {
			continue
		}
		if value, ok := byteToken(token); ok {
			pending = append(pending, value)
			continue
		}
		flush()
		out.WriteString(strings.ReplaceAll(token, wordBoundary, " "))
	}
	flush()
	return out.String()
}

// byteToken reports whether a vocab token is a byte-fallback token like
// <0x41>, and its byte. Hugging Face accepts lowercase hex here too.
func byteToken(token string) (byte, bool) {
	if len(token) != 6 || token[0] != '<' || token[1] != '0' || token[2] != 'x' || token[5] != '>' {
		return 0, false
	}
	value, err := strconv.ParseUint(token[3:5], 16, 8)
	if err != nil {
		return 0, false
	}
	return byte(value), true
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
// characters fall back to their bytes, and the symbols are merged. fuse_unk is
// applied here, to this pre-token's model output only, which is where Hugging
// Face applies it: added tokens never reach this function and are never fused.
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
			if bytes, ok := t.byteFallbackIDs(sub); ok {
				symbols = append(symbols, bytes...)
			} else if t.hasUnk {
				symbols = append(symbols, t.unkID)
			}
		} else if t.hasUnk {
			symbols = append(symbols, t.unkID)
		}
		i += size
	}
	if len(symbols) == 0 {
		return out
	}
	symbols = t.merge(symbols)
	if t.fuseUnk && t.hasUnk {
		symbols = fuseUnknowns(symbols, t.unkID)
	}
	return append(out, symbols...)
}

// byteFallbackIDs returns the byte-token ids for one character, or false when
// any of its bytes has no byte token. Hugging Face falls back for the whole
// character or not at all, so a partial run is never produced.
func (t *Tokenizer) byteFallbackIDs(char string) ([]int32, bool) {
	ids := make([]int32, len(char))
	for i := 0; i < len(char); i++ {
		if !t.byteOK[char[i]] {
			return nil, false
		}
		ids[i] = t.byteID[char[i]]
	}
	return ids, true
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
		id, ok := vocab[token.Content]
		if !ok {
			return nil, fmt.Errorf("tokenizer: added token %q is not in the vocab", token.Content)
		}
		if id != token.ID {
			return nil, &ErrUnsupported{What: fmt.Sprintf(
				"added token %q declares id %d but the vocab has %d", token.Content, token.ID, id)}
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
