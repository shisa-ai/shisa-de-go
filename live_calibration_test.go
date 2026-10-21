package shisade

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// What a live answer's calibration should be, derived independently of the
// client's own selection.
//
// The live suite can be pointed at an endpoint whose responses name a different
// checkpoint than the configured model id does -- a gateway holding an alias is
// the hosted default -- and the client tempers with the record for the id the
// response named. A check that reads its expectation out of `DecisionModel`
// would then compare the configured record against the served one and report a
// correct answer as a failure, or (worse) agree with a client that picked the
// wrong record.
//
// So the expectation is rebuilt here from the answer's own identity, by the rule
// README and `docs/PORT.md` state: the record of the checkpoint the response
// named; the configured identity only when the response named nothing or echoed
// the request; and no automatic record at all when the response named a
// checkpoint this package has no record for. Which per-read fit that record
// contributes is `docs/READOUT-DE2.md` section 7. `servedRecord` implements the
// same rule in `client.go`; this file is the independent statement of it, and a
// disagreement between the two is the finding.
//
// A record the caller supplied is not part of that rule -- it is the caller's
// assertion and is never replaced -- so it is handled first and separately.

// liveIdentitySource is where a live answer's checkpoint identity came from.
type liveIdentitySource string

const (
	// liveIdentityServed is an id the response reported that differs from the
	// request. It is the only evidence about the weights a name cannot override.
	liveIdentityServed liveIdentitySource = "served"
	// liveIdentityModel is the configured model id, which decides when the
	// response reported nothing or echoed the request.
	liveIdentityModel liveIdentitySource = "model"
	// liveIdentityTokenizer is the configured tokenizer source, consulted only
	// when the configured model id names no released checkpoint.
	liveIdentityTokenizer liveIdentitySource = "tokenizer"
	// liveIdentitySupplied is a record the caller passed. It is not an identity
	// the response can overrule.
	liveIdentitySupplied liveIdentitySource = "supplied"
)

// liveAnswerIdentity is the checkpoint an answer's weights came from, and which
// name said so.
func liveAnswerIdentity(model *DecisionModel, answer Answer) (string, liveIdentitySource) {
	if served := answer.ServedModel; served != "" && served != model.Model &&
		(model.Readout == nil || served != model.Readout.model) {
		return ReleasedCheckpoint(served), liveIdentityServed
	}
	// Nothing was reported, or the server echoed the request: the response says
	// nothing about which weights answered, so the configured identity decides.
	if checkpoint := ReleasedCheckpoint(model.Model); checkpoint != "" {
		return checkpoint, liveIdentityModel
	}
	if checkpoint := ReleasedCheckpoint(os.Getenv("SHISA_DE_TOKENIZER")); checkpoint != "" {
		return checkpoint, liveIdentityTokenizer
	}
	return "", liveIdentityModel
}

// liveExpectation is what the client should have done with this answer's
// calibration: which record describes the weights, whether it applies, and why
// not when it does not.
type liveExpectation struct {
	// Checkpoint is the released checkpoint whose weights answered, or "" when
	// no released checkpoint was named.
	Checkpoint string
	// Source says which name decided that.
	Source liveIdentitySource
	// Record is the shipped record for Checkpoint, or the supplied one. It is
	// the zero Calibration when there is none.
	Record Calibration
	// Applied is whether that record describes these weights under this readout,
	// before the per-read fit is considered.
	Applied bool
	// Reason says why Applied is false.
	Reason string
}

