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
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/shisa-ai/shisa-de-go/tokenizer"
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
	Name string `json:"name"`
	// APIKey is the bearer token the Python client sent, when the scenario used
	// one. The replay builds its readout with the same key so the recorded
	// Authorization header is compared rather than assumed.
	APIKey string        `json:"api_key"`
	Steps  []readoutStep `json:"steps"`
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

	// The image read and the chat probe. `State` is the evidence the question is
	// asked about, which those two reads need because the client renders the
	// local comparison prompt from it rather than sending it.
	State               json.RawMessage `json:"state"`
	ImageURL            string          `json:"image_url"`
	TopLogprobs         int             `json:"top_logprobs"`
	RequireStringSystem bool            `json:"require_string_system"`
	SystemRender        *string         `json:"system_render"`
}

type replayPair struct {
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Headers  map[string]string `json:"headers"`
	Request  json.RawMessage   `json:"request"`
	Status   int               `json:"status"`
	Response json.RawMessage   `json:"response"`
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
	// Messages is the image read's request messages, as JSON: the fixture holds a
	// Python list of dicts and the client holds Objects, so the two are compared
	// as JSON values rather than as Go ones.
	Messages json.RawMessage `json:"messages"`
	// SystemRender is image reads only.
	SystemRender *string `json:"system_render"`
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
	ThinkSystem    string `json:"think_system"`
	MaxCodes       int    `json:"max_codes"`
	MaxOptions     int    `json:"max_options"`
	AnswerTokenID  int    `json:"answer_token_id"`
	Scenarios      int    `json:"scenarios"`
	Exchanges      int    `json:"exchanges"`
	// Operations and Errors pin the fixture's mix, not just its size: a
	// regeneration that dropped the image read's refusals would shrink these
	// rather than only reordering the totals.
	Operations map[string]int `json:"operations"`
	Errors     int            `json:"errors"`
	Sha256     string         `json:"sha256"`
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
	if meta.PythonRepo == "" {
		t.Fatal("the readout provenance records no Python revision, so the fixture " +
			"cannot be traced to the client that produced it")
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
	if meta.ThinkSystem != ThinkSystem {
		t.Errorf("the fixture was recorded with the thinking system line %q, and this package "+
			"renders %q", meta.ThinkSystem, ThinkSystem)
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
	operations := map[string]int{}
	errors := 0
	for _, scenario := range fixture.Scenarios {
		for _, step := range scenario.Steps {
			operations[step.Op]++
			if step.Error != nil {
				errors++
			}
		}
	}
	if len(meta.Operations) == 0 {
		t.Error("the provenance records no per-operation counts")
	}
	for op, want := range meta.Operations {
		if operations[op] != want {
			t.Errorf("the fixture has %d %s steps, the provenance records %d",
				operations[op], op, want)
		}
	}
	for op, got := range operations {
		if _, recorded := meta.Operations[op]; !recorded {
			t.Errorf("the fixture has %d %s steps the provenance does not record", got, op)
		}
	}
	if errors != meta.Errors {
		t.Errorf("the fixture records %d refusals, the provenance records %d", errors, meta.Errors)
	}
	// The image read is the reason this fixture grew, so a fixture without it is
	// not the one that was recorded.
	if meta.Operations["read-image"] == 0 {
		t.Error("the provenance records no image reads")
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
				APIKey:     scenario.APIKey,
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
				case "read-image":
					slots, err := readout.CodeSlots(step.Codes)
					if err != nil {
						t.Fatalf("CodeSlots(%d): %v", step.Codes, err)
					}
					state, err := DecodeJSON(step.State)
					if err != nil {
						t.Fatalf("decoding the step's state: %v", err)
					}
					question := imageQuestion(step.Codes)
					// The client resolves the codes itself; the fixture's slots are only
					// used to check that the option set is the one the Python client used.
					_ = slots
					read, err := readout.ReadImage(ctx, state, question, step.ImageURL,
						ImageReadOptions{TopLogprobs: step.TopLogprobs, Debug: step.Debug,
							RequireStringSystem: step.RequireStringSystem})
					verifyRead(t, step, read, err)
				case "probe-chat-render":
					state, err := DecodeJSON(step.State)
					if err != nil {
						t.Fatalf("decoding the step's state: %v", err)
					}
					verdict := readout.ProbeChatRender(ctx, state, NewNoul("Is this a health check?"))
					if step.SystemRender == nil || verdict != *step.SystemRender {
						t.Errorf("the chat probe reported %q, and the Python client reported %s",
							verdict, formatOptionalString(step.SystemRender))
					}
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
	if !sameOptionalString(read.Sampled, want.Sampled) {
		t.Errorf("sampled = %q, want %q", formatOptionalString(read.Sampled),
			formatOptionalString(want.Sampled))
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
	// The messages are compared as JSON, because the fixture holds a Python list
	// of dicts and the client holds ordered Objects: the value is what has to
	// agree, not the Go type that carries it.
	gotMessages, err := json.Marshal(read.Messages)
	if err != nil {
		t.Errorf("encoding the sent messages: %v", err)
	}
	if !sameRawJSON(t, gotMessages, want.Messages) {
		t.Errorf("the debug messages differ:\n got %s\nwant %s", gotMessages, want.Messages)
	}
	if read.SystemRender != derefString(want.SystemRender) {
		t.Errorf("system render = %q, want %s", read.SystemRender,
			formatOptionalString(want.SystemRender))
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
	if !sameOptionalString(thought.Text, want.Text) {
		t.Errorf("the debug thought text is %q, want %q", formatOptionalString(thought.Text),
			formatOptionalString(want.Text))
	}
	return thought
}

func requireReadoutError(t *testing.T, err error, want *replayStepError) {
	t.Helper()
	if err == nil {
		t.Fatalf("the read succeeded, and the fixture records %s: %s", want.Type, want.Message)
	}
	// The Python class is asserted too, so a scenario recorded from a different
	// exception than the one it names fails here rather than passing quietly.
	switch want.Type {
	case "ReadoutError":
		var readoutError *ReadoutError
		if !errors.As(err, &readoutError) {
			t.Fatalf("the error is %T (%v), want a *ReadoutError", err, err)
		}
		var httpError *ReadoutHTTPError
		if errors.As(err, &httpError) {
			t.Errorf("the error is %v, and the Python client raised %s", err, want.Type)
		}
	case "ReadoutHTTPError":
		var httpError *ReadoutHTTPError
		if !errors.As(err, &httpError) {
			t.Fatalf("the error is %T (%v), want a *ReadoutHTTPError", err, err)
		}
	default:
		t.Fatalf("the fixture records the error class %q, which this test does not know", want.Type)
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
	if exchange.Method != "" && r.Method != exchange.Method {
		s.t.Errorf("request %d used %s, want %s", s.at, r.Method, exchange.Method)
	}
	if exchange.Path != "" && r.URL.Path != exchange.Path {
		s.t.Errorf("request %d went to %s, want %s", s.at, r.URL.Path, exchange.Path)
	}
	// Every header the Python client set itself, and only those: Go's transport
	// adds its own that the fixture never recorded.
	if len(exchange.Headers) == 0 {
		s.t.Errorf("request %d: the fixture records no headers", s.at)
	}
	for name, want := range exchange.Headers {
		if got := r.Header.Get(name); got != want {
			s.t.Errorf("request %d sent %s: %q, want %q", s.at, name, got, want)
		}
	}
	if r.Header.Get("Content-Type") != "application/json" {
		s.t.Errorf("request %d sent Content-Type %q, want application/json",
			s.at, r.Header.Get("Content-Type"))
	}
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

// closeEnough compares two distributions to a relative tolerance, with an
// absolute floor so a probability near zero is still compared. A NaN on either
// side fails: NaN != NaN, and a comparison written as `abs(got-want) > tol`
// would let a readout that returned NaN for every code pass the gate.
func closeEnough(got, want map[string]float64, tolerance float64) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		actual, ok := got[key]
		if !ok {
			return false
		}
		if math.IsNaN(actual) || math.IsNaN(value) ||
			math.IsInf(actual, 0) || math.IsInf(value, 0) {
			return false
		}
		if math.Abs(actual-value) > tolerance*math.Max(math.Abs(value), 1e-9) {
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

// imageQuestion is the question the image scenarios ask, spelled the same way the
// generator spells it: the same instructions and the same option keys, because
// both reach the prompt the server renders.
func imageQuestion(count int) Choice {
	if count < 2 {
		count = 2
	}
	criteria := make([]Option, count)
	for index := range criteria {
		criteria[index] = Option{Key: fmt.Sprintf("k%d", index),
			Description: fmt.Sprintf("Option %d", index)}
	}
	return NewChoice("What colour is this?", criteria)
}

// sameRawJSON compares two encoded JSON values, so a fixture's Python list of
// dicts and a client's ordered Objects are compared by what they say rather than
// by the Go type carrying it.
func sameRawJSON(t *testing.T, got, want []byte) bool {
	t.Helper()
	if len(want) == 0 {
		want = []byte("null")
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Errorf("the client's value is not JSON: %v", err)
		return false
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Errorf("the fixture's value is not JSON: %v", err)
		return false
	}
	return sameJSON(t, "value", gotValue, wantValue)
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

// TestNewReadoutRefusesDE1 is the DE-2-only guard. This client implements one
// contract, and a checkpoint read through the other one returns answers nobody
// has measured, so the readout refuses it before it loads or sends anything.
func TestNewReadoutRefusesDE1(t *testing.T) {
	cases := []struct {
		name   string
		config ReadoutConfig
	}{
		{"model id", ReadoutConfig{BaseURL: "http://x", Model: "shisa-ai/shisa-de-1"}},
		{"bare model id", ReadoutConfig{BaseURL: "http://x", Model: "de-1"}},
		{"tokenizer source", ReadoutConfig{BaseURL: "http://x", Model: "local-vllm",
			Tokenizer: "shisa-ai/shisa-de-1"}},
		{"declared", ReadoutConfig{BaseURL: "http://x", Model: "local-vllm", Family: "de1"}},
		{"declared over a de2 model", ReadoutConfig{BaseURL: "http://x",
			Model: "shisa-ai/shisa-de-2", Family: "DE-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewReadout(tc.config)
			if err == nil {
				t.Fatal("a DE-1 checkpoint should not be read through the DE-2 contract")
			}
			var unsupported *UnsupportedFamilyError
			if !errors.As(err, &unsupported) {
				t.Fatalf("the error is %T (%v), want a *UnsupportedFamilyError", err, err)
			}
			if unsupported.Family != FamilyDE1 {
				t.Errorf("the error names %s, want %s", unsupported.Family, FamilyDE1)
			}
		})
	}
}

// TestNewReadoutRecordsTheFamily checks the provenance a caller reads back, and
// that an id saying nothing is reported as an assumption rather than a
// detection.
func TestNewReadoutRecordsTheFamily(t *testing.T) {
	cases := []struct {
		config ReadoutConfig
		want   FamilySource
	}{
		{ReadoutConfig{BaseURL: "http://x", Model: "shisa-ai/shisa-de-2"}, SourceModelID},
		{ReadoutConfig{BaseURL: "http://x", Model: "local-vllm",
			Tokenizer: "shisa-ai/shisa-de-2"}, SourceTokenizer},
		{ReadoutConfig{BaseURL: "http://x", Model: "local-vllm"}, SourceAssumed},
		{ReadoutConfig{BaseURL: "http://x", Model: "local-vllm", Family: "de-2"}, SourceDeclared},
	}
	for _, tc := range cases {
		readout, err := NewReadout(tc.config)
		if err != nil {
			t.Fatalf("NewReadout(%q): %v", tc.config.Model, err)
		}
		family, source := readout.ResolvedFamily()
		if family != FamilyDE2 || source != tc.want {
			t.Errorf("%q resolves to %s (%s), want %s (%s)",
				tc.config.Model, family, source, FamilyDE2, tc.want)
		}
	}
}

// TestReadoutHTTPErrorIsAReadoutError checks the taxonomy the Python client has
// and depends on: `ReadoutHTTPError(ReadoutError)`, so one handler covers a
// failed read and a rejected request.
func TestReadoutHTTPErrorIsAReadoutError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "extra_forbidden: logprob_token_ids", http.StatusBadRequest)
	}))
	defer server.Close()
	readout, err := NewReadout(ReadoutConfig{BaseURL: server.URL, Model: "de2-test",
		HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	readout.SetTokenizer(newFixtureTokenizer(map[string]int{AnswerPrefix: 101, "A": 65}))

	_, err = readout.ReadCodes(context.Background(), "<model>"+AnswerPrefix,
		testSlots(t, readout, 1), false)
	if err == nil {
		t.Fatal("a 400 should fail the read")
	}
	var httpError *ReadoutHTTPError
	if !errors.As(err, &httpError) {
		t.Fatalf("the error is %T (%v), want a *ReadoutHTTPError", err, err)
	}
	var readoutError *ReadoutError
	if !errors.As(err, &readoutError) {
		t.Fatalf("errors.As missed the ReadoutError inside %v", err)
	}
	if httpError.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", httpError.Status)
	}
	if !strings.Contains(httpError.Detail, "logprob_token_ids") {
		t.Errorf("detail = %q, want the server's message", httpError.Detail)
	}
}

// TestReadoutRejectsMalformedResponses covers the responses that would otherwise
// be read as something the server did not send.
func TestReadoutRejectsMalformedResponses(t *testing.T) {
	t.Run("null token id", func(t *testing.T) {
		// Go's decoder turns a null element into token 0; continuing a thought
		// with an invented token is worse than an error.
		readout := stubReadout(t, `{"choices":[{"token_ids":[null,101]}]}`)
		_, err := readout.Think(context.Background(), "<model>"+AnswerPrefix, 16, false)
		if err == nil {
			t.Fatal("a null token id should not become token 0")
		}
		if !strings.Contains(err.Error(), "token_ids[0]") {
			t.Errorf("the error is %v, want one naming the bad element", err)
		}
	})

	t.Run("fractional token id", func(t *testing.T) {
		readout := stubReadout(t, `{"choices":[{"token_ids":[1.5]}]}`)
		if _, err := readout.Think(context.Background(), "<model>"+AnswerPrefix, 16, false); err == nil {
			t.Error("a fractional token id should be refused")
		}
	})

	t.Run("null fallback entry", func(t *testing.T) {
		// `prompt_logprobs: 0` with a null entry at the answer boundary used to
		// dereference a nil map value and panic.
		readout := stubReadout(t, `{"choices":[{"text":"A","logprobs":{"top_logprobs":[{}]},`+
			`"prompt_logprobs":[null,{"65":null}]}]}`)
		_, err := readout.ReadCodes(context.Background(), "<model>"+AnswerPrefix,
			testSlots(t, readout, 1), false)
		if err == nil {
			t.Fatal("a null fallback entry should be an error")
		}
		if !strings.Contains(err.Error(), "null entry") {
			t.Errorf("the error is %v, want one naming the null entry", err)
		}
	})

	t.Run("null token_ids field", func(t *testing.T) {
		readout := stubReadout(t, `{"choices":[{"token_ids":null}]}`)
		_, err := readout.Think(context.Background(), "<model>"+AnswerPrefix, 16, false)
		if err == nil || !strings.Contains(err.Error(), "return_token_ids") {
			t.Errorf("a null token_ids field gave %v, want the missing-token_ids error", err)
		}
	})
}

// TestSetTokenizerDiscardsDerivedState checks that swapping the tokenizer does
// not leave ids and boundary approvals resolved from the previous one behind.
func TestSetTokenizerDiscardsDerivedState(t *testing.T) {
	readout := stubReadout(t, `{"choices":[{"text":"A"}]}`)
	slots := testSlots(t, readout, 2)
	if slots[0].TokenID != 65 {
		t.Fatalf("the first stub tokenizer resolves A to %d", slots[0].TokenID)
	}
	// A second tokenizer that resolves the same codes to different ids, with the
	// same terminator. Without invalidation the old slots would be reused.
	readout.SetTokenizer(newFixtureTokenizer(map[string]int{
		AnswerPrefix: 101, "A": 9001, "B": 9002}))
	again := testSlots(t, readout, 2)
	if again[0].TokenID != 9001 {
		t.Errorf("A resolves to %d after the swap, want 9001: the slot cache is stale",
			again[0].TokenID)
	}
}

// TestReadoutResponseCap pins the truncation that stops a runaway body, and the
// size of the real cap.
func TestReadoutResponseCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, strings.Repeat("x", 64))
	}))
	defer server.Close()
	client := server.Client()

	var into map[string]any
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if err := doJSONLimit(client, request, nil, &into, 32); err == nil {
		t.Error("a body past the cap should not parse")
	}
	request, err = http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if err := doJSONLimit(client, request, nil, &into, 64); err == nil {
		t.Error("a truncated body is not JSON, so it should not parse either")
	}

	// The cap is pinned exactly, not as a lower bound: it has to clear the
	// largest honest response (`prompt_logprobs: 0` returns one entry per prompt
	// token, and the pinned prompts are about 8,500), and a shrink that still
	// cleared a lax bound would otherwise pass.
	if maxResponseBytes != 64<<20 {
		t.Errorf("maxResponseBytes is %d, want %d", maxResponseBytes, 64<<20)
	}
}

