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
	"time"
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
	Wire               map[string]any             `json:"wire"`
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
	// Recorded is the answer object as the generator wrote it: `to_dict` plus
	// `level`, `legend` and `wire` beside it. Keeping it raw is what makes the
	// `to_dict` comparison whole rather than relative to the fields this struct
	// names.
	Recorded json.RawMessage `json:"-"`
	// Legend and Wire are recorded beside `to_dict`, which carries neither: the
	// legend is the level text per position and `to_wire` is the TypeSafe shape.
	Legend         map[string]string `json:"legend"`
	Wire           map[string]any    `json:"wire"`
	MissingFromTop []string          `json:"missing_from_top"`
	ThoughtTokens  int               `json:"thought_tokens"`
	ThoughtClosed  *bool             `json:"thought_closed"`
}

// UnmarshalJSON fills the named fields and keeps the object as it was written,
// so the `to_dict` comparison is against the recorded map rather than against a
// map rebuilt from the fields this struct happens to name.
func (a *answerFixture) UnmarshalJSON(data []byte) error {
	type plain answerFixture
	var fields plain
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*a = answerFixture(fields)
	a.Recorded = append(json.RawMessage(nil), data...)
	return nil
}

type clientMeta struct {
	Endpoint           string `json:"endpoint"`
	Model              string `json:"model"`
	ServingFingerprint string `json:"serving_fingerprint"`
	Recorded           string `json:"recorded"`
	PythonRepo         string `json:"python_repo"`
	ReadoutVersion     string `json:"readout_version"`
	TokenizerRevision  string `json:"tokenizer_revision"`
	Cases              int    `json:"cases"`
	Exchanges          int    `json:"exchanges"`
	SHA256             string `json:"sha256"`
}

func loadClientFixture(t *testing.T) (clientFixture, clientMeta) {
	t.Helper()
	return readClientFixture(t, filepath.Join("testdata", "client_parity.json"),
		filepath.Join("testdata", "client_parity.meta.json"))
}

// readClientFixture loads a fixture and checks it against its provenance. The
// paths are arguments so the integrity checks can be run against a mutated copy.
func readClientFixture(t *testing.T, path, metaPath string) (clientFixture, clientMeta) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	metaRaw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("reading %s: %v", metaPath, err)
	}
	var meta clientMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("%s: %v", metaPath, err)
	}
	var fixture clientFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	for _, problem := range fixtureProblems(raw, fixture, meta) {
		t.Error(problem)
	}
	return fixture, meta
}

// fixtureProblems is everything the provenance and the fixture disagree about.
//
// It returns the problems rather than reporting them so the checks can be run
// against a mutated copy; a check that cannot be shown to fail is a check nobody
// has made.
func fixtureProblems(raw []byte, fixture clientFixture, meta clientMeta) []string {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != meta.SHA256 {
		report("the fixture is %s, the provenance records %s", got, meta.SHA256)
	}
	if fixture.Endpoint != meta.Endpoint || fixture.Model != meta.Model {
		report("the fixture is for %s %s, the provenance records %s %s",
			fixture.Endpoint, fixture.Model, meta.Endpoint, meta.Model)
	}
	if len(fixture.Cases) != meta.Cases {
		report("the fixture holds %d cases, the provenance records %d",
			len(fixture.Cases), meta.Cases)
	}
	total := 0
	for _, one := range fixture.Cases {
		total += len(one.Exchanges)
	}
	if total != meta.Exchanges {
		report("the fixture holds %d exchanges, the provenance records %d",
			total, meta.Exchanges)
	}
	if meta.ServingFingerprint == "" || meta.Recorded == "" || meta.TokenizerRevision == "" {
		report("the provenance does not say when, against what, or with which tokenizer " +
			"the fixture was recorded")
	}
	if meta.ReadoutVersion != ReadoutVersionFor(FamilyDE2) {
		report("the fixture was recorded against readout %q, this package reads %q",
			meta.ReadoutVersion, ReadoutVersionFor(FamilyDE2))
	}
	// The fingerprint is the serving shape the responses came from, which is what
	// makes the numbers reproducible. It is checked against the responses the
	// fixture holds, not against its own shape: an endpoint label would pass a
	// shape test and fail this one.
	served := map[string]bool{}
	created := map[string]bool{}
	for _, one := range fixture.Cases {
		for _, exchange := range one.Exchanges {
			var body struct {
				SystemFingerprint string `json:"system_fingerprint"`
				Created           int64  `json:"created"`
			}
			if err := json.Unmarshal(exchange.Response, &body); err != nil {
				report("a recorded response is not JSON: %v", err)
				continue
			}
			if body.SystemFingerprint != "" {
				served[body.SystemFingerprint] = true
			}
			if body.Created != 0 {
				created[time.Unix(body.Created, 0).UTC().Format(time.DateOnly)] = true
			}
		}
	}
	switch {
	case len(served) != 1:
		report("the recorded responses carry %d system fingerprints: %v",
			len(served), sortedKeysOf(served))
	case sortedKeysOf(served)[0] != meta.ServingFingerprint:
		report("the provenance records fingerprint %q, the responses carry %q",
			meta.ServingFingerprint, sortedKeysOf(served)[0])
	}
	// The date is what the responses say, when they say anything: a fixture
	// regenerated on a different day with a stale date would otherwise pass.
	if len(created) != 0 && (len(created) != 1 || !created[meta.Recorded]) {
		report("the provenance records %s, the responses were created on %v",
			meta.Recorded, sortedKeysOf(created))
	}
	return problems
}