// liveExpectedCalibration derives the expectation from the answer's identity.
func liveExpectedCalibration(model *DecisionModel, answer Answer) liveExpectation {
	if model.calibrationSupplied {
		expectation := liveExpectation{
			Checkpoint: ReleasedCheckpoint(model.Calibration.Model),
			Source:     liveIdentitySupplied,
			Record:     model.Calibration,
			Applied:    model.Calibration.Fitted(),
		}
		if !expectation.Applied {
			expectation.Reason = "the supplied record holds no fitted temperature"
		}
		return expectation
	}
	checkpoint, source := liveAnswerIdentity(model, answer)
	expectation := liveExpectation{Checkpoint: checkpoint, Source: source}
	if checkpoint == "" {
		if source == liveIdentityServed {
			expectation.Reason = fmt.Sprintf(
				"the server reported %s, which names no released checkpoint",
				pythonQuote(answer.ServedModel))
		} else {
			expectation.Reason = fmt.Sprintf(
				"%s names no released checkpoint and neither does the tokenizer source",
				pythonQuote(model.Model))
		}
		return expectation
	}
	record, err := CalibrationForCheckpoint(model.Family, checkpoint, "")
	if err != nil {
		expectation.Reason = fmt.Sprintf("loading the record for %s: %v", checkpoint, err)
		return expectation
	}
	expectation.Record = record
	switch {
	case record.Model != checkpoint:
		expectation.Reason = fmt.Sprintf(
			"%s has no shipped record of its own; the family record is fitted on %s",
			checkpoint, record.Model)
	case !record.Fitted():
		expectation.Reason = fmt.Sprintf("the record for %s holds no fitted temperature", checkpoint)
	case !CalibrationCompatibleWith(record.ReadoutVersion, model.ReadoutVersion):
		expectation.Reason = fmt.Sprintf(
			"the record for %s was fitted against readout %s, which %s does not reproduce",
			checkpoint, record.ReadoutVersion, model.ReadoutVersion)
	default:
		expectation.Applied = true
	}
	return expectation
}

// liveAnswerRead is the record's name for the read an answer came from.
//
// A strategy names the whole policy (`repeat2-think`) while the record keys the
// read that produced the answer, which is the policy's last stage (`think`). The
// image read has its own name and no fit in either shipped record, so an image
// answer stays raw.
func liveAnswerRead(isImage bool, answer Answer) string {
	if isImage {
		return "image"
	}
	if answer.Strategy == StrategyRepeatThink {
		return "think"
	}
	return answer.Strategy
}

// liveAssertCalibration requires one answer's calibration to be what its own
// identity says it should be, and returns the record it expected.
//
// The three fields are one statement: `Calibrated`, the `Temperature` that
// produced it, and the `Calibration` that names the record it came from. A
// client that tempered with one record and named another would pass a check on
// any one of them alone.
func liveAssertCalibration(t *testing.T, model *DecisionModel, answer Answer, read string) liveExpectation {
	t.Helper()
	expectation := liveExpectedCalibration(model, answer)
	want := 1.0
	covered := read != "think" || len(expectation.Record.Reads) != 0
	if expectation.Applied && covered {
		want = expectation.Record.TemperatureFor(answer.Type, read)
	}
	// `answerFromRead` reports a temperature of exactly 1.0 as not applied: a
	// record with no fit for this read leaves the answer raw, and the identifier
	// is the record that tempered it, so there is none.
	wantID := ""
	if want != 1.0 {
		wantID = expectation.Record.ID()
	}
	if answer.Temperature != want {
		t.Errorf("the answer was tempered at %v and the %s identity gives %v: %s",
			answer.Temperature, expectation.Source, want, expectation.describe())
	}
	if answer.Calibrated != (wantID != "") {
		t.Errorf("calibrated is %v and the %s identity gives %v: %s",
			answer.Calibrated, expectation.Source, wantID != "", expectation.describe())
	}
	if answer.Calibration != wantID {
		t.Errorf("the answer names calibration %q and the %s identity gives %q: %s",
			answer.Calibration, expectation.Source, wantID, expectation.describe())
	}
	return expectation
}

// describe is the expectation as one line for a failure message.
func (e liveExpectation) describe() string {
	if e.Applied {
		return fmt.Sprintf("applied %s (%s)", e.Record.ID(), e.Checkpoint)
	}
	return fmt.Sprintf("no record applies (%s): %s", e.Source, e.Reason)
}

// -- offline coverage ---------------------------------------------------------

