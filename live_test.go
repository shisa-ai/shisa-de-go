package shisade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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
// The set includes an image question, so the gate covers the one read that goes
// to a different endpoint path; the recorder measures whether the endpoint will
// serve an image at all, and a refusal that names images skips that case with the
// server's own answer as the reason rather than failing on a capability the rest
// of the gate does not use. A probe that failed for any other reason stops the
// recording, so a deployment fact is never inferred from a bug or a bad minute.
// `TestLiveImageReadUsesTheImage` is the check the parity comparison cannot make,
// because agreement on an answer about nothing is still agreement: it asks two
// images that differ only in colour the same question and requires the answer to
// follow. The rest are the DE-2 half of the Python `tests/test_live.py`; the DE-1
// case is absent because this port has no DE-1 read. `docs/PORT.md` records both.
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
	// Policy names a whole read for this question, and Probability asks for the
	// one-read view, so the set covers more than the family's default read.
	Policy      *string `json:"policy"`
	Probability bool    `json:"probability"`
	// Image makes the question an image question. It is a data URL so both clients
	// send the same bytes and neither needs a file the other can see.
	Image string `json:"image"`
}

// call is the per-call settings this question asks for.
func (q liveQuestion) call() CallOptions {
	return CallOptions{
		Probability: q.Probability,
		Image:       q.Image,
		Overrides:   OptionOverrides{Policy: q.Policy},
	}
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

// liveDrift is how far the Python client's own passes were apart.
type liveDrift struct {
	MaxProbabilityDelta float64  `json:"max_probability_delta"`
	ValueDelta          *float64 `json:"value_delta"`
	SameValue           bool     `json:"same_value"`
	SameStrategy        bool     `json:"same_strategy"`
	SameRequests        bool     `json:"same_requests"`
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
	ThoughtClosed  *bool              `json:"thought_closed"`
	MissingFromTop []string           `json:"missing_from_top"`
	// SystemRender is recorded for an image question only: whether the server
	// rendered the system turn as the string form or with the space vLLM's
	// content-part template writes. The text read has no such field, so a text
	// question leaves it empty and nothing compares it.
	SystemRender string `json:"system_render"`
}

// livePythonMeta is the provenance written beside it.
type livePythonMeta struct {
	Endpoint           string `json:"endpoint"`
	Model              string `json:"model"`
	ServingFingerprint string `json:"serving_fingerprint"`
	Recorded           string `json:"recorded"`
	PythonRepo         string `json:"python_repo"`
	ReadoutVersion     string `json:"readout_version"`
	TokenizerRevision  string `json:"tokenizer_revision"`
	Questions          int    `json:"questions"`
	Passes             int    `json:"passes"`
	Requests           int    `json:"requests"`
	// Images is whether the endpoint answered an image request, and ImagesReason
	// why not when it did not. The recorder leaves the image questions out when it
	// is false, so the Go side skips the same case for the same measured reason
	// rather than failing on a capability the text gate does not use.
	Images       bool     `json:"images"`
	ImagesReason string   `json:"images_reason"`
	SelfDelta    float64  `json:"self_delta"`
	Unstable     []string `json:"unstable"`
	SHA256       string   `json:"sha256"`
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
	if _, err := os.Stat(filepath.Join(repo, "shisa_de")); err != nil {
		t.Skipf("no shisa_de package under %s: %v", repo, err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 to record with")
	}

	// The recorder ships in this repository, so it is found from the package
	// directory `go test` runs in. Deriving it from the Python checkout's
	// neighbourhood would turn a differently-named or relocated Go checkout into
	// a silent skip of the milestone's gate. Its absence is a broken checkout,
	// and the operator asked for the live suite by name.
	script := filepath.Join("scripts", "live_parity.py")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("the recorder is not at %s: %v", script, err)
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "live_python.json")
	command := exec.Command("python3", script, "--out", out, "--quiet")
	command.Env = append(os.Environ(), "SHISA_DE_PYTHON_REPO="+repo)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		// Exit status 3 is the recorder saying "the output is written, but my own
		// passes disagreed on the chosen value". That is a verdict about the
		// server and it belongs to the reporting below, not to the exec layer,
		// which is what the message at the top of the test describes.
		if !liveRecorderExitIsUnstable(err) {
			t.Fatalf("recording the Python answers: %v\n%s", err, stderr.String())
		}
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
	// The digest ties the answers to the provenance. Without it a stale
	// `.meta.json` beside a fresh `.json` would be reported as one measurement.
	if digest := sha256.Sum256(raw); hex.EncodeToString(digest[:]) != meta.SHA256 {
		t.Fatalf("the recording is not the one its provenance describes: sha256 %x, meta says %s",
			digest, meta.SHA256)
	}
	t.Logf("the Python client: %d passes, %d requests, fingerprint %s, self-delta %.3g",
		meta.Passes, meta.Requests, meta.ServingFingerprint, meta.SelfDelta)
	t.Logf("recorded %s from %s at %s, tokenizer %q",
		meta.ReadoutVersion, meta.PythonRepo, meta.Recorded, meta.TokenizerRevision)
	return recorded, meta
}

// liveTolerance is how far two probabilities may differ.
//
// The read is `temperature: 0` against a deterministic server, so the two
// clients should agree exactly; the tolerance is for the float arithmetic on the
// way to the softmax, not for the server changing its mind. The measured delta
// is 1e-16 to 1e-19, so this leaves three orders of magnitude of room and no
// more. It is also what a chosen numeric value is compared with, in
// `liveSameValue`. A chosen value that differs is reported as itself rather than
// hidden behind this.
const liveTolerance = 1e-9

// liveSumTolerance is how far a distribution may be from summing to one. It is a
// different quantity from `liveTolerance`: summing 256 probabilities of a few
// parts in 10^4 accumulates a relative error near 256*eps, and it is this
// client's own arithmetic rather than a comparison against the recording.
const liveSumTolerance = 1e-9

// liveRecorderExitIsUnstable reports whether a recorder exit status means
// "the recording is written, but the recorder's own passes disagreed on the
// chosen value" rather than "there is no recording".
//
// The two are different verdicts: the first is a statement about the served
// checkpoint, which the test reports as inconclusive; the second is a broken
// harness. Treating the first as the second made the inconclusive reporting
// unreachable, so the classification is its own function with its own test.
func liveRecorderExitIsUnstable(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 3
}

// liveSameEndpoint applies the client's own endpoint normalization to both
// sides, so `http://host/v1/` and `http://host` are the same endpoint.
func liveSameEndpoint(a, b string) bool {
	normalize := func(value string) string {
		value = strings.TrimRight(value, "/")
		return strings.TrimSuffix(value, "/v1")
	}
	return normalize(a) == normalize(b)
}

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
	// The recorder leaves the image questions out when the endpoint will not answer
	// an image request, so the count it should have recorded depends on that.
	asked := 0
	for _, question := range set.Questions {
		if question.Image != "" && !meta.Images {
			continue
		}
		asked++
	}
	if meta.Questions != asked {
		t.Fatalf("the recording holds %d questions and the set asks for %d of them",
			meta.Questions, asked)
	}
	if !meta.Images && meta.ImagesReason == "" {
		t.Fatal("the recording left the image questions out without saying why")
	}
	if meta.Images && meta.ImagesReason != "" {
		t.Fatalf("the endpoint served an image and the recording also gives a reason it could not: %s",
			meta.ImagesReason)
	}
	if meta.Model != liveModelID() {
		t.Fatalf("the recording is of %s and the suite is pointed at %s", meta.Model, liveModelID())
	}
	// The gate's whole premise is one endpoint for both halves. The recorder
	// resolves `SHISA_DE_ENDPOINT` the way this side does, so a mismatch here
	// means it stopped doing that -- two clients against two servers is the one
	// comparison the design exists to prevent, and nothing below could then be a
	// port difference.
	wantEndpoint := os.Getenv(EndpointEnv)
	if wantEndpoint == "" {
		wantEndpoint = DefaultEndpoint
	}
	if !liveSameEndpoint(wantEndpoint, meta.Endpoint) {
		t.Fatalf("the recording is of %s and this suite is talking to %s; the gate needs "+
			"one endpoint for both clients", meta.Endpoint, wantEndpoint)
	}
	if meta.SelfDelta > liveTolerance {
		t.Fatalf("the Python client's own passes differ by %.3g, beyond the %.0e tolerance: "+
			"the served checkpoint is not repeatable right now, so this run cannot tell a "+
			"port difference from the server's. Re-run, or compare the recorded exchanges "+
			"offline with TestClientParity.", meta.SelfDelta, liveTolerance)
	}
	if len(meta.Unstable) != 0 {
		t.Fatalf("the Python client's own passes disagree for %v: "+
			"the served checkpoint is not repeatable right now, so this run cannot tell a "+
			"port difference from the server's. Re-run, or compare the recorded exchanges "+
			"offline with TestClientParity.", meta.Unstable)
	}

	model, recorder := liveModel(t, OptionOverrides{})
	if version := model.ReadoutVersion; version != meta.ReadoutVersion {
		t.Fatalf("the readout is %s and the recording is %s", version, meta.ReadoutVersion)
	}

	// Every response this test reads is checked against the recording's serving
	// shape, including the ones a gate client makes: a shape that moved only on
	// the thinking path would otherwise go unnoticed.
	recorders := []*liveRecorder{recorder}

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
			eager, eagerRecorder := liveModel(t, overrides)
			gates[gate] = eager
			recorders = append(recorders, eagerRecorder)
		}
	}

	for _, question := range set.Questions {
		t.Run(question.ID, func(t *testing.T) {
			if question.Image != "" && !meta.Images {
				// The endpoint refused an image request. That is a property of the
				// deployment, not of the port, and the reason is the server's own
				// answer: the recorder aborts rather than recording a skip when the
				// capability could not be measured at all.
				t.Skipf("the endpoint refused an image request: %s", meta.ImagesReason)
			}
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
				map[string]Question{question.Head: asked}, question.call())
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			got, ok := decision.Answers[question.Head]
			if !ok {
				t.Fatalf("no answer for %s", question.Head)
			}
			compareLiveAnswer(t, question.Image != "", got, want, decision.Raw[question.Head])
		})
	}

	// The serving shape is checked after the questions, so a parity failure is
	// reported as one rather than as a moved endpoint.
	for _, seen := range recorders {
		if fingerprint := seen.fingerprint(t); fingerprint != meta.ServingFingerprint {
			t.Errorf("the endpoint is serving %s and the Python client saw %s in this same "+
				"session; the shape moved mid-run", fingerprint, meta.ServingFingerprint)
		}
	}
}

