package shisade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The live suite: real requests against a served DE-2, opt in with
// `SHISA_DE_LIVE=1`.
//
//	go test . -run Live                       # skipped
//	SHISA_DE_LIVE=1 go test . -run Live        # spends requests
//
// `SHISA_DE_MODEL`, `SHISA_DE_ENDPOINT` and `SHISA_DE_TOKENIZER` point the suite
// at another served checkpoint, which is the same environment contract the
// Python suite uses. The key comes from `SHISA_DE_API_KEY` or `SHISA_API_KEY`.
//
// Two things live here. `TestLiveParityAgainstPython` is the M6 gate: the fixed
// question set in `testdata/live_questions.json`, asked of the same endpoint by
// both clients, has to produce the same chosen value and the same distribution.
// The rest are the DE-2 half of the Python `tests/test_live.py`; the DE-1 and
// image cases are absent because this port has neither read, which
// `docs/PORT.md` records.
//
// These tests spend real requests and are not deterministic in the way an
// offline test is: a server that changes its mind between two runs makes them
// fail. That is what they are for, so a failure here is a finding about the
// endpoint or about the port, never something to paper over with a retry.

// liveSkip skips unless the live suite was asked for.
func liveSkip(t *testing.T) {
	t.Helper()
	if os.Getenv("SHISA_DE_LIVE") != "1" {
		t.Skip("set SHISA_DE_LIVE=1 to run against a live endpoint")
	}
}

// liveModel is the environment as a client, plus the transport that saw the
// responses.
//
// The tokenizer source is read from `SHISA_DE_TOKENIZER` here rather than by the
// library, so the library keeps one way to name a tokenizer and the suite still
// matches the Python suite's environment contract.
func liveModel(t *testing.T, overrides OptionOverrides) (*DecisionModel, *liveRecorder) {
	t.Helper()
	liveSkip(t)

	recorder := &liveRecorder{base: http.DefaultTransport, fingerprints: map[string]int{}}
	config := DecisionModelConfig{
		BaseURL:    os.Getenv(EndpointEnv),
		Model:      liveModelID(),
		Tokenizer:  os.Getenv("SHISA_DE_TOKENIZER"),
		HTTPClient: &http.Client{Timeout: 600 * time.Second, Transport: recorder},
		Overrides:  overrides,
	}
	model, err := FromPretrained(liveModelID(), config)
	if err != nil {
		t.Fatalf("FromPretrained(%s): %v", liveModelID(), err)
	}
	t.Cleanup(model.Close)
	return model, recorder
}

// liveModelID is the checkpoint under test, defaulting to the one this port can
// read.
func liveModelID() string {
	if id := os.Getenv("SHISA_DE_MODEL"); id != "" {
		return id
	}
	return "shisa-ai/shisa-de-2"
}

// liveRecorder is a transport that passes every request through and remembers
// the served `system_fingerprint`.
//
// The fingerprint is not on any client type, and it is what makes a recorded
// number reproducible, so the suite reads it off the wire. A response is put
// back exactly as it arrived.
type liveRecorder struct {
	base http.RoundTripper

	mu           sync.Mutex
	fingerprints map[string]int
	requests     int
}

func (r *liveRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := r.base.RoundTrip(request)
	if err != nil {
		return response, err
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("reading a %s response: %w", request.URL.Path, err)
	}
	response.Body = io.NopCloser(bytes.NewReader(body))

	var envelope struct {
		SystemFingerprint string `json:"system_fingerprint"`
	}
	// A body that is not JSON is not this transport's business: the client
	// reports it with the status and the detail, which is more useful than a
	// decode error from here.
	_ = json.Unmarshal(body, &envelope)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests++
	if envelope.SystemFingerprint != "" {
		r.fingerprints[envelope.SystemFingerprint]++
	}
	return response, nil
}

// fingerprint is the one serving shape every response carried.
func (r *liveRecorder) fingerprint(t *testing.T) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make([]string, 0, len(r.fingerprints))
	for name := range r.fingerprints {
		seen = append(seen, name)
	}
	sort.Strings(seen)
	if len(seen) == 0 {
		t.Fatal("no response carried a system_fingerprint, so the numbers have no serving shape")
	}
	if len(seen) > 1 {
		t.Fatalf("the responses carried %d serving shapes: %v", len(seen), seen)
	}
	return seen[0]
}