// TestLiveCalibrationIdentity pins the rule the live checks derive their
// expectation from, without an endpoint.
//
// Every case here is a configuration the suite can be pointed at: the released
// checkpoint itself, a gateway that answers its alias with another checkpoint's
// weights, a server that echoes the request, one that names a checkpoint this
// package has no record for, and a caller-supplied record. The last is the only
// one where the response cannot move the choice.
func TestLiveCalibrationIdentity(t *testing.T) {
	de2, err := CalibrationForCheckpoint(FamilyDE2, "shisa-ai/shisa-de-2", "")
	if err != nil {
		t.Fatalf("CalibrationForCheckpoint(de2): %v", err)
	}
	de21, err := CalibrationForCheckpoint(FamilyDE2, "shisa-ai/shisa-de-2.1", "")
	if err != nil {
		t.Fatalf("CalibrationForCheckpoint(de2.1): %v", err)
	}
	family, err := CalibrationFor(FamilyDE2)
	if err != nil {
		t.Fatalf("CalibrationFor(de2): %v", err)
	}
	client := func(model string, calibration Calibration, supplied bool) *DecisionModel {
		return &DecisionModel{
			Model:               model,
			Family:              FamilyDE2,
			ReadoutVersion:      DE2ReadoutVersion,
			Calibration:         calibration,
			calibrationSupplied: supplied,
		}
	}

	for _, testCase := range []struct {
		name       string
		model      *DecisionModel
		served     string
		checkpoint string
		source     liveIdentitySource
		record     Calibration
		applied    bool
	}{
		{
			// The ordinary case: the response names the checkpoint the id does.
			name:  "the served id and the configured id agree",
			model: client("shisa-ai/shisa-de-2", de2, false), served: "shisa-ai/shisa-de-2",
			checkpoint: "shisa-ai/shisa-de-2", source: liveIdentityModel,
			record: de2, applied: true,
		},
		{
			// The alias: a name cannot say which weights answered, the response can.
			name:  "an alias served by another checkpoint moves the record",
			model: client("shisa-ai/shisa-de-2", de2, false), served: "shisa-ai/shisa-de-2.1",
			checkpoint: "shisa-ai/shisa-de-2.1", source: liveIdentityServed,
			record: de21, applied: true,
		},
		{
			// A response that names nothing leaves the configured choice alone.
			name:  "silence leaves the configured record",
			model: client("shisa-ai/shisa-de-2", de2, false), served: "",
			checkpoint: "shisa-ai/shisa-de-2", source: liveIdentityModel,
			record: de2, applied: true,
		},
		{
			// A server that echoes the request reveals nothing, so the configured
			// identity decides; this is the alias the client cannot see through.
			name:  "an echo is not evidence",
			model: client("shisa-ai/shisa-de-2", de2, false), served: "shisa-ai/shisa-de-2",
			checkpoint: "shisa-ai/shisa-de-2", source: liveIdentityModel,
			record: de2, applied: true,
		},
		{
			// The case a record chosen from the request would get wrong: the
			// response says these are not DE-2's weights, and this package has no
			// record for them, so nothing may be applied.
			name:  "an uncovered served id applies nothing",
			model: client("shisa-ai/shisa-de-2", de2, false), served: "de2-v4-lr5e5-e3-s7",
			checkpoint: "", source: liveIdentityServed, applied: false,
		},
		{
			// The mirror: an uncovered configured id whose record is held, against a
			// response that names the checkpoint of that record.
			name:  "an uncovered configured id yields to the served one",
			model: client("de2-v4-lr5e5-e3-s7", family, false), served: "shisa-ai/shisa-de-2",
			checkpoint: "shisa-ai/shisa-de-2", source: liveIdentityServed,
			record: de2, applied: true,
		},
		{
			name:  "an uncovered configured id holds the family record",
			model: client("de2-v4-lr5e5-e3-s7", family, false), served: "",
			checkpoint: "", source: liveIdentityModel, applied: false,
		},
		{
			// A supplied record is the caller's assertion about the weights.
			name:  "a supplied record is never replaced",
			model: client("shisa-ai/shisa-de-2", de21, true), served: "shisa-ai/shisa-de-2",
			checkpoint: "shisa-ai/shisa-de-2.1", source: liveIdentitySupplied,
			record: de21, applied: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			expectation := liveExpectedCalibration(testCase.model,
				Answer{ServedModel: testCase.served})
			if expectation.Checkpoint != testCase.checkpoint {
				t.Errorf("the checkpoint is %q, want %q", expectation.Checkpoint, testCase.checkpoint)
			}
			if expectation.Source != testCase.source {
				t.Errorf("the identity came from %q, want %q", expectation.Source, testCase.source)
			}
			if expectation.Applied != testCase.applied {
				t.Errorf("applied is %v, want %v (%s)", expectation.Applied, testCase.applied,
					expectation.Reason)
			}
			if testCase.applied && expectation.Record.ID() != testCase.record.ID() {
				t.Errorf("the record is %q, want %q", expectation.Record.ID(), testCase.record.ID())
			}
			if !testCase.applied && expectation.Reason == "" {
				t.Error("an unapplied record was not explained")
			}
		})
	}
}