// TestReadoutSendsTheBearerToken checks the header the endpoint authenticates
// with, which nothing else in the tree exercises.
func TestReadoutSendsTheBearerToken(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"shisa-ai/shisa-de-2"},{"id":"other"}]}`)
	}))
	defer server.Close()
	readout, err := NewReadout(ReadoutConfig{BaseURL: server.URL, Model: "shisa-ai/shisa-de-2",
		APIKey: "secret", HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	models, err := readout.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if got != "Bearer secret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer secret")
	}
	if len(models) != 2 || models[0] != "shisa-ai/shisa-de-2" {
		t.Errorf("models = %v", models)
	}

	// No key, no header.
	got = "unset"
	anon, err := NewReadout(ReadoutConfig{BaseURL: server.URL, Model: "shisa-ai/shisa-de-2",
		HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	if _, err := anon.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if got != "" {
		t.Errorf("Authorization = %q with no API key, want it absent", got)
	}
}

// TestReadoutConcurrentReads exercises the claim that a Readout is safe for
// concurrent use: the tokenizer load, the slot cache, the boundary approvals and
// the capability flag are all shared state, and the race detector is what
// decides whether the mutex covers them.
func TestReadoutConcurrentReads(t *testing.T) {
	fixture, _ := loadReadoutFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"text":"A","logprobs":{"top_logprobs":[`+
			`{"token_id:65":-0.1,"token_id:66":-1.1}]}}],"usage":{"prompt_tokens":8}}`)
	}))
	defer server.Close()
	readout, err := NewReadout(ReadoutConfig{BaseURL: server.URL, Model: "de2-test",
		HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	readout.SetTokenizer(newFixtureTokenizer(fixture.Tokenizer.Words))

	prompt := "<model>" + AnswerPrefix
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for i := 0; i < 8; i++ {
				slots, err := readout.CodeSlots(2)
				if err != nil {
					t.Errorf("CodeSlots: %v", err)
					return
				}
				if _, err := readout.ReadCodes(context.Background(), prompt, slots, false); err != nil {
					t.Errorf("ReadCodes: %v", err)
					return
				}
			}
		}()
	}
	group.Wait()
}