// parityTransport serves the recorded responses for the requests this client
// makes, and fails the test for a request the Python client never made.
type parityTransport struct {
	t         *testing.T
	caseName  string
	exchanges []clientExchange
	// A call with several heads reads them concurrently, so the transport is
	// reached from several goroutines at once.
	mu sync.Mutex
	// byBody is a queue per request body, not a single index: two recorded
	// exchanges may carry the same body, and collapsing them would leave one
	// exchange unused, which fails the case for the wrong reason.
	byBody    map[string][]int
	used      map[int]bool
	unmatched []string
}

func newParityTransport(t *testing.T, name string, exchanges []clientExchange) *parityTransport {
	byBody := make(map[string][]int, len(exchanges))
	for index, exchange := range exchanges {
		key := canonicalJSON(t, exchange.Request)
		byBody[key] = append(byBody[key], index)
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
	queue, ok := p.byBody[key]
	if !ok || len(queue) == 0 {
		p.unmatched = append(p.unmatched, key)
		p.mu.Unlock()
		return nil, fmt.Errorf("%s: the client sent a request the Python client never made: %s",
			p.caseName, truncate(key))
	}
	index := queue[0]
	p.byBody[key] = queue[1:]
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

	if len(got.Answers) != len(want.Answers) {
		t.Errorf("the answer heads are %v, the Python client's are %v",
			sortedKeysOf(got.Answers), sortedKeysOf(want.Answers))
	}
	for head, answer := range want.Answers {
		actual, ok := got.Answers[head]
		if !ok {
			t.Errorf("head %q is missing from the answers", head)
			continue
		}
		compareAnswer(t, head, answer, actual)
		// `to_dict` is compared whole, not field by field: it is the shape the
		// Python client returns and the fixture records, so a key this port
		// drops or a value it holds as a pointer shows up here.
		recorded, err := recordedToDict(answer)
		if err != nil {
			t.Errorf("head %q: %v", head, err)
		} else {
			compareMap(t, "answer "+head, jsonShapeMap(t, recorded),
				jsonShapeMap(t, actual.ToDict()))
		}
		// The TypeSafe body, likewise whole.
		compareMap(t, "answer "+head+" wire", jsonShapeMap(t, answer.Wire),
			jsonShapeMap(t, actual.ToWire()))
	}
	compareMap(t, "wire", jsonShapeMap(t, want.Wire), jsonShapeMap(t, got.ToWire()))
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
	if want.Legend != nil && !sameStringMap(got.Legend, want.Legend) {
		t.Errorf("head %q: legend is %v, the Python client's was %v",
			head, got.Legend, want.Legend)
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

func sortedKeysOf[M ~map[string]V, V any](values M) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// recordedToDict is the recorded `Answer.to_dict()`: the answer object as the
// generator wrote it, minus the `level`, `legend` and `wire` it adds beside
// `to_dict` because `to_dict` carries none of them.
//
// The map is the recorded one, not a rebuild, so a key the Python client grows
// that this port does not return fails the comparison instead of being dropped
// by a struct that does not name it.
func recordedToDict(answer answerFixture) (map[string]any, error) {
	if len(answer.Recorded) == 0 {
		return nil, fmt.Errorf("the recorded answer object was not kept")
	}
	var recorded map[string]any
	if err := json.Unmarshal(answer.Recorded, &recorded); err != nil {
		return nil, fmt.Errorf("the recorded answer object is not JSON: %w", err)
	}
	for _, added := range []string{"level", "legend", "wire"} {
		if _, ok := recorded[added]; !ok {
			return nil, fmt.Errorf("the recorded answer object has no %q; the generator "+
				"records it beside to_dict", added)
		}
		delete(recorded, added)
	}
	return recorded, nil
}

func sameOptionalFloat(got, want *float64) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return sameFloat(*got, *want)
}

// TestFixtureIntegrityChecksCanFail pins that the provenance checks reject a
// fixture whose metadata disagrees with the responses it holds.
//
// A sha256 and a case count say the fixture has not changed since it was
// digested; they say nothing about whether the date and fingerprint beside it
// describe the responses inside it. These two do, and without them the
// fingerprint field could hold an endpoint label and still pass.
func TestFixtureIntegrityChecksCanFail(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "client_parity.json"))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	var meta map[string]any
	metaRaw, err := os.ReadFile(filepath.Join("testdata", "client_parity.meta.json"))
	if err != nil {
		t.Fatalf("reading the provenance: %v", err)
	}
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("the provenance is not JSON: %v", err)
	}

	var fixture clientFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("the fixture is not JSON: %v", err)
	}
	problems := func(mutate func(map[string]any)) []string {
		t.Helper()
		copied := map[string]any{}
		for key, value := range meta {
			copied[key] = value
		}
		// The digest is recomputed first, so a mutation is what the checks see
		// rather than a stale hash, and a mutation of the digest itself still
		// wins.
		sum := sha256.Sum256(raw)
		copied["sha256"] = hex.EncodeToString(sum[:])
		mutate(copied)
		encoded, err := json.Marshal(copied)
		if err != nil {
			t.Fatalf("encoding the provenance: %v", err)
		}
		var mutated clientMeta
		if err := json.Unmarshal(encoded, &mutated); err != nil {
			t.Fatalf("decoding the provenance: %v", err)
		}
		return fixtureProblems(raw, fixture, mutated)
	}

	// The unmutated provenance is accepted, so a problem below is the mutation.
	if got := problems(func(map[string]any) {}); len(got) != 0 {
		t.Fatalf("the checked-in fixture has problems: %v", got)
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"an endpoint label in the fingerprint field", func(meta map[string]any) {
			meta["serving_fingerprint"] = "shisa-hosted-2026-10-10"
		}, "the responses carry"},
		{"a date the responses do not fall on", func(meta map[string]any) {
			meta["recorded"] = "2026-10-09"
		}, "were created on"},
		{"a readout version this package does not read", func(meta map[string]any) {
			meta["readout_version"] = "de2-codebook-v0"
		}, "this package reads"},
		{"a case count that disagrees", func(meta map[string]any) {
			meta["cases"] = float64(1)
		}, "the provenance records"},
		{"a digest that disagrees", func(meta map[string]any) {
			meta["sha256"] = strings.Repeat("0", 64)
		}, "the provenance records"},
		{"no tokenizer revision", func(meta map[string]any) {
			meta["tokenizer_revision"] = ""
		}, "does not say when"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := problems(tc.mutate)
			if len(got) == 0 {
				t.Fatalf("%s was accepted", tc.name)
			}
			found := false
			for _, problem := range got {
				if strings.Contains(problem, tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("the problems are %v, none mentioning %q", got, tc.want)
			}
		})
	}
}

