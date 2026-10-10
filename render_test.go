package shisade

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shisa-ai/shisa-de-go/tokenizer"
)

// The pinned tokenizer is the one the render fixtures and the parity vectors
// were generated from: scripts/gen_tokenizer_parity.py, model
// shisa-ai/shisa-de-2, revision 8f21734b27b3462471c4662d5638e52195b005a3
// (tokenizer.PinnedRepo/PinnedSnapshot). The codebook's token_ids were audited
// on google/gemma-4-26B-A4B-it@4d7ae4984b7db7de8f8457170b3f1a419ee76d52
// (data/codebook-de2.json).

type renderFixture struct {
	Renders       []renderCase        `json:"renders"`
	Serialization []serializationCase `json:"serialization"`
}

type renderCase struct {
	Name         string            `json:"name"`
	Kind         string            `json:"kind"`
	Instructions string            `json:"instructions"`
	Criteria     []criterion       `json:"criteria"`
	Levels       []json.RawMessage `json:"levels"`
	State        json.RawMessage   `json:"state"`
	Repeat       int               `json:"repeat"`
	Thinking     bool              `json:"thinking"`
	OptionLimit  int               `json:"option_limit"`
	PayloadJSON  string            `json:"payload_json"`
	Prompt       string            `json:"prompt"`
	Tokens       []int             `json:"tokens"`
}

type criterion struct {
	Key         string          `json:"key"`
	Description json.RawMessage `json:"description"`
}

type serializationCase struct {
	Name string `json:"name"`
	JSON string `json:"json"`
}

// renderMeta is the provenance scripts/gen_render_parity.py records beside the
// fixture: the snapshot it was generated from, and the counts and digest of the
// bytes it wrote.
type renderMeta struct {
	Model         string `json:"model"`
	Revision      string `json:"revision"`
	Transformers  string `json:"transformers"`
	Tokenizers    string `json:"tokenizers"`
	VocabSize     int    `json:"vocab_size"`
	CloseID       int    `json:"close_id"`
	Renders       int    `json:"renders"`
	Serialization int    `json:"serialization"`
	Tokens        int    `json:"tokens"`
	Sha256        string `json:"sha256"`
}

// TestRenderFixtureProvenance checks the fixture against the provenance the
// generator wrote: the same snapshot the tokenizer is pinned to, and the counts
// and digest the corpus was recorded with. A fixture that silently shrank, or
// came from another snapshot, fails here rather than passing a smaller gate.
func TestRenderFixtureProvenance(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "render_parity.json"))
	if err != nil {
		t.Fatalf("reading the render fixture: %v", err)
	}
	metaRaw, err := os.ReadFile(filepath.Join("testdata", "render_parity.meta.json"))
	if err != nil {
		t.Fatalf("reading the render provenance: %v", err)
	}
	var meta renderMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("parsing the render provenance: %v", err)
	}
	if meta.Model != tokenizer.PinnedRepo || meta.Revision != tokenizer.PinnedSnapshot {
		t.Fatalf("the fixture was generated from %s@%s, want %s@%s",
			meta.Model, meta.Revision, tokenizer.PinnedRepo, tokenizer.PinnedSnapshot)
	}
	// The model id and revision are the generator's own constants, so they cannot
	// catch a mis-pin at generation time on their own. The identity it recorded
	// from the tokenizer it actually loaded can, and it is the same identity
	// VerifyPinned enforces on the tokenizer this package loads.
	if meta.VocabSize != tokenizer.PinnedVocabSize || meta.CloseID != tokenizer.PinnedCloseID {
		t.Fatalf("the fixture's tokenizer has vocab %d and <channel|> %d, want %d and %d",
			meta.VocabSize, meta.CloseID, tokenizer.PinnedVocabSize, tokenizer.PinnedCloseID)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != meta.Sha256 {
		t.Fatalf("the render fixture hashes to %s, the provenance records %s", got, meta.Sha256)
	}
	fixture := loadRenderFixture(t)
	if len(fixture.Renders) != meta.Renders {
		t.Errorf("the fixture has %d renders, the provenance records %d",
			len(fixture.Renders), meta.Renders)
	}
	if len(fixture.Serialization) != meta.Serialization {
		t.Errorf("the fixture has %d serialization cases, the provenance records %d",
			len(fixture.Serialization), meta.Serialization)
	}
	total := 0
	for _, tc := range fixture.Renders {
		total += len(tc.Tokens)
	}
	if total != meta.Tokens {
		t.Errorf("the fixture holds %d prompt tokens, the provenance records %d", total, meta.Tokens)
	}
}

