package shisade

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// The DE-2 readout replay gate.
//
// scripts/gen_readout_parity.py runs the Python client's read_codes, think and
// read-after-a-thought against a vLLM-shaped stub and records every request body
// and response body in order, plus what the Python client made of them. This test
// serves those responses back and requires the Go client to send the same
// requests and derive the same results.
//
// The stub tokenizer is data-driven from the fixture's recorded word list, so a
// divergence between the two implementations shows up as a wrong prompt token
// count or a moved answer boundary rather than passing quietly.

type readoutFixture struct {
	Tokenizer struct {
		Words map[string]int `json:"words"`
	} `json:"tokenizer"`
	Scenarios []readoutScenario `json:"scenarios"`
}

type readoutScenario struct {
	Name  string        `json:"name"`
	Steps []readoutStep `json:"steps"`
}

type readoutStep struct {
	Op        string           `json:"op"`
	Prompt    string           `json:"prompt"`
	PromptIDs []int            `json:"prompt_ids"`
	Codes     int              `json:"codes"`
	Debug     bool             `json:"debug"`
	Budget    int              `json:"budget"`
	Exchanges []replayPair     `json:"exchanges"`
	Result    *readResult      `json:"result"`
	Thought   *thoughtResult   `json:"thought"`
	Error     *replayStepError `json:"error"`
}

type replayPair struct {
	Request  json.RawMessage `json:"request"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response"`
}

type replayStepError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Match   string `json:"match"`
}

type readResult struct {
	Logprobs       map[string]float64  `json:"logprobs"`
	Probabilities  map[string]float64  `json:"probabilities"`
	Requests       int                 `json:"requests"`
	PromptTokens   int                 `json:"prompt_tokens"`
	MissingFromTop []string            `json:"missing_from_top"`
	Ranks          map[string]int      `json:"ranks"`
	Sampled        *string             `json:"sampled"`
	Prompt         *string             `json:"prompt"`
	TopLogprobs    map[string]*float64 `json:"top_logprobs"`
}

type thoughtResult struct {
	PromptIDs    []int   `json:"prompt_ids"`
	TokenIDs     []int   `json:"token_ids"`
	Closed       bool    `json:"closed"`
	PromptTokens int     `json:"prompt_tokens"`
	Text         *string `json:"text"`
}

type readoutMeta struct {
	PythonRepo     string `json:"python_repo"`
	ReadoutVersion string `json:"readout_version"`
	MaxCodes       int    `json:"max_codes"`
	MaxOptions     int    `json:"max_options"`
	AnswerTokenID  int    `json:"answer_token_id"`
	Scenarios      int    `json:"scenarios"`
	Exchanges      int    `json:"exchanges"`
	Sha256         string `json:"sha256"`
}

// loadReadoutFixture returns the recorded scenarios and the provenance the
// generator wrote beside them. The provenance is not just a digest check: it
// carries the answer token id the read after a thought closes on.
func loadReadoutFixture(t *testing.T) (readoutFixture, readoutMeta) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "readout_parity.json"))
	if err != nil {
		t.Fatalf("reading the readout fixture: %v", err)
	}
	var fixture readoutFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parsing the readout fixture: %v", err)
	}
	if len(fixture.Scenarios) == 0 {
		t.Fatal("the readout fixture holds no scenarios")
	}
	if len(fixture.Tokenizer.Words) == 0 {
		t.Fatal("the readout fixture holds no tokenizer words")
	}
	metaRaw, err := os.ReadFile(filepath.Join("testdata", "readout_parity.meta.json"))
	if err != nil {
		t.Fatalf("reading the readout provenance: %v", err)
	}
	var meta readoutMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("parsing the readout provenance: %v", err)
	}
	if meta.AnswerTokenID == 0 {
		t.Fatal("the readout provenance records no answer token id")
	}
	return fixture, meta
}