// TestRecordedToDictKeepsUnmodelledKeys pins that the whole-map `to_dict`
// comparison is against the recorded object rather than a rebuild.
func TestRecordedToDictKeepsUnmodelledKeys(t *testing.T) {
	recorded := []byte(`{"type":"choice","value":"a","confidence":0.5,
	    "probabilities":{"a":1},"calibrated":true,"temperature":1.89,"requests":1,
	    "strategy":"repeat2","score_semantics":"option-softmax","stages":1,
	    "logical_reads":1,"level":"a","legend":null,"wire":{"type":"choice"},
	    "finalists":["a","b"]}`)
	var answer answerFixture
	if err := json.Unmarshal(recorded, &answer); err != nil {
		t.Fatalf("decoding the recorded answer: %v", err)
	}
	got, err := recordedToDict(answer)
	if err != nil {
		t.Fatalf("recordedToDict: %v", err)
	}
	// `finalists` is not a field of answerFixture. A rebuild would drop it; the
	// recorded map keeps it, which is what makes the comparison whole.
	if _, ok := got["finalists"]; !ok {
		t.Errorf("an unmodelled recorded key was dropped: %v", got)
	}
	for _, added := range []string{"level", "legend", "wire"} {
		if _, ok := got[added]; ok {
			t.Errorf("the recorded %q was not removed: %v", added, got)
		}
	}
	if got["value"] != "a" || got["calibrated"] != true {
		t.Errorf("the recorded map lost a modelled key: %v", got)
	}
	// A recorded object with no `level`/`legend`/`wire` is a generator that
	// stopped writing them, and it is reported rather than silently compared.
	var bare answerFixture
	if err := json.Unmarshal([]byte(`{"type":"choice","value":"a"}`), &bare); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if _, err := recordedToDict(bare); err == nil {
		t.Error("a recorded object with no level, legend or wire was accepted")
	}
}

