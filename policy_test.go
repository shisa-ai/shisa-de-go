package shisade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// policyServer answers each request with the next recorded body, so a policy
// that should spend three requests fails loudly when it spends two or four.
//
// Each body carries one %d, which the server fills with the prompt-token count
// it computed for the request. That is what makes the client's own prompt-token
// check meaningful here instead of vacuous.
type policyServer struct {
	bodies    []string
	tokenizer Tokenizer
	// A call with several heads reads them concurrently, so the server is
	// reached from several goroutines at once.
	mu       sync.Mutex
	at       int
	requests []map[string]any
}

func (s *policyServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			// A GET (the model listing) has no body, which is not an error.
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
				t.Errorf("the client sent a body that is not JSON: %v", err)
			}
		}
		s.mu.Lock()
		s.requests = append(s.requests, body)
		if s.at >= len(s.bodies) {
			at := s.at
			s.mu.Unlock()
			t.Errorf("the policy made request %d, but only %d responses were recorded",
				at+1, len(s.bodies))
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[],"usage":{"prompt_tokens":0}}`)
			return
		}
		body_ := s.bodies[s.at]
		s.at++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// A named placeholder rather than a format verb: a body with no
		// placeholder (the model listing) must go out byte for byte.
		fmt.Fprint(w, strings.ReplaceAll(body_, promptTokensPlaceholder,
			strconv.Itoa(s.tokenCount(body))))
	}
}

// tokenCount is how many tokens the prompt cost, counted the way the client
// counts it, so the recorded usage agrees.
func (s *policyServer) tokenCount(body map[string]any) int {
	switch prompt := body["prompt"].(type) {
	case string:
		return len(s.tokenizer.Encode(prompt))
	case []any:
		return len(prompt)
	default:
		return 0
	}
}

// promptTokensPlaceholder is filled in with the prompt-token count the client
// computed for the request.
const promptTokensPlaceholder = "{{prompt_tokens}}"

// codeReadBody is a completion that answers a code read from its top-k, keyed by
// token id.
func codeReadBody(logprobs map[string]float64) string {
	codes, err := CodesFor(len(logprobs))
	if err != nil {
		panic(err)
	}
	entries := make([]string, 0, len(codes))
	for index, code := range codes {
		entries = append(entries, fmt.Sprintf("%q:%v",
			fmt.Sprintf("token_id:%d", 1000+index), logprobs[code]))
	}
	return fmt.Sprintf(`{"choices":[{"text":%q,"logprobs":{"top_logprobs":[{%s}]}}],`+
		`"usage":{"prompt_tokens":%s}}`, codes[0], strings.Join(entries, ","), promptTokensPlaceholder)
}

// thinkBody is a thinking response carrying the given generated tokens.
func thinkBody(tokens ...int) string {
	ids := make([]string, 0, len(tokens))
	for _, token := range tokens {
		ids = append(ids, fmt.Sprint(token))
	}
	return fmt.Sprintf(`{"choices":[{"token_ids":[%s]}],"usage":{"prompt_tokens":%s}}`,
		strings.Join(ids, ","), promptTokensPlaceholder)
}

// codeTokenizer is the stub tokenizer with the first `count` codes, so a wide
// choice can be read.
func codeTokenizer(t *testing.T, count int) *fixtureTokenizer {
	t.Helper()
	codes, err := CodesFor(count)
	if err != nil {
		t.Fatalf("CodesFor(%d): %v", count, err)
	}
	words := map[string]int{AnswerPrefix: 101}
	for index, code := range codes {
		words[code] = 1000 + index
	}
	return newFixtureTokenizer(words)
}

func newPolicyReadout(t *testing.T, count int, bodies ...string) (*Readout, *policyServer) {
	t.Helper()
	tokenizer := codeTokenizer(t, count)
	server := &policyServer{bodies: bodies, tokenizer: tokenizer}
	httpServer := httptest.NewServer(server.handler(t))
	t.Cleanup(httpServer.Close)
	readout, err := NewReadout(ReadoutConfig{
		BaseURL: httpServer.URL, Model: "de2-test", HTTPClient: httpServer.Client(),
	})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	readout.SetTokenizer(tokenizer)
	return readout, server
}

// TestResolveOptionsDefaults pins the family defaults and the two settings that
// have two names.
func TestResolveOptionsDefaults(t *testing.T) {
	for _, tc := range []struct {
		family string
		policy string
		reads  string
	}{
		{string(FamilyDE2), "repeat-think", "double"},
		{string(FamilyDE1), "direct", "single"},
	} {
		options, err := ResolveOptions(Family(tc.family), nil, OptionOverrides{})
		if err != nil {
			t.Fatalf("ResolveOptions(%s): %v", tc.family, err)
		}
		if options.Policy != tc.policy {
			t.Errorf("%s defaulted to policy %q, want %q", tc.family, options.Policy, tc.policy)
		}
		if options.Reads() != tc.reads {
			t.Errorf("%s defaulted to reads %q, want %q", tc.family, options.Reads(), tc.reads)
		}
		if options.ThinkGate != ThinkGate || options.ThinkBudget != ThinkBudget {
			t.Errorf("%s defaulted to gate %v and budget %d", tc.family, options.ThinkGate, options.ThinkBudget)
		}
		if options.ReasoningProb() != options.ThinkGate || options.ReasoningLen() != options.ThinkBudget {
			t.Error("the two names of the gate and the budget disagree")
		}
	}
	options, err := ResolveOptions(FamilyDE2, nil, OptionOverrides{})
	if err != nil {
		t.Fatalf("ResolveOptions: %v", err)
	}
	if !options.Reasoning() {
		t.Error("the DE-2 default does not reason")
	}
	if options.Compound() {
		t.Error("a DE-2 read reports itself as compound")
	}
	if _, err := ResolveOptions("de3", nil, OptionOverrides{}); err == nil {
		t.Error("an unknown family was accepted")
	}
}

// TestResolveOptionsLayers pins the layering: a call's settings change one part
// of the model's read, and the two names of a setting may both be given if they
// agree.
func TestResolveOptionsLayers(t *testing.T) {
	base, err := ResolveOptions(FamilyDE2, nil, OptionOverrides{})
	if err != nil {
		t.Fatalf("ResolveOptions: %v", err)
	}
	policy := "direct"
	gate := 0.5
	budget := 16
	overflow := OverflowStrategies[1]

	cases := []struct {
		name      string
		overrides OptionOverrides
		want      string
		reads     string
		reasoning bool
	}{
		{"policy names a whole read", OptionOverrides{Policy: &policy}, "direct", "single", false},
		{"reads narrows the inherited read", OptionOverrides{Reads: stringPointer("single")},
			"direct", "single", false},
		{"reads double keeps the inherited reasoning", OptionOverrides{Reads: stringPointer("double")},
			"repeat-think", "double", true},
		{"reasoning true reads twice first", OptionOverrides{Reasoning: boolPointer(true)},
			"repeat-think", "double", true},
		{"reasoning false on the default", OptionOverrides{Reasoning: boolPointer(false)},
			"repeat", "double", false},
		{"gate under one name", OptionOverrides{ThinkGate: &gate}, "repeat-think", "double", true},
		{"gate under the other", OptionOverrides{ReasoningProb: &gate}, "repeat-think", "double", true},
		{"budget under one name", OptionOverrides{ThinkBudget: &budget}, "repeat-think", "double", true},
		{"budget under the other", OptionOverrides{ReasoningLen: &budget}, "repeat-think", "double", true},
		{"overflow by name", OptionOverrides{Overflow: &overflow}, "repeat-think", "double", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options, err := ResolveOptions(FamilyDE2, &base, tc.overrides)
			if err != nil {
				t.Fatalf("ResolveOptions: %v", err)
			}
			if options.Policy != tc.want || options.Reads() != tc.reads || options.Reasoning() != tc.reasoning {
				t.Errorf("resolved to %q (%s, reasoning=%v), want %q (%s, reasoning=%v)",
					options.Policy, options.Reads(), options.Reasoning(), tc.want, tc.reads, tc.reasoning)
			}
		})
	}

	// Both names of a setting, carrying the same value, is allowed.
	agreeing := 0.4
	options, err := ResolveOptions(FamilyDE2, &base, OptionOverrides{ThinkGate: &agreeing, ReasoningProb: &agreeing})
	if err != nil {
		t.Fatalf("both names carrying the same value were refused: %v", err)
	}
	if options.ThinkGate != 0.4 {
		t.Errorf("the gate is %v, want 0.4", options.ThinkGate)
	}
	// A policy and a part that agree are allowed.
	repeatThink := "repeat-think"
	if _, err := ResolveOptions(FamilyDE2, &base, OptionOverrides{
		Policy: &repeatThink, Reasoning: boolPointer(true)}); err != nil {
		t.Errorf("a policy and a part that agree were refused: %v", err)
	}
}

// TestResolveOptionsRefusals pins every setting the Python client validates. A
// combination neither contract defines is an error rather than an unmeasured
// read.
func TestResolveOptionsRefusals(t *testing.T) {
	base, err := ResolveOptions(FamilyDE2, nil, OptionOverrides{})
	if err != nil {
		t.Fatalf("ResolveOptions: %v", err)
	}
	bad := 1.5
	below := -0.5
	other := 0.5
	unknown := "sometimes"
	zero := 0

	cases := []struct {
		name      string
		family    Family
		overrides OptionOverrides
		want      string
	}{
		{"unknown policy", FamilyDE2, OptionOverrides{Policy: &unknown}, "unknown policy"},
		{"bad reads", FamilyDE2, OptionOverrides{Reads: &unknown}, "reads must be"},
		{"reasoning after a single read", FamilyDE2,
			OptionOverrides{Reads: stringPointer("single"), Reasoning: boolPointer(true)},
			"cannot be combined with reads='single'"},
		{"policy disagreeing with reads", FamilyDE2,
			OptionOverrides{Policy: stringPointer("direct"), Reads: stringPointer("double")},
			"disagrees with"},
		{"policy disagreeing with reasoning", FamilyDE2,
			OptionOverrides{Policy: stringPointer("repeat"), Reasoning: boolPointer(true)},
			"disagrees with"},
		{"gate above one", FamilyDE2, OptionOverrides{ThinkGate: &bad}, "must be a probability"},
		{"gate below zero", FamilyDE2, OptionOverrides{ThinkGate: &below}, "must be a probability"},
		{"gate under the other name", FamilyDE2, OptionOverrides{ReasoningProb: &bad}, "must be a probability"},
		{"the two names disagreeing", FamilyDE2,
			OptionOverrides{ThinkGate: &bad, ReasoningProb: &other}, "set the same thing"},
		{"budget of zero", FamilyDE2, OptionOverrides{ThinkBudget: &zero}, "must be a positive integer"},
		{"budget under the other name", FamilyDE2, OptionOverrides{ReasoningLen: &zero}, "must be a positive integer"},
		{"unknown overflow", FamilyDE2, OptionOverrides{Overflow: &unknown}, "overflow must be"},
		// A double read and reasoning are DE-2 reads; DE-1 was measured on one.
		{"a double read on DE-1", FamilyDE1, OptionOverrides{Reads: stringPointer("double")},
			"not defined for de1"},
		{"reasoning on DE-1", FamilyDE1, OptionOverrides{Reasoning: boolPointer(true)},
			"not defined for de1"},
		{"compound on DE-2", FamilyDE2, OptionOverrides{Compound: boolPointer(true)},
			"not defined for de2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveOptions(tc.family, &base, tc.overrides)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error is %v, want one containing %q", err, tc.want)
			}
		})
	}
	// compound=false on DE-2 is the inherited value and is not an error.
	if _, err := ResolveOptions(FamilyDE2, &base, OptionOverrides{Compound: boolPointer(false)}); err != nil {
		t.Errorf("compound=false on DE-2 was refused: %v", err)
	}
	// A gate of exactly 0 or 1 is in range.
	for _, value := range []float64{0, 1} {
		if _, err := ResolveOptions(FamilyDE2, &base, OptionOverrides{ThinkGate: &value}); err != nil {
			t.Errorf("a gate of %v was refused: %v", value, err)
		}
	}
}

// TestReadPolicyDirect pins the cheapest read: one prompt, one request, and the
// direct temperature.
func TestReadPolicyDirect(t *testing.T) {
	readout, server := newPolicyReadout(t, 3, codeReadBody(map[string]float64{"A": -0.1, "B": -2.0, "C": -3.0}))
	options := ReadOptions{Family: FamilyDE2, Policy: "direct", ThinkGate: ThinkGate, ThinkBudget: ThinkBudget}
	question := NewNoul("Is this urgent?")
	read, err := ReadPolicy(context.Background(), readout, map[string]any{"evidence": "x"}, question, options, false)
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	if read.Strategy != StrategyDirect {
		t.Errorf("strategy = %q, want %q", read.Strategy, StrategyDirect)
	}
	if read.LogicalReads() != 1 || read.Requests() != 1 {
		t.Errorf("logical reads %d, requests %d; want 1 and 1", read.LogicalReads(), read.Requests())
	}
	if read.Thought != nil {
		t.Error("the direct read produced a thought")
	}
	if len(read.Components) != 1 || read.Components[0].Stage != StrategyDirect {
		t.Errorf("components = %v, want one direct stage", read.Components)
	}
	if server.at != 1 {
		t.Errorf("the read spent %d requests, want 1", server.at)
	}
	// A direct read renders the user turn once.
	prompt, _ := server.requests[0]["prompt"].(string)
	if strings.Count(prompt, "Read the same input again before answering") != 0 {
		t.Error("the direct read wrote the user turn twice")
	}
}

// TestReadPolicyRepeatStopsAtTheGate pins the gate: the repeated read is the
// answer when its raw top probability is at or above it, and the gate is read
// before any calibration.
func TestReadPolicyRepeatStopsAtTheGate(t *testing.T) {
	// A top probability of about 0.73, just above the gate.
	readout, server := newPolicyReadout(t, 2, codeReadBody(map[string]float64{"A": -0.314, "B": -1.314}))
	options := ReadOptions{Family: FamilyDE2, Policy: "repeat-think", ThinkGate: ThinkGate, ThinkBudget: ThinkBudget}
	read, err := ReadPolicy(context.Background(), readout, map[string]any{"evidence": "x"},
		NewNoul("Is this urgent?"), options, false)
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	if read.Strategy != StrategyRepeat {
		t.Errorf("strategy = %q, want %q above the gate", read.Strategy, StrategyRepeat)
	}
	if read.Thought != nil || read.Requests() != 1 {
		t.Errorf("an above-gate read thought: thought=%v requests=%d", read.Thought, read.Requests())
	}
	if server.at != 1 {
		t.Errorf("the read spent %d requests, want 1", server.at)
	}
	// The repeated read renders the user turn twice.
	prompt, _ := server.requests[0]["prompt"].(string)
	if !strings.Contains(prompt, "Read the same input again before answering") {
		t.Error("the repeated read did not write the user turn twice")
	}
	// A policy of "repeat" stops there even when the read is unsure.
	readout, server = newPolicyReadout(t, 2, codeReadBody(map[string]float64{"A": -0.9, "B": -1.0}))
	options.Policy = "repeat"
	read, err = ReadPolicy(context.Background(), readout, map[string]any{"evidence": "x"},
		NewNoul("Is this urgent?"), options, false)
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	if read.Strategy != StrategyRepeat || server.at != 1 {
		t.Errorf("policy repeat went past the repeated read: %q, %d requests", read.Strategy, server.at)
	}
}

// TestReadPolicyThinksWhenUnsure pins the whole three-request read: the repeated
// read, the thought, and the read after it.
func TestReadPolicyThinksWhenUnsure(t *testing.T) {
	readout, server := newPolicyReadout(t, 2,
		codeReadBody(map[string]float64{"A": -0.9, "B": -1.0}),
		thinkBody(500, 501),
		codeReadBody(map[string]float64{"A": -1.2, "B": -0.5}))
	options := ReadOptions{Family: FamilyDE2, Policy: "repeat-think", ThinkGate: ThinkGate, ThinkBudget: ThinkBudget}
	read, err := ReadPolicy(context.Background(), readout, map[string]any{"evidence": "x"},
		NewNoul("Is this urgent?"), options, false)
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	if read.Strategy != StrategyRepeatThink {
		t.Fatalf("strategy = %q, want %q", read.Strategy, StrategyRepeatThink)
	}
	if read.LogicalReads() != 2 || read.Requests() != 3 {
		t.Errorf("logical reads %d, requests %d; want 2 and 3", read.LogicalReads(), read.Requests())
	}
	if read.Thought == nil {
		t.Fatal("the unsure read produced no thought")
	}
	if read.ThoughtTokens() != 2 || read.Thought.Closed {
		t.Errorf("the thought has %d tokens and closed=%v, want 2 and false",
			read.ThoughtTokens(), read.Thought.Closed)
	}
	if len(read.Components) != 2 || read.Components[0].Stage != StrategyRepeat ||
		read.Components[1].Stage != "think" {
		t.Errorf("components = %v, want repeat2 then think", read.Components)
	}
	// The final read answers with the read after the thought, not the first.
	probabilities := read.Read().Probabilities
	if probabilities["B"] <= probabilities["A"] {
		t.Errorf("the final read is not the one after the thought: %v", probabilities)
	}
	if server.at != 3 {
		t.Errorf("the read spent %d requests, want 3", server.at)
	}
	// The third request continues the thought's tokens and adds the closing
	// terminator, which is the only way the model's own close can be missing.
	after, ok := server.requests[2]["prompt"].([]any)
	if !ok {
		t.Fatalf("the read after a thought sent %T, want a list of token ids",
			server.requests[2]["prompt"])
	}
	if len(after) == 0 || int(after[len(after)-1].(float64)) != 101 {
		t.Errorf("the read after a thought did not end on the terminator: %v", after)
	}
	if len(after) < 2 || int(after[len(after)-2].(float64)) != 501 {
		t.Errorf("the read after a thought did not continue the thought: %v", after)
	}
}

// TestReadPolicyWritesTheCloseEvenWhenTheModelDid pins that a thought the model
// closed is still followed by the terminator, which the read needs.
func TestReadPolicyWritesTheCloseEvenWhenTheModelDid(t *testing.T) {
	readout, server := newPolicyReadout(t, 2,
		codeReadBody(map[string]float64{"A": -0.9, "B": -1.0}),
		thinkBody(500, 501, 101),
		codeReadBody(map[string]float64{"A": -0.5, "B": -1.2}))
	options := ReadOptions{Family: FamilyDE2, Policy: "repeat-think", ThinkGate: ThinkGate, ThinkBudget: ThinkBudget}
	read, err := ReadPolicy(context.Background(), readout, map[string]any{"evidence": "x"},
		NewNoul("Is this urgent?"), options, false)
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	if !read.Thought.Closed {
		t.Error("the thought is not reported as closed")
	}
	if read.ThoughtTokens() != 2 {
		t.Errorf("the thought has %d tokens, want 2: the close is not part of it",
			read.ThoughtTokens())
	}
	after := server.requests[2]["prompt"].([]any)
	if int(after[len(after)-1].(float64)) != 101 {
		t.Error("the read after a closed thought did not end on the terminator")
	}
}

// TestReadPolicyOptionCap pins that a question above the option cap never thinks,
// however unsure the read is.
func TestReadPolicyOptionCap(t *testing.T) {
	codes, err := CodesFor(27)
	if err != nil {
		t.Fatalf("CodesFor(27): %v", err)
	}
	logprobs := map[string]float64{}
	for index, code := range codes {
		// A flat distribution over 27 options: the top probability is well
		// below the gate.
		logprobs[code] = -3.0 - float64(index)*0.001
	}
	readout, server := newPolicyReadout(t, 27, codeReadBody(logprobs))
	options := ReadOptions{Family: FamilyDE2, Policy: "repeat-think", ThinkGate: ThinkGate, ThinkBudget: ThinkBudget}
	question := NewChoice("Which queue?", wideOptions(27))
	read, err := ReadPolicy(context.Background(), readout, map[string]any{"evidence": "x"}, question, options, false)
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	if read.Strategy != StrategyRepeat {
		t.Errorf("a 27-option question above the gate reasoned: %q", read.Strategy)
	}
	if server.at != 1 {
		t.Errorf("the read spent %d requests, want 1", server.at)
	}
	// At the cap it does reason.
	readout, server = newPolicyReadout(t, 26, codeReadBody(flatLogprobs(t, 26)),
		thinkBody(500, 501), codeReadBody(flatLogprobs(t, 26)))
	question = NewChoice("Which queue?", wideOptions(26))
	read, err = ReadPolicy(context.Background(), readout, map[string]any{"evidence": "x"}, question, options, false)
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	if read.Strategy != StrategyRepeatThink {
		t.Errorf("a 26-option question below the gate did not reason: %q (spent %d)",
			read.Strategy, server.at)
	}
}

// TestReadPolicyRefusesABadPolicyAndQuestion pins the two validations that
// happen before any request.
func TestReadPolicyRefusesABadPolicyAndQuestion(t *testing.T) {
	readout, server := newPolicyReadout(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	options := ReadOptions{Family: FamilyDE2, Policy: "sometimes", ThinkGate: ThinkGate, ThinkBudget: ThinkBudget}
	if _, err := ReadPolicy(context.Background(), readout, map[string]any{"e": "x"},
		NewNoul("q?"), options, false); err == nil {
		t.Error("an unknown policy was accepted")
	}
	options.Policy = "repeat-think"
	// A noul with fewer than two options is not a question the readout can
	// render, and it is refused before the first request.
	if _, err := ReadPolicy(context.Background(), readout, map[string]any{"e": "x"},
		NewChoice("q?", nil), options, false); err == nil {
		t.Error("a question with no options was accepted")
	}
	if server.at != 0 {
		t.Errorf("a refused policy spent %d requests", server.at)
	}
}

// TestPolicyReadRaw pins the shape the recorded fixtures carry.
func TestPolicyReadRaw(t *testing.T) {
	readout, _ := newPolicyReadout(t, 2,
		codeReadBody(map[string]float64{"A": -0.9, "B": -1.0}),
		thinkBody(500, 501),
		codeReadBody(map[string]float64{"A": -1.2, "B": -0.5}))
	options := ReadOptions{Family: FamilyDE2, Policy: "repeat-think", ThinkGate: ThinkGate, ThinkBudget: ThinkBudget}
	read, err := ReadPolicy(context.Background(), readout, map[string]any{"evidence": "x"},
		NewNoul("Is this urgent?"), options, true)
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	raw := read.Raw(true)
	if raw["strategy"] != StrategyRepeatThink {
		t.Errorf("raw strategy = %v", raw["strategy"])
	}
	if raw["logical_reads"] != 2 || raw["requests"] != 3 {
		t.Errorf("raw logical_reads=%v requests=%v, want 2 and 3",
			raw["logical_reads"], raw["requests"])
	}
	if raw["thought_tokens"] != 2 || raw["thought_closed"] != false {
		t.Errorf("raw thought_tokens=%v thought_closed=%v", raw["thought_tokens"], raw["thought_closed"])
	}
	components, ok := raw["components"].([]any)
	if !ok || len(components) != 2 {
		t.Fatalf("raw components = %v, want two", raw["components"])
	}
	first := components[0].(map[string]any)
	if first["stage"] != StrategyRepeat {
		t.Errorf("the first component's stage is %v", first["stage"])
	}
	// The debug detail is only carried when it was asked for.
	if _, ok := first["prompt"]; !ok {
		t.Error("a debug read did not keep its prompt")
	}
	if _, ok := first["top_logprobs"]; !ok {
		t.Error("a debug read did not keep its top logprobs")
	}
	if _, ok := raw["thought"]; !ok {
		t.Error("a debug read did not keep its thought text")
	}
	// A non-debug read keeps the prompt out.
	readout, _ = newPolicyReadout(t, 2, codeReadBody(map[string]float64{"A": -0.9, "B": -1.0}),
		thinkBody(500, 501),
		codeReadBody(map[string]float64{"A": -1.2, "B": -0.5}))
	read, err = ReadPolicy(context.Background(), readout, map[string]any{"evidence": "x"},
		NewNoul("Is this urgent?"), options, false)
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	plain := read.Raw(false)
	if _, ok := plain["thought"]; ok {
		t.Error("a non-debug read kept its thought text")
	}
	if _, ok := plain["components"].([]any)[0].(map[string]any)["prompt"]; ok {
		t.Error("a non-debug read kept its prompt")
	}
	// The slices and maps in Raw are copies, so a caller cannot change the read.
	plain["missing_from_top"] = []string{"A"}
	if len(read.MissingFromTop()) != 0 {
		t.Error("Raw handed out its own slice")
	}
}

// TestPolicyReadAccessors pins the arithmetic that a caller reads off the result.
func TestPolicyReadAccessors(t *testing.T) {
	readout, _ := newPolicyReadout(t, 2,
		codeReadBody(map[string]float64{"A": -0.9, "B": -1.0}),
		thinkBody(500, 501),
		codeReadBody(map[string]float64{"A": -1.2, "B": -0.5}))
	options := ReadOptions{Family: FamilyDE2, Policy: "repeat-think", ThinkGate: ThinkGate, ThinkBudget: ThinkBudget}
	read, err := ReadPolicy(context.Background(), readout, map[string]any{"evidence": "x"},
		NewNoul("Is this urgent?"), options, false)
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	// The prompt is counted twice for the two code reads plus the thought's own.
	if read.PromptTokens() < 3 {
		t.Errorf("prompt tokens = %d, want the two reads and the thought", read.PromptTokens())
	}
	if read.Requests() != 3 {
		t.Errorf("requests = %d, want 3", read.Requests())
	}
	if read.MissingFromTop() == nil {
		t.Error("MissingFromTop returned nil, which is not a list a caller can range over")
	}
	if read.ElapsedMS < 0 {
		t.Errorf("elapsed = %v", read.ElapsedMS)
	}
	// A repeat read reports no thought at all.
	readout, _ = newPolicyReadout(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	read, err = ReadPolicy(context.Background(), readout, map[string]any{"evidence": "x"},
		NewNoul("Is this urgent?"), options, false)
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	if read.ThoughtTokens() != 0 {
		t.Errorf("a repeat read reports %d thought tokens", read.ThoughtTokens())
	}
}

func wideOptions(count int) []Option {
	options := make([]Option, 0, count)
	for index := 0; index < count; index++ {
		options = append(options, Option{Key: fmt.Sprintf("q%02d", index),
			Description: fmt.Sprintf("Queue %d", index)})
	}
	return options
}

func flatLogprobs(t *testing.T, count int) map[string]float64 {
	t.Helper()
	codes, err := CodesFor(count)
	if err != nil {
		t.Fatalf("CodesFor(%d): %v", count, err)
	}
	logprobs := make(map[string]float64, count)
	for index, code := range codes {
		logprobs[code] = -3.0 - float64(index)*0.001
	}
	return logprobs
}

func stringPointer(value string) *string { return &value }