// TestReadoutFixtureProvenance checks the fixture against the provenance the
// generator wrote, the way the render and tokenizer fixtures are checked.
func TestReadoutFixtureProvenance(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "readout_parity.json"))
	if err != nil {
		t.Fatalf("reading the readout fixture: %v", err)
	}
	fixture, meta := loadReadoutFixture(t)
	if meta.ReadoutVersion != DE2ReadoutVersion {
		t.Errorf("the fixture was recorded against %q, and this package implements %q",
			meta.ReadoutVersion, DE2ReadoutVersion)
	}
	if meta.MaxCodes != MaxCodes || meta.MaxOptions != MaxOptions {
		t.Errorf("the fixture records limits %d/%d, want %d/%d",
			meta.MaxCodes, meta.MaxOptions, MaxCodes, MaxOptions)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != meta.Sha256 {
		t.Fatalf("the readout fixture hashes to %s, the provenance records %s", got, meta.Sha256)
	}
	if len(fixture.Scenarios) != meta.Scenarios {
		t.Errorf("the fixture has %d scenarios, the provenance records %d",
			len(fixture.Scenarios), meta.Scenarios)
	}
	total := 0
	for _, scenario := range fixture.Scenarios {
		for _, step := range scenario.Steps {
			total += len(step.Exchanges)
		}
	}
	if total != meta.Exchanges {
		t.Errorf("the fixture holds %d exchanges, the provenance records %d", total, meta.Exchanges)
	}
}

// TestReadoutReplay is the gate: the same requests, and the same results.
func TestReadoutReplay(t *testing.T) {
	fixture, meta := loadReadoutFixture(t)
	for _, scenario := range fixture.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			replay := &replayServer{t: t}
			server := httptest.NewServer(replay)
			defer server.Close()

			readout, err := NewReadout(ReadoutConfig{
				BaseURL:    server.URL,
				Model:      "de2-test",
				HTTPClient: server.Client(),
			})
			if err != nil {
				t.Fatalf("NewReadout: %v", err)
			}
			readout.SetTokenizer(newFixtureTokenizer(fixture.Tokenizer.Words))

			ctx := context.Background()
			var thought Thought
			for _, step := range scenario.Steps {
				replay.exchanges, replay.at = step.Exchanges, 0
				switch step.Op {
				case "read-codes":
					slots, err := readout.CodeSlots(step.Codes)
					if err != nil {
						t.Fatalf("CodeSlots(%d): %v", step.Codes, err)
					}
					read, err := readout.ReadCodes(ctx, step.Prompt, slots, step.Debug)
					verifyRead(t, step, read, err)
				case "think":
					thought = verifyThink(t, step, readout, ctx)
				case "read-codes-after-thought":
					// The construction is the client's, not the fixture's: the
					// thinking prompt's ids, the thought's ids, one terminator.
					ids := append([]int(nil), thought.PromptIDs...)
					ids = append(ids, thought.TokenIDs...)
					ids = append(ids, meta.AnswerTokenID)
					if !equalInts(ids, step.PromptIDs) {
						t.Errorf("the read after the thought uses %v, and the Python client used %v",
							ids, step.PromptIDs)
					}
					slots, err := readout.CodeSlots(step.Codes)
					if err != nil {
						t.Fatalf("CodeSlots(%d): %v", step.Codes, err)
					}
					read, err := readout.ReadCodesAfterThought(ctx, ids, slots, step.Debug)
					verifyRead(t, step, read, err)
				default:
					t.Fatalf("unknown step op %q", step.Op)
				}
				if replay.at != len(step.Exchanges) {
					t.Errorf("%s: the client sent %d requests, and the fixture records %d",
						step.Op, replay.at, len(step.Exchanges))
				}
			}
		})
	}
}

