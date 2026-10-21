package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	shisade "github.com/shisa-ai/shisa-de-go"
)

// The command line is tested two ways: the flag-to-config mapping, which needs
// no server, and `run` end to end against a stub endpoint with a stub tokenizer.
// The stub is what lets `explain` run at all, because the real tokenizer is a
// download.

// cliTokenizer is a tokenizer over a fixed word list, longest match first.
//
// It exists so the boundary check has something to pass: the rendered prompt
// ends with the answer terminator, and each code appended to the terminator is
// its own token, which is the property the real tokenizer has to have.
type cliTokenizer struct {
	words   map[string]int
	byID    map[int]string
	ordered []string
}

func newCLITokenizer(words map[string]int) *cliTokenizer {
	tokenizer := &cliTokenizer{words: words, byID: map[int]string{}}
	for word, id := range words {
		tokenizer.byID[id] = word
		tokenizer.ordered = append(tokenizer.ordered, word)
	}
	sort.Slice(tokenizer.ordered, func(i, j int) bool {
		if len(tokenizer.ordered[i]) != len(tokenizer.ordered[j]) {
			return len(tokenizer.ordered[i]) > len(tokenizer.ordered[j])
		}
		return tokenizer.ordered[i] < tokenizer.ordered[j]
	})
	return tokenizer
}