// -- the fixed question set ---------------------------------------------------

// liveQuestionSet is `testdata/live_questions.json`.
type liveQuestionSet struct {
	Questions []liveQuestion `json:"questions"`
}

type liveQuestion struct {
	ID           string          `json:"id"`
	Head         string          `json:"head"`
	Type         string          `json:"type"`
	Instructions string          `json:"instructions"`
	Evidence     json.RawMessage `json:"evidence"`
	Options      []liveOption    `json:"options"`
	Levels       []string        `json:"levels"`
	// Gate overrides the think gate for this question, which is how the thinking
	// read is exercised without sending every other question through a thought.
	Gate *float64 `json:"gate"`
}

type liveOption struct {
	Key         string `json:"key"`
	Description any    `json:"description"`
}

// question is the spec as the Go question object.
func (q liveQuestion) question() (Question, error) {
	switch q.Type {
	case "noul":
		return NewNoul(q.Instructions), nil
	case "choice":
		options := make([]Option, 0, len(q.Options))
		for _, option := range q.Options {
			options = append(options, Option{Key: option.Key, Description: option.Description})
		}
		return NewChoice(q.Instructions, options), nil
	case "score":
		levels := make([]any, 0, len(q.Levels))
		for _, level := range q.Levels {
			levels = append(levels, level)
		}
		return NewScore(q.Instructions, levels), nil
	default:
		return nil, fmt.Errorf("%s: unknown question type %q", q.ID, q.Type)
	}
}

// state is the evidence as the JSON value the readout serializes into the
// prompt. `DecodeJSON` and not `encoding/json`, so a number keeps the shape the
// file gave it.
func (q liveQuestion) state() (any, error) {
	return DecodeJSON(q.Evidence)
}

func loadLiveQuestions(t *testing.T) liveQuestionSet {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "live_questions.json"))
	if err != nil {
		t.Fatalf("reading the question set: %v", err)
	}
	var set liveQuestionSet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatalf("parsing the question set: %v", err)
	}
	if len(set.Questions) == 0 {
		t.Fatal("the question set is empty")
	}
	return set
}

// -- the recorded Python answers ----------------------------------------------

// livePython is what `scripts/live_parity.py` writes.
type livePython struct {
	Questions map[string]livePythonAnswer `json:"questions"`
	Drift     map[string]liveDrift        `json:"drift"`
	SelfDelta float64                     `json:"self_delta"`
}

// liveDrift is how far the Python client's own two passes were apart.
type liveDrift struct {
	MaxProbabilityDelta float64 `json:"max_probability_delta"`
	SameValue           bool    `json:"same_value"`
	SameStrategy        bool    `json:"same_strategy"`
	SameRequests        bool    `json:"same_requests"`
}

type livePythonAnswer struct {
	Type           string             `json:"type"`
	Value          any                `json:"value"`
	Label          any                `json:"label"`
	Probabilities  map[string]float64 `json:"probabilities"`
	Strategy       string             `json:"strategy"`
	Calibrated     bool               `json:"calibrated"`
	Temperature    float64            `json:"temperature"`
	Requests       int                `json:"requests"`
	Confidence     *float64           `json:"confidence"`
	Level          *string            `json:"level"`
	ThoughtTokens  int                `json:"thought_tokens"`
	MissingFromTop []string           `json:"missing_from_top"`
}

// livePythonMeta is the provenance written beside it.
type livePythonMeta struct {
	Endpoint           string   `json:"endpoint"`
	Model              string   `json:"model"`
	ServingFingerprint string   `json:"serving_fingerprint"`
	Recorded           string   `json:"recorded"`
	PythonRepo         string   `json:"python_repo"`
	ReadoutVersion     string   `json:"readout_version"`
	TokenizerRevision  string   `json:"tokenizer_revision"`
	Questions          int      `json:"questions"`
	Passes             int      `json:"passes"`
	Requests           int      `json:"requests"`
	SelfDelta          float64  `json:"self_delta"`
	Unstable           []string `json:"unstable"`
	SHA256             string   `json:"sha256"`
}

// livePythonRepo is the Python client the answers come from.
func livePythonRepo() string {
	if repo := os.Getenv("SHISA_DE_PYTHON_REPO"); repo != "" {
		return repo
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "data", "dev", "shisa", "shisa-de")
}

