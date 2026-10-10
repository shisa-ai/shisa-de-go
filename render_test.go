package shisade

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shisa-ai/shisa-de-go/tokenizer"
)

// The pinned checkpoint, as recorded in data/codebook-de2.json.
const (
	pinnedRepo     = "shisa-ai/shisa-de-2"
	pinnedSnapshot = "8f21734b27b3462471c4662d5638e52195b005a3"
)

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

// TestRenderMatchesTheReadoutPage pins the three prompts the readout page names,
// so a change to the scaffold cannot pass by agreeing with itself.
func TestRenderMatchesTheReadoutPage(t *testing.T) {
	fixture := loadRenderFixture(t)
	want := map[string]int{
		"spam-choice-repeat2": 238, // section 2: "That prompt is 238 tokens"
		"spam-choice-repeat1": 137, // section 2: "the one-pass prompt is 137"
		"jp-choice-repeat2":   216,
	}
	seen := map[string]bool{}
	for _, tc := range fixture.Renders {
		if expected, ok := want[tc.Name]; ok {
			seen[tc.Name] = true
			if len(tc.Tokens) != expected {
				t.Errorf("%s: the fixture has %d tokens, and the readout page says %d",
					tc.Name, len(tc.Tokens), expected)
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

func pinnedTokenizer(t *testing.T) *tokenizer.Tokenizer {
	t.Helper()
	candidates := []string{}
	if path := os.Getenv("SHISA_DE_TOKENIZER_JSON"); path != "" {
		candidates = append(candidates, path)
	}
	candidates = append(candidates, filepath.Join("testdata", "tokenizer.json"))
	for _, revision := range []string{pinnedSnapshot, "main"} {
		if path, ok := tokenizer.Lookup(pinnedRepo, revision); ok {
			candidates = append(candidates, path)
		}
	}
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			continue
		}
		loaded, err := tokenizer.FromFile(path)
		if err != nil {
			t.Fatalf("loading %s: %v", path, err)
		}
		return loaded
	}
	return nil
}
