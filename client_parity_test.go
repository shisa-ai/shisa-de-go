package shisade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The friendly layer's parity gate.
//
// scripts/gen_client_parity.py drove the Python `DecisionModel` against the
// hosted DE-2 endpoint through a transport that forwarded every request to the
// real server and recorded the request and response bodies. This test rebuilds
// each case, points a Go client at a stub that serves exactly those recorded
// responses, and requires the Go answer to be the Python answer.
//
// No network is used: the recorded exchanges are the only thing the client sees.
// A request the Python client never made has no recorded response, and the stub
// fails the case rather than guessing.

type clientFixture struct {
	Endpoint string       `json:"endpoint"`
	Model    string       `json:"model"`
	Cases    []clientCase `json:"cases"`
}

type clientCase struct {
	Name      string            `json:"name"`
	State     json.RawMessage   `json:"state"`
	Kind      string            `json:"kind"`
	Questions []questionFixture `json:"questions"`
	Heads     []questionFixture `json:"heads"`
	Options   callFixture       `json:"options"`
	Model     modelFixture      `json:"model"`
	Exchanges []clientExchange  `json:"exchanges"`
	Decision  decisionFixture   `json:"decision"`
}

type modelFixture struct {
	ID                string  `json:"id"`
	DeclaredModel     *string `json:"declared_model"`
	DeclaredTokenizer *string `json:"declared_tokenizer"`
}

type questionFixture struct {
	Head         string            `json:"head"`
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Options      []optionFixture   `json:"options"`
	Labels       []string          `json:"labels"`
	Levels       []json.RawMessage `json:"levels"`
}

type optionFixture struct {
	Key         string          `json:"key"`
	Description json.RawMessage `json:"description"`
}

type callFixture struct {
	Policy               string `json:"policy"`
	Probability          bool   `json:"probability"`
	Calibrated           *bool  `json:"calibrated"`
	Debug                bool   `json:"debug"`
	IncludeConfidence    bool   `json:"include_confidence"`
	IncludeProbabilities bool   `json:"include_probabilities"`
}

type clientExchange struct {
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Request  json.RawMessage `json:"request"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response"`
}

type decisionFixture struct {
	Values             map[string]json.RawMessage `json:"values"`
	Answers            map[string]answerFixture   `json:"answers"`
	Raw                map[string]json.RawMessage `json:"raw"`
	Usage              map[string]json.RawMessage `json:"usage"`
	Meta               map[string]json.RawMessage `json:"meta"`
	CalibrationLevel   string                     `json:"calibration_level"`
	CalibrationApplied bool                       `json:"calibration_applied"`
	FamilySource       string                     `json:"family_source"`
	CalibrationID      string                     `json:"calibration_id"`
}

type answerFixture struct {
	Type           string             `json:"type"`
	Value          json.RawMessage    `json:"value"`
	Calibrated     bool               `json:"calibrated"`
	Temperature    float64            `json:"temperature"`
	Requests       int                `json:"requests"`
	Confidence     *float64           `json:"confidence"`
	Probabilities  map[string]float64 `json:"probabilities"`
	Strategy       string             `json:"strategy"`
	ScoreSemantics string             `json:"score_semantics"`
	Stages         int                `json:"stages"`
	LogicalReads   int                `json:"logical_reads"`
	Level          *string            `json:"level"`
	MissingFromTop []string           `json:"missing_from_top"`
	ThoughtTokens  int                `json:"thought_tokens"`
	ThoughtClosed  *bool              `json:"thought_closed"`
}

type clientMeta struct {
	Endpoint           string `json:"endpoint"`
	Model              string `json:"model"`
	ServingFingerprint string `json:"serving_fingerprint"`
	Recorded           string `json:"recorded"`
	PythonRepo         string `json:"python_repo"`
	ReadoutVersion     string `json:"readout_version"`
	Cases              int    `json:"cases"`
	Exchanges          int    `json:"exchanges"`
	SHA256             string `json:"sha256"`
}