// recordPythonAnswers runs the fixed set through the Python client, live.
//
// The answers are produced now rather than read from a checked-in recording,
// because the served checkpoint is deterministic within a session and not
// across sessions: a recording made earlier is a measurement of a server that
// has since answered differently, so comparing against it would report the
// endpoint's drift as the port's.
//
// The script runs the set twice and records how far its own passes were apart,
// which is what lets the comparison below tell "the server moved" from "the port
// differs".
func recordPythonAnswers(t *testing.T) (livePython, livePythonMeta) {
	t.Helper()
	repo := livePythonRepo()
	if repo == "" {
		t.Skip("no Python repository to record from; set SHISA_DE_PYTHON_REPO")
	}
	script := filepath.Join(repo, "..", "shisa-de-go", "scripts", "live_parity.py")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("the recorder is not at %s: %v", script, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "shisa_de")); err != nil {
		t.Skipf("no shisa_de package under %s: %v", repo, err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 to record with")
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "live_python.json")
	command := exec.Command("python3", script, "--out", out, "--quiet")
	command.Env = append(os.Environ(), "SHISA_DE_PYTHON_REPO="+repo)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("recording the Python answers: %v\n%s", err, stderr.String())
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the recorded Python answers: %v", err)
	}
	var recorded livePython
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatalf("parsing the recorded Python answers: %v", err)
	}
	metaRaw, err := os.ReadFile(strings.TrimSuffix(out, ".json") + ".meta.json")
	if err != nil {
		t.Fatalf("reading the recording's provenance: %v", err)
	}
	var meta livePythonMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("parsing the recording's provenance: %v", err)
	}
	t.Logf("the Python client: %d passes, %d requests, fingerprint %s, self-delta %.3g",
		meta.Passes, meta.Requests, meta.ServingFingerprint, meta.SelfDelta)
	return recorded, meta
}

// liveTolerance is how far two probabilities may differ.
//
// The read is `temperature: 0` against a deterministic server, so the two
// clients should agree exactly; the tolerance is for the float arithmetic on the
// way to the softmax, not for the server changing its mind. A chosen value that
// differs is reported as itself rather than hidden behind this.
const liveTolerance = 1e-6

// TestLiveParityAgainstPython is the M6 gate.
//
// The same fixed questions go through both clients against the same served
// checkpoint, in one session, and the answers have to agree: the chosen value,
// the label, the strategy, the request count and the level exactly, and every
// probability within `liveTolerance`.
//
// The Python side is recorded now rather than replayed from a checked-in file.
// The checkpoint is deterministic within a session -- the recorder runs the set
// twice and records how far its own passes were apart -- but it is not
// deterministic across sessions, so a recording made earlier is a measurement of
// a server that has since answered differently. When the recorder's own passes
// disagree beyond the tolerance, this reports the server's repeatability rather
// than a parity failure, because that is what it is.
func TestLiveParityAgainstPython(t *testing.T) {
	liveSkip(t)

	recorded, meta := recordPythonAnswers(t)
	set := loadLiveQuestions(t)
	if meta.Questions != len(set.Questions) {
		t.Fatalf("the recording holds %d questions and the set has %d", meta.Questions, len(set.Questions))
	}
	if meta.Model != liveModelID() {
		t.Fatalf("the recording is of %s and the suite is pointed at %s", meta.Model, liveModelID())
	}
	if meta.SelfDelta > liveTolerance {
		t.Fatalf("the Python client's own passes differ by %.3g, beyond the %.0e tolerance: "+
			"the served checkpoint is not repeatable right now, so this run cannot tell a "+
			"port difference from the server's. Re-run, or compare the recorded exchanges "+
			"offline with TestClientParity.", meta.SelfDelta, liveTolerance)
	}
	if len(meta.Unstable) != 0 {
		t.Fatalf("the Python client's own passes disagree on the chosen value for %v",
			meta.Unstable)
	}

	model, recorder := liveModel(t, OptionOverrides{})
	if version := model.ReadoutVersion; version != meta.ReadoutVersion {
		t.Fatalf("the readout is %s and the recording is %s", version, meta.ReadoutVersion)
	}
	if endpoint := os.Getenv(EndpointEnv); endpoint != "" && endpoint != meta.Endpoint {
		t.Logf("comparing against %s, where the recording came from %s", endpoint, meta.Endpoint)
	}

	// The thinking question needs the recording's gate, which is a constructor
	// setting, so a second client is built for it.
	gates := map[float64]*DecisionModel{}
	for _, question := range set.Questions {
		if question.Gate == nil {
			continue
		}
		if _, ok := gates[*question.Gate]; !ok {
			gate := *question.Gate
			overrides := OptionOverrides{ReasoningProb: &gate}
			eager, _ := liveModel(t, overrides)
			gates[gate] = eager
		}
	}

	for _, question := range set.Questions {
		t.Run(question.ID, func(t *testing.T) {
			want, ok := recorded.Questions[question.ID]
			if !ok {
				t.Fatalf("the recording has no %s", question.ID)
			}
			client := model
			if question.Gate != nil {
				client = gates[*question.Gate]
			}
			asked, err := question.question()
			if err != nil {
				t.Fatal(err)
			}
			state, err := question.state()
			if err != nil {
				t.Fatalf("the evidence is not JSON: %v", err)
			}
			decision, err := client.Decide(context.Background(), state,
				map[string]Question{question.Head: asked}, CallOptions{})
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			got, ok := decision.Answers[question.Head]
			if !ok {
				t.Fatalf("no answer for %s", question.Head)
			}
			compareLiveAnswer(t, got, want)
		})
	}

	// The serving shape is checked after the questions, so a parity failure is
	// reported as one rather than as a moved endpoint.
	if fingerprint := recorder.fingerprint(t); fingerprint != meta.ServingFingerprint {
		t.Errorf("the endpoint is serving %s and the Python client saw %s in this same "+
			"session; the shape moved mid-run", fingerprint, meta.ServingFingerprint)
	}
}