func loadRenderFixture(t *testing.T) renderFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "render_parity.json"))
	if err != nil {
		t.Fatalf("reading the render fixture: %v", err)
	}
	var fixture renderFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parsing the render fixture: %v", err)
	}
	if len(fixture.Renders) == 0 {
		t.Fatal("the render fixture holds no cases")
	}
	if len(fixture.Serialization) == 0 {
		t.Fatal("the render fixture holds no serialization cases")
	}
	return fixture
}

// question rebuilds the case's question from the fixture.
func (c renderCase) question(t *testing.T) Question {
	t.Helper()
	switch c.Kind {
	case "noul":
		return NewNoul(c.Instructions)
	case "choice":
		criteria := make([]Option, len(c.Criteria))
		for i, entry := range c.Criteria {
			criteria[i] = Option{Key: entry.Key, Description: decode(t, entry.Description)}
		}
		return NewChoice(c.Instructions, criteria)
	case "score":
		levels := make([]any, len(c.Levels))
		for i, raw := range c.Levels {
			levels[i] = decode(t, raw)
		}
		return NewScore(c.Instructions, levels)
	default:
		t.Fatalf("unknown question kind %q", c.Kind)
		return nil
	}
}

func (c renderCase) decodedState(t *testing.T) any {
	t.Helper()
	return decode(t, c.State)
}

// decode parses a fixture fragment the way Python's parser would, so numbers
// keep their int-or-float distinction.
func decode(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	value, err := DecodeJSON(raw)
	if err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	return value
}

// TestRenderParity is the gate: the user turn and the prompt must be
// byte-identical to what the Python client produced for the same case.
func TestRenderParity(t *testing.T) {
	fixture := loadRenderFixture(t)
	for _, tc := range fixture.Renders {
		question := tc.question(t)
		state := tc.decodedState(t)

		payload, options, err := Payload(state, question, tc.OptionLimit)
		if err != nil {
			t.Errorf("%s: Payload: %v", tc.Name, err)
			continue
		}
		codes, err := CodesFor(len(options))
		if err != nil {
			t.Errorf("%s: CodesFor: %v", tc.Name, err)
			continue
		}
		if len(codes) != len(options) {
			t.Errorf("%s: %d codes for %d options", tc.Name, len(codes), len(options))
		}

		gotPayload, err := PythonJSON(payload)
		if err != nil {
			t.Errorf("%s: PythonJSON: %v", tc.Name, err)
			continue
		}
		if gotPayload != tc.PayloadJSON {
			t.Errorf("%s: the user turn differs:\n got %s\nwant %s",
				tc.Name, truncate(gotPayload), truncate(tc.PayloadJSON))
			continue
		}

		prompt, err := Render(state, question, RenderOptions{
			Repeat:      tc.Repeat,
			Thinking:    tc.Thinking,
			OptionLimit: tc.OptionLimit,
		})
		if err != nil {
			t.Errorf("%s: Render: %v", tc.Name, err)
			continue
		}
		if prompt != tc.Prompt {
			t.Errorf("%s: the prompt differs:\n got %q\nwant %q",
				tc.Name, truncate(prompt), truncate(tc.Prompt))
		}
	}
}

// TestRenderMatchesTheReadoutPage pins the token counts the readout page names,
// so a change to the scaffold cannot pass by agreeing with itself.
//
// The Japanese choice is not on the page. Its 216 is the count in
// testdata/render_parity.json, kept as a regression pin on the render and the
// tokenizer together rather than as a published value.
func TestRenderMatchesTheReadoutPage(t *testing.T) {
	fixture := loadRenderFixture(t)
	want := map[string]struct {
		tokens int
		source string
	}{
		"spam-choice-repeat2": {238, `docs/READOUT-DE2.md section 2: "That prompt is 238 tokens"`},
		"spam-choice-repeat1": {137, `docs/READOUT-DE2.md section 2: "the one-pass prompt is 137"`},
		"jp-choice-repeat2":   {216, "the render fixture's own count, a regression pin and not a page value"},
	}
	seen := map[string]bool{}
	for _, tc := range fixture.Renders {
		if expected, ok := want[tc.Name]; ok {
			seen[tc.Name] = true
			if len(tc.Tokens) != expected.tokens {
				t.Errorf("%s: the fixture has %d tokens, and %s says %d",
					tc.Name, len(tc.Tokens), expected.source, expected.tokens)
			}
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("the fixture lost the %s case", name)
		}
	}
}

// TestRenderTokenCounts re-tokenizes every fixture prompt with this package's
// tokenizer and requires the Python client's token count.
func TestRenderTokenCounts(t *testing.T) {
	pinned := pinnedTokenizer(t)
	if pinned == nil {
		t.Skip("no tokenizer.json found; set SHISA_DE_TOKENIZER_JSON to check token counts")
	}
	fixture := loadRenderFixture(t)
	for _, tc := range fixture.Renders {
		got := pinned.Encode(tc.Prompt)
		if len(got) != len(tc.Tokens) {
			t.Errorf("%s: %d tokens, want %d", tc.Name, len(got), len(tc.Tokens))
			continue
		}
		for i := range got {
			if got[i] != tc.Tokens[i] {
				t.Errorf("%s: token %d is %d, want %d", tc.Name, i, got[i], tc.Tokens[i])
				break
			}
		}
	}
}