func verifyRead(t *testing.T, step readoutStep, read CodeRead, err error) {
	t.Helper()
	if step.Error != nil {
		requireReadoutError(t, err, step.Error)
		return
	}
	if err != nil {
		t.Fatalf("the read failed: %v", err)
	}
	want := step.Result
	if want == nil {
		t.Fatal("the fixture step has neither a result nor an error")
	}
	if read.Requests != want.Requests {
		t.Errorf("requests = %d, want %d", read.Requests, want.Requests)
	}
	if read.PromptTokens != want.PromptTokens {
		t.Errorf("prompt tokens = %d, want %d", read.PromptTokens, want.PromptTokens)
	}
	if !equalStrings(read.MissingFromTop, want.MissingFromTop) {
		t.Errorf("missing from top = %v, want %v", read.MissingFromTop, want.MissingFromTop)
	}
	if !reflect.DeepEqual(read.Ranks, want.Ranks) {
		t.Errorf("ranks = %v, want %v", read.Ranks, want.Ranks)
	}
	if read.Sampled != derefString(want.Sampled) {
		t.Errorf("sampled = %q, want %q", read.Sampled, derefString(want.Sampled))
	}
	if read.Prompt != derefString(want.Prompt) {
		t.Errorf("the debug prompt is %d bytes, want %d",
			len(read.Prompt), len(derefString(want.Prompt)))
	}
	if !reflect.DeepEqual(read.Logprobs, want.Logprobs) {
		t.Errorf("logprobs differ:\n got %v\nwant %v", read.Logprobs, want.Logprobs)
	}
	// The probabilities are the same softmax over the same logprobs, but Go's
	// math.Exp and CPython's math.exp need not agree to the last bit, so this
	// compares to a relative 1e-12 rather than exactly.
	if !closeEnough(read.Probabilities, want.Probabilities, 1e-12) {
		t.Errorf("probabilities differ:\n got %v\nwant %v", read.Probabilities, want.Probabilities)
	}
	if !reflect.DeepEqual(read.TopLogprobs, want.TopLogprobs) {
		t.Errorf("the debug top logprobs differ:\n got %v\nwant %v",
			read.TopLogprobs, want.TopLogprobs)
	}
}

func verifyThink(t *testing.T, step readoutStep, readout *Readout, ctx context.Context) Thought {
	t.Helper()
	thought, err := readout.Think(ctx, step.Prompt, step.Budget, step.Debug)
	if step.Error != nil {
		requireReadoutError(t, err, step.Error)
		return Thought{}
	}
	if err != nil {
		t.Fatalf("Think: %v", err)
	}
	want := step.Thought
	if want == nil {
		t.Fatal("the fixture step has neither a thought nor an error")
	}
	if !equalInts(thought.PromptIDs, want.PromptIDs) {
		t.Errorf("the thinking prompt is %v, want %v", thought.PromptIDs, want.PromptIDs)
	}
	if !equalInts(thought.TokenIDs, want.TokenIDs) {
		t.Errorf("the thought is %v, want %v", thought.TokenIDs, want.TokenIDs)
	}
	if thought.Closed != want.Closed {
		t.Errorf("closed = %v, want %v", thought.Closed, want.Closed)
	}
	if thought.PromptTokens != want.PromptTokens {
		t.Errorf("prompt tokens = %d, want %d", thought.PromptTokens, want.PromptTokens)
	}
	if thought.Text != derefString(want.Text) {
		t.Errorf("the debug thought text is %q, want %q", thought.Text, derefString(want.Text))
	}
	return thought
}

func requireReadoutError(t *testing.T, err error, want *replayStepError) {
	t.Helper()
	if err == nil {
		t.Fatalf("the read succeeded, and the fixture records %s: %s", want.Type, want.Message)
	}
	var readoutError *ReadoutError
	if !errors.As(err, &readoutError) {
		t.Fatalf("the error is %T (%v), want a *ReadoutError", err, err)
	}
	if want.Match == "" {
		t.Fatal("the fixture's error has no match pattern")
	}
	pattern, compileErr := regexp.Compile(want.Match)
	if compileErr != nil {
		t.Fatalf("the fixture's match pattern %q: %v", want.Match, compileErr)
	}
	if !pattern.MatchString(err.Error()) {
		t.Errorf("the error %q does not match %q; the Python client said %q",
			err, want.Match, want.Message)
	}
}

// replayServer answers with the recorded responses, in order, and fails the test
// when the client's request does not match the one the Python client sent.
type replayServer struct {
	t         *testing.T
	exchanges []replayPair
	at        int
}