// TestReadoutLocalFilesOnly checks that the config flag reaches the tokenizer
// resolver: an uncached source is refused rather than fetched. A fetch would
// fail with the hub's own error, so the sentinel is what proves the local-only
// path was taken.
func TestReadoutLocalFilesOnly(t *testing.T) {
	readout, err := NewReadout(ReadoutConfig{
		BaseURL:        "http://replay.invalid",
		Model:          "shisa-ai/shisa-de-2",
		Tokenizer:      "shisa-ai/this-repository-does-not-exist-local-only-test@0000000000000000000000000000000000000000",
		LocalFilesOnly: true,
	})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	_, err = readout.CodeSlots(1)
	if err == nil {
		t.Fatal("an uncached tokenizer should not resolve with LocalFilesOnly")
	}
	if !errors.Is(err, tokenizer.ErrNotCached) {
		t.Fatalf("the error is %v, want tokenizer.ErrNotCached", err)
	}
	var readoutError *ReadoutError
	if !errors.As(err, &readoutError) {
		t.Errorf("the error is %T, want it wrapped in a *ReadoutError", err)
	}
}

// TestCheckCodeBoundaryIsCallable pins the exported boundary check. It used to
// take the readout mutex and then call a helper that took it again, so every
// call hung while holding the lock; nothing in the tree called it.
func TestCheckCodeBoundaryIsCallable(t *testing.T) {
	readout := stubReadout(t, `{"choices":[{"text":"A"}]}`)
	slots := testSlots(t, readout, 2)
	prefixID, err := readout.AnswerPrefixID()
	if err != nil {
		t.Fatalf("AnswerPrefixID: %v", err)
	}
	if err := readout.CheckCodeBoundary([]int{prefixID}, slots); err != nil {
		t.Fatalf("CheckCodeBoundary: %v", err)
	}
	if err := readout.CheckCodeBoundary([]int{prefixID, prefixID}, slots); err != nil {
		t.Fatalf("CheckCodeBoundary with a longer prompt: %v", err)
	}
	// The second call must not hang either: the approvals are cached, and the
	// cache is read under the same lock.
	if err := readout.CheckCodeBoundary([]int{prefixID}, slots); err != nil {
		t.Fatalf("CheckCodeBoundary on the cached path: %v", err)
	}
	if err := readout.CheckCodeBoundary([]int{prefixID + 1}, slots); err == nil {
		t.Error("a prompt that does not end on the terminator should be refused")
	}
}

// TestCheckCodeBoundaryRefusesAForeignTokenID covers the cache keyed by code
// alone: a slot carrying an approved code but a token id the current tokenizer
// never produced must not skip the per-code check. `Slot` is exported with
// exported fields and the read methods take `[]Slot`, so a caller can build one.
func TestCheckCodeBoundaryRefusesAForeignTokenID(t *testing.T) {
	readout := stubReadout(t, `{"choices":[{"text":"A"}]}`)
	slots := testSlots(t, readout, 1)
	prefixID, err := readout.AnswerPrefixID()
	if err != nil {
		t.Fatalf("AnswerPrefixID: %v", err)
	}
	if err := readout.CheckCodeBoundary([]int{prefixID}, slots); err != nil {
		t.Fatalf("CheckCodeBoundary: %v", err)
	}
	forged := []Slot{{Code: slots[0].Code, TokenID: 999, Text: slots[0].Text}}
	err = readout.CheckCodeBoundary([]int{prefixID}, forged)
	if err == nil {
		t.Fatal("an approved code with a foreign token id should not skip the check")
	}
	if !strings.Contains(err.Error(), "boundary moves") {
		t.Errorf("the error is %v, want the boundary-moves error", err)
	}
}

// TestCheckCodeBoundaryDoesNotCacheAcrossATokenizerSwap is the stale-generation
// branch of checkCodeBoundaryFor: a read that captured a tokenizer, lost a
// SetTokenizer race, and then reaches the check must re-check its codes and
// cache nothing. Without the generation guard it would write approvals for a
// tokenizer the readout no longer holds, and the next read under the new
// tokenizer would skip the per-code check.
func TestCheckCodeBoundaryDoesNotCacheAcrossATokenizerSwap(t *testing.T) {
	readout := stubReadout(t, `{"choices":[{"text":"A"}]}`)
	slots := testSlots(t, readout, 1)
	tok, generation, err := readout.tokenizerSnapshot()
	if err != nil {
		t.Fatalf("tokenizerSnapshot: %v", err)
	}
	prefixID, err := readout.answerPrefixIDFor(tok)
	if err != nil {
		t.Fatalf("answerPrefixIDFor: %v", err)
	}

	// The swap the read lost the race against. The new tokenizer resolves A to
	// the same id and the same terminator, so only the cache decides whether the
	// check runs.
	readout.SetTokenizer(newFixtureTokenizer(map[string]int{AnswerPrefix: 101, "A": 65}))

	// The stale read's check passes, against its own tokenizer.
	if err := readout.checkCodeBoundaryFor(tok, generation, []int{prefixID}, slots); err != nil {
		t.Fatalf("the stale read's boundary check: %v", err)
	}
	if len(readout.boundaryChecked) != 0 {
		t.Fatalf("a read that lost the tokenizer race cached %v", readout.boundaryChecked)
	}

	// Nothing was approved, so a foreign token id under the new tokenizer still
	// has to be checked and still has to fail.
	forged := []Slot{{Code: slots[0].Code, TokenID: 999, Text: slots[0].Text}}
	if err := readout.CheckCodeBoundary([]int{prefixID}, forged); err == nil {
		t.Error("the stale read left an approval the new tokenizer trusted")
	}
}