// TestParityComparatorsCanFail pins that the replay's comparators reject a
// changed value.
//
// The gate is only as strong as the comparisons it makes, and a comparator that
// returns true for everything would let every recorded case pass. Each one here
// is fed a difference the fixture actually carries.
func TestParityComparatorsCanFail(t *testing.T) {
	legend := map[string]string{"0": "none", "1": "low"}
	if !sameStringMap(legend, map[string]string{"0": "none", "1": "low"}) {
		t.Error("an equal legend was rejected")
	}
	if sameStringMap(legend, map[string]string{"0": "none", "1": "LOW"}) {
		t.Error("a changed legend text was accepted")
	}
	if sameStringMap(legend, map[string]string{"0": "none"}) {
		t.Error("a shortened legend was accepted")
	}
	// An absent legend and an empty one are the same, which is what a choice
	// records.
	if !sameStringMap(nil, map[string]string{}) {
		t.Error("an absent legend was not equal to an empty one")
	}
	if !sameOptionalString(stringPointer("a"), stringPointer("a")) {
		t.Error("equal optional strings were rejected")
	}
	if sameOptionalString(stringPointer("a"), stringPointer("b")) ||
		sameOptionalString(stringPointer("a"), nil) {
		t.Error("a changed optional string was accepted")
	}
	if !sameJSON(t, "test", float64(1.5), float64(1.5)) {
		t.Error("equal numbers were rejected")
	}
	if sameJSON(t, "test", float64(1.5), float64(1.6)) {
		t.Error("a changed number was accepted")
	}
	if sameJSON(t, "test", "", nil) {
		t.Error("an empty string was accepted in place of null")
	}
	if !sameJSON(t, "test", map[string]any{"a": []any{float64(1)}},
		map[string]any{"a": []any{float64(1)}}) {
		t.Error("equal nested values were rejected")
	}
}

// sameStringMap compares two string maps, treating an absent and an empty one as
// the same, which is how the recorded side writes a legend a choice has none of.
func sameStringMap(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
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