// compareLiveAnswer requires one answer to be the recorded one.
func compareLiveAnswer(t *testing.T, got Answer, want livePythonAnswer) {
	t.Helper()

	if got.Type != want.Type {
		t.Errorf("the type is %s and the recording is %s", got.Type, want.Type)
	}
	if chosen, wanted := got.Value(), want.Value; !liveSameValue(chosen, wanted) {
		t.Errorf("the chosen value is %v and the recording is %v", chosen, wanted)
	}
	if label, wanted := got.Label(), want.Label; !liveSameValue(label, wanted) {
		t.Errorf("the label is %v and the recording is %v", label, wanted)
	}
	if got.Strategy != want.Strategy {
		t.Errorf("the strategy is %s and the recording is %s", got.Strategy, want.Strategy)
	}
	if got.Calibrated != want.Calibrated {
		t.Errorf("calibrated is %v and the recording is %v", got.Calibrated, want.Calibrated)
	}
	if got.Requests != want.Requests {
		t.Errorf("the answer cost %d requests and the recording cost %d", got.Requests, want.Requests)
	}
	if got.ThoughtTokens != want.ThoughtTokens {
		t.Errorf("the thought is %d tokens and the recording is %d", got.ThoughtTokens, want.ThoughtTokens)
	}
	if level, wanted := got.Level, want.Level; (level == nil) != (wanted == nil) ||
		(level != nil && *level != *wanted) {
		t.Errorf("the level is %v and the recording is %v", level, wanted)
	}
	if !equalStrings(got.MissingFromTop, want.MissingFromTop) {
		t.Errorf("missing from top is %v and the recording is %v",
			got.MissingFromTop, want.MissingFromTop)
	}

	// The distribution: the same keys, and every value within tolerance.
	if len(got.Probabilities) != len(want.Probabilities) {
		t.Errorf("the distribution has %d keys and the recording has %d",
			len(got.Probabilities), len(want.Probabilities))
	}
	keys := make([]string, 0, len(want.Probabilities))
	for key := range want.Probabilities {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	worst, worstKey := 0.0, ""
	for _, key := range keys {
		expected, ok := want.Probabilities[key]
		if !ok {
			t.Errorf("the recording has no probability for %q", key)
			continue
		}
		actual, ok := got.Probabilities[key]
		if !ok {
			t.Errorf("no probability for %q", key)
			continue
		}
		if delta := math.Abs(actual - expected); delta > worst {
			worst, worstKey = delta, key
		}
	}
	if worst > liveTolerance {
		t.Errorf("%s is off by %.3g (%.10f against %.10f), tolerance %.0e",
			worstKey, worst, got.Probabilities[worstKey], want.Probabilities[worstKey], liveTolerance)
	}
	if worst > 0 {
		t.Logf("the largest probability delta is %.3g at %s", worst, worstKey)
	}

	// The distribution is a distribution, which is the one thing the recording
	// cannot show about this run.
	total := 0.0
	for _, probability := range got.Probabilities {
		total += probability
	}
	if math.Abs(total-1) > 1e-6 {
		t.Errorf("the distribution sums to %.10f, not 1", total)
	}
	if len(got.Probabilities) == 0 {
		t.Error("the answer carries no distribution")
	}
}

// liveSameValue compares a chosen value to the recorded one.
//
// A number is compared within tolerance, because the JSON round trip through the
// recording is the same number but not necessarily the same float; everything
// else is compared as it prints.
func liveSameValue(got, want any) bool {
	gotNumber, gotIsNumber := liveNumber(got)
	wantNumber, wantIsNumber := liveNumber(want)
	if gotIsNumber && wantIsNumber {
		return math.Abs(gotNumber-wantNumber) <= liveTolerance
	}
	return fmt.Sprint(got) == fmt.Sprint(want)
}

func liveNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	}
	return 0, false
}