// TestSetTokenizerForgetsBoundaryApprovals checks the approval half of the
// invalidation, not just the slot half.
func TestSetTokenizerForgetsBoundaryApprovals(t *testing.T) {
	readout := stubReadout(t, `{"choices":[{"text":"A"}]}`)
	slots := testSlots(t, readout, 1)
	prefixID, err := readout.AnswerPrefixID()
	if err != nil {
		t.Fatalf("AnswerPrefixID: %v", err)
	}
	if err := readout.CheckCodeBoundary([]int{prefixID}, slots); err != nil {
		t.Fatalf("CheckCodeBoundary: %v", err)
	}
	// The same code, an id only the first tokenizer produces, and the same
	// terminator, so only the per-code half can catch it.
	readout.SetTokenizer(newFixtureTokenizer(map[string]int{AnswerPrefix: 101, "A": 9001}))
	if err := readout.CheckCodeBoundary([]int{prefixID}, slots); err == nil {
		t.Error("an approval from the previous tokenizer should not survive the swap")
	}
}

// TestCloseEnough is the direct test the helper never had. Every probability in
// the replay fixture agrees far inside the tolerance, so without this a
// `closeEnough` that returned true unconditionally would pass the gate.
func TestCloseEnough(t *testing.T) {
	cases := []struct {
		name      string
		got, want map[string]float64
		ok        bool
	}{
		{"equal", map[string]float64{"A": 0.25, "B": 0.75},
			map[string]float64{"A": 0.25, "B": 0.75}, true},
		{"reordered", map[string]float64{"B": 0.75, "A": 0.25},
			map[string]float64{"A": 0.25, "B": 0.75}, true},
		{"missing key", map[string]float64{"A": 0.25},
			map[string]float64{"A": 0.25, "B": 0.75}, false},
		{"extra key", map[string]float64{"A": 0.25, "B": 0.75, "C": 0},
			map[string]float64{"A": 0.25, "B": 0.75}, false},
		{"wrong key", map[string]float64{"A": 0.25, "Z": 0.75},
			map[string]float64{"A": 0.25, "B": 0.75}, false},
		{"out of tolerance", map[string]float64{"A": 0.25, "B": 0.750001},
			map[string]float64{"A": 0.25, "B": 0.75}, false},
		{"inside tolerance", map[string]float64{"A": 0.25, "B": 0.75 + 1e-14},
			map[string]float64{"A": 0.25, "B": 0.75}, true},
		{"NaN got", map[string]float64{"A": 0.25, "B": math.NaN()},
			map[string]float64{"A": 0.25, "B": 0.75}, false},
		{"NaN want", map[string]float64{"A": 0.25, "B": 0.75},
			map[string]float64{"A": 0.25, "B": math.NaN()}, false},
		{"Inf got", map[string]float64{"A": 0.25, "B": math.Inf(1)},
			map[string]float64{"A": 0.25, "B": 0.75}, false},
		{"Inf want", map[string]float64{"A": 0.25, "B": 0.75},
			map[string]float64{"A": 0.25, "B": math.Inf(-1)}, false},
		// The absolute floor: a near-zero probability is compared absolutely, so
		// a relative test that divided by ~0 cannot pass anything.
		{"near zero inside the floor", map[string]float64{"A": 1, "B": 1e-12},
			map[string]float64{"A": 1, "B": 0}, false},
		{"near zero equal", map[string]float64{"A": 1, "B": 1e-12},
			map[string]float64{"A": 1, "B": 1e-12}, true},
		{"both empty", map[string]float64{}, map[string]float64{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := closeEnough(tc.got, tc.want, 1e-12); got != tc.ok {
				t.Errorf("closeEnough = %v, want %v", got, tc.ok)
			}
		})
	}
}

// TestZeroReadoutReportsItself checks that the zero value fails the way every
// other unusable readout does, rather than panicking inside net/http.
func TestZeroReadoutReportsItself(t *testing.T) {
	var readout Readout
	_, err := readout.ListModels(context.Background())
	if err == nil {
		t.Fatal("a zero Readout should not list models")
	}
	if !strings.Contains(err.Error(), "NewReadout") {
		t.Errorf("the error is %v, want one naming the constructor", err)
	}
	_, err = readout.ReadCodes(context.Background(), "<model>"+AnswerPrefix, nil, false)
	if err == nil {
		t.Error("a zero Readout should not read codes")
	}
}

// TestReadoutHTTPErrorWithoutAnEmbed covers the value a caller can build from
// outside the package: it must report itself rather than panic, and errors.As
// must not hand back a nil *ReadoutError.
func TestReadoutHTTPErrorWithoutAnEmbed(t *testing.T) {
	err := &ReadoutHTTPError{Status: 503, Detail: "unavailable"}
	if message := err.Error(); !strings.Contains(message, "503") {
		t.Errorf("Error() = %q, want one naming the status", message)
	}
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		t.Errorf("Unwrap() = %v, want nil", unwrapped)
	}
	var readoutError *ReadoutError
	if errors.As(err, &readoutError) {
		t.Errorf("errors.As found a %v inside an HTTP error with no embed", readoutError)
	}
}

// TestReadoutSlot covers the exported code resolver: a code must be exactly one
// token, which is what the whole readout depends on.
func TestReadoutSlot(t *testing.T) {
	readout := stubReadout(t, `{"choices":[{"text":"A"}]}`)
	slot, err := readout.Slot("A")
	if err != nil {
		t.Fatalf("Slot: %v", err)
	}
	if slot.Code != "A" || slot.TokenID != 65 || slot.Text != "A" {
		t.Errorf("Slot = %+v, want A at 65", slot)
	}
	// The cache answers the second call without the tokenizer.
	again, err := readout.Slot("A")
	if err != nil || again != slot {
		t.Errorf("the second Slot call gave %+v (%v)", again, err)
	}
	// A code the tokenizer does not hold as one token cannot be read.
	if _, err := readout.Slot("not-a-code"); err == nil {
		t.Error("a multi-token code should be refused")
	}
	if _, err := readout.CodeSlots(0); err != nil {
		t.Errorf("CodeSlots(0): %v", err)
	}
}

// TestReadoutRenderIsThePackageRender checks the method the client layer will
// call, and that it carries the readout's own system line.
func TestReadoutRenderIsThePackageRender(t *testing.T) {
	readout, err := NewReadout(ReadoutConfig{BaseURL: "http://x", Model: "shisa-ai/shisa-de-2",
		SystemPrompt: "You are a careful classifier."})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	question := NewChoice("Is this spam?", []Option{
		{Key: "yes", Description: "Yes"}, {Key: "no", Description: "No"}})
	state := map[string]any{"text": "hello"}
	options := RenderOptions{Repeat: 2, OptionLimit: MaxCodes}

	got, err := readout.Render(state, question, options)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got, "You are a careful classifier.") {
		t.Errorf("the readout's system line is missing: %q", truncate(got))
	}
	// An explicit option overrides the readout's line.
	options.SystemPrompt = "Another line."
	overridden, err := readout.Render(state, question, options)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(overridden, "careful classifier") || !strings.Contains(overridden, "Another line.") {
		t.Errorf("the explicit system line was not used: %q", truncate(overridden))
	}
	// The method and the package function agree when neither is overridden.
	direct, err := Render(state, question, RenderOptions{Repeat: 2, OptionLimit: MaxCodes,
		SystemPrompt: "You are a careful classifier."})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != direct {
		t.Error("Readout.Render and Render disagree on the same options")
	}
}