func loadClientFixture(t *testing.T) (clientFixture, clientMeta) {
	t.Helper()
	path := filepath.Join("testdata", "client_parity.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	metaPath := filepath.Join("testdata", "client_parity.meta.json")
	metaRaw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("reading %s: %v", metaPath, err)
	}
	var meta clientMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("%s: %v", metaPath, err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != meta.SHA256 {
		t.Errorf("%s is %s, the provenance records %s", path, got, meta.SHA256)
	}
	var fixture clientFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if fixture.Endpoint != meta.Endpoint || fixture.Model != meta.Model {
		t.Errorf("the fixture is for %s %s, the provenance records %s %s",
			fixture.Endpoint, fixture.Model, meta.Endpoint, meta.Model)
	}
	if len(fixture.Cases) != meta.Cases {
		t.Errorf("the fixture holds %d cases, the provenance records %d",
			len(fixture.Cases), meta.Cases)
	}
	total := 0
	for _, one := range fixture.Cases {
		total += len(one.Exchanges)
	}
	if total != meta.Exchanges {
		t.Errorf("the fixture holds %d exchanges, the provenance records %d", total, meta.Exchanges)
	}
	if meta.ServingFingerprint == "" || meta.Recorded == "" {
		t.Error("the provenance does not say when or against what the fixture was recorded")
	}
	return fixture, meta
}

// parityTransport serves the recorded responses for the requests this client
// makes, and fails the test for a request the Python client never made.
type parityTransport struct {
	t         *testing.T
	caseName  string
	byBody    map[string]int
	exchanges []clientExchange
	// A call with several heads reads them concurrently, so the transport is
	// reached from several goroutines at once.
	mu        sync.Mutex
	used      map[int]bool
	unmatched []string
}

func newParityTransport(t *testing.T, name string, exchanges []clientExchange) *parityTransport {
	byBody := make(map[string]int, len(exchanges))
	for index, exchange := range exchanges {
		byBody[canonicalJSON(t, exchange.Request)] = index
	}
	return &parityTransport{t: t, caseName: name, byBody: byBody, exchanges: exchanges,
		used: map[int]bool{}}
}

func (p *parityTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	key := canonicalJSON(p.t, body)
	p.mu.Lock()
	index, ok := p.byBody[key]
	if !ok {
		p.unmatched = append(p.unmatched, key)
		p.mu.Unlock()
		return nil, fmt.Errorf("%s: the client sent a request the Python client never made: %s",
			p.caseName, truncate(key))
	}
	p.used[index] = true
	p.mu.Unlock()
	exchange := p.exchanges[index]
	if request.Method != exchange.Method {
		p.t.Errorf("%s: request %d used %s, the Python client used %s",
			p.caseName, index, request.Method, exchange.Method)
	}
	if request.URL.Path != exchange.Path {
		p.t.Errorf("%s: request %d went to %s, the Python client went to %s",
			p.caseName, index, request.URL.Path, exchange.Path)
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: exchange.Status,
		Status:     fmt.Sprintf("%d %s", exchange.Status, http.StatusText(exchange.Status)),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(string(exchange.Response))),
		Request:    request,
	}, nil
}

func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("a recorded body is not valid JSON: %v (%s)", err, truncate(string(raw)))
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("canonicalizing a recorded body: %v", err)
	}
	return string(encoded)
}

// TestClientParity replays every recorded case and requires the Go answer to be
// the Python answer.
func TestClientParity(t *testing.T) {
	fixture, _ := loadClientFixture(t)
	pinned := pinnedTokenizer(t)
	if pinned == nil {
		t.Skip("no tokenizer.json found; the replay needs the pinned tokenizer to send the same ids")
	}
	for _, one := range fixture.Cases {
		one := one
		t.Run(one.Name, func(t *testing.T) {
			transport := newParityTransport(t, one.Name, one.Exchanges)
			model := buildParityModel(t, fixture, one, transport, pinned)
			got := runParityCase(t, model, one)
			transport.mu.Lock()
			unmatched, used := len(transport.unmatched), len(transport.used)
			transport.mu.Unlock()
			if unmatched != 0 {
				t.Fatalf("the client sent %d request(s) the Python client never made", unmatched)
			}
			if used != len(transport.exchanges) {
				t.Errorf("the replay used %d of the %d recorded exchanges",
					used, len(transport.exchanges))
			}
			compareDecision(t, one.Decision, got, model)
		})
	}
}