// TestLiveCalibrationRecordsDiffer is what makes the live check able to tell two
// records apart: the per-read fits are not shared between the shipped
// checkpoints. If they were, a wrong record would pass every live assertion.
func TestLiveCalibrationRecordsDiffer(t *testing.T) {
	de2, err := CalibrationForCheckpoint(FamilyDE2, "shisa-ai/shisa-de-2", "")
	if err != nil {
		t.Fatalf("CalibrationForCheckpoint(de2): %v", err)
	}
	de21, err := CalibrationForCheckpoint(FamilyDE2, "shisa-ai/shisa-de-2.1", "")
	if err != nil {
		t.Fatalf("CalibrationForCheckpoint(de2.1): %v", err)
	}
	for _, read := range []string{"direct", "repeat2", "think"} {
		for _, kind := range []string{"choice", "noul"} {
			one := de2.TemperatureFor(kind, read)
			other := de21.TemperatureFor(kind, read)
			if one == other {
				t.Errorf("%s/%s is %v in both records, so a live check cannot tell them apart",
					read, kind, one)
			}
		}
	}
}

// TestLiveAnswerRead pins the record's name for the read an answer came from.
func TestLiveAnswerRead(t *testing.T) {
	for _, testCase := range []struct {
		isImage  bool
		strategy string
		want     string
	}{
		{false, StrategyDirect, "direct"},
		{false, StrategyRepeat, "repeat2"},
		{false, StrategyRepeatThink, "think"},
		{true, StrategyDirect, "image"},
	} {
		got := liveAnswerRead(testCase.isImage, Answer{Strategy: testCase.strategy})
		if got != testCase.want {
			t.Errorf("liveAnswerRead(%v, %q) is %q, want %q",
				testCase.isImage, testCase.strategy, got, testCase.want)
		}
	}
	// An image answer is never calibrated: neither record holds a fit for that
	// read, and the image path selects no policy.
	de21, err := CalibrationForCheckpoint(FamilyDE2, "shisa-ai/shisa-de-2.1", "")
	if err != nil {
		t.Fatalf("CalibrationForCheckpoint(de2.1): %v", err)
	}
	if got := de21.TemperatureFor("choice", liveAnswerRead(true, Answer{})); got != 1.0 {
		t.Errorf("the image read is tempered at %v, want 1.0", got)
	}
}