func (t *cliTokenizer) Encode(text string) []int {
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

func (t *cliTokenizer) Decode(ids []int) string {
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

// cliTokenizerFor builds the stub tokenizer over every codebook code, which is
// what `doctor`'s boundary check needs.
func cliTokenizerFor(t *testing.T) *cliTokenizer {
	t.Helper()
	codes, err := shisade.Codebook()
	if err != nil {
		t.Fatalf("Codebook: %v", err)
	}
	words := map[string]int{shisade.AnswerPrefix: 101}
	for index, code := range codes {
		words[code] = 1000 + index
	}
	return newCLITokenizer(words)
}

// cliServer answers the model listing and the code reads the command line makes.
type cliServer struct {
	server    *httptest.Server
	tokenizer *cliTokenizer

	mu       sync.Mutex
	requests []map[string]any
	// paths[i] is the path requests[i] was sent to, so a test can tell the chat
	// endpoint's requests from the completions ones. It is kept beside the body
	// rather than inside it: the body is what the client sent.
	paths []string
	// methods[i] is the method requests[i] used, for the replay below, which
	// compares the requests a recorded Python run made against the ones this
	// command line makes.
	methods []string
	// models is the id list `/v1/models` returns.
	models []string
	// completionStatus, when non-zero, is the status every completion answers
	// with, so a test can watch what a failed read does.
	completionStatus int
}

func newCLIServer(t *testing.T, models ...string) *cliServer {
	t.Helper()
	stub := &cliServer{tokenizer: cliTokenizerFor(t), models: models}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
				t.Errorf("the client sent a body that is not JSON: %v", err)
			}
		}
		stub.mu.Lock()
		stub.requests = append(stub.requests, body)
		stub.paths = append(stub.paths, r.URL.Path)
		stub.methods = append(stub.methods, r.Method)
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/models") && stub.completionStatus != 0 {
			w.WriteHeader(stub.completionStatus)
			fmt.Fprint(w, `{"error":{"message":"the read is not available"}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/models") {
			entries := make([]string, 0, len(stub.models))
			for _, id := range stub.models {
				entries = append(entries, fmt.Sprintf(`{"id":%q}`, id))
			}
			fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(entries, ","))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			fmt.Fprint(w, stub.chat(body))
			return
		}
		fmt.Fprint(w, stub.completion(body))
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

// completion is a code read that answers from its top-k, with the prompt-token
// count the client computed so `usage` agrees.
func (s *cliServer) completion(body map[string]any) string {
	tokens := 0
	switch prompt := body["prompt"].(type) {
	case string:
		tokens = len(s.tokenizer.Encode(prompt))
	case []any:
		tokens = len(prompt)
	}
	codes, err := shisade.Codebook()
	if err != nil {
		return `{"choices":[],"usage":{"prompt_tokens":0}}`
	}
	entries := make([]string, 0, len(codes))
	for index := range codes {
		// A steep spread: the first code is the answer by a wide margin, so the
		// top probability clears the think gate whatever the option count and
		// the default policy stops at the repeated read. A thought would need a
		// stub that answers `token_ids`, which this one does not.
		logprob := -8.0 - float64(index)
		if index == 0 {
			logprob = -0.1
		}
		entries = append(entries, fmt.Sprintf(`"token_id:%d":%v`, 1000+index, logprob))
	}
	return fmt.Sprintf(`{"choices":[{"text":%q,"logprobs":{"top_logprobs":[{%s}]}}],`+
		`"usage":{"prompt_tokens":%d}}`, codes[0], strings.Join(entries, ","), tokens)
}

// chat answers the chat endpoint, which serves two calls: `doctor --probe` asks
// how the server renders the system turn, and `ask --image` reads an answer
// position there.
//
// The stub renders the pinned template from the messages it was sent, so its
// prompt tokens are the ones the client would have rendered itself and the
// verdict is `string`. That is what a server rendering the text scaffold
// returns; a stock vLLM writes one extra space and reports `differs`, which the
// readout's own replay gate covers.
func (s *cliServer) chat(body map[string]any) string {
	messages, _ := body["messages"].([]any)
	if len(messages) != 2 {
		return `{"choices":[],"usage":{"prompt_tokens":0}}`
	}
	system, _ := messages[0].(map[string]any)["content"].(string)
	user, _ := messages[1].(map[string]any)["content"].(string)
	served := "<bos><|turn>system\n" + system + "<turn|>\n<|turn>user\n" + user +
		"<turn|>\n<|turn>model\n<|channel>thought\n" + shisade.AnswerPrefix
	ids := s.tokenizer.Encode(served)
	// The first prompt position carries no logprob, which is what
	// `prompt_logprobs: 0` returns.
	promptLogprobs := make([]any, 0, len(ids))
	promptLogprobs = append(promptLogprobs, nil)
	for _, id := range ids[1:] {
		promptLogprobs = append(promptLogprobs, map[string]any{
			strconv.Itoa(id): map[string]any{
				"logprob": -0.01, "rank": 1, "decoded_token": s.tokenizer.Decode([]int{id}),
			},
		})
	}
	response := map[string]any{
		"prompt_logprobs": promptLogprobs,
		"usage": map[string]any{
			"prompt_tokens": len(ids), "completion_tokens": 1},
		"choices": []any{},
	}
	if _, isRead := body["logprobs"]; isRead {
		codes, err := shisade.Codebook()
		if err != nil {
			return `{"choices":[],"usage":{"prompt_tokens":0}}`
		}
		limit, _ := body["top_logprobs"].(float64)
		top := make([]any, 0, len(codes))
		for index, code := range codes {
			if float64(len(top)) >= limit {
				break
			}
			// The same steep spread `completion` uses, so the first option is
			// the answer by a wide margin.
			logprob := -8.0 - float64(index)
			if index == 0 {
				logprob = -0.1
			}
			top = append(top, map[string]any{"token": code, "logprob": logprob})
		}
		response["choices"] = []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": codes[0]},
			"logprobs": map[string]any{"content": []any{map[string]any{
				"token": codes[0], "logprob": -0.1, "top_logprobs": top}}},
		}}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return `{"choices":[],"usage":{"prompt_tokens":0}}`
	}
	return string(encoded)
}

func (s *cliServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// installCLIModel replaces the client constructor with one that injects the stub
// readout, and restores it when the test ends.
func installCLIModel(t *testing.T, server *cliServer) {
	t.Helper()
	previous := newModel
	t.Cleanup(func() { newModel = previous })
	newModel = func(config shisade.DecisionModelConfig) (*shisade.DecisionModel, error) {
		return stubModel(server, config)
	}
}

// stubModel is the client `installCLIModel` builds: the real constructor with a
// readout over the stub tokenizer and the stub transport.
func stubModel(server *cliServer, config shisade.DecisionModelConfig) (*shisade.DecisionModel, error) {
	config.BaseURL = server.server.URL
	config.HTTPClient = server.server.Client()
	readout, err := shisade.NewReadout(shisade.ReadoutConfig{
		BaseURL: server.server.URL, Model: config.Model,
		Family: string(config.Family), HTTPClient: server.server.Client(),
	})
	if err != nil {
		return nil, err
	}
	readout.SetTokenizer(server.tokenizer)
	config.Readout = readout
	return shisade.NewDecisionModel(config)
}

// invoke runs the command line and returns the exit status and both streams.
func invoke(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	status := run(context.Background(), argv, &stdout, &stderr)
	return status, stdout.String(), stderr.String()
}

// TestModelConfigMapsFlags pins the flag-to-config mapping, which is the part of
// the command line that has no server in it.
func TestModelConfigMapsFlags(t *testing.T) {
	t.Run("the defaults", func(t *testing.T) {
		parsed, err := parse("doctor", nil)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		config, err := parsed.modelConfig()
		if err != nil {
			t.Fatalf("modelConfig: %v", err)
		}
		if config.Model != defaultModel {
			t.Errorf("the model is %q, want %q", config.Model, defaultModel)
		}
		// Everything left nil is inherited, which is what makes the library's
		// own defaults apply.
		if config.Family != "" {
			t.Errorf("the family is %q, want unset", config.Family)
		}
		if config.Calibration != nil {
			t.Errorf("the calibration is set, want nil for auto")
		}
		if config.Overrides.Policy != nil || config.Overrides.Reads != nil ||
			config.Overrides.Reasoning != nil || config.Overrides.Compound != nil {
			t.Errorf("the overrides are not empty: %+v", config.Overrides)
		}
		if config.Timeout.Seconds() != 120 {
			t.Errorf("the timeout is %v, want 120s", config.Timeout)
		}
	})

	t.Run("every setting", func(t *testing.T) {
		parsed, err := parse("doctor", []string{
			"--base-url", "http://example.invalid/openai",
			"--model", "shisa-ai/shisa-de-2",
			"--tokenizer", "some/tokenizer",
			"--family", "de2",
			"--policy", "repeat",
			"--reads", "single",
			"--reasoning-prob", "0.55",
			"--reasoning-len", "64",
			"--timeout", "7.5",
		})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		config, err := parsed.modelConfig()
		if err != nil {
			t.Fatalf("modelConfig: %v", err)
		}
		if config.BaseURL != "http://example.invalid/openai" ||
			config.Tokenizer != "some/tokenizer" {
			t.Errorf("the endpoint or tokenizer is wrong: %+v", config)
		}
		if config.Family != shisade.FamilyDE2 {
			t.Errorf("the family is %q", config.Family)
		}
		if config.Overrides.Policy == nil || *config.Overrides.Policy != "repeat" {
			t.Errorf("the policy override is %v", config.Overrides.Policy)
		}
		if config.Overrides.Reads == nil || *config.Overrides.Reads != "single" {
			t.Errorf("the reads override is %v", config.Overrides.Reads)
		}
		if config.Overrides.ReasoningProb == nil || *config.Overrides.ReasoningProb != 0.55 {
			t.Errorf("the gate is %v", config.Overrides.ReasoningProb)
		}
		if config.Overrides.ReasoningLen == nil || *config.Overrides.ReasoningLen != 64 {
			t.Errorf("the budget is %v", config.Overrides.ReasoningLen)
		}
		if config.Timeout.Seconds() != 7.5 {
			t.Errorf("the timeout is %v", config.Timeout)
		}
	})

	t.Run("the negative booleans", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			argv []string
			want *bool
		}{
			{"--no-reasoning", []string{"--no-reasoning"}, boolPointer(false)},
			{"--reasoning", []string{"--reasoning"}, boolPointer(true)},
			{"--no-compound", []string{"--no-compound"}, boolPointer(false)},
		} {
			parsed, err := parse("ask", tc.argv)
			if err != nil {
				t.Fatalf("%s: parse: %v", tc.name, err)
			}
			config, err := parsed.modelConfig()
			if err != nil {
				t.Fatalf("%s: modelConfig: %v", tc.name, err)
			}
			got := config.Overrides.Reasoning
			if tc.argv[0] == "--no-compound" {
				got = config.Overrides.Compound
			}
			if got == nil || *got != *tc.want {
				t.Errorf("%s: the override is %v, want %v", tc.name, got, *tc.want)
			}
			// The one that was not given stays nil, so the library's default
			// applies to it.
			other := config.Overrides.Compound
			if tc.argv[0] == "--no-compound" {
				other = config.Overrides.Reasoning
			}
			if other != nil {
				t.Errorf("%s: the other override is %v, want nil", tc.name, other)
			}
		}
	})

	t.Run("a named calibration", func(t *testing.T) {
		parsed, err := parse("doctor", []string{"--calibration", "de2"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		config, err := parsed.modelConfig()
		if err != nil {
			t.Fatalf("modelConfig: %v", err)
		}
		if config.Calibration == nil || !config.Calibration.Fitted() {
			t.Errorf("the calibration is %v", config.Calibration)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		cases := []struct {
			name string
			argv []string
		}{
			{"an unknown policy", []string{"--policy", "guess"}},
			{"an unknown reads", []string{"--reads", "triple"}},
			{"an unknown family", []string{"--family", "de9"}},
			{"a calibration that is neither a family nor a file",
				[]string{"--calibration", "nope"}},
			{"an unknown flag", []string{"--nope"}},
			{"a missing value", []string{"--model"}},
			{"an unexpected argument", []string{"leftover"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				parsed, err := parse("doctor", tc.argv)
				if err != nil {
					return // the flag layer refused it
				}
				if _, err := parsed.modelConfig(); err == nil {
					t.Errorf("%s was accepted", tc.name)
				}
			})
		}
	})
}

// TestRunDispatch pins the exit statuses and the streams for a good and a bad
// invocation.
func TestRunDispatch(t *testing.T) {
	for _, argv := range [][]string{
		{"doctor", "--policy", "guess"},
		{"ask", "--nope"},
		{"explain", "--reads", "triple"},
	} {
		status, stdout, stderr := invoke(t, argv...)
		if status != 2 {
			t.Errorf("%v: exit status %d, want 2", argv, status)
		}
		if stdout != "" {
			t.Errorf("%v: a refused command wrote to stdout: %q", argv, stdout)
		}
		if !strings.Contains(stderr, "usage: shisa-de") {
			t.Errorf("%v: the error does not carry the usage: %q", argv, stderr)
		}
	}

	for _, argv := range [][]string{{"--help"}, {"-h"}, {"doctor", "--help"}} {
		status, stdout, _ := invoke(t, argv...)
		if status != 0 || !strings.Contains(stdout, "usage: shisa-de") {
			t.Errorf("%v: status %d, stdout %q", argv, status, stdout)
		}
	}

	// `help` is not one of `argparse`'s subcommands, so it is an unknown word
	// and exits 2 like any other; only `-h`/`--help` print the usage.
	if status, stdout, stderr := invoke(t, "help"); status != 2 || stdout != "" ||
		!strings.Contains(stderr, "unknown command") {
		t.Errorf("the bare word help gave %d, stdout %q, stderr %q", status, stdout, stderr)
	}

	if status, _, stderr := invoke(t, "nonsense"); status != 2 ||
		!strings.Contains(stderr, "unknown command") {
		t.Errorf("an unknown command gave %d and %q", status, stderr)
	}
	if status, _, stderr := invoke(t); status != 2 || !strings.Contains(stderr, "usage") {
		t.Errorf("no arguments gave %d and %q", status, stderr)
	}
}

// TestDoctorText is `doctor` end to end, without the probe.
func TestDoctorText(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2", "something-else")
	installCLIModel(t, server)

	status, stdout, stderr := invoke(t, "doctor", "--model", "shisa-ai/shisa-de-2")
	if status != 0 {
		t.Fatalf("doctor exited %d: %s", status, stderr)
	}
	want := []string{
		"endpoint        " + server.server.URL,
		"model           shisa-ai/shisa-de-2",
		"family          de2 (",
		"readout         de2-codebook-v1, policy repeat-think",
		"calibration     shisa-ai/shisa-de-2|de2-codebook-v1|",
		"  fitted on     shisa-ai/shisa-de-2 (de2-codebook-v1, ",
		"  match         checkpoint",
		"  applied       yes",
		"models endpoint 200",
		"model listed    yes (of 2 served ids)",
		"boundary check  passed",
		"ok              True",
	}
	for _, line := range want {
		if !strings.Contains(stdout, line) {
			t.Errorf("the report has no %q line:\n%s", line, stdout)
		}
	}
	// The chat-system line is the probe's answer, so a report with no `--probe`
	// has none: the Python command line prints it only under `--probe` too.
	if strings.Contains(stdout, "chat system") {
		t.Errorf("the report has a chat-system line without --probe:\n%s", stdout)
	}
	// A checkpoint the record does not cover is reported and is not fatal: the
	// level is `family`, not `mismatch`, so the endpoint is still declared ok
	// and the report says the record was not applied. That is the Python
	// client's rule, where `matched = level != "mismatch"`.
	//
	// The id has to name DE-2 or the family resolution refuses it first, and it
	// has to name no released checkpoint: `Applicability` accepts either the
	// model id or the tokenizer source as the checkpoint's name, so a different
	// `--tokenizer` does not make a covered model uncovered, and a suffix such as
	// `-preview` or `-FP8-dynamic` does not either, because a name that contains a
	// released checkpoint's name as its own token is that checkpoint. A version
	// this package has no record for is what leaves the record uncovered.
	uncovered := newCLIServer(t, "shisa-ai/shisa-de-2.2")
	installCLIModel(t, uncovered)
	status, stdout, _ = invoke(t, "doctor", "--model", "shisa-ai/shisa-de-2.2")
	if status != 0 {
		t.Errorf("a same-family record exited %d, want 0:\n%s", status, stdout)
	}
	for _, line := range []string{
		"  match         family",
		"  applied       no",
		"model listed    yes",
		"boundary check  passed",
		"ok              True",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("the uncovered report has no %q line:\n%s", line, stdout)
		}
	}
	// The whole line, quoted the way Python's `!r` quotes it: single quotes, not
	// Go's %q double quotes. This is the line the parity diff against the Python
	// command line has to agree on byte for byte.
	if !strings.Contains(stdout,
		"                fitted on 'shisa-ai/shisa-de-2', not on "+
			"'shisa-ai/shisa-de-2.2': same family, another checkpoint") {
		t.Errorf("the uncovered report does not explain the mismatch the way Python does:\n%s", stdout)
	}
}

// TestDoctorQuotesLikePython pins `doctor`'s note line, which is `!r` in the
// Python command line and would be Go's %q without `pythonQuote`.
func TestDoctorQuotesLikePython(t *testing.T) {
	// An id that names neither family: the client assumes DE-2 and says so.
	server := newCLIServer(t, "my-adapter")
	installCLIModel(t, server)

	status, stdout, stderr := invoke(t, "doctor", "--model", "my-adapter")
	if status != 0 {
		t.Fatalf("doctor exited %d: %s\n%s", status, stderr, stdout)
	}
	if !strings.Contains(stdout,
		"note            'my-adapter' carries no DE-1 or DE-2 slug; 'de2' was assumed, not detected") {
		t.Errorf("the note is not quoted the way Python's !r quotes it:\n%s", stdout)
	}
}

// TestDoctorJSON pins that `--json` prints the report the way `json.dumps`
// would, which is not Go's `json.MarshalIndent`.
func TestDoctorJSON(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	status, stdout, stderr := invoke(t, "doctor", "--model", "shisa-ai/shisa-de-2", "--json")
	if status != 0 {
		t.Fatalf("doctor exited %d: %s", status, stderr)
	}
	if !strings.HasPrefix(stdout, "{\n  \"base_url\": ") {
		t.Errorf("the report does not start the way json.dumps(indent=2) starts:\n%s",
			firstLines(stdout, 3))
	}
	// It has to be JSON, and it has to be the whole report.
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, firstLines(stdout, 5))
	}
	for _, key := range []string{"base_url", "model", "readout_version", "policy",
		"calibration", "calibration_level", "models_status", "boundary_check", "ok"} {
		if _, ok := report[key]; !ok {
			t.Errorf("the report has no %q: %v", key, report)
		}
	}
	if report["ok"] != true {
		t.Errorf("ok is %v", report["ok"])
	}
	// An empty container stays on one line, which is the tell that this is not
	// Go's encoder.
	if strings.Contains(stdout, "{\n\n") || strings.Contains(stdout, "[\n\n") {
		t.Errorf("an empty container was broken across lines:\n%s", stdout)
	}
}

// TestDoctorProbeSpendsOneQuestion pins that `--probe` reports the read and
// fails the report when the read fails.
func TestDoctorProbeSpendsOneQuestion(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	status, stdout, stderr := invoke(t, "doctor", "--model", "shisa-ai/shisa-de-2", "--probe")
	if status != 0 {
		t.Fatalf("doctor --probe exited %d: %s\n%s", status, stderr, stdout)
	}
	if !strings.Contains(stdout, "read probe      passed (") {
		t.Errorf("the probe did not report a pass:\n%s", stdout)
	}
	// The chat probe is the second half of `--probe`: it is what an image
	// question would be rendered with.
	if !strings.Contains(stdout, "chat system     string") {
		t.Errorf("the probe did not report the chat rendering:\n%s", stdout)
	}
	// The listing, then one read, then the chat probe. The default policy's
	// repeated read is one request with the user turn written twice, and the
	// stub answers confidently, so no thought follows and no read after one.
	if got := server.requestCount(); got != 3 {
		t.Errorf("the probe spent %d requests, want 3", got)
	}
}

// TestAsk prints the head-to-value mapping the Python command line prints.
func TestAsk(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	status, stdout, stderr := invoke(t, "ask", "--model", "shisa-ai/shisa-de-2",
		"--labels", "billing,outage")
	if status != 0 {
		t.Fatalf("ask exited %d: %s", status, stderr)
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(stdout), &values); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, stdout)
	}
	if values["intent"] != "billing" {
		t.Errorf("the answer is %v, want billing", values["intent"])
	}
	if !strings.HasPrefix(stdout, "{\n  \"intent\": ") {
		t.Errorf("the answer is not indented the way json.dumps(indent=2) indents:\n%s",
			firstLines(stdout, 3))
	}

	// `--confidence` returns the label with its confidence, so the value is no
	// longer a bare string.
	_, stdout, _ = invoke(t, "ask", "--model", "shisa-ai/shisa-de-2", "--labels", "billing,outage",
		"--confidence")
	var withConfidence map[string]any
	if err := json.Unmarshal([]byte(stdout), &withConfidence); err != nil {
		t.Fatalf("the confident answer is not JSON: %v\n%s", err, stdout)
	}
	if _, ok := withConfidence["intent"].(map[string]any); !ok {
		t.Errorf("--confidence did not change the shape: %v", withConfidence["intent"])
	}
}

// TestAskState pins the three ways `--state` can be read.
func TestAskState(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	// A JSON state goes through as an object, which is what the prompt carries.
	_, _, _ = invoke(t, "ask", "--model", "shisa-ai/shisa-de-2", "--state", `{"a": 1}`)
	prompts := server.prompts(t)
	if len(prompts) == 0 || !strings.Contains(prompts[len(prompts)-1], `"a": 1`) {
		t.Errorf("a JSON state did not reach the prompt: %v", prompts)
	}
	// Text that is not JSON is the state itself.
	_, _, _ = invoke(t, "ask", "--model", "shisa-ai/shisa-de-2", "--state", "plain words")
	prompts = server.prompts(t)
	if !strings.Contains(prompts[len(prompts)-1], "plain words") {
		t.Errorf("a plain state did not reach the prompt: %v", prompts)
	}
	// A `{` that is not JSON is a state, not an error.
	status, _, stderr := invoke(t, "ask", "--model", "shisa-ai/shisa-de-2", "--state", "{not json")
	if status != 0 {
		t.Fatalf("a malformed JSON state exited %d: %s", status, stderr)
	}
	prompts = server.prompts(t)
	if !strings.Contains(prompts[len(prompts)-1], "{not json") {
		t.Errorf("a malformed JSON state did not reach the prompt: %v", prompts)
	}
	// No `--state` is the built-in example.
	_, _, _ = invoke(t, "ask", "--model", "shisa-ai/shisa-de-2")
	prompts = server.prompts(t)
	if !strings.Contains(prompts[len(prompts)-1], "Order 4812") {
		t.Errorf("the built-in example did not reach the prompt: %v", prompts)
	}
	// An explicitly empty `--state` is an empty state, not the built-in example:
	// that is the distinction `optionalStringValue` exists for, and a
	// `stateFrom` that "simplified" to `text == ""` would silently lose it.
	status, _, stderr = invoke(t, "ask", "--model", "shisa-ai/shisa-de-2", "--state", "")
	if status != 0 {
		t.Fatalf("an empty state exited %d: %s", status, stderr)
	}
	prompts = server.prompts(t)
	last := prompts[len(prompts)-1]
	if strings.Contains(last, "Order 4812") {
		t.Errorf("--state '' fell back to the built-in example:\n%s", last)
	}
	if !strings.Contains(last, `"evidence": ""`) {
		t.Errorf("the empty state did not reach the prompt as an empty string:\n%s", last)
	}
}

func (s *cliServer) prompts(t *testing.T) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	prompts := make([]string, 0, len(s.requests))
	for _, request := range s.requests {
		if prompt, ok := request["prompt"].(string); ok {
			prompts = append(prompts, prompt)
		}
	}
	return prompts
}

// TestExplain is the walkthrough end to end.
func TestExplain(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	status, stdout, stderr := invoke(t, "explain", "--model", "shisa-ai/shisa-de-2",
		"--labels", "billing,outage")
	if status != 0 {
		t.Fatalf("explain exited %d: %s", status, stderr)
	}
	for _, heading := range []string{
		"1. The prompt the model receives",
		"2. The answer slots",
		"3. One request: max_tokens 1, temperature 0, the 2 answer codes by token id",
		"4. The distribution over the option letters",
		"5. The same request without Python",
	} {
		if !strings.Contains(stdout, heading) {
			t.Errorf("the walkthrough has no %q:\n%s", heading, stdout)
		}
	}
	for _, line := range []string{
		"   A  token   1000",
		"   B  token   1001",
		"   POST " + server.server.URL + "/v1/completions",
		"   prompt tokens ",
		"   answer: A (",
		"readout de2-codebook-v1, policy repeat-think; 256 codes available, 2 used",
		"the user turn is written twice, joined by '\\n\\nRead the same input again before answering:\\n'",
		"top probability ",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("the walkthrough has no %q line:\n%s", line, stdout)
		}
	}
	// The answer-slot marker, which is how the walkthrough ties a top-k entry to
	// the code it is read as.
	if !strings.Contains(stdout, "<- answer slot A") {
		t.Errorf("no top-k entry is marked as an answer slot:\n%s", stdout)
	}
	// The curl line, quoted the way `shlex.quote` quotes it.
	if !strings.Contains(stdout, "curl -s "+shlexQuote(server.server.URL+"/v1/completions")) {
		t.Errorf("the curl line is not quoted the way shlex.quote quotes:\n%s", stdout)
	}
	if !strings.Contains(stdout, "-d '{\"model\":") {
		t.Errorf("the curl body is not a single-quoted json.dumps body:\n%s", stdout)
	}
	// The body carries the two fields that name the codes, in the order
	// json.dumps writes them.
	if !strings.Contains(stdout,
		`"return_tokens_as_token_ids": true, "logprob_token_ids": [1000, 1001]}`) {
		t.Errorf("the curl body does not name the codes in order:\n%s", stdout)
	}
	if got := server.requestCount(); got != 1 {
		t.Errorf("explain spent %d requests, want 1", got)
	}
}

// TestExplainDirect pins the two lines that depend on the policy.
func TestExplainDirect(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	status, stdout, stderr := invoke(t, "explain", "--model", "shisa-ai/shisa-de-2",
		"--policy", "direct")
	if status != 0 {
		t.Fatalf("explain exited %d: %s", status, stderr)
	}
	if !strings.Contains(stdout, "readout de2-codebook-v1, policy direct;") {
		t.Errorf("the policy line is wrong:\n%s", stdout)
	}
	if strings.Contains(stdout, "the user turn is written twice") {
		t.Errorf("a direct read claims to repeat the user turn:\n%s", stdout)
	}
	if strings.Contains(stdout, "against the gate") {
		t.Errorf("a direct read reports a gate:\n%s", stdout)
	}
	// A direct read writes the user turn once, so the prompt carries no
	// separator.
	if strings.Contains(stdout, "Read the same input again") {
		t.Errorf("a direct prompt carries the repeat separator:\n%s", stdout)
	}
}

// TestShlexQuote pins Python's `shlex.quote`, which is what makes the curl line
// pasteable.
func TestShlexQuote(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"", "''"},
		{"plain", "plain"},
		{"a-b_c.d/e:f,g+h=i@j%k", "a-b_c.d/e:f,g+h=i@j%k"},
		{"has space", "'has space'"},
		{"it's", `'it'"'"'s'`},
		{`say "hi"`, `'say "hi"'`},
		{"$HOME", "'$HOME'"},
		{"{\"a\": 1}", `'{"a": 1}'`},
		{"line\nbreak", "'line\nbreak'"},
	} {
		if got := shlexQuote(tc.text); got != tc.want {
			t.Errorf("shlexQuote(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

// TestStateAndLabels pins the two helpers the commands share.
func TestStateAndLabels(t *testing.T) {
	state := stateFrom("", false)
	example, ok := state.(shisade.Object)
	if !ok {
		t.Fatalf("the built-in state is a %T, want shisade.Object", state)
	}
	// The order is the Python command line's, because it reaches the prompt.
	encoded, err := shisade.PythonJSON(example)
	if err != nil {
		t.Fatalf("PythonJSON: %v", err)
	}
	if !strings.HasPrefix(encoded, `{"ticket": "Order 4812`) ||
		!strings.Contains(encoded, `", "account": "customer since 2021`) {
		t.Errorf("the built-in state serializes as %s", encoded)
	}

	// A JSON object keeps its numbers apart, which is why the parse is
	// DecodeJSON and not encoding/json.
	state = stateFrom(`{"n": 3}`, true)
	object, ok := state.(shisade.Object)
	if !ok {
		t.Fatalf("a JSON state is a %T, want shisade.Object", state)
	}
	encoded, err = shisade.PythonJSON(object)
	if err != nil {
		t.Fatalf("PythonJSON: %v", err)
	}
	if encoded != `{"n": 3}` {
		t.Errorf("the state serializes as %s, want {\"n\": 3}", encoded)
	}

	if head, labels := labelsFrom(""); head != "intent" || len(labels) != 4 {
		t.Errorf("the built-in labels are %q %v", head, labels)
	}
	head, labels := labelsFrom(" a , b ,, c ")
	if head != "intent" || strings.Join(labels, "|") != "a|b|c" {
		t.Errorf("the parsed labels are %q %v", head, labels)
	}
	if _, labels := labelsFrom("   "); len(labels) != 0 {
		t.Errorf("a whitespace-only label list gave %v", labels)
	}
}

// TestBestCode pins the tie-break, which is Python's `max` over insertion order.
func TestBestCode(t *testing.T) {
	slots := []shisade.Slot{{Code: "A"}, {Code: "B"}, {Code: "C"}}
	best, top, err := bestCode(shisade.CodeRead{Probabilities: map[string]float64{
		"A": 0.5, "B": 0.5, "C": 0.1,
	}}, slots)
	if err != nil {
		t.Fatalf("bestCode: %v", err)
	}
	if best != "A" || top != 0.5 {
		t.Errorf("the answer is %q at %v, want A at 0.5", best, top)
	}
	if _, _, err := bestCode(shisade.CodeRead{}, slots); err == nil {
		t.Error("an empty distribution was answered instead of reported")
	}
	// A code the read did not return is not a candidate, which is Python's
	// `max` over the dict's own keys.
	best, top, err = bestCode(shisade.CodeRead{Probabilities: map[string]float64{"B": 0.0}}, slots)
	if err != nil {
		t.Fatalf("bestCode: %v", err)
	}
	if best != "B" || top != 0.0 {
		t.Errorf("the answer is %q at %v, want B at 0", best, top)
	}
	best, _, err = bestCode(shisade.CodeRead{Probabilities: map[string]float64{"B": 0.25}}, slots)
	if err != nil {
		t.Fatalf("bestCode: %v", err)
	}
	if best != "B" {
		t.Errorf("the answer is %q, want B", best)
	}
}

// TestSortedTopLogprobs pins the ordering, including the null Python cannot
// order.
func TestSortedTopLogprobs(t *testing.T) {
	first, second := -0.1, -0.9
	entries := sortedTopLogprobs(map[string]*float64{
		"token_id:1": &second, "token_id:2": &first, "token_id:3": nil,
	})
	if len(entries) != 3 {
		t.Fatalf("sortedTopLogprobs gave %d entries", len(entries))
	}
	if entries[0].token != "token_id:2" || entries[1].token != "token_id:1" {
		t.Errorf("the order is %v, want the higher logprob first", entries)
	}
	if entries[2].token != "token_id:3" {
		t.Errorf("the null did not sort last: %v", entries)
	}
	// The null is carried as a null, not collapsed into 0: the walkthrough
	// prints `None` for it, and 0.0000 would be a number the server never sent.
	if !entries[2].null || entries[0].null || entries[1].null {
		t.Errorf("the null flag is %v, %v, %v", entries[0].null, entries[1].null, entries[2].null)
	}
}

// TestExplainReportsAnUnreadableQuestion is the failure the walkthrough has to
// report before it prints anything.
//
// The stub tokenizer has no word for the answer terminator, so it tokenizes as
// ten runes and `AnswerPrefixID` refuses it. The boundary check is before the
// walkthrough's first print in both implementations, so a question whose codes
// were not read from the answer position is an error rather than a walkthrough
// that lies. (`TestFailedRead` covers the read, which is after sections 1-2.)
func TestExplainReportsAnUnreadableQuestion(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)
	server.tokenizer = newCLITokenizer(map[string]int{})
	status, stdout, stderr := invoke(t, "explain", "--model", "shisa-ai/shisa-de-2")
	if status != 1 {
		t.Errorf("explain exited %d, want 1", status)
	}
	if stdout != "" {
		t.Errorf("a failed walkthrough printed to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "shisa-de explain: ") {
		t.Errorf("the failure is %q", stderr)
	}
	if server.requestCount() != 0 {
		t.Errorf("a failed walkthrough spent %d requests", server.requestCount())
	}
}

func boolPointer(value bool) *bool { return &value }

func firstLines(text string, count int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > count {
		lines = lines[:count]
	}
	return strings.Join(lines, "\n")
}

// TestDoctorNotOKExitsOne pins the exit contract `README.md` advertises: a
// report that is not ok is a non-zero exit, in both output forms.
func TestDoctorNotOKExitsOne(t *testing.T) {
	// The endpoint answers, but it does not list the served model, so `Health`
	// reports `model listed no` and `ok False`.
	server := newCLIServer(t, "something-else")
	installCLIModel(t, server)

	status, stdout, stderr := invoke(t, "doctor", "--model", "shisa-ai/shisa-de-2")
	if status != 1 {
		t.Fatalf("a report that is not ok exited %d, want 1: %s\n%s", status, stderr, stdout)
	}
	if !strings.Contains(stdout, "model listed    no") ||
		!strings.Contains(stdout, "ok              False") {
		t.Errorf("the report does not say why it is not ok:\n%s", stdout)
	}
	if stderr != "" {
		t.Errorf("a report that is not ok wrote to stderr: %q", stderr)
	}

	// The same status from `--json`, which returns before the text form.
	status, stdout, _ = invoke(t, "doctor", "--model", "shisa-ai/shisa-de-2", "--json")
	if status != 1 {
		t.Errorf("doctor --json exited %d, want 1:\n%s", status, stdout)
	}
	if !strings.Contains(stdout, `"ok": false`) {
		t.Errorf("the JSON report does not carry ok false:\n%s", stdout)
	}
}

// TestReportOrderCoversHealth pins the coupling between the report `Health`
// builds and the order `doctor --json` prints it in.
//
// A key added to `Health` and forgotten in `reportOrder` would vanish from the
// JSON form while the text form still printed it, and nothing else would notice.
func TestReportOrderCoversHealth(t *testing.T) {
	// Three invocations, because three report keys are conditional:
	// `family_note` needs a model id that names neither family,
	// `calibration_note` a same-family record that does not cover the
	// checkpoint, and `calibration_mismatch` a record from the other family.
	// Between them the union is the whole key set.
	server := newCLIServer(t, "shisa-ai/shisa-de-2", "my-adapter", "shisa-ai/shisa-de-2.2")
	for _, argv := range [][]string{
		{"--model", "shisa-ai/shisa-de-2", "--probe"},
		{"--model", "my-adapter", "--probe"},
		{"--model", "shisa-ai/shisa-de-2.2", "--probe"},
	} {
		parsed, err := parse("doctor", argv)
		if err != nil {
			t.Fatalf("parse %v: %v", argv, err)
		}
		config, err := parsed.modelConfig()
		if err != nil {
			t.Fatalf("modelConfig %v: %v", argv, err)
		}
		model, err := stubModel(server, config)
		if err != nil {
			t.Fatalf("stubModel %v: %v", argv, err)
		}
		report := model.Health(context.Background(), true)
		ordered := orderedReport(report)
		if len(ordered) != len(report) {
			t.Errorf("%v: the ordered report has %d keys, the report has %d: %v",
				argv, len(ordered), len(report), report)
		}
		// Every key the report carries has to be one `orderedReport` prints,
		// because it drops an unknown key silently.
		known := make(map[string]bool, len(reportOrder))
		for _, key := range reportOrder {
			known[key] = true
		}
		for key := range report {
			if !known[key] {
				t.Errorf("%v: the report carries %q, which reportOrder does not print", argv, key)
			}
		}
		if len(argv) == 3 && argv[0] == "--model" && argv[1] == "my-adapter" {
			if _, ok := report["family_note"]; !ok {
				t.Errorf("an assumed family reported no family_note: %v", report)
			}
		}
	}

	// `calibration_mismatch` is the one key no `--calibration` value can reach:
	// this package ships no DE-1 record, and a DE-2 record on a DE-2 model is
	// never a family mismatch. A record from the other family is built here so
	// the key is still exercised.
	parsed, err := parse("doctor", []string{"--model", "shisa-ai/shisa-de-2"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	config, err := parsed.modelConfig()
	if err != nil {
		t.Fatalf("modelConfig: %v", err)
	}
	config.Calibration = &shisade.Calibration{
		Family: shisade.FamilyDE1, ReadoutVersion: shisade.ReadoutVersionFor(shisade.FamilyDE1),
		Model: "shisa-ai/shisa-de-1", Temperatures: map[string]float64{"choice": 1.5},
	}
	model, err := stubModel(server, config)
	if err != nil {
		t.Fatalf("stubModel: %v", err)
	}
	report := model.Health(context.Background(), true)
	if _, ok := report["calibration_mismatch"]; !ok {
		t.Fatalf("a DE-1 record on a DE-2 model reported no mismatch: %v", report)
	}
	if len(orderedReport(report)) != len(report) {
		t.Errorf("the ordered mismatch report drops a key: %v", report)
	}
}

// TestChoiceFlagsRejectAnEmptyValue pins the difference between a flag that was
// not given and one that was given empty, which is what `argparse`'s `choices`
// is: `--policy "$POLICY"` with `POLICY` unset is a bad invocation, not the
// default policy.
func TestChoiceFlagsRejectAnEmptyValue(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	for _, argv := range [][]string{
		{"doctor", "--policy", ""},
		{"doctor", "--reads", ""},
		{"doctor", "--family", ""},
		{"doctor", "--calibration", ""},
	} {
		status, stdout, stderr := invoke(t, argv...)
		if status != 2 {
			t.Errorf("%v: exit status %d, want 2", argv, status)
		}
		if stdout != "" {
			t.Errorf("%v: a refused command wrote to stdout: %q", argv, stdout)
		}
		if !strings.Contains(stderr, "usage: shisa-de") {
			t.Errorf("%v: the error does not carry the usage: %q", argv, stderr)
		}
	}

	// A spelling `argparse`'s `choices` does not accept is refused rather than
	// normalized.
	if status, _, stderr := invoke(t, "doctor", "--family", "DE-2"); status != 2 ||
		!strings.Contains(stderr, "unknown family") {
		t.Errorf("--family DE-2 gave %d and %q", status, stderr)
	}
	// The values `choices` does accept still work.
	if status, _, stderr := invoke(t, "doctor", "--model", "shisa-ai/shisa-de-2",
		"--family", "de2", "--calibration", "de2"); status != 0 {
		t.Errorf("a spelled-out family and record gave %d and %q", status, stderr)
	}
}

// TestFailedRead pins what a read failure does, which is where the walkthrough's
// print order is visible.
func TestFailedRead(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	server.completionStatus = http.StatusInternalServerError
	installCLIModel(t, server)

	// `explain` prints the prompt and the slots before it takes the read, which
	// is the Python command line's order, so a failed read leaves half the
	// walkthrough on stdout rather than nothing.
	status, stdout, stderr := invoke(t, "explain", "--model", "shisa-ai/shisa-de-2",
		"--labels", "billing,outage")
	if status != 1 {
		t.Fatalf("a failed read exited %d, want 1", status)
	}
	for _, line := range []string{"1. The prompt the model receives", "2. The answer slots"} {
		if !strings.Contains(stdout, line) {
			t.Errorf("a failed read printed no %q:\n%s", line, stdout)
		}
	}
	if strings.Contains(stdout, "3. One request") {
		t.Errorf("a failed read printed the request section:\n%s", stdout)
	}
	if !strings.Contains(stderr, "shisa-de explain: ") {
		t.Errorf("the failure did not reach stderr: %q", stderr)
	}

	// `ask` has nothing to print before its read, so it reports and exits.
	status, stdout, stderr = invoke(t, "ask", "--model", "shisa-ai/shisa-de-2")
	if status != 1 {
		t.Fatalf("a failed classify exited %d, want 1", status)
	}
	if stdout != "" {
		t.Errorf("a failed classify wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "shisa-de ask: ") {
		t.Errorf("the classify failure did not reach stderr: %q", stderr)
	}
}

// TestStateFromTrimsPythonsWhitespace pins that `--state` is stripped with
// Python's `str.strip()`, which takes U+001C-U+001F and `strings.TrimSpace`
// does not. A JSON state behind one of those four characters is JSON to the
// Python command line.
func TestStateFromTrimsPythonsWhitespace(t *testing.T) {
	state := stateFrom("\x1c{\"a\": 1}", true)
	object, ok := state.(shisade.Object)
	if !ok {
		t.Fatalf("a JSON state behind U+001C is a %T, want shisade.Object", state)
	}
	encoded, err := shisade.PythonJSON(object)
	if err != nil {
		t.Fatalf("PythonJSON: %v", err)
	}
	if encoded != `{"a": 1}` {
		t.Errorf("the state serializes as %s", encoded)
	}

	// The labels are split with the same rule.
	if _, labels := labelsFrom("\x1cbilling\x1c, outage"); len(labels) != 2 ||
		labels[0] != "billing" || labels[1] != "outage" {
		t.Errorf("the labels are %v", labels)
	}
}

// TestExplainPrintsTheGateLikePython pins the gate's spelling: Python's f-string
// on a float is `str(float)`, so an integral gate is `1.0` and not `1`. The
// default 0.7 hides the difference, and `--reasoning-prob 1` is a legal value.
func TestExplainPrintsTheGateLikePython(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	status, stdout, stderr := invoke(t, "explain", "--model", "shisa-ai/shisa-de-2",
		"--reasoning-prob", "1")
	if status != 0 {
		t.Fatalf("explain exited %d: %s", status, stderr)
	}
	if !strings.Contains(stdout, "against the gate 1.0: ") {
		t.Errorf("the gate is not spelled the way Python spells it:\n%s", stdout)
	}
	if strings.Contains(stdout, "against the gate 1: ") {
		t.Errorf("the gate printed as an integer:\n%s", stdout)
	}
	// The help default is spelled with the same rule, so a reader who copies it
	// gets a value the parser accepts and the same spelling in the output.
	status, stdout, _ = invoke(t, "explain", "--help")
	if status != 0 {
		t.Fatalf("explain --help exited %d", status)
	}
	if !strings.Contains(stdout, "the think gate (default: 0.7)") {
		t.Errorf("the help does not spell the gate default:\n%s", stdout)
	}
}

// TestAskImage is `ask --image` end to end.
//
// The image readout posts to the chat endpoint rather than the completions one,
// and the question is the JSON the server renders, so what this checks is that
// the flag reaches the request at all: the read itself is covered by the
// readout's own replay gate.
func TestAskImage(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	const image = "data:image/png;base64,iVBORw0KGgo="
	status, stdout, stderr := invoke(t, "ask", "--model", "shisa-ai/shisa-de-2",
		"--image", image, "--labels", "red,green,blue")
	if status != 0 {
		t.Fatalf("ask --image exited %d: %s", status, stderr)
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(stdout), &values); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, stdout)
	}
	if values["intent"] != "red" {
		t.Errorf("the answer is %v, want the first label the stub answers with", values["intent"])
	}

	// The read went to the chat endpoint, with the image beside the question.
	requests := server.bodiesFor(t, "/v1/chat/completions")
	if len(requests) != 1 {
		t.Fatalf("the image read sent %d chat requests, want 1", len(requests))
	}
	messages, _ := requests[0]["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("the read sent %v messages, want 2", requests[0]["messages"])
	}
	parts, _ := messages[1].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("the user turn is %v, want two content parts", messages[1])
	}
	if url := parts[0].(map[string]any)["image_url"].(map[string]any)["url"]; url != image {
		t.Errorf("the image URL is %v, want %q", url, image)
	}
	// An image question with no `--state` is about the image alone, which is the
	// Python command line's `{} if args.image and args.state is None`.
	if text, _ := parts[1].(map[string]any)["text"].(string); !strings.Contains(text, `"evidence": {}`) {
		t.Errorf("the evidence is not an empty object:\n%s", text)
	}
	// The image path keeps the letter limit and does not repeat the user turn, so
	// the question appears exactly once.
	if text, _ := parts[1].(map[string]any)["text"].(string); strings.Count(text, `"criterion"`) != 1 {
		t.Errorf("the question is written more than once:\n%s", text)
	}
}

// TestAskImageKeepsAnExplicitState pins the other half of that rule: a state the
// caller gave is the state, image or not.
func TestAskImageKeepsAnExplicitState(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	status, _, stderr := invoke(t, "ask", "--model", "shisa-ai/shisa-de-2",
		"--image", "data:image/png;base64,iVBORw0KGgo=", "--state", `{"note": "a red square"}`)
	if status != 0 {
		t.Fatalf("ask --image --state exited %d: %s", status, stderr)
	}
	requests := server.bodiesFor(t, "/v1/chat/completions")
	if len(requests) != 1 {
		t.Fatalf("the image read sent %d chat requests, want 1", len(requests))
	}
	messages, _ := requests[0]["messages"].([]any)
	parts, _ := messages[1].(map[string]any)["content"].([]any)
	text, _ := parts[1].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"note": "a red square"`) {
		t.Errorf("the state did not reach the question:\n%s", text)
	}
}

// TestAskImageRefusesABadReference pins that a bad image fails before any
// request, which is the promise `PrepareImage` makes.
func TestAskImageRefusesABadReference(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	status, stdout, stderr := invoke(t, "ask", "--model", "shisa-ai/shisa-de-2",
		"--image", "photo.gif")
	if status != 1 {
		t.Errorf("ask --image with a bad extension exited %d, want 1", status)
	}
	if stdout != "" {
		t.Errorf("a refused image wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "unsupported image extension '.gif'") {
		t.Errorf("the error is %q", stderr)
	}
	if got := server.requestCount(); got != 0 {
		t.Errorf("the command sent %d requests for a rejected reference", got)
	}
}

// TestAskImageTopLogprobsReachesTheRequest pins the flag's one effect.
func TestAskImageTopLogprobsReachesTheRequest(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	status, _, stderr := invoke(t, "ask", "--model", "shisa-ai/shisa-de-2",
		"--image", "data:image/png;base64,iVBORw0KGgo=",
		"--image-top-logprobs", "64", "--labels", "red,green,blue")
	if status != 0 {
		t.Fatalf("ask exited %d: %s", status, stderr)
	}
	requests := server.bodiesFor(t, "/v1/chat/completions")
	if len(requests) != 1 {
		t.Fatalf("the image read sent %d chat requests, want 1", len(requests))
	}
	if requests[0]["top_logprobs"] != float64(64) {
		t.Errorf("the request asked for top_logprobs %v, want 64", requests[0]["top_logprobs"])
	}
}

// TestAskImageTopLogprobsDefault pins the default the Python command line reads
// with `getattr(args, "image_top_logprobs", 20)`.
func TestAskImageTopLogprobsDefault(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	status, _, stderr := invoke(t, "ask", "--model", "shisa-ai/shisa-de-2",
		"--image", "data:image/png;base64,iVBORw0KGgo=", "--labels", "red,green,blue")
	if status != 0 {
		t.Fatalf("ask exited %d: %s", status, stderr)
	}
	requests := server.bodiesFor(t, "/v1/chat/completions")
	if len(requests) != 1 || requests[0]["top_logprobs"] != float64(shisade.DefaultImageTopLogprobs) {
		t.Errorf("the request asked for %v, want %d",
			requests[0]["top_logprobs"], shisade.DefaultImageTopLogprobs)
	}
}

// TestAskHelpNamesTheImageFlags pins that the flags a command takes are the ones
// its usage lists, which is what `commonFlags` is written by hand for.
func TestAskHelpNamesTheImageFlags(t *testing.T) {
	status, stdout, _ := invoke(t, "ask", "--help")
	if status != 0 {
		t.Fatalf("ask --help exited %d", status)
	}
	for _, wanted := range []string{"--image REFERENCE", "--image-top-logprobs N"} {
		if !strings.Contains(stdout, wanted) {
			t.Errorf("ask --help does not mention %s:\n%s", wanted, stdout)
		}
	}
	// The flags belong to `ask`, and a command that does not take them must not
	// advertise them: `doctor` has no state to attach an image to.
	_, stdout, _ = invoke(t, "doctor", "--help")
	if strings.Contains(stdout, "--image") {
		t.Errorf("doctor --help advertises the image flags:\n%s", stdout)
	}
	// And they are not accepted where they are not registered.
	if status, _, stderr := invoke(t, "explain", "--image", "x.png"); status != 2 ||
		!strings.Contains(stderr, "image") {
		t.Errorf("explain accepted --image: %d, %q", status, stderr)
	}
}

// TestDoctorProbeReportsTheChatSystem pins the line the reference command line
// prints after the read probe.
func TestDoctorProbeReportsTheChatSystem(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	status, stdout, stderr := invoke(t, "doctor", "--model", "shisa-ai/shisa-de-2",
		"--probe", "--json")
	if status != 0 {
		t.Fatalf("doctor --probe --json exited %d: %s", status, stderr)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("the report is not JSON: %v", err)
	}
	if report["chat_system_render"] != "string" {
		t.Errorf("chat_system_render is %v, want string", report["chat_system_render"])
	}
	// Without the probe there is nothing to report, and the key is absent rather
	// than null: `doctor --json` is read next to the Python one.
	_, stdout, _ = invoke(t, "doctor", "--model", "shisa-ai/shisa-de-2", "--json")
	// A fresh map: unmarshalling into a live one merges keys, which would leave
	// the probe's verdict in place and make this pass for the wrong reason.
	report = map[string]any{}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("the report is not JSON: %v", err)
	}
	if _, present := report["chat_system_render"]; present {
		t.Errorf("a report with no probe carries chat_system_render: %v",
			report["chat_system_render"])
	}
}

// bodiesFor is every request body the stub received for one path.
func (s *cliServer) bodiesFor(t *testing.T, path string) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	bodies := make([]map[string]any, 0, len(s.requests))
	for index, body := range s.requests {
		if s.paths[index] == path {
			bodies = append(bodies, body)
		}
	}
	return bodies
}

// TestAskRefusesAnEmptyImage pins the difference `CallOptions.Image` cannot
// express: an absent `--image` is a text read, and `--image ""` is an unusable
// reference, which the Python command line reports rather than answering the
// text question.
func TestAskRefusesAnEmptyImage(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)

	status, stdout, stderr := invoke(t, "ask", "--model", "shisa-ai/shisa-de-2",
		"--image", "", "--labels", "billing,outage")
	if status != 1 {
		t.Errorf("ask --image '' exited %d, want 1", status)
	}
	if stdout != "" {
		t.Errorf("a refused image wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "image is empty") {
		t.Errorf("the error is %q", stderr)
	}
	if got := server.requestCount(); got != 0 {
		t.Errorf("the command sent %d requests for an empty reference", got)
	}
	// An absent --image is still a text read.
	status, _, stderr = invoke(t, "ask", "--model", "shisa-ai/shisa-de-2",
		"--labels", "billing,outage")
	if status != 0 {
		t.Fatalf("ask with no --image exited %d: %s", status, stderr)
	}
}

// TestDoctorReportsADisagreeingTokenizerOnStderr pins the one place a command
// line user can see the alias case.
//
// The report's fields cannot show it: with `--model shisa-ai/shisa-de-2
// --tokenizer shisa-ai/shisa-de-2.1` the record is DE-2's, `match` is
// `checkpoint` and `applied` is `yes`, which is exactly what a server really
// serving DE-2 prints. The warning is the difference, and it goes to stderr,
// where `warnings.warn` writes in the Python client.
func TestDoctorReportsADisagreeingTokenizerOnStderr(t *testing.T) {
	server := newCLIServer(t, "shisa-ai/shisa-de-2")
	installCLIModel(t, server)
	status, stdout, stderr := invoke(t, "doctor",
		"--model", "shisa-ai/shisa-de-2", "--tokenizer", "shisa-ai/shisa-de-2.1")
	if status != 0 {
		t.Errorf("doctor exited %d:\n%s", status, stdout)
	}
	if !strings.Contains(stderr, "the model id names 'shisa-ai/shisa-de-2'") ||
		!strings.Contains(stderr, "the tokenizer names 'shisa-ai/shisa-de-2.1'") {
		t.Errorf("the disagreement is not reported on stderr:\n%s", stderr)
	}
	// The report itself still says what it always said, which is why the warning
	// has to carry the signal.
	for _, line := range []string{"  match         checkpoint", "  applied       yes"} {
		if !strings.Contains(stdout, line) {
			t.Errorf("the report has no %q line:\n%s", line, stdout)
		}
	}
	// The same checkpoint twice says nothing.
	status, _, stderr = invoke(t, "doctor",
		"--model", "shisa-ai/shisa-de-2", "--tokenizer", "shisa-ai/shisa-de-2-FP8-dynamic")
	if status != 0 {
		t.Errorf("doctor exited %d", status)
	}
	if strings.Contains(stderr, "the model id names") {
		t.Errorf("an agreeing tokenizer was reported:\n%s", stderr)
	}
}