// liveSystemRenderProblem is the image read's one judgement compared against the
// recording, as a string rather than an error so it can be tested directly.
//
// `isImage` comes from the question set, not from the recording, because an empty
// recorded verdict means "nothing was recorded" and not "this was a text
// question". Inferring image-ness from the recording made a missing verdict pass
// the comparison on both sides, which is the failure the comparison exists to
// catch. A text question has no such key and none is expected; an image question
// must carry one of the three verdicts on both sides.
func liveSystemRenderProblem(isImage bool, want string, raw map[string]any) string {
	rendered, present := raw["system_render"].(string)
	if !isImage {
		if want != "" || present {
			return fmt.Sprintf("a text question recorded a system render (%q in the recording, "+
				"%q in the answer); only the image read renders the system turn", want, rendered)
		}
		return ""
	}
	switch want {
	case "string", "differs", "unverified":
	default:
		return fmt.Sprintf("the recording carries no system render for an image question (want %q); "+
			"the comparison would otherwise pass against nothing", want)
	}
	if !present {
		return fmt.Sprintf("the recording says the server rendered the system turn as %q "+
			"and the answer carries no system_render at all", want)
	}
	if rendered != want {
		return fmt.Sprintf("the server rendered the system turn as %q and the recording is "+
			"%q; the image read answers either way, so this is the judgement rather than "+
			"the verdict", rendered, want)
	}
	return ""
}