// TestLiveCalibrationAssertionAcceptsWhatItDerived pins the other half: an answer
// built from the derived expectation passes the assertion.
func TestLiveCalibrationAssertionAcceptsWhatItDerived(t *testing.T) {
	model := &DecisionModel{Model: "shisa-ai/shisa-de-2", Family: FamilyDE2,
		ReadoutVersion: DE2ReadoutVersion}
	expectation := liveExpectedCalibration(model, Answer{ServedModel: "shisa-ai/shisa-de-2.1"})
	if !expectation.Applied {
		t.Fatalf("no record applies: %s", expectation.Reason)
	}
	temperature := expectation.Record.TemperatureFor("choice", "repeat2")
	answer := Answer{Type: "choice", Strategy: StrategyRepeat, Calibrated: true,
		Temperature: temperature, Calibration: expectation.Record.ID(),
		ServedModel: "shisa-ai/shisa-de-2.1"}
	if got := liveAssertCalibration(t, model, answer, "repeat2"); got.Record.ID() != expectation.Record.ID() {
		t.Errorf("the assertion derived %q, want %q", got.Record.ID(), expectation.Record.ID())
	}
}

// TestLiveParityRecord pins which record the gate hands both clients.
//
// It is the record for the checkpoint the configured identity names, because that
// is the one the clients would resolve for themselves; an uncovered id gets the
// family record, which both clients still apply as given. The gate does not need
// the served checkpoint here: it supplies the record precisely because the two
// clients choose differently, and the recording says which one it was given.
func TestLiveParityRecord(t *testing.T) {
	for _, testCase := range []struct {
		model string
		want  string
	}{
		{"shisa-ai/shisa-de-2.1", "shisa-ai/shisa-de-2.1"},
		{"shisa-ai/shisa-de-2", "shisa-ai/shisa-de-2"},
		{"shisa-ai/shisa-de-2.1-FP8-dynamic", "shisa-ai/shisa-de-2.1"},
		{"de2-v4-lr5e5-e3-s7", "shisa-ai/shisa-de-2"},
	} {
		t.Run(testCase.model, func(t *testing.T) {
			t.Setenv("SHISA_DE_MODEL", testCase.model)
			t.Setenv("SHISA_DE_TOKENIZER", "")
			record, path := liveParityRecord(t)
			if record.Model != testCase.want {
				t.Errorf("%s resolves to the record for %q, want %q",
					testCase.model, record.Model, testCase.want)
			}
			if !record.Fitted() {
				t.Errorf("the record for %s holds no fit, so the gate would compare raw answers",
					testCase.want)
			}
			// The file the recorder is pointed at has to be the one that was
			// loaded: the gate hashes those bytes and compares them with the
			// recording's digest.
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			fromFile, err := LoadCalibrationFile(path)
			if err != nil {
				t.Fatalf("LoadCalibrationFile(%s): %v", path, err)
			}
			if fromFile.ID() != record.ID() {
				t.Errorf("%s loads %q and the gate holds %q", path, fromFile.ID(), record.ID())
			}
			if len(raw) == 0 {
				t.Errorf("%s is empty", path)
			}
		})
	}
}

// TestLiveParityRecordIdentifierMatchesPython pins the one cross-language
// assumption the gate's shared record rests on: both clients spell a record's
// identifier the same way.
//
// The gate gives both clients one file and then compares the recording's
// identifier with the one this side built from the same bytes. If the two
// spellings drifted -- a float rendered differently, a key order that stopped
// being sorted -- the gate would fail on every run with a message about a
// different record rather than about the port. This catches that offline, against
// the Python client's own loader.
func TestLiveParityRecordIdentifierMatchesPython(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 to compare the identifier with")
	}
	repo := livePythonRepo()
	if _, err := os.Stat(filepath.Join(repo, "shisa_de")); err != nil {
		t.Skipf("no shisa_de package under %s: %v", repo, err)
	}
	const script = "import sys\n" +
		"sys.path.insert(0, sys.argv[1])\n" +
		"from shisa_de.calibration import load_calibration_file\n" +
		"print(load_calibration_file(sys.argv[2]).id)\n"
	for _, name := range []string{CalibrationFiles[FamilyDE2], CheckpointCalibrationFiles["shisa-ai/shisa-de-2.1"]} {
		t.Run(filepath.Base(name), func(t *testing.T) {
			path, err := filepath.Abs(name)
			if err != nil {
				t.Fatalf("resolving %s: %v", name, err)
			}
			record, err := LoadCalibrationFile(path)
			if err != nil {
				t.Fatalf("LoadCalibrationFile(%s): %v", path, err)
			}
			command := exec.Command("python3", "-c", script, repo, path)
			var stderr bytes.Buffer
			command.Stderr = &stderr
			output, err := command.Output()
			if err != nil {
				if strings.Contains(stderr.String(), "ModuleNotFoundError") {
					t.Skipf("the Python client cannot be imported here: %s",
						strings.TrimSpace(stderr.String()))
				}
				t.Fatalf("reading the record with the Python client: %v\n%s", err, stderr.String())
			}
			if got := strings.TrimSpace(string(output)); got != record.ID() {
				t.Errorf("the Python client reads %q and this one %q from %s", got, record.ID(), path)
			}
		})
	}
}