func TestRenderShape(t *testing.T) {
	state := map[string]any{"text": "hello"}
	question := NewChoice("Which?", []Option{{Key: "a", Description: "A"}, {Key: "b", Description: "B"}})

	direct, err := Render(state, question, RenderOptions{Repeat: 2, OptionLimit: MaxCodes})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(direct, "<bos><|turn>system\n") {
		t.Errorf("the direct prompt does not open the system turn: %q", truncate(direct))
	}
	if !strings.HasSuffix(direct, "<|turn>model\n<|channel>thought\n"+AnswerPrefix) {
		t.Errorf("the direct prompt does not end on the answer prefix: %q", truncate(direct))
	}
	if strings.Count(direct, InputRepeat) != 1 {
		t.Errorf("a repeated prompt should hold the separator once, found %d", strings.Count(direct, InputRepeat))
	}

	thinking, err := Render(state, question, RenderOptions{Repeat: 1, Thinking: true, OptionLimit: MaxCodes})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(thinking, "<|think|>\n"+ThinkSystem) {
		t.Errorf("the thinking prompt does not carry the thinking marker and system line: %q", truncate(thinking))
	}
	if strings.HasSuffix(thinking, AnswerPrefix) {
		t.Error("the thinking prompt must not write the answer prefix; the model opens the thought itself")
	}
	if !strings.HasSuffix(thinking, "<|turn>model\n") {
		t.Errorf("the thinking prompt should end at the model turn: %q", truncate(thinking))
	}
	if strings.Count(thinking, InputRepeat) != 0 {
		t.Error("the thinking render writes the user turn once")
	}
}

func TestRenderRefusesWhatItCannotRender(t *testing.T) {
	blank := NewNoul("  ")
	if _, err := Render(nil, blank, RenderOptions{}); err == nil {
		t.Error("a blank question should not render")
	}
	wide := NewChoice("Which?", make([]Option, MaxOptions+1))
	if _, err := Render(nil, wide, RenderOptions{OptionLimit: MaxOptions}); err == nil {
		t.Error("a choice past the letter limit should not render")
	}
	if _, err := Render(nil, wide, RenderOptions{OptionLimit: MaxCodes}); err != nil {
		t.Errorf("the same choice within the codebook limit: %v", err)
	}
	unrenderable := NewChoice("Which?", []Option{{Key: "a", Description: []byte("x")}, {Key: "b", Description: "B"}})
	_, err := Render(nil, unrenderable, RenderOptions{})
	if err == nil {
		t.Fatal("an unrenderable description should not render")
	}
	var readoutErr *ReadoutError
	if !errors.As(err, &readoutErr) {
		t.Errorf("the error is %v, want a *ReadoutError", err)
	}
}

func TestRenderDefaultOptionLimit(t *testing.T) {
	// A zero limit means the letter limit, so a 26-option choice renders and a
	// 27-option one does not.
	at26 := NewChoice("Which?", make([]Option, MaxOptions))
	if _, err := Render(nil, at26, RenderOptions{}); err != nil {
		t.Errorf("a 26-option choice with the default limit: %v", err)
	}
	at27 := NewChoice("Which?", make([]Option, MaxOptions+1))
	if _, err := Render(nil, at27, RenderOptions{}); err == nil {
		t.Error("a 27-option choice with the default limit should not render")
	}
}

// pinnedTokenizer returns the pinned tokenizer, or nil when it is not on this
// machine and the test should skip.
func pinnedTokenizer(t *testing.T) *tokenizer.Tokenizer {
	t.Helper()
	loaded, path, err := tokenizer.LoadPinned(tokenizer.PinnedRepo, tokenizer.PinnedSnapshot)
	if errors.Is(err, tokenizer.ErrNoPinnedTokenizer) {
		if os.Getenv(tokenizer.DownloadEnv) == "1" {
			loaded, err = tokenizer.Load(tokenizer.PinnedRepo + "@" + tokenizer.PinnedSnapshot)
			if err != nil {
				t.Fatalf("downloading the pinned tokenizer: %v", err)
			}
			path = tokenizer.PinnedRepo + "@" + tokenizer.PinnedSnapshot
		} else if os.Getenv(tokenizer.RequireEnv) == "1" {
			t.Fatalf("no tokenizer.json found and %s=1", tokenizer.RequireEnv)
		} else {
			return nil
		}
	}
	if err != nil {
		t.Fatalf("loading the pinned tokenizer: %v", err)
	}
	if err := tokenizer.VerifyPinned(loaded); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return loaded
}