func buildParityModel(t *testing.T, fixture clientFixture, one clientCase,
	transport http.RoundTripper, pinned Tokenizer) *DecisionModel {
	t.Helper()
	readout, err := NewReadout(ReadoutConfig{
		BaseURL:    "http://parity.invalid/openai",
		Model:      fixture.Model,
		Tokenizer:  fixture.Model,
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	readout.SetTokenizer(pinned)

	model := fixture.Model
	if one.Model.DeclaredModel != nil {
		model = *one.Model.DeclaredModel
	}
	tokenizer := fixture.Model
	if one.Model.DeclaredTokenizer != nil {
		tokenizer = *one.Model.DeclaredTokenizer
	}
	built, err := NewDecisionModel(DecisionModelConfig{
		BaseURL:   "http://parity.invalid/openai",
		Model:     model,
		Tokenizer: tokenizer,
		Readout:   readout,
	})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	return built
}

func runParityCase(t *testing.T, model *DecisionModel, one clientCase) *Decision {
	t.Helper()
	ctx := context.Background()
	overrides := OptionOverrides{}
	if one.Options.Policy != "" {
		policy := one.Options.Policy
		overrides.Policy = &policy
	}
	switch one.Kind {
	case "decide":
		questions := map[string]Question{}
		for _, spec := range one.Questions {
			questions[spec.Head] = parityQuestion(t, spec)
		}
		decision, err := model.Decide(ctx, decodeState(t, one.State), questions, CallOptions{
			Probability: one.Options.Probability,
			Calibrated:  one.Options.Calibrated,
			Debug:       one.Options.Debug,
			Overrides:   overrides,
		})
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		return decision
	case "classify":
		heads := map[string]HeadSpec{}
		for _, spec := range one.Heads {
			heads[spec.Head] = parityHead(t, spec)
		}
		decision, err := model.Classify(ctx, decodeState(t, one.State), heads, ClassifyOptions{
			CallOptions: CallOptions{
				Probability: one.Options.Probability,
				Calibrated:  one.Options.Calibrated,
				Debug:       one.Options.Debug,
				Overrides:   overrides,
			},
			IncludeConfidence:    one.Options.IncludeConfidence,
			IncludeProbabilities: one.Options.IncludeProbabilities,
		})
		if err != nil {
			t.Fatalf("Classify: %v", err)
		}
		return decision
	}
	t.Fatalf("unknown case kind %q", one.Kind)
	return nil
}

func decodeState(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var state any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decoding the state: %v", err)
	}
	return state
}

func parityQuestion(t *testing.T, spec questionFixture) Question {
	t.Helper()
	switch spec.Type {
	case "noul":
		return NewNoul(spec.Instructions)
	case "score":
		return NewScore(spec.Instructions, decodeLevels(t, spec.Levels))
	default:
		return NewChoice(spec.Instructions, parityOptions(t, spec.Options))
	}
}

func parityHead(t *testing.T, spec questionFixture) HeadSpec {
	t.Helper()
	switch spec.Type {
	case "labels":
		options := make([]Option, 0, len(spec.Labels))
		for _, label := range spec.Labels {
			options = append(options, Option{Key: label})
		}
		return HeadSpec{Options: options}
	case "mapping":
		return HeadSpec{Options: parityOptions(t, spec.Options)}
	case "levels":
		return HeadSpec{Levels: decodeLevels(t, spec.Levels)}
	case "multi_label":
		options := make([]Option, 0, len(spec.Options))
		for _, option := range spec.Options {
			options = append(options, Option{Key: option.Key})
		}
		return HeadSpec{Options: options, MultiLabel: true, Instructions: spec.Instructions}
	case "score":
		return HeadSpec{Levels: decodeLevels(t, spec.Levels), Instructions: spec.Instructions}
	case "noul":
		return HeadSpec{Question: NewNoul(spec.Instructions)}
	default:
		return HeadSpec{Options: parityOptions(t, spec.Options), Instructions: spec.Instructions}
	}
}

func parityOptions(t *testing.T, specs []optionFixture) []Option {
	t.Helper()
	options := make([]Option, 0, len(specs))
	for _, spec := range specs {
		var description any
		if len(spec.Description) != 0 {
			if err := json.Unmarshal(spec.Description, &description); err != nil {
				t.Fatalf("decoding option %q's description: %v", spec.Key, err)
			}
		}
		options = append(options, Option{Key: spec.Key, Description: description})
	}
	return options
}

func decodeLevels(t *testing.T, raw []json.RawMessage) []any {
	t.Helper()
	levels := make([]any, 0, len(raw))
	for _, item := range raw {
		var level any
		if err := json.Unmarshal(item, &level); err != nil {
			t.Fatalf("decoding a level: %v", err)
		}
		levels = append(levels, level)
	}
	return levels
}

// compareDecision requires the Go decision to be the recorded Python one.
func compareDecision(t *testing.T, want decisionFixture, got *Decision, model *DecisionModel) {
	t.Helper()
	if model.CalibrationLevel != ApplicabilityLevel(want.CalibrationLevel) {
		t.Errorf("calibration level is %q, the Python client's was %q",
			string(model.CalibrationLevel), want.CalibrationLevel)
	}
	if model.CalibrationApplied != want.CalibrationApplied {
		t.Errorf("calibration applied is %v, the Python client's was %v",
			model.CalibrationApplied, want.CalibrationApplied)
	}
	if string(model.FamilySource) != want.FamilySource {
		t.Errorf("family source is %q, the Python client's was %q",
			string(model.FamilySource), want.FamilySource)
	}
	if model.Calibration.ID() != want.CalibrationID {
		t.Errorf("calibration id is %q, the Python client's was %q",
			model.Calibration.ID(), want.CalibrationID)
	}

	gotAnswers := map[string]any{}
	for head, answer := range got.Answers {
		gotAnswers[head] = jsonShape(t, answer.ToDict())
	}
	if len(gotAnswers) != len(want.Answers) {
		t.Errorf("the answer heads are %v, the Python client's are %v",
			sortedKeys(gotAnswers), sortedKeys2(want.Answers))
	}
	for head, answer := range want.Answers {
		actual, ok := got.Answers[head]
		if !ok {
			t.Errorf("head %q is missing from the answers", head)
			continue
		}
		compareAnswer(t, head, answer, actual)
	}
	compareMap(t, "values", rawToAny(t, want.Values), jsonShapeMap(t, got.Values))
	compareMap(t, "raw", rawToAny(t, want.Raw), jsonShapeMap(t, nestedToAny(got.Raw)))
	compareMap(t, "usage", rawToAny(t, want.Usage), jsonShapeMap(t, got.Usage))
	// The endpoint is where the Go client was pointed, which in the replay is
	// the stub; everything else in the meta block is the client's own decision.
	compareMap(t, "meta", withoutKey(rawToAny(t, want.Meta), "base_url"),
		withoutKey(jsonShapeMap(t, got.Meta), "base_url"))
}

func compareAnswer(t *testing.T, head string, want answerFixture, got Answer) {
	t.Helper()
	if got.Type != want.Type {
		t.Errorf("head %q: type is %q, the Python client's was %q", head, got.Type, want.Type)
	}
	if got.Calibrated != want.Calibrated {
		t.Errorf("head %q: calibrated is %v, the Python client's was %v",
			head, got.Calibrated, want.Calibrated)
	}
	if got.Temperature != want.Temperature {
		t.Errorf("head %q: temperature is %v, the Python client's was %v",
			head, got.Temperature, want.Temperature)
	}
	if got.Requests != want.Requests {
		t.Errorf("head %q: requests is %d, the Python client's was %d",
			head, got.Requests, want.Requests)
	}
	if got.Strategy != want.Strategy {
		t.Errorf("head %q: strategy is %q, the Python client's was %q",
			head, got.Strategy, want.Strategy)
	}
	if got.ScoreSemantics != want.ScoreSemantics {
		t.Errorf("head %q: score semantics is %q, the Python client's was %q",
			head, got.ScoreSemantics, want.ScoreSemantics)
	}
	if got.Stages != want.Stages || got.LogicalReads != want.LogicalReads {
		t.Errorf("head %q: %d stages and %d logical reads, the Python client's were %d and %d",
			head, got.Stages, got.LogicalReads, want.Stages, want.LogicalReads)
	}
	if got.ThoughtTokens != want.ThoughtTokens {
		t.Errorf("head %q: %d thought tokens, the Python client's was %d",
			head, got.ThoughtTokens, want.ThoughtTokens)
	}
	if !sameOptionalBool(got.ThoughtClosed, want.ThoughtClosed) {
		t.Errorf("head %q: thought closed is %v, the Python client's was %v",
			head, got.ThoughtClosed, want.ThoughtClosed)
	}
	if !sameOptionalFloat(got.Confidence, want.Confidence) {
		t.Errorf("head %q: confidence is %v, the Python client's was %v",
			head, formatOptionalFloat(got.Confidence), formatOptionalFloat(want.Confidence))
	}
	if !closeEnough(got.Probabilities, want.Probabilities, 1e-12) {
		t.Errorf("head %q: probabilities are %v, the Python client's were %v",
			head, got.Probabilities, want.Probabilities)
	}
	if !sameOptionalString(got.Level, want.Level) {
		t.Errorf("head %q: level is %v, the Python client's was %v",
			head, formatOptionalString(got.Level), formatOptionalString(want.Level))
	}
	if !sameStrings(got.MissingFromTop, want.MissingFromTop) {
		t.Errorf("head %q: missing from top is %v, the Python client's was %v",
			head, got.MissingFromTop, want.MissingFromTop)
	}
	var wantValue any
	if len(want.Value) != 0 {
		if err := json.Unmarshal(want.Value, &wantValue); err != nil {
			t.Fatalf("head %q: the recorded value is not JSON: %v", head, err)
		}
	}
	if !sameJSON(t, "head "+head+" value", jsonShape(t, got.Value()), wantValue) {
		t.Errorf("head %q: value is %v, the Python client's was %v",
			head, got.Value(), wantValue)
	}
}

// compareMap compares a Go map to a recorded one, ignoring wall-clock timing,
// which is a property of the machine rather than of the client.
func compareMap(t *testing.T, name string, want map[string]any, got map[string]any) {
	t.Helper()
	if name == "usage" || name == "meta" {
		want = withoutKey(want, "wall_ms")
	}
	got = withoutKey(got, "wall_ms")
	if !sameJSON(t, name, got, want) {
		t.Errorf("%s is %v, the Python client's was %v", name, got, want)
	}
}

// jsonShape round-trips a value through JSON, so a comparison sees the same
// shapes on both sides: `map[string]int` becomes `map[string]any` and every
// number becomes a float64, which is what decoding the recorded side produces.
func jsonShape(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding a value for comparison: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decoding a value for comparison: %v", err)
	}
	return decoded
}