// liveCodeReadBody is the stub read body with the model id the response reports.
//
// The stub bodies carry no `model` field, which is what a server that reports
// nothing looks like. Injecting one is how the served-identity cases are reached
// without an endpoint.
func liveCodeReadBody(served string, logprobs map[string]float64) string {
	return strings.Replace(codeReadBody(logprobs),
		"{", fmt.Sprintf(`{"model":%q,`, served), 1)
}

// TestLiveCalibrationAgainstAServedId checks the derived rule against the client
// itself, through a stub server that names a checkpoint in its responses.
//
// The live suite cannot be run without an endpoint, so without this the rule in
// this file and `servedRecord` would only ever be compared against each other in
// production. These are the three cases the hosted gateway produces: a response
// naming another released checkpoint, a response naming one this package has no
// record for, and a response that echoes the request.
func TestLiveCalibrationAgainstAServedId(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		served     string
		checkpoint string
		calibrated bool
		record     string
	}{
		{
			// The alias: the request named DE-2 and the response says DE-2.1.
			name: "another released checkpoint", served: "shisa-ai/shisa-de-2.1",
			checkpoint: "shisa-ai/shisa-de-2.1", calibrated: true,
			record: "shisa-ai/shisa-de-2.1",
		},
		{
			// A checkpoint with no shipped record: nothing may be applied.
			name: "a checkpoint with no record", served: "de2-v4-lr5e5-e3-s7",
			checkpoint: "", calibrated: false,
		},
		{
			// The server echoes the request, so it reveals nothing and the
			// configured identity decides.
			name: "an echo", served: "shisa-ai/shisa-de-2",
			checkpoint: "shisa-ai/shisa-de-2", calibrated: true,
			record: "shisa-ai/shisa-de-2",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			model, _ := newStubModel(t, 2,
				liveCodeReadBody(testCase.served, map[string]float64{"A": -0.1, "B": -1.0}))
			decision, err := model.Decide(context.Background(), map[string]any{"e": "x"},
				map[string]Question{"urgent": NewNoul("Is it urgent?")}, CallOptions{})
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			answer := decision.Answers["urgent"]
			if answer.ServedModel != testCase.served {
				t.Fatalf("the answer reports %q and the server named %q",
					answer.ServedModel, testCase.served)
			}
			expectation := liveAssertCalibration(t, model, answer,
				liveAnswerRead(false, answer))
			if expectation.Checkpoint != testCase.checkpoint {
				t.Errorf("the identity is %q, want %q", expectation.Checkpoint, testCase.checkpoint)
			}
			if answer.Calibrated != testCase.calibrated {
				t.Errorf("calibrated is %v, want %v", answer.Calibrated, testCase.calibrated)
			}
			if testCase.record == "" {
				if answer.Calibration != "" {
					t.Errorf("the answer names %q and no record describes those weights",
						answer.Calibration)
				}
				return
			}
			if expectation.Record.Model != testCase.record {
				t.Errorf("the record is fitted on %q, want %q",
					expectation.Record.Model, testCase.record)
			}
		})
	}
}