// -- the ported live cases ----------------------------------------------------
//
// The DE-2 half of the Python `tests/test_live.py`. The DE-1 overflow case, the
// model card's 161-token example and the image case are absent: this port has no
// DE-1 read and no image path, which `docs/PORT.md` records.

// liveSpam is the Python suite's obvious case.
var liveSpam = map[string]any{
	"sms":    "WINNER!! You have won a $1000 gift card. Claim it now: bit.ly/xyz",
	"sender": "+1-555-0199",
}

// TestLiveHealthReportsTheEndpointAndPassesTheBoundaryCheck is
// `test_health_reports_the_endpoint_and_passes_the_boundary_check`.
func TestLiveHealthReportsTheEndpointAndPassesTheBoundaryCheck(t *testing.T) {
	model, _ := liveModel(t, OptionOverrides{})
	report := model.Health(context.Background(), false)
	if report["models_status"] != 200 {
		t.Errorf("the listing is %v: %v", report["models_status"], report)
	}
	if report["model_listed"] != true {
		t.Errorf("the model is not listed: %v", report)
	}
	if report["boundary_check"] != "passed" {
		t.Errorf("the boundary check is %v", report["boundary_check"])
	}
	if report["ok"] != true {
		t.Errorf("the report is not ok: %v", report)
	}
}

// TestLiveHealthProbeServesARead is `test_health_probe_serves_a_read`, minus the
// `chat_system_render` assertion: this port has no chat-render probe, so
// `Health` has no such entry.
func TestLiveHealthProbeServesARead(t *testing.T) {
	model, _ := liveModel(t, OptionOverrides{})
	report := model.Health(context.Background(), true)
	probe, _ := report["read_probe"].(string)
	if !strings.HasPrefix(probe, "passed") {
		t.Errorf("the probe is %q: %v", probe, report)
	}
	if _, ok := report["chat_system_render"]; ok {
		t.Error("the report carries the image path's chat render probe")
	}
	if report["ok"] != true {
		t.Errorf("the report is not ok: %v", report)
	}
}

// TestLiveClassifyAnswersAnObviousCase is
// `test_classify_answers_an_obvious_case`.
func TestLiveClassifyAnswersAnObviousCase(t *testing.T) {
	model, _ := liveModel(t, OptionOverrides{})
	result, err := model.Classify(context.Background(), liveSpam,
		map[string]HeadSpec{"intent": {Options: []Option{
			{Key: "spam", Description: "Unsolicited bulk or scam message"},
			{Key: "ham", Description: "Ordinary message"}}}}, ClassifyOptions{})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if result.Values["intent"] != "spam" {
		t.Errorf("the answer is %v, want spam", result.Values["intent"])
	}
	confidence := result.Answers["intent"].Confidence
	if confidence == nil || *confidence < 0 || *confidence > 1 {
		t.Errorf("the confidence is %v", confidence)
	}
}