func (s *replayServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("reading the request: %v", err)
	}
	if s.at >= len(s.exchanges) {
		s.t.Errorf("the client sent request %d, and the fixture records %d", s.at+1, len(s.exchanges))
		http.Error(w, "unexpected request", http.StatusInternalServerError)
		return
	}
	exchange := s.exchanges[s.at]
	s.at++
	var got, want any
	if err := json.Unmarshal(body, &got); err != nil {
		s.t.Errorf("the request is not JSON: %v", err)
	}
	if err := json.Unmarshal(exchange.Request, &want); err != nil {
		s.t.Errorf("the fixture's request is not JSON: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		s.t.Errorf("request %d differs:\n got %s\nwant %s", s.at, compactJSON(got), compactJSON(want))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(exchange.Status)
	if _, err := w.Write(exchange.Response); err != nil {
		s.t.Errorf("writing the response: %v", err)
	}
}

func compactJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	if len(encoded) > 600 {
		return string(encoded[:600]) + "..."
	}
	return string(encoded)
}

// fixtureTokenizer is the Python test suite's CodeStubTokenizer, built from the
// word list the fixture recorded: one token per character, with the two-letter
// codes and the answer terminator as single tokens.
type fixtureTokenizer struct {
	words   map[string]int
	byID    map[int]string
	ordered []string
}

func newFixtureTokenizer(words map[string]int) *fixtureTokenizer {
	tokenizer := &fixtureTokenizer{words: words, byID: map[int]string{}}
	for word, id := range words {
		tokenizer.byID[id] = word
	}
	for word := range words {
		tokenizer.ordered = append(tokenizer.ordered, word)
	}
	// Longest first, so a match at a position is the longest word there, the way
	// the Python regex alternation is ordered. Ties are broken lexicographically
	// so the slice is deterministic.
	sort.Slice(tokenizer.ordered, func(i, j int) bool {
		if len(tokenizer.ordered[i]) != len(tokenizer.ordered[j]) {
			return len(tokenizer.ordered[i]) > len(tokenizer.ordered[j])
		}
		return tokenizer.ordered[i] < tokenizer.ordered[j]
	})
	return tokenizer
}