// TestBoundaryMessagesQuoteLikePython pins the quoting in the messages a failing
// boundary check produces.
//
// These reach `doctor`'s `boundary_check` field in both output forms and
// `explain`'s stderr, and they are otherwise word-for-word the Python client's,
// which interpolates them with `!r`. Go's `%q` would be double-quoted, so the
// two command lines would differ exactly on the run the check exists to
// produce.
func TestBoundaryMessagesQuoteLikePython(t *testing.T) {
	readout := stubReadout(t, `{"choices":[{"text":"A"}]}`)
	slots := testSlots(t, readout, 2)
	prefixID, err := readout.AnswerPrefixID()
	if err != nil {
		t.Fatalf("AnswerPrefixID: %v", err)
	}

	// The does-not-end message: the prompt's last token is not the answer
	// prefix. (The terminator message proper — "the generation-prompt terminator
	// ... is not one token" — is reached through `AnswerPrefixID` and is the one
	// sentence of the boundary pair this port deliberately rewords, because
	// Python's says "the image readout".)
	err = readout.CheckCodeBoundary([]int{prefixID + 1}, slots)
	if err == nil {
		t.Fatal("a prompt that does not end on the terminator was accepted")
	}
	// The whole sentence, because the point is that it is Python's sentence.
	want := "shisade: the prompt does not end on '<channel|>' (token " +
		fmt.Sprint(prefixID) + "), so the answer is not at the first generated position. " +
		"The tokenizer ('de2-test') does not match the served model ('de2-test')."
	if err.Error() != want {
		t.Errorf("the terminator message is\n  %s\nwant\n  %s", err, want)
	}

	// The per-code message: a slot whose token id the tokenizer never produced.
	foreign := []Slot{{Code: slots[0].Code, TokenID: slots[0].TokenID + 9999}}
	err = readout.CheckCodeBoundary([]int{prefixID}, foreign)
	if err == nil {
		t.Fatal("a slot with a foreign token id was accepted")
	}
	want = "shisade: the answer boundary moves for '" + slots[0].Code + "': appended to " +
		"'<channel|>' it does not tokenize as token " + fmt.Sprint(foreign[0].TokenID) +
		". The tokenizer ('de2-test') does not match the served model ('de2-test')."
	if err.Error() != want {
		t.Errorf("the boundary message is\n  %s\nwant\n  %s", err, want)
	}
}

// TestReadoutMessagesQuoteLikePython pins the quoting in the three readout
// messages that reach the command line only through a failing read, and are
// otherwise word-for-word the Python client's.
//
// `TestBoundaryMessagesQuoteLikePython` covers the two boundary sentences. The
// code-slot sentence and the prompt-token sentence are here, and the
// fallback-nothing sentence below them, because each one's quoted id would print
// double-quoted under Go's `%q` where Python writes `!r`.
func TestReadoutMessagesQuoteLikePython(t *testing.T) {
	readout := stubReadout(t, `{"choices":[{"text":"A"}]}`)

	// The code-slot message: a code the tokenizer does not hold as one token.
	_, err := readout.Slot("not-a-code")
	if err == nil {
		t.Fatal("a multi-token code was accepted")
	}
	want := "shisade: answer code 'not-a-code' is not one token for tokenizer " +
		"'de2-test'; this checkpoint cannot be read with the codebook"
	if err.Error() != want {
		t.Errorf("the slot message is\n  %s\nwant\n  %s", err, want)
	}

	// The prompt-token message: the server counted a different prompt.
	_, err = readout.checkPromptTokens(completionResponse{
		Usage: completionUsage{PromptTokens: 99}}, 42)
	if err == nil {
		t.Fatal("a mismatched prompt-token count was accepted")
	}
	want = "shisade: the server counted 99 prompt tokens where the client tokenized 42: " +
		"the tokenizer ('de2-test') does not match the served model ('de2-test')"
	if err.Error() != want {
		t.Errorf("the prompt-token message is\n  %s\nwant\n  %s", err, want)
	}
}

// TestFallbackMessageQuotesLikePython is the fallback-nothing sentence: the
// code is missing from the top-k and the `prompt_logprobs` fallback returns
// nothing for it.
func TestFallbackMessageQuotesLikePython(t *testing.T) {
	// The read's first request answers with the first code only, so the second
	// is missing from the top-k; the fallback request then answers with an empty
	// `prompt_logprobs`, which is what the sentence is about.
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			fmt.Fprintf(w, `{"choices":[{"text":"A","logprobs":{"top_logprobs":[{"token_id:1000":-0.1}]}}],`+
				`"usage":{"prompt_tokens":1}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"text":"","prompt_logprobs":[null]}],"usage":{"prompt_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	readout, err := NewReadout(ReadoutConfig{BaseURL: server.URL, Model: "de2-test",
		HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	readout.SetTokenizer(newFixtureTokenizer(map[string]int{
		AnswerPrefix: 101, "AA": 1114113, "AB": 1114114, "A": 1000, "B": 1001}))
	slots := testSlots(t, readout, 2)

	// The prompt has to end on the answer terminator for the boundary check to
	// pass, so the read gets there rather than failing on the prompt.
	_, err = readout.ReadCodes(context.Background(), AnswerPrefix, slots, false)
	if err == nil {
		t.Fatal("a code with no logprob and no fallback was read anyway")
	}
	want := "shisade: code '" + slots[1].Code + "' was not returned for its token id and the " +
		"prompt_logprobs fallback returned nothing for it"
	if err.Error() != want {
		t.Errorf("the fallback message is\n  %s\nwant\n  %s", err, want)
	}
}

// -- the image read -----------------------------------------------------------

// The chat endpoint's own tests.
//
// scripts/gen_readout_parity.py records the Python client's request bodies,
// responses and results for this read, and TestReadoutReplay requires the Go
// client to reproduce them. What follows is what a recorded exchange cannot
// state: the shapes the fixture does not carry, the readouts that cannot send at
// all, and the choices this port makes where the Python client's answer depends
// on its dict's insertion order.

// chatStubReadout is a readout whose transport answers from one handler, with a
// count of the requests it received. The tokenizer resolves the pinned template's
// fragments as single tokens, so a rendered prompt has a stable token count.
func chatStubReadout(t *testing.T, handler http.HandlerFunc) (*Readout, *int32) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	readout, err := NewReadout(ReadoutConfig{BaseURL: server.URL, Model: "de2-test",
		HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	readout.SetTokenizer(newFixtureTokenizer(map[string]int{
		AnswerPrefix: 101, "AA": 1114113, "AB": 1114114, "A": 65, "B": 66,
		"<bos>": 200, "<|turn>": 201, "<turn|>": 202, "system\n": 203, "user\n": 204,
		"model\n": 205, "<|channel>thought\n": 206}))
	return readout, &calls
}

// chatBody is a chat response carrying one answer position with the given top-k
// and one prompt-logprob entry, which is the terminator the boundary check needs.
func chatBody(top string) string {
	return `{"choices":[{"message":{"role":"assistant","content":"A"},` +
		`"logprobs":{"content":[{"token":"A","logprob":-0.1,"top_logprobs":[` + top + `]}]}}],` +
		`"prompt_logprobs":[null,{"101":{"logprob":-0.01,"rank":1,` +
		`"decoded_token":"<channel|>"}}],"usage":{"prompt_tokens":9}}`
}

const imageTestURL = "data:image/png;base64,iVBORw0KGgo="

// TestReadImageRefusesALocalPath is the one thing ReadImage must never forward:
// the request carries the URL itself, so a file name would be sent as the image.
func TestReadImageRefusesALocalPath(t *testing.T) {
	readout, calls := chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, chatBody(`{"token":"A","logprob":-0.1}`))
	})
	_, err := readout.ReadImage(context.Background(), map[string]any{"a": 1},
		imageQuestion(2), "cat.png", ImageReadOptions{})
	if err == nil {
		t.Fatal("ReadImage accepted a local file name")
	}
	var imageError *ImageError
	if !errors.As(err, &imageError) {
		t.Fatalf("got a %T, want an *ImageError", err)
	}
	if !strings.Contains(imageError.Message, "prepare_image()") {
		t.Errorf("the message does not say what to call instead: %v", err)
	}
	if *calls != 0 {
		t.Errorf("the readout sent %d requests for a rejected reference", *calls)
	}
}

// TestReadImageRefusesMoreOptionsThanTopLogprobs is the arithmetic that stops a
// read the server cannot satisfy: with no fallback, an option outside the top-k
// is an error, so asking is pointless.
func TestReadImageRefusesMoreOptionsThanTopLogprobs(t *testing.T) {
	readout, calls := chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, chatBody(`{"token":"A","logprob":-0.1}`))
	})
	_, err := readout.ReadImage(context.Background(), map[string]any{"a": 1},
		imageQuestion(4), imageTestURL, ImageReadOptions{TopLogprobs: 3})
	if err == nil {
		t.Fatal("a 4-option read from a top-3 distribution was attempted")
	}
	want := "shisade: 4 options cannot be read from a top-3 distribution: the server returns " +
		"at most 3 tokens, so at least 1 option letters would be missing and the image readout " +
		"has no letter fallback; pass top_logprobs=4 or higher and serve the model with " +
		"--max-logprobs 4 or higher"
	if err.Error() != want {
		t.Errorf("the message is\n  %s\nwant\n  %s", err, want)
	}
	if *calls != 0 {
		t.Errorf("the readout sent %d requests for an impossible read", *calls)
	}
}

// TestReadImageRejectsABadTopLogprobs pins the guard before the arithmetic, so a
// zero or negative limit is named as what it is rather than as a shortfall.
func TestReadImageRejectsABadTopLogprobs(t *testing.T) {
	readout, calls := chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, chatBody(`{"token":"A","logprob":-0.1}`))
	})
	for _, limit := range []int{-1, -100} {
		_, err := readout.ReadImage(context.Background(), map[string]any{"a": 1},
			imageQuestion(2), imageTestURL, ImageReadOptions{TopLogprobs: limit})
		if err == nil || !strings.Contains(err.Error(), "top_logprobs must be a positive integer") {
			t.Errorf("top_logprobs=%d gave %v, want the positive-integer error", limit, err)
		}
	}
	if *calls != 0 {
		t.Errorf("the readout sent %d requests for a rejected limit", *calls)
	}
}

// TestReadImageRefusesAResponseWithNoChoices pins the one chat-response refusal
// that has no fixture behind it.
//
// The message is the Python client's `_first_choice` verbatim, which is why the
// read does not name itself in it: both chat reads send the same request shape,
// and Python does not distinguish them here either.
func TestReadImageRefusesAResponseWithNoChoices(t *testing.T) {
	readout, _ := chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[]}`)
	})
	_, err := readout.ReadImage(context.Background(), map[string]any{"a": 1},
		imageQuestion(2), imageTestURL, ImageReadOptions{})
	if err == nil {
		t.Fatal("a chat response with no choices was read")
	}
	const want = "the chat response carries no choices"
	var readoutError *ReadoutError
	if !errors.As(err, &readoutError) {
		t.Fatalf("got a %T, want a *ReadoutError", err)
	}
	if readoutError.Message != want {
		t.Errorf("the message is\n  %s\nwant it to be exactly\n  %s", readoutError.Message, want)
	}
}