func jsonShapeMap(t *testing.T, values map[string]any) map[string]any {
	t.Helper()
	shaped, _ := jsonShape(t, values).(map[string]any)
	if shaped == nil {
		return map[string]any{}
	}
	return shaped
}

// sameJSON compares two JSON-shaped values, allowing floats to differ by a
// relative 1e-12. The Python client sums a softmax in a different order and
// rounds differently on CPython 3.12+, so exact equality is not the right test;
// see docs/PORT.md.
func sameJSON(t *testing.T, path string, got, want any) bool {
	t.Helper()
	switch typed := want.(type) {
	case nil:
		if got != nil {
			t.Logf("%s: %v, want null", path, got)
			return false
		}
		return true
	case bool:
		actual, ok := got.(bool)
		if !ok || actual != typed {
			t.Logf("%s: %v, want %v", path, got, typed)
			return false
		}
		return true
	case float64:
		actual, ok := got.(float64)
		if !ok || !sameFloat(actual, typed) {
			t.Logf("%s: %v, want %v", path, got, typed)
			return false
		}
		return true
	case string:
		actual, ok := got.(string)
		if !ok || actual != typed {
			t.Logf("%s: %q, want %q", path, got, typed)
			return false
		}
		return true
	case []any:
		actual, ok := got.([]any)
		if !ok || len(actual) != len(typed) {
			t.Logf("%s: %v, want %v", path, got, typed)
			return false
		}
		equal := true
		for index := range typed {
			if !sameJSON(t, fmt.Sprintf("%s[%d]", path, index), actual[index], typed[index]) {
				equal = false
			}
		}
		return equal
	case map[string]any:
		actual, ok := got.(map[string]any)
		if !ok || len(actual) != len(typed) {
			t.Logf("%s: %v, want %v", path, got, typed)
			return false
		}
		equal := true
		for key, value := range typed {
			member, present := actual[key]
			if !present {
				t.Logf("%s: the key %q is missing (got %v)", path, key, actual)
				equal = false
				continue
			}
			if !sameJSON(t, path+"."+key, member, value) {
				equal = false
			}
		}
		return equal
	default:
		t.Logf("%s: the recorded value has an unsupported type %T", path, want)
		return false
	}
}

func sameFloat(got, want float64) bool {
	if math.IsNaN(got) || math.IsNaN(want) || math.IsInf(got, 0) || math.IsInf(want, 0) {
		return false
	}
	return math.Abs(got-want) <= 1e-12*math.Max(1, math.Abs(want))
}

func nestedToAny(values map[string]map[string]any) map[string]any {
	converted := make(map[string]any, len(values))
	for key, value := range values {
		converted[key] = value
	}
	return converted
}

func rawToAny(t *testing.T, values map[string]json.RawMessage) map[string]any {
	t.Helper()
	decoded := make(map[string]any, len(values))
	for key, raw := range values {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("%s is not valid JSON: %v", key, err)
		}
		decoded[key] = value
	}
	return decoded
}

func withoutKey(values map[string]any, key string) map[string]any {
	trimmed := make(map[string]any, len(values))
	for name, value := range values {
		if name == key {
			continue
		}
		trimmed[name] = value
	}
	return trimmed
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys2(values map[string]answerFixture) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sameOptionalFloat(got, want *float64) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return sameFloat(*got, *want)
}

func sameOptionalString(got, want *string) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}

func sameOptionalBool(got, want *bool) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}

func formatOptionalFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func formatOptionalString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