// compareLiveAnswer requires one answer to be the recorded one.
//
// `raw` is the head's entry in `Decision.Raw`, which is where the image read's
// `system_render` verdict lives; a text answer has no such key and none is
// expected.
func compareLiveAnswer(t *testing.T, isImage bool, got Answer, want livePythonAnswer, raw map[string]any) {
	t.Helper()

	if problem := liveSystemRenderProblem(isImage, want.SystemRender, raw); problem != "" {
		t.Error(problem)
	}
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
	// The temperature is the direct signal for "the same per-read fit was
	// selected". It is not implied by the distribution: on a peaked answer two
	// temperatures can produce the same probabilities, so a record-selection bug
	// can hide inside the probability tolerance. The JSON round trip preserves
	// the float, so this is compared exactly.
	if got.Temperature != want.Temperature {
		t.Errorf("the temperature is %v and the recording is %v", got.Temperature, want.Temperature)
	}
	if (got.Confidence == nil) != (want.Confidence == nil) ||
		(got.Confidence != nil && math.Abs(*got.Confidence-*want.Confidence) > liveTolerance) {
		t.Errorf("the confidence is %v and the recording is %v", got.Confidence, want.Confidence)
	}
	if got.Requests != want.Requests {
		t.Errorf("the answer cost %d requests and the recording cost %d", got.Requests, want.Requests)
	}
	if got.ThoughtTokens != want.ThoughtTokens {
		t.Errorf("the thought is %d tokens and the recording is %d", got.ThoughtTokens, want.ThoughtTokens)
	}
	if (got.ThoughtClosed == nil) != (want.ThoughtClosed == nil) ||
		(got.ThoughtClosed != nil && *got.ThoughtClosed != *want.ThoughtClosed) {
		t.Errorf("the thought closure is %v and the recording is %v",
			got.ThoughtClosed, want.ThoughtClosed)
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
	if math.Abs(total-1) > liveSumTolerance {
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
// The DE-2 half of the Python `tests/test_live.py`. The DE-1 overflow case and
// the model card's 161-token example are absent because this port has no DE-1
// read; the Python suite's image case is ported twice, as the gate's `image-color`
// question and as `TestLiveImageReadUsesTheImage` above. `docs/PORT.md` records
// both.

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

// TestLiveHealthProbeServesARead is `test_health_probe_serves_a_read`.
func TestLiveHealthProbeServesARead(t *testing.T) {
	model, _ := liveModel(t, OptionOverrides{})
	report := model.Health(context.Background(), true)
	probe, _ := report["read_probe"].(string)
	if !strings.HasPrefix(probe, "passed") {
		t.Errorf("the probe is %q: %v", probe, report)
	}
	// The chat probe runs whenever `--probe` does, and it reports how the server
	// renders the system turn: `string` when the server's prompt tokens match the
	// local render through the system line, `differs` when they do not.
	// `unverified` is a failure here, as it is in the Python suite's
	// `test_health_probe_serves_a_read`, which accepts the same two values: the
	// probe asks a text question, so a server that returns fewer prompt positions
	// than the local render needs is reporting something the text path cannot use
	// either.
	switch report["chat_system_render"] {
	case ChatSystemString, ChatSystemDiffers:
	default:
		t.Errorf("the chat probe reports %v: %v", report["chat_system_render"], report)
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
			if math.Abs(total-1) > liveSumTolerance {
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
			// The wire shape of a choice answer is exactly these four keys, which is
			// the set the Python case asserts. A count alone would pass an answer
			// that dropped one and added another.
			wire := answer.ToWire()
			for _, key := range []string{"type", "choice", "probabilities", "confidence"} {
				if _, ok := wire[key]; !ok {
					t.Errorf("the wire answer has no %q: %v", key, wire)
				}
			}
			if len(wire) != 4 {
				t.Errorf("the wire answer is %v, want exactly the four choice keys", wire)
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
	// The record carries a fit for the thinking read (`think:choice`), so a
	// `repeat2-think` answer against a checkpoint the record covers is calibrated
	// at that fit. The expectation is read from the record rather than written as
	// a literal, because the suite can be pointed at another served checkpoint:
	// one the shipped record does not cover answers uncalibrated at 1.0, which is
	// what the Python suite's own `calibrated is False` asserts.
	covered := len(model.Calibration.Reads) != 0
	wantCalibrated := model.CalibrationApplied && covered
	wantTemperature := 1.0
	if wantCalibrated {
		wantTemperature = model.Calibration.TemperatureFor("choice", "think")
	}
	if answer.Calibrated != wantCalibrated {
		t.Errorf("calibrated is %v and this checkpoint's record gives %v",
			answer.Calibrated, wantCalibrated)
	}
	if answer.Temperature != wantTemperature {
		t.Errorf("the temperature is %v and the record's thinking fit is %v",
			answer.Temperature, wantTemperature)
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

// TestLiveImageReadUsesTheImage is the check the parity gate cannot make: that
// the endpoint answered from the pixels rather than accepting the image content
// part and ignoring it.
//
// The gate asks one image question and compares the two clients against each
// other, so a server that discarded the image would produce agreement on an
// answer about nothing. This asks the same question of two images that differ
// only in colour and requires the answer to follow, which is the Python live
// suite's own image test. It is the one live check whose failure means the image
// read is not reading.
func TestLiveImageReadUsesTheImage(t *testing.T) {
	liveSkip(t)
	model, _ := liveModel(t, OptionOverrides{})

	for _, testCase := range []struct {
		want string
		rgb  color.RGBA
	}{
		{"red", color.RGBA{R: 0xff, A: 0xff}},
		{"blue", color.RGBA{B: 0xff, A: 0xff}},
	} {
		t.Run(testCase.want, func(t *testing.T) {
			image := solidPNG(t, testCase.rgb)
			decision, err := model.Classify(context.Background(), nil,
				map[string]HeadSpec{
					"color": {Options: []Option{{Key: "red"}, {Key: "blue"}},
						Instructions: "What is the dominant color in this image?"},
				},
				ClassifyOptions{
					CallOptions:          CallOptions{Image: image},
					IncludeProbabilities: true,
				})
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			answer, ok := decision.Answers["color"]
			if !ok {
				t.Fatal("no answer for the color head")
			}
			if got := answer.Value(); got != testCase.want {
				t.Errorf("the image is %s and the answer is %v; the read is not using the image",
					testCase.want, got)
			}
			// An image answer is never calibrated in either client, and one request
			// is what the read costs. Both are cheap ways to catch a path that fell
			// through to the text read.
			if answer.Calibrated {
				t.Error("an image answer is calibrated; the image path selects no record")
			}
			if answer.Requests != 1 {
				t.Errorf("the read cost %d requests, want 1", answer.Requests)
			}
			if got := decision.Meta["input_type"]; got != "image" {
				t.Errorf("the report says the input was %v, want image", got)
			}
		})
	}
}

// solidPNG is a 32x32 image of one colour, built in memory so the test needs no
// file and the two colours differ in nothing else.
func solidPNG(t *testing.T, rgba color.RGBA) string {
	t.Helper()
	frame := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for x := 0; x < 32; x++ {
		for y := 0; y < 32; y++ {
			frame.SetRGBA(x, y, rgba)
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, frame); err != nil {
		t.Fatalf("encoding the %v image: %v", rgba, err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
}

// -- the harness's own logic, offline -----------------------------------------
//
// These do not spend a request and do not need `SHISA_DE_LIVE=1`. They pin the
// parts of the gate that decide whether a run means anything, which is where the
// two ways the gate can quietly stop being a gate live: an exit status that
// discards a recording, and two endpoints compared as if they were one.

// TestLiveRecorderExitClassification is the branch that decides whether a
// recorder run whose own passes disagreed is a usable recording or a failure.
// It was unreachable when any non-zero exit was a plain error, which made the
// "the server moved, so this run is inconclusive" reporting dead code. Exit 4 is
// the probe's "the vision capability could not be measured", which is a failure
// like any other rather than an unstable recording.
func TestLiveRecorderExitClassification(t *testing.T) {
	for _, testCase := range []struct {
		status   int
		unstable bool
	}{{0, false}, {1, false}, {2, false}, {3, true}, {4, false}} {
		t.Run(fmt.Sprint(testCase.status), func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=TestLiveRecorderExitHelper")
			command.Env = append(os.Environ(), "SHISA_DE_RECORDER_EXIT="+fmt.Sprint(testCase.status))
			err := command.Run()
			if got := liveRecorderExitIsUnstable(err); got != testCase.unstable {
				t.Errorf("exit %d is classified unstable=%v, want %v (err %v)",
					testCase.status, got, testCase.unstable, err)
			}
		})
	}
}

// TestLiveSystemRenderComparison pins the image read's one judgement against the
// recording, including the two cases that would otherwise pass: a recorded
// verdict and an answer that carries none, and an image question whose recording
// carries none.
func TestLiveSystemRenderComparison(t *testing.T) {
	for _, tc := range []struct {
		name    string
		isImage bool
		want    string
		raw     map[string]any
		problem bool
	}{
		{"a text answer has nothing to compare", false, "",
			map[string]any{"probabilities": map[string]any{}}, false},
		{"a text answer that recorded a verdict", false, "differs",
			map[string]any{"system_render": "differs"}, true},
		{"the same verdict", true, "differs",
			map[string]any{"system_render": "differs"}, false},
		{"the string form", true, "string",
			map[string]any{"system_render": "string"}, false},
		{"a different verdict", true, "differs",
			map[string]any{"system_render": "string"}, true},
		{"no verdict recorded at all", true, "differs",
			map[string]any{"probabilities": map[string]any{}}, true},
		{"a null verdict in the recording", true, "",
			map[string]any{"system_render": "differs"}, true},
		{"an unreadable verdict in the recording", true, "something",
			map[string]any{"system_render": "something"}, true},
		{"a verdict that is not a string", true, "differs",
			map[string]any{"system_render": 7}, true},
		{"a nil raw entry", true, "differs", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problem := liveSystemRenderProblem(tc.isImage, tc.want, tc.raw)
			if tc.problem && problem == "" {
				t.Errorf("want %q against %v reported no problem", tc.want, tc.raw)
			}
			if !tc.problem && problem != "" {
				t.Errorf("want %q against %v reported %s", tc.want, tc.raw, problem)
			}
		})
	}
}

// TestLiveQuestionSetHasAnImageCase keeps the live gate covering the one read
// that needs a different endpoint path. Without it nothing in the live suite
// ever sends an image, and the chat request would be compared against nothing.
func TestLiveQuestionSetHasAnImageCase(t *testing.T) {
	set := loadLiveQuestions(t)
	images := 0
	for _, question := range set.Questions {
		if question.Image == "" {
			continue
		}
		images++
		if !strings.HasPrefix(question.Image, "data:image/png;base64,") {
			t.Errorf("%s carries an image that is not a PNG data URL: %.40s...",
				question.ID, question.Image)
		}
		// A data URL, so the two clients send the same bytes with no file either
		// one has to see, and its own call is what selects the image read.
		if call := question.call(); call.Image != question.Image {
			t.Errorf("%s does not put its image on the call", question.ID)
		}
	}
	if images == 0 {
		t.Fatal("no question in the live set carries an image")
	}
}

// TestLiveRecorderExitHelper exits with the status its parent asked for. It is
// not a test of its own; the child's `-test.run` selects it by name.
func TestLiveRecorderExitHelper(t *testing.T) {
	status, err := strconv.Atoi(os.Getenv("SHISA_DE_RECORDER_EXIT"))
	if err != nil {
		return
	}
	os.Exit(status)
}

// TestLiveSameEndpoint checks the normalization the gate's endpoint comparison
// uses, which is the client's own: a trailing slash and a `/v1` suffix are the
// same endpoint, and a different host or port is not.
func TestLiveSameEndpoint(t *testing.T) {
	for _, testCase := range []struct {
		left, right string
		same        bool
	}{
		{"https://api.shisa.ai/openai", "https://api.shisa.ai/openai", true},
		{"https://api.shisa.ai/openai/", "https://api.shisa.ai/openai", true},
		{"https://api.shisa.ai/openai/v1", "https://api.shisa.ai/openai", true},
		{"https://api.shisa.ai/openai/v1/", "https://api.shisa.ai/openai/", true},
		{"http://127.0.0.1:8021", "https://api.shisa.ai/openai", false},
		{"http://127.0.0.1:8021", "http://127.0.0.1:8022", false},
		// Not the same endpoint just because the paths line up.
		{"https://api.shisa.ai/openai", "https://api.shisa.ai/other", false},
	} {
		if got := liveSameEndpoint(testCase.left, testCase.right); got != testCase.same {
			t.Errorf("liveSameEndpoint(%q, %q) is %v, want %v",
				testCase.left, testCase.right, got, testCase.same)
		}
	}
}

// TestLiveRecorderRefusesASinglePass checks that the recorder will not report a
// self-delta it never measured. One pass has no pair to compare, and the
// script's whole contribution to the gate is that comparison.
func TestLiveRecorderRefusesASinglePass(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 to run the recorder with")
	}
	if _, err := os.Stat(filepath.Join("scripts", "live_parity.py")); err != nil {
		t.Fatalf("the recorder is not at scripts/live_parity.py: %v", err)
	}
	// No key and no network: the argument check runs before either is needed.
	command := exec.Command("python3", filepath.Join("scripts", "live_parity.py"),
		"--passes", "1", "--out", filepath.Join(t.TempDir(), "unused.json"))
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		t.Fatal("the recorder accepted --passes 1, so its self-delta would be unmeasured")
	}
	if !strings.Contains(stderr.String(), "--passes must be at least 2") {
		t.Errorf("the refusal does not say why: %q", stderr.String())
	}
}