// TestReadImageHasNoFallback pins the difference from ReadCodes: a code outside
// the returned top-k ends the read after one request instead of buying a second.
func TestReadImageHasNoFallback(t *testing.T) {
	readout, calls := chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, chatBody(`{"token":"A","logprob":-0.1}`))
	})
	_, err := readout.ReadImage(context.Background(), map[string]any{"a": 1},
		imageQuestion(2), imageTestURL, ImageReadOptions{})
	if err == nil {
		t.Fatal("a read with a code outside the top-k was answered anyway")
	}
	want := "shisade: option letters B are missing from the returned top 20 logprobs and the " +
		"image readout has no letter fallback; raise the server's --max-logprobs above 20 or " +
		"pass a higher top_logprobs"
	if err.Error() != want {
		t.Errorf("the message is\n  %s\nwant\n  %s", err, want)
	}
	// One request, and no fallback: this is the whole difference from ReadCodes,
	// which would have sent a second.
	if *calls != 1 {
		t.Errorf("the readout sent %d requests, want exactly 1", *calls)
	}
}

// TestReadImageBoundaryRefusals pins the substitute for the text path's local
// boundary check. The image readout has no prompt to tokenize, so the server's
// last prompt token is the evidence, and a missing or wrong one is an error
// rather than permission to read another position.
func TestReadImageBoundaryRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "no prompt_logprobs at all",
			body: `{"choices":[{"logprobs":{"content":[{"top_logprobs":[]}]}}]}`,
			want: "carries no root-level prompt_logprobs",
		},
		{
			name: "empty entry at the boundary",
			body: `{"choices":[{"logprobs":{"content":[{"top_logprobs":[]}]}}],` +
				`"prompt_logprobs":[null,{}]}`,
			want: "carries no token at the answer boundary",
		},
		{
			name: "the wrong token",
			body: `{"choices":[{"logprobs":{"content":[{"top_logprobs":[]}]}}],` +
				`"prompt_logprobs":[null,{"7":{"logprob":-1,"decoded_token":"x"}}]}`,
			want: "the last prompt token is 'x' (id 7), not '<channel|>' (id 101)",
		},
		{
			name: "the right id, the wrong decoding",
			body: `{"choices":[{"logprobs":{"content":[{"top_logprobs":[]}]}}],` +
				`"prompt_logprobs":[null,{"101":{"logprob":-1,"decoded_token":"oops"}}]}`,
			want: "the last prompt token is 'oops' (id 101), not '<channel|>' (id 101)",
		},
		{
			// The Python client reads this with `.get`, so an entry that omits
			// the field is None there and must not be an empty string here.
			name: "no decoded_token",
			body: `{"choices":[{"logprobs":{"content":[{"top_logprobs":[]}]}}],` +
				`"prompt_logprobs":[null,{"7":{"logprob":-1}}]}`,
			want: "the last prompt token is None (id 7), not '<channel|>' (id 101)",
		},
		{
			// A key that is not a token id is reported as itself. Printing an id
			// it does not have would name a token that is not in the response.
			name: "a key that is not a token id",
			body: `{"choices":[{"logprobs":{"content":[{"top_logprobs":[]}]}}],` +
				`"prompt_logprobs":[null,{"oops":{"logprob":-1,"decoded_token":"x"}}]}`,
			want: "the last prompt token is 'x' (id oops), not '<channel|>' (id 101)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readout, _ := chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tc.body)
			})
			_, err := readout.ReadImage(context.Background(), map[string]any{"a": 1},
				imageQuestion(2), imageTestURL, ImageReadOptions{})
			if err == nil {
				t.Fatal("the read was answered without boundary evidence")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error is\n  %v\nwant it to contain\n  %s", err, tc.want)
			}
		})
	}
}

// TestBoundaryEntryIsDeterministic pins the one place this port chooses where the
// Python client depends on its dict's insertion order.
//
// `prompt_logprobs: 0` returns one token per position, so several at the
// boundary is an ambiguous response. Go's map iteration is randomised, so
// taking "the first key" would report a different token each run.
func TestBoundaryEntryIsDeterministic(t *testing.T) {
	entries := map[string]*chatPromptEntry{
		"7":   {DecodedToken: stringPointer("x")},
		"101": {DecodedToken: stringPointer(AnswerPrefix)},
		"3":   {DecodedToken: stringPointer("y")},
	}
	// The terminator is preferred by id wherever it sits.
	for range 20 {
		key, entry := boundaryEntry(entries, 101)
		if key != "101" || *entry.DecodedToken != AnswerPrefix {
			t.Fatalf("boundaryEntry chose %q (%q), want the terminator", key, *entry.DecodedToken)
		}
	}
	// Without it, the lowest id is reported, so the message is the same twice.
	without := map[string]*chatPromptEntry{
		"7": {DecodedToken: stringPointer("x")},
		"3": {DecodedToken: stringPointer("y")},
	}
	for range 20 {
		key, _ := boundaryEntry(without, 101)
		if key != "3" {
			t.Fatalf("boundaryEntry chose %q, want the lowest id", key)
		}
	}
	// A key that is not a token id cannot be the boundary, but it is what the
	// server sent, so it is reported as itself rather than as an id it does not
	// have.
	key, entry := boundaryEntry(map[string]*chatPromptEntry{
		"oops": {DecodedToken: stringPointer("z")}}, 101)
	if key != "oops" || *entry.DecodedToken != "z" {
		t.Errorf("boundaryEntry chose %q (%q), want the unparsable key reported",
			key, *entry.DecodedToken)
	}
}