// TestLiveOneCallCanAskSeveralHeads is `test_one_call_can_ask_several_heads`.
func TestLiveOneCallCanAskSeveralHeads(t *testing.T) {
	model, _ := liveModel(t, OptionOverrides{})
	result, err := model.Decide(context.Background(), liveSpam, map[string]Question{
		"is_spam": NewNoul("Is this message spam?"),
		"ask": NewChoice("What does the sender want?", []Option{
			{Key: "card_details", Description: "Payment card details"},
			{Key: "callback", Description: "A support callback"},
			{Key: "nothing", Description: "Nothing; it is routine"}}),
		"risk": NewScore("How risky is acting on this message?",
			[]any{"Safe", "Suspicious", "Dangerous"}),
	}, CallOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if noul, _ := result.Values["is_spam"].(float64); noul <= 0.5 {
		t.Errorf("the noul answer is %v, want above 0.5", result.Values["is_spam"])
	}
	if result.Values["ask"] != "card_details" {
		t.Errorf("the choice answer is %v, want card_details", result.Values["ask"])
	}
	// `Decide` returns the probability-weighted position; the level the answer
	// sits on is on the answer object.
	if score, _ := result.Values["risk"].(float64); score <= 0.5 {
		t.Errorf("the score answer is %v, want above 0.5", result.Values["risk"])
	}
	if level := result.Answers["risk"].Level; level == nil ||
		(*level != "Suspicious" && *level != "Dangerous") {
		t.Errorf("the risk level is %v", level)
	}
	// One request per head, plus two for any DE-2 head unsure enough to think.
	thinking := 0
	for _, answer := range result.Answers {
		if answer.ThoughtClosed != nil {
			thinking++
		}
	}
	requests, _ := result.Usage["requests"].(int)
	if requests != 3+2*thinking {
		t.Errorf("the call spent %d requests, want %d (3 heads, %d thinking)",
			requests, 3+2*thinking, thinking)
	}
	if tokens, _ := result.Usage["input_tokens"].(int); tokens <= 0 {
		t.Errorf("the call reports %d input tokens", result.Usage["input_tokens"])
	}
}

// TestLiveAnswersAreJSONSerializable is `test_answers_are_json_serializable`.
func TestLiveAnswersAreJSONSerializable(t *testing.T) {
	model, _ := liveModel(t, OptionOverrides{})
	result, err := model.Classify(context.Background(), liveSpam,
		map[string]HeadSpec{"intent": {Options: []Option{
			{Key: "spam"}, {Key: "ham"}}}}, ClassifyOptions{IncludeProbabilities: true})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	encoded, err := json.Marshal(result.Values)
	if err != nil {
		t.Fatalf("the values are not JSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("the values do not round trip: %v", err)
	}
	answer, _ := decoded["intent"].(map[string]any)
	label, _ := answer["label"].(string)
	if label != "spam" && label != "ham" {
		t.Errorf("the serialized label is %v", answer["label"])
	}
}

// TestLiveDE2ReadsAWideChoiceInOnePrompt is
// `test_de2_reads_a_wide_choice_in_one_prompt`: 27 and 128 options are one
// request, 256 is two, and the answer is the option the evidence names.
func TestLiveDE2ReadsAWideChoiceInOnePrompt(t *testing.T) {
	model, _ := liveModel(t, OptionOverrides{})
	for _, testCase := range []struct {
		count    int
		requests int
	}{{27, 1}, {128, 1}, {256, 2}} {
		t.Run(fmt.Sprint(testCase.count), func(t *testing.T) {
			options := make([]Option, 0, testCase.count)
			for index := 0; index < testCase.count; index++ {
				options = append(options, Option{
					Key:         fmt.Sprintf("label_%d", index),
					Description: fmt.Sprintf("Category number %d", index)})
			}
			result, err := model.Decide(context.Background(), map[string]any{"category": 7},
				map[string]Question{"wide": NewChoice("Which category number is stated?", options)},
				CallOptions{})
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			answer := result.Answers["wide"]
			if answer.Choice == nil || *answer.Choice != "label_7" {
				t.Errorf("the answer is %v, want label_7", answer.Choice)
			}
			if len(answer.Probabilities) != testCase.count {
				t.Errorf("the distribution has %d keys, want %d",
					len(answer.Probabilities), testCase.count)
			}
			total := 0.0
			for _, probability := range answer.Probabilities {
				total += probability
			}
			if math.Abs(total-1) > 1e-6 {
				t.Errorf("the distribution sums to %.10f", total)
			}
			if answer.Strategy != "repeat2" {
				t.Errorf("the strategy is %s, want repeat2", answer.Strategy)
			}
			if len(answer.MissingFromTop) != 0 {
				t.Errorf("the answer fell back for %v", answer.MissingFromTop)
			}
			if answer.Requests != testCase.requests {
				t.Errorf("the answer cost %d requests, want %d", answer.Requests, testCase.requests)
			}
			if answer.Confidence == nil {
				t.Error("the answer carries no confidence")
			}
			if version, _ := result.Meta["readout_version"].(string); version != "de2-codebook-v1" {
				t.Errorf("the readout version is %v", result.Meta["readout_version"])
			}
			// The wire shape of a choice answer is exactly these four keys.
			wire := answer.ToWire()
			if len(wire) != 4 {
				t.Errorf("the wire answer is %v", wire)
			}
		})
	}
}

// TestLiveDE2ThinksWhenTheRepeatedReadIsUnsure is
// `test_de2_thinks_when_the_repeated_read_is_unsure`.
//
// A gate of 1.0 sends every question that is not certain through a thought.
func TestLiveDE2ThinksWhenTheRepeatedReadIsUnsure(t *testing.T) {
	gate := 1.0
	model, _ := liveModel(t, OptionOverrides{ReasoningProb: &gate})
	if model.ThinkGate() != 1.0 {
		t.Fatalf("the gate is %v, want 1.0", model.ThinkGate())
	}
	result, err := model.Decide(context.Background(), "What is 17 * 23 - 111?",
		map[string]Question{"sum": NewChoice("Which option is correct?", []Option{
			{Key: "a", Description: "270"},
			{Key: "b", Description: "280"},
			{Key: "c", Description: "290"},
			{Key: "d", Description: "391"}})}, CallOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	answer := result.Answers["sum"]
	if answer.Strategy != "repeat2-think" {
		t.Errorf("the strategy is %s, want repeat2-think", answer.Strategy)
	}
	if answer.Requests != 3 {
		t.Errorf("the answer cost %d requests, want 3", answer.Requests)
	}
	if answer.ThoughtTokens <= 0 || answer.ThoughtTokens > model.ThinkBudget() {
		t.Errorf("the thought is %d tokens, budget %d", answer.ThoughtTokens, model.ThinkBudget())
	}
	if answer.ThoughtClosed == nil {
		t.Error("the answer does not say whether the model closed the thought")
	}
	if result.Values["sum"] != "b" {
		t.Errorf("the answer is %v, want b", result.Values["sum"])
	}
	if tokens, _ := result.Usage["thought_tokens"].(int); tokens != answer.ThoughtTokens {
		t.Errorf("the usage reports %v thought tokens and the answer %d",
			result.Usage["thought_tokens"], answer.ThoughtTokens)
	}
	// The record carries a fit for the thinking read (`think:choice=6.8`), so a
	// `repeat2-think` answer is calibrated and carries that temperature.
	//
	// The Python suite asserts `answer.calibrated is False` here, which is stale:
	// it was written before the DE-2 record gained the `think` fit. The Python
	// client returns True and 6.8 for this exact call, which is what this checks.
	if !answer.Calibrated {
		t.Error("the answer is not calibrated, and the record covers the thinking read")
	}
	if answer.Temperature != 6.8 {
		t.Errorf("the temperature is %v, want the record's thinking fit 6.8", answer.Temperature)
	}
	if answer.Confidence == nil {
		t.Error("the answer carries no confidence")
	}
}

// TestLiveModelIsTheRecordedCheckpoint keeps the suite honest about what it
// measured: the endpoint it is pointed at is serving the model it thinks it is.
func TestLiveModelIsTheRecordedCheckpoint(t *testing.T) {
	model, recorder := liveModel(t, OptionOverrides{})
	// The probe, because the fingerprint rides on a completion response and the
	// plain report only lists models.
	report := model.Health(context.Background(), true)
	if report["model"] != liveModelID() {
		t.Errorf("the report is of %v, want %s", report["model"], liveModelID())
	}
	t.Logf("model %s, family %s (%v), readout %v, serving %s, %d requests",
		report["model"], report["model_family"], report["family_source"],
		report["readout_version"], recorder.fingerprint(t), recorder.requests)
}