func (t *fixtureTokenizer) Encode(text string) []int {
	ids := make([]int, 0, len(text))
	for i := 0; i < len(text); {
		matched := false
		for _, word := range t.ordered {
			if strings.HasPrefix(text[i:], word) {
				ids = append(ids, t.words[word])
				i += len(word)
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		ids = append(ids, int(r))
		i += size
	}
	return ids
}

func (t *fixtureTokenizer) Decode(ids []int) string {
	var out strings.Builder
	for _, id := range ids {
		if word, ok := t.byID[id]; ok {
			out.WriteString(word)
			continue
		}
		out.WriteRune(rune(id))
	}
	return out.String()
}

func equalInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func closeEnough(got, want map[string]float64, tolerance float64) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if math.Abs(got[key]-value) > tolerance {
			return false
		}
	}
	return true
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// TestReadoutRejectsAnAmbiguousFallback pins the one place this port is stricter
// than the Python client: `prompt_logprobs: 0` returns the prompt's own token and
// nothing else, so more than one entry is a server this client will not guess at.
// The Python client takes the first entry in insertion order instead.
func TestReadoutRejectsAnAmbiguousFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"prompt_logprobs":[null,{"a":{"logprob":-1.0},`+
			`"b":{"logprob":-2.0}}]}],"usage":{"prompt_tokens":3}}`)
	}))
	defer server.Close()

	readout, err := NewReadout(ReadoutConfig{BaseURL: server.URL, Model: "de2-test",
		HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	readout.SetTokenizer(newFixtureTokenizer(map[string]int{AnswerPrefix: 101, "AA": 1114113}))
	slot := Slot{Code: "AA", TokenID: 1114113, Text: "AA"}

	_, _, _, err = readout.letterLogprob(context.Background(), textPrompt("<model>"+AnswerPrefix), slot)
	if err == nil {
		t.Fatal("a fallback with two entries should not be guessed at")
	}
	if !strings.Contains(err.Error(), "returns one") {
		t.Errorf("the error is %v, want one naming the ambiguity", err)
	}
}

// TestNewReadoutRequiresAnEndpoint checks the config validation, which is the
// only thing that can fail before a request.
func TestNewReadoutRequiresAnEndpoint(t *testing.T) {
	if _, err := NewReadout(ReadoutConfig{Model: "m"}); err == nil {
		t.Error("a readout without a base URL should not be built")
	}
	if _, err := NewReadout(ReadoutConfig{BaseURL: "http://x"}); err == nil {
		t.Error("a readout without a model should not be built")
	}
	readout, err := NewReadout(ReadoutConfig{BaseURL: "http://x/", Model: "m"})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	if readout.baseURL != "http://x" {
		t.Errorf("the base URL is %q, want the trailing slash trimmed", readout.baseURL)
	}
	if readout.maxLogprobs != DefaultMaxLogprobs {
		t.Errorf("max logprobs = %d, want %d", readout.maxLogprobs, DefaultMaxLogprobs)
	}
	if readout.tokenizerSource != "m" {
		t.Errorf("the tokenizer source is %q, want the model id", readout.tokenizerSource)
	}
	revisioned, err := NewReadout(ReadoutConfig{BaseURL: "http://x", Model: "m",
		Tokenizer: "org/tok", TokenizerRevision: "abc"})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	if revisioned.tokenizerSource != "org/tok@abc" {
		t.Errorf("the tokenizer source is %q, want org/tok@abc", revisioned.tokenizerSource)
	}
}

// TestReadoutResponseShapes covers the responses the replay fixture does not
// carry: a completion with no choice, and a usage block a server left out.
func TestReadoutResponseShapes(t *testing.T) {
	t.Run("no choices", func(t *testing.T) {
		readout := stubReadout(t, `{"choices":[],"usage":{"prompt_tokens":3}}`)
		_, err := readout.ReadCodes(context.Background(), "<model>"+AnswerPrefix,
			testSlots(t, readout, 2), false)
		if err == nil {
			t.Fatal("a completion with no choices should be an error")
		}
		if !strings.Contains(err.Error(), "no choices") {
			t.Errorf("the error is %v, want one naming the missing choice", err)
		}
	})

	t.Run("no usage", func(t *testing.T) {
		// The Python client tolerates a server that reports no prompt_tokens, so
		// this does too: the check has nothing to compare against.
		readout := stubReadout(t, `{"choices":[{"text":"A","logprobs":{"top_logprobs":[`+
			`{"token_id:65":-0.5,"token_id:66":-1.5}]}}]}`)
		read, err := readout.ReadCodes(context.Background(), "<model>"+AnswerPrefix,
			testSlots(t, readout, 2), false)
		if err != nil {
			t.Fatalf("ReadCodes: %v", err)
		}
		if read.PromptTokens != 0 {
			t.Errorf("prompt tokens = %d, want 0 for a server that reported none", read.PromptTokens)
		}
		if read.Requests != 1 {
			t.Errorf("requests = %d, want 1", read.Requests)
		}
	})
}

// TestThinkRejectsABadBudget checks the one argument the Python client validates
// before it spends a request.
func TestThinkRejectsABadBudget(t *testing.T) {
	readout := stubReadout(t, `{"choices":[{"token_ids":[]}]}`)
	for _, budget := range []int{0, -1} {
		if _, err := readout.Think(context.Background(), "<model>"+AnswerPrefix, budget, false); err == nil {
			t.Errorf("a budget of %d should be refused", budget)
		}
	}
}

// stubReadout is a Readout over a server that answers every request with the
// same body, with the fixture's stub tokenizer installed.
func stubReadout(t *testing.T, body string) *Readout {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	readout, err := NewReadout(ReadoutConfig{BaseURL: server.URL, Model: "de2-test",
		HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	readout.SetTokenizer(newFixtureTokenizer(map[string]int{
		AnswerPrefix: 101, "AA": 1114113, "AB": 1114114, "A": 65, "B": 66}))
	return readout
}

// testSlots is the first count codes, resolved through the readout's own
// tokenizer so the ids match what stubReadout's server is keyed by.
func testSlots(t *testing.T, readout *Readout, count int) []Slot {
	t.Helper()
	slots, err := readout.CodeSlots(count)
	if err != nil {
		t.Fatalf("CodeSlots(%d): %v", count, err)
	}
	return slots
}