// TestContentTopLogprobsShapes pins the per-entry validation, which is what makes
// a malformed response name the entry rather than the whole body.
func TestContentTopLogprobsShapes(t *testing.T) {
	withTop := func(entries string) chatChoice {
		var choice chatChoice
		body := `{"logprobs":{"content":[{"top_logprobs":[` + entries + `]}]}}`
		if err := json.Unmarshal([]byte(body), &choice); err != nil {
			t.Fatalf("the test body is not valid JSON: %v", err)
		}
		return choice
	}
	for _, tc := range []struct {
		name    string
		entries string
		want    string
	}{
		{"a logprob as a string", `{"token":"A","logprob":"nope"}`,
			"malformed top_logprobs entry: {'token': 'A', 'logprob': 'nope'}"},
		{"a token as a number", `{"token":5,"logprob":-1}`,
			"malformed top_logprobs entry: {'token': 5, 'logprob': -1}"},
		{"a boolean logprob", `{"token":"A","logprob":true}`,
			"malformed top_logprobs entry"},
		{"a missing logprob", `{"token":"A"}`,
			"malformed top_logprobs entry: {'token': 'A'}"},
		{"a null entry", `null`, "malformed top_logprobs entry: None"},
		{"a bare string", `"A"`, "malformed top_logprobs entry: 'A'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := contentTopLogprobs(withTop(tc.entries))
			if err == nil {
				t.Fatal("a malformed entry was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error is\n  %v\nwant it to contain\n  %s", err, tc.want)
			}
		})
	}

	t.Run("first occurrence wins", func(t *testing.T) {
		// A repeated token cannot carry two logprobs, so the first is the one
		// that was read at that position.
		top, err := contentTopLogprobs(withTop(
			`{"token":"A","logprob":-0.1},{"token":"A","logprob":-9.9}`))
		if err != nil {
			t.Fatalf("contentTopLogprobs: %v", err)
		}
		if top["A"] != -0.1 {
			t.Errorf("A scored %v, want the first occurrence", top["A"])
		}
	})

	t.Run("no content", func(t *testing.T) {
		_, err := contentTopLogprobs(chatChoice{})
		if err == nil || !strings.Contains(err.Error(), "no logprobs.content") {
			t.Errorf("an empty choice gave %v, want the missing-content error", err)
		}
	})

	t.Run("content without top_logprobs", func(t *testing.T) {
		var choice chatChoice
		if err := json.Unmarshal([]byte(`{"logprobs":{"content":[{}]}}`), &choice); err != nil {
			t.Fatalf("the test body is not valid JSON: %v", err)
		}
		_, err := contentTopLogprobs(choice)
		if err == nil || !strings.Contains(err.Error(), "carries no top_logprobs") {
			t.Errorf("an empty position gave %v, want the missing-top_logprobs error", err)
		}
	})
}

// TestSystemRenderNeedsEnoughPromptTokens pins the "unverified" verdict: a
// response that does not carry the tokens after the system line cannot say
// whether the server rendered it the way the text scaffold does.
func TestSystemRenderNeedsEnoughPromptTokens(t *testing.T) {
	// Two entries is enough for the boundary check and far too few to compare,
	// because the local render is dozens of tokens long.
	readout, _ := chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, chatBody(`{"token":"A","logprob":-0.1},{"token":"B","logprob":-0.9}`))
	})
	read, err := readout.ReadImage(context.Background(), map[string]any{"a": 1},
		imageQuestion(2), imageTestURL, ImageReadOptions{})
	if err != nil {
		t.Fatalf("ReadImage: %v", err)
	}
	if read.SystemRender != ChatSystemUnverified {
		t.Errorf("system render = %q, want %q", read.SystemRender, ChatSystemUnverified)
	}
	// And the strict form refuses it, because anything but `string` is a
	// rendering the client cannot vouch for.
	_, err = readout.ReadImage(context.Background(), map[string]any{"a": 1},
		imageQuestion(2), imageTestURL, ImageReadOptions{RequireStringSystem: true})
	if err == nil {
		t.Fatal("the strict form accepted an unverified system render")
	}
	if !strings.Contains(err.Error(), "(system render: unverified)") {
		t.Errorf("the error does not name the verdict: %v", err)
	}
}

// TestSystemRenderComparesTheWholeSystemLine pins the token the comparison is
// anchored on, so a server whose prompt differs *before* the system line is still
// reported as differing.
func TestSystemRenderComparesTheWholeSystemLine(t *testing.T) {
	readout, _ := chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {
		var body chatRequest
		decoded, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(decoded, &body); err != nil {
			t.Errorf("the request is not JSON: %v", err)
		}
		// The response echoes the prompt the client would have rendered, so the
		// verdict is "string"; the test then checks the comparison really ran by
		// looking at the token count it had to work with.
		fmt.Fprint(w, chatBody(`{"token":"A","logprob":-0.1},{"token":"B","logprob":-0.9}`))
	})
	state := map[string]any{"note": "one image"}
	question := imageQuestion(2)
	local, err := readout.Render(state, question, RenderOptions{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	tok, err := readout.EnsureTokenizer()
	if err != nil {
		t.Fatalf("EnsureTokenizer: %v", err)
	}
	// A response that carries exactly the local render's tokens, which is what a
	// server rendering the text scaffold returns.
	localIDs := tok.Encode(local)
	entries := make([]string, 0, len(localIDs))
	for _, id := range localIDs {
		decoded, err := json.Marshal(tok.Decode([]int{id}))
		if err != nil {
			t.Fatalf("encoding the decoded token: %v", err)
		}
		entries = append(entries, fmt.Sprintf(`{"%d":{"logprob":-0.01,"rank":1,`+
			`"decoded_token":%s}}`, id, decoded))
	}
	response := `{"choices":[{"logprobs":{"content":[{"top_logprobs":[` +
		`{"token":"A","logprob":-0.1},{"token":"B","logprob":-0.9}]}]}}],` +
		`"prompt_logprobs":[null,` + strings.Join(entries[1:], ",") + `]}`
	readout, _ = chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, response)
	})
	read, err := readout.ReadImage(context.Background(), state, question, imageTestURL,
		ImageReadOptions{})
	if err != nil {
		t.Fatalf("ReadImage: %v", err)
	}
	if read.SystemRender != ChatSystemString {
		t.Errorf("system render = %q, want %q: a server echoing the local render is the "+
			"text scaffold's rendering", read.SystemRender, ChatSystemString)
	}
}

// TestReadImageOnAZeroReadout pins the promise that an unusable readout reports
// why instead of panicking.
func TestReadImageOnAZeroReadout(t *testing.T) {
	var readout Readout
	_, err := readout.ReadImage(context.Background(), map[string]any{"a": 1},
		imageQuestion(2), imageTestURL, ImageReadOptions{})
	if err == nil {
		t.Fatal("a zero readout read an image")
	}
	var readoutError *ReadoutError
	if !errors.As(err, &readoutError) {
		t.Errorf("got a %T, want a *ReadoutError", err)
	}
	if !strings.Contains(err.Error(), "tokenizer") {
		t.Errorf("the error does not name what is missing: %v", err)
	}
	// A readout with a tokenizer but no transport is the other unusable shape,
	// and it is the one that would otherwise panic inside net/http.
	built, err := NewReadout(ReadoutConfig{BaseURL: "http://x", Model: "de2-test"})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	// A tokenizer, so the read gets as far as the transport it does not have.
	// Without one this would try the hub, which no offline test may do.
	built.SetTokenizer(newFixtureTokenizer(map[string]int{AnswerPrefix: 101, "A": 65, "B": 66}))
	built.client = nil
	_, err = built.ReadImage(context.Background(), map[string]any{"a": 1},
		imageQuestion(2), imageTestURL, ImageReadOptions{})
	if err == nil || !strings.Contains(err.Error(), "NewReadout") {
		t.Errorf("a readout without a client gave %v, want the error naming NewReadout", err)
	}
}

// TestReadImageKeepsTheLetterLimit pins the option limit the image path keeps.
//
// DE-2 reads a text choice of up to MaxCodes options in one prompt, but an image
// question is a chat request the server renders, so it keeps the letter limit.
// DecisionModel refuses a wider one rather than letting the readout do it.
func TestReadImageKeepsTheLetterLimit(t *testing.T) {
	readout, calls := chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, chatBody(`{"token":"A","logprob":-0.1}`))
	})
	_, err := readout.ReadImage(context.Background(), map[string]any{"a": 1},
		imageQuestion(MaxOptions+1), imageTestURL, ImageReadOptions{TopLogprobs: 64})
	if err == nil {
		t.Fatal("an image question above the letter limit was read")
	}
	var questionError *QuestionError
	if !errors.As(err, &questionError) {
		t.Fatalf("got a %T, want a *QuestionError", err)
	}
	if *calls != 0 {
		t.Errorf("the readout sent %d requests for a question it cannot render", *calls)
	}
}

// TestProbeChatRenderReportsUnavailable pins the prefix a caller reads when the
// chat endpoint cannot be asked at all.
func TestProbeChatRenderReportsUnavailable(t *testing.T) {
	readout, calls := chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no chat here", http.StatusNotFound)
	})
	verdict := readout.ProbeChatRender(context.Background(), map[string]any{"health": "check"},
		NewNoul("Is this a health check?"))
	if !strings.HasPrefix(verdict, "unavailable: ") {
		t.Errorf("the probe reported %q, want an unavailable prefix", verdict)
	}
	if !strings.Contains(verdict, "404") {
		t.Errorf("the probe does not report the status: %q", verdict)
	}
	if *calls != 1 {
		t.Errorf("the probe sent %d requests, want 1", *calls)
	}
}

// TestProbeChatRenderSendsTheTextScaffold pins what the probe asks: the text
// scaffold's messages, with no image and no logprobs, so the answer says
// something about the prompt an image question will be rendered with.
func TestProbeChatRenderSendsTheTextScaffold(t *testing.T) {
	var got map[string]any
	readout, _ := chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {
		decoded, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(decoded, &got); err != nil {
			t.Errorf("the request is not JSON: %v", err)
		}
		fmt.Fprint(w, chatBody(`{"token":"A","logprob":-0.1}`))
	})
	readout.ProbeChatRender(context.Background(), map[string]any{"health": "check"},
		NewNoul("Is this a health check?"))

	if got["max_tokens"] != float64(1) || got["temperature"] != float64(0) {
		t.Errorf("the probe sent max_tokens=%v temperature=%v, want 1 and 0",
			got["max_tokens"], got["temperature"])
	}
	if got["prompt_logprobs"] != float64(0) {
		t.Errorf("the probe sent prompt_logprobs=%v, want 0", got["prompt_logprobs"])
	}
	for _, absent := range []string{"logprobs", "top_logprobs", "stop_token_ids"} {
		if _, present := got[absent]; present {
			t.Errorf("the probe sent %s, which it does not need", absent)
		}
	}
	if kwargs, ok := got["chat_template_kwargs"].(map[string]any); !ok ||
		kwargs["enable_thinking"] != false {
		t.Errorf("the probe sent chat_template_kwargs=%v, want enable_thinking false",
			got["chat_template_kwargs"])
	}
	messages, ok := got["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("the probe sent %v messages, want 2", got["messages"])
	}
	first := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != DirectSystem {
		t.Errorf("the probe's system message is %v, want the direct system line", first)
	}
	second := messages[1].(map[string]any)
	if _, isString := second["content"].(string); !isString {
		t.Errorf("the probe's user turn is %T, want the JSON text the text scaffold sends",
			second["content"])
	}
}

// TestReadImageSendsTheImagePartFirst pins the content-part order, which is part
// of the prompt: the image is what the question is about.
func TestReadImageSendsTheImagePartFirst(t *testing.T) {
	var got map[string]any
	readout, _ := chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {
		decoded, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(decoded, &got); err != nil {
			t.Errorf("the request is not JSON: %v", err)
		}
		fmt.Fprint(w, chatBody(`{"token":"A","logprob":-0.1},{"token":"B","logprob":-0.9}`))
	})
	readout.ReadImage(context.Background(), map[string]any{"a": 1}, imageQuestion(2),
		imageTestURL, ImageReadOptions{Debug: true})

	messages, ok := got["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("the read sent %v messages, want 2", got["messages"])
	}
	parts, ok := messages[1].(map[string]any)["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("the user turn is %v, want two content parts",
			messages[1].(map[string]any)["content"])
	}
	first := parts[0].(map[string]any)
	if first["type"] != "image_url" {
		t.Errorf("the first content part is %v, want the image", first)
	}
	if url := first["image_url"].(map[string]any)["url"]; url != imageTestURL {
		t.Errorf("the image URL is %v, want %q", url, imageTestURL)
	}
	second := parts[1].(map[string]any)
	if second["type"] != "text" {
		t.Errorf("the second content part is %v, want the question", second)
	}
	// The image URL must not be in the text part: the file name never reaches a
	// request, and neither does the payload twice.
	if text, _ := second["text"].(string); strings.Contains(text, "iVBORw0KGgo") {
		t.Errorf("the text part carries the image payload: %s", text)
	}
	// The image readout keeps the letter limit and does not repeat the user turn,
	// so the text part is the payload exactly once.
	payload, _, err := Payload(map[string]any{"a": 1}, imageQuestion(2), MaxOptions)
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	want, err := PythonJSON(payload)
	if err != nil {
		t.Fatalf("PythonJSON: %v", err)
	}
	if second["text"] != want {
		t.Errorf("the text part is\n  %v\nwant the rendered payload\n  %s", second["text"], want)
	}
}

// TestCodeSlotsForDoesNotCacheAcrossATokenizerSwap is the image read's half of
// the stale-generation rule.
//
// ReadImage resolves its slots, its terminator and its system-render comparison
// from one snapshot, so a SetTokenizer that lands mid-read must not leave the
// cache holding ids from the tokenizer the read started with.
func TestCodeSlotsForDoesNotCacheAcrossATokenizerSwap(t *testing.T) {
	readout := stubReadout(t, `{"choices":[{"text":"A"}]}`)
	tok, generation, err := readout.tokenizerSnapshot()
	if err != nil {
		t.Fatalf("tokenizerSnapshot: %v", err)
	}

	// The swap the read lost the race against.
	readout.SetTokenizer(newFixtureTokenizer(map[string]int{
		AnswerPrefix: 101, "A": 65, "B": 66}))

	slots, err := readout.codeSlotsFor(tok, generation, 2)
	if err != nil {
		t.Fatalf("codeSlotsFor: %v", err)
	}
	if len(slots) != 2 || slots[0].Text != "A" || slots[1].Text != "B" {
		t.Fatalf("the stale read resolved %v", slots)
	}
	if len(readout.slots) != 0 {
		t.Fatalf("a read that lost the tokenizer race cached %v", readout.slots)
	}
	// The current generation still caches, or the guard would be a permanent
	// cache miss rather than a generation check.
	fresh, err := readout.CodeSlots(2)
	if err != nil {
		t.Fatalf("CodeSlots: %v", err)
	}
	if len(fresh) != 2 || len(readout.slots) != 2 {
		t.Fatalf("the current generation cached %v", readout.slots)
	}
}

// TestReadImageKeepsOneTokenizerAcrossASwap pins the whole read, not just the
// slot helper: the tokenizer is swapped from inside the HTTP handler, which is
// between the snapshot the read takes and the system-render comparison it makes
// afterwards.
//
// Resolving those two against different tokenizers would let the read confirm
// the answer boundary with one and report the server's rendering with another.
// The response carries exactly the local render's tokens, so the verdict is
// `string` under the tokenizer the read started with and cannot be under the
// replacement, whose word list tokenizes the same text differently.
func TestReadImageKeepsOneTokenizerAcrossASwap(t *testing.T) {
	var readout *Readout
	readout, _ = chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {})
	state := map[string]any{"note": "one image"}
	question := imageQuestion(2)
	local, err := readout.Render(state, question, RenderOptions{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	tok, err := readout.EnsureTokenizer()
	if err != nil {
		t.Fatalf("EnsureTokenizer: %v", err)
	}
	localIDs := tok.Encode(local)
	entries := make([]string, 0, len(localIDs))
	for _, id := range localIDs {
		decoded, err := json.Marshal(tok.Decode([]int{id}))
		if err != nil {
			t.Fatalf("encoding the decoded token: %v", err)
		}
		entries = append(entries, fmt.Sprintf(`{"%d":{"logprob":-0.01,"rank":1,`+
			`"decoded_token":%s}}`, id, decoded))
	}
	response := `{"choices":[{"logprobs":{"content":[{"top_logprobs":[` +
		`{"token":"A","logprob":-0.1},{"token":"B","logprob":-0.9}]}]}}],` +
		`"prompt_logprobs":[null,` + strings.Join(entries[1:], ",") + `]}`

	// The swap happens between the read's snapshot and its system-render
	// comparison. The replacement resolves the terminator to nothing at all, so
	// a read that resolved its boundary there would fail rather than answer.
	readout, _ = chatStubReadout(t, func(w http.ResponseWriter, r *http.Request) {
		readout.SetTokenizer(newFixtureTokenizer(map[string]int{
			AnswerPrefix + "x": 101, "A": 65, "B": 66}))
		fmt.Fprint(w, response)
	})
	read, err := readout.ReadImage(context.Background(), state, question, imageTestURL,
		ImageReadOptions{})
	if err != nil {
		t.Fatalf("the read used the replacement tokenizer part way through: %v", err)
	}
	if read.SystemRender != ChatSystemString {
		t.Errorf("system render = %q, want %q: the server echoed the render the read "+
			"started with", read.SystemRender, ChatSystemString)
	}
	if len(read.Probabilities) != 2 {
		t.Errorf("the read returned %v", read.Probabilities)
	}
}
