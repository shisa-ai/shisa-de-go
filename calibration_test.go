package shisade

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTemperBinary pins binary temperature scaling, including the two
// temperatures the Python client treats as "no fit".
func TestTemperBinary(t *testing.T) {
	cases := []struct {
		name        string
		p           float64
		temperature float64
		want        float64
	}{
		{"no fit at one", 0.3, 1.0, 0.3},
		{"no fit at zero", 0.3, 0.0, 0.3},
		{"no fit below zero", 0.3, -2.0, 0.3},
		{"a half stays a half", 0.5, 2.0, 0.5},
		// A temperature above 1 flattens: the fit corrects an overconfident
		// read, so 0.9 becomes 0.75.
		{"flattens toward the middle", 0.9, 2.0, 0.7500000000000001},
		// The 1e-12 floor keeps the arithmetic finite, so neither end is exact.
		{"one is floored, not divided by zero", 1.0, 2.0, 0.9999990000010001},
		{"zero is floored, not divided by zero", 0.0, 2.0, 9.99999000001e-07},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TemperBinary(tc.p, tc.temperature)
			if math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("TemperBinary(%v, %v) = %v, want %v", tc.p, tc.temperature, got, tc.want)
			}
		})
	}
	// The measured DE-2 repeat2 fit, on the recorded read.
	got := TemperBinary(0.9999988155306109, 1.89)
	if math.Abs(got-0.9992688929544757) > 1e-15 {
		t.Errorf("TemperBinary on the recorded read = %v, want 0.9992688929544757", got)
	}
	// A temperature of exactly 1 must return the input bit for bit, not a
	// recomputed value.
	if got := TemperBinary(0.1234567890123456789, 1); got != 0.1234567890123456789 {
		t.Errorf("a temperature of 1 changed the probability: %v", got)
	}
}

// TestTemperDistribution pins multiclass temperature scaling and the fact that
// it cannot move the argmax.
func TestTemperDistribution(t *testing.T) {
	raw := map[string]float64{"A": 0.9999988155306109, "B": 6.475946049178305e-07,
		"C": 5.368747842534138e-07}
	got := TemperDistribution(raw, 1.89)
	want := map[string]float64{"A": 0.9989880955110652, "B": 0.0005310285188485659,
		"C": 0.00048087597008630184}
	if !closeEnough(got, want, 1e-15) {
		t.Errorf("TemperDistribution = %v, want %v", got, want)
	}
	total := 0.0
	for _, value := range got {
		total += value
	}
	if math.Abs(total-1) > 1e-12 {
		t.Errorf("the tempered distribution sums to %v", total)
	}
	// A temperature of 1 returns a copy, not the same map.
	unchanged := TemperDistribution(raw, 1)
	if len(unchanged) != len(raw) {
		t.Fatalf("TemperDistribution with no fit changed the size: %v", unchanged)
	}
	unchanged["A"] = 0
	if raw["A"] == 0 {
		t.Error("TemperDistribution with no fit aliased the caller's map")
	}
	if _, ok := TemperDistribution(nil, 2.0)["A"]; ok {
		t.Error("TemperDistribution of an empty map invented a key")
	}
}

// TestConfidence pins the statistic and its clipping.
func TestConfidence(t *testing.T) {
	cases := []struct {
		name          string
		probabilities map[string]float64
		want          float64
	}{
		{"empty", map[string]float64{}, 0},
		{"one option", map[string]float64{"A": 1}, 1},
		{"even", map[string]float64{"A": 0.5, "B": 0.5}, 0},
		{"even four", map[string]float64{"A": .25, "B": .25, "C": .25, "D": .25}, 0},
		{"two of three", map[string]float64{"A": 0.9, "B": 0.1}, 0.8},
		{"all on one", map[string]float64{"A": 1, "B": 0}, 1},
		{"three of a kind", map[string]float64{"A": 0.1, "B": 0.45, "C": 0.45}, 0.17500000000000004},
		// The clip only matters for a distribution that does not sum to 1: a
		// normalized one always has a peak of at least 1/k. Python's
		// max(0.0, ...) is what keeps this from going negative.
		{"under even", map[string]float64{"A": 0.1, "B": 0.3, "C": 0.3}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Confidence(tc.probabilities); math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("Confidence(%v) = %v, want %v", tc.probabilities, got, tc.want)
			}
		})
	}
}

// TestShippedCalibration pins the record this repository ships. The id is on
// every answer, so a change to it is a change to what a caller can tell about
// which checkpoint a number beside it was fitted on.
func TestShippedCalibration(t *testing.T) {
	record, err := CalibrationFor(FamilyDE2)
	if err != nil {
		t.Fatalf("CalibrationFor(de2): %v", err)
	}
	want := "shisa-ai/shisa-de-2|de2-codebook-v1|vllm-0.30.0-merged-bf16-triton-tp1[" +
		"direct:choice=1.81,noul=2.69;repeat2:choice=1.89,noul=2.76;think:choice=6.8,noul=6.8]"
	if got := record.ID(); got != want {
		t.Errorf("the shipped record's id is\n  %s\nwant\n  %s", got, want)
	}
	if !record.Fitted() {
		t.Error("the shipped record is not fitted")
	}
	if record.Family != FamilyDE2 || record.Model != "shisa-ai/shisa-de-2" {
		t.Errorf("the shipped record is for %q %q", record.Family, record.Model)
	}
	if record.ReadoutVersion != ReadoutVersionFor(FamilyDE2) {
		t.Errorf("the shipped record was fitted against %q, the readout is %q",
			record.ReadoutVersion, ReadoutVersionFor(FamilyDE2))
	}
	if record.FittedOn == "" || record.ServingShape == "" || record.Source == "" {
		t.Error("the shipped record does not carry its provenance")
	}
	if len(record.TestECE) == 0 || len(record.Withheld) == 0 {
		t.Error("the shipped record lost its measurements or its withheld fit")
	}
}

// TestCalibrationTemperatureFor pins per-read selection: a record with per-read
// temperatures applies only the entry for the read, and 1.0 to a read it has no
// fit for. A record without them has one fit, for the direct read.
func TestCalibrationTemperatureFor(t *testing.T) {
	record, err := CalibrationFor(FamilyDE2)
	if err != nil {
		t.Fatalf("CalibrationFor: %v", err)
	}
	cases := []struct {
		questionType string
		read         string
		want         float64
	}{
		{"noul", "direct", 2.69},
		{"noul", "repeat2", 2.76},
		{"noul", "think", 6.8},
		{"choice", "direct", 1.81},
		{"choice", "repeat2", 1.89},
		{"choice", "think", 6.8},
		// A score distribution uses the choice fit.
		{"score", "repeat2", 1.89},
		// A read the record does not name stays raw.
		{"choice", "", 1.0},
		{"choice", "image", 1.0},
		{"noul", "bogus", 1.0},
	}
	for _, tc := range cases {
		if got := record.TemperatureFor(tc.questionType, tc.read); got != tc.want {
			t.Errorf("TemperatureFor(%q, %q) = %v, want %v",
				tc.questionType, tc.read, got, tc.want)
		}
	}

	// A single-fit record ignores the read name and falls back to the direct
	// fit, which is the DE-1 shape.
	single := Calibration{Temperatures: map[string]float64{"noul": 2.0, "choice": 3.0},
		ReadoutVersion: "de1-letter-slots-v3", Model: "shisa-ai/shisha-de-1"}
	if got := single.TemperatureFor("choice", "repeat2"); got != 3.0 {
		t.Errorf("a single-fit record returned %v for a named read, want 3.0", got)
	}
	if got := single.TemperatureFor("choice", ""); got != 3.0 {
		t.Errorf("a single-fit record returned %v, want 3.0", got)
	}
	if got := single.TemperatureFor("noul", ""); got != 2.0 {
		t.Errorf("a single-fit record returned %v for a noul, want 2.0", got)
	}
	empty := Calibration{}
	if got := empty.TemperatureFor("choice", ""); got != 1.0 {
		t.Errorf("an empty record returned %v, want 1.0", got)
	}
}

// TestCalibrationFitted pins "a record is fitted when some temperature is not
// 1.0", including the per-read tables.
func TestCalibrationFitted(t *testing.T) {
	if (Calibration{}).Fitted() {
		t.Error("a zero record reports itself as fitted")
	}
	if (Calibration{Temperatures: map[string]float64{"choice": 1.0}}).Fitted() {
		t.Error("a record of only 1.0 reports itself as fitted")
	}
	if !(Calibration{Temperatures: map[string]float64{"choice": 1.0, "noul": 1.5}}).Fitted() {
		t.Error("a record with one temperature above 1.0 is not reported as fitted")
	}
	perRead := Calibration{
		Temperatures: map[string]float64{"choice": 1.0},
		Reads:        map[string]map[string]float64{"think": {"choice": 6.8}},
	}
	if !perRead.Fitted() {
		t.Error("a record whose only fit is in a per-read table is not reported as fitted")
	}
}

// TestCalibrationID pins the identifier's shape for both record forms.
func TestCalibrationID(t *testing.T) {
	single := Calibration{
		Temperatures:   map[string]float64{"noul": 2.69, "choice": 1.81},
		ReadoutVersion: "de2-codebook-v1",
		Model:          "shisa-ai/shisa-de-2",
		ServingShape:   "vllm-0.30.0",
	}
	if got, want := single.ID(), "shisa-ai/shisa-de-2|de2-codebook-v1|vllm-0.30.0[choice=1.81,noul=2.69]"; got != want {
		t.Errorf("ID() = %q, want %q", got, want)
	}
	perRead := Calibration{
		Temperatures: map[string]float64{"noul": 2.76, "choice": 1.89},
		Reads: map[string]map[string]float64{
			"repeat2": {"noul": 2.76, "choice": 1.89},
			"direct":  {"noul": 2.69, "choice": 1.81},
		},
		ReadoutVersion: "de2-codebook-v1",
		Model:          "shisa-ai/shisa-de-2",
		ServingShape:   "vllm-0.30.0",
	}
	want := "shisa-ai/shisa-de-2|de2-codebook-v1|vllm-0.30.0[" +
		"direct:choice=1.81,noul=2.69;repeat2:choice=1.89,noul=2.76]"
	if got := perRead.ID(); got != want {
		t.Errorf("ID() = %q, want %q", got, want)
	}
	// A whole-number temperature keeps Python's float formatting, so 6.0 is
	// "6.0" and not "6".
	whole := Calibration{Temperatures: map[string]float64{"choice": 6.0}}
	if got := whole.ID(); !strings.HasSuffix(got, "[choice=6.0]") {
		t.Errorf("ID() = %q, want a 6.0 temperature spelled the way Python spells it", got)
	}
}

// TestCalibrationApplicability pins the four levels and the reasons that go
// with a mismatch.
func TestCalibrationApplicability(t *testing.T) {
	record, err := CalibrationFor(FamilyDE2)
	if err != nil {
		t.Fatalf("CalibrationFor: %v", err)
	}
	cases := []struct {
		name       string
		model      string
		version    string
		options    ApplicabilityOptions
		wantLevel  ApplicabilityLevel
		wantReason bool
	}{
		{"the fitted checkpoint", "shisa-ai/shisa-de-2", "de2-codebook-v1",
			ApplicabilityOptions{Family: FamilyDE2}, ApplicabilityCheckpoint, false},
		{"the fitted checkpoint, case-insensitively", "SHISA-AI/Shisa-DE-2", "de2-codebook-v1",
			ApplicabilityOptions{Family: FamilyDE2}, ApplicabilityCheckpoint, false},
		{"an aliased checkpoint, recognised by its tokenizer", "de2-served-as", "de2-codebook-v1",
			ApplicabilityOptions{Family: FamilyDE2, Tokenizer: "shisa-ai/shisa-de-2"},
			ApplicabilityCheckpoint, false},
		{"another checkpoint of the same family", "de2-v4-lr5e5-e3-s7", "de2-codebook-v1",
			ApplicabilityOptions{Family: FamilyDE2}, ApplicabilityFamily, true},
		{"another readout version", "shisa-ai/shisa-de-2", "de1-letter-slots-v3",
			ApplicabilityOptions{Family: FamilyDE2}, ApplicabilityMismatch, true},
		{"another family", "shisa-ai/shisa-de-1", "de2-codebook-v1",
			ApplicabilityOptions{}, ApplicabilityMismatch, true},
		// The family is read from the model id when the caller does not say.
		{"another family, inferred", "shisa-ai/shisa-de-1", "de2-codebook-v1",
			ApplicabilityOptions{}, ApplicabilityMismatch, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			level, reasons := record.Applicability(tc.model, tc.version, tc.options)
			if level != tc.wantLevel {
				t.Errorf("level = %q, want %q (reasons %v)", level, tc.wantLevel, reasons)
			}
			if tc.wantReason != (len(reasons) != 0) {
				t.Errorf("reasons = %v, want %v", reasons, tc.wantReason)
			}
		})
	}

	// A record with no temperature has nothing to apply and nothing to mismatch
	// on, so an unfitted record reports "unfitted" rather than "family".
	unfitted := Calibration{Family: FamilyDE2, ReadoutVersion: "de2-codebook-v1",
		Model: "shisa-ai/shisa-de-2"}
	if level, reasons := unfitted.Applicability("another-de2", "de2-codebook-v1",
		ApplicabilityOptions{Family: FamilyDE2}); level != ApplicabilityUnfitted {
		t.Errorf("an unfitted record reports %q, want %q (reasons %v)",
			level, ApplicabilityUnfitted, reasons)
	}
	// But a family or version mismatch still wins over "unfitted".
	if level, _ := unfitted.Applicability("shisa-ai/shisa-de-1", "de2-codebook-v1",
		ApplicabilityOptions{}); level != ApplicabilityMismatch {
		t.Errorf("an unfitted record for the wrong family reports %q", level)
	}
	// Both reasons are reported when both are wrong.
	_, reasons := record.Applicability("shisa-ai/shisa-de-1", "de1-letter-slots-v3",
		ApplicabilityOptions{})
	if len(reasons) != 2 {
		t.Errorf("a double mismatch reported %d reasons, want 2: %v", len(reasons), reasons)
	}
}

// TestCalibrationMatches pins the boolean form: only a mismatch is false.
func TestCalibrationMatches(t *testing.T) {
	record, err := CalibrationFor(FamilyDE2)
	if err != nil {
		t.Fatalf("CalibrationFor: %v", err)
	}
	ok, reasons := record.Matches("de2-v4-lr5e5-e3-s7", "de2-codebook-v1",
		ApplicabilityOptions{Family: FamilyDE2})
	if !ok || len(reasons) != 0 {
		t.Errorf("Matches on another checkpoint = %v %v, want true with no reasons", ok, reasons)
	}
	ok, reasons = record.Matches("shisa-ai/shisa-de-1", "de2-codebook-v1", ApplicabilityOptions{})
	if ok || len(reasons) == 0 {
		t.Errorf("Matches on the wrong family = %v %v, want false with reasons", ok, reasons)
	}
}

// TestCalibrationForUnknownFamily pins that this package ships one record and
// says so rather than falling back.
func TestCalibrationForUnknownFamily(t *testing.T) {
	if _, err := CalibrationFor(FamilyDE1); err == nil {
		t.Error("CalibrationFor(de1) succeeded, but this package ships no DE-1 record")
	} else if !strings.Contains(err.Error(), "de2") {
		t.Errorf("the error does not name the family that does ship: %v", err)
	}
}

// TestResolveCalibration pins the `--calibration` argument: a family name, a
// path, "auto", and neither.
func TestResolveCalibration(t *testing.T) {
	for _, spec := range []string{"", "auto"} {
		if _, ok, err := ResolveCalibration(spec); ok || err != nil {
			t.Errorf("ResolveCalibration(%q) = %v, %v; want no record and no error", spec, ok, err)
		}
	}
	for _, spec := range []string{"de2", "DE-2", "de_2"} {
		record, ok, err := ResolveCalibration(spec)
		if err != nil || !ok {
			t.Fatalf("ResolveCalibration(%q): ok=%v err=%v", spec, ok, err)
		}
		if record.Model != "shisa-ai/shisa-de-2" {
			t.Errorf("ResolveCalibration(%q) returned the record for %q", spec, record.Model)
		}
	}
	if _, _, err := ResolveCalibration("de1"); err == nil {
		t.Error("ResolveCalibration(\"de1\") succeeded, but no DE-1 record ships")
	}
	if _, _, err := ResolveCalibration("/nonexistent/calibration.json"); err == nil {
		t.Error("ResolveCalibration of a missing path succeeded")
	} else if !strings.Contains(err.Error(), "neither a family") {
		t.Errorf("the error does not explain what was expected: %v", err)
	}

	// A path to a real record file.
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	body := `{"model":"shisa-ai/shisa-de-2","readout_version":"de2-codebook-v1",
	          "serving_shape":"test","fitted_on":"now","temperatures":{"choice":2.0},
	          "family":"de2"}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing the record: %v", err)
	}
	record, ok, err := ResolveCalibration(path)
	if err != nil || !ok {
		t.Fatalf("ResolveCalibration(%q): ok=%v err=%v", path, ok, err)
	}
	if got := record.TemperatureFor("choice", ""); got != 2.0 {
		t.Errorf("the record from disk has temperature %v, want 2.0", got)
	}
}

// TestCalibrationFromJSON pins the record's defaulting and its refusals.
func TestCalibrationFromJSON(t *testing.T) {
	// The family is read from the model id when the record does not say.
	record, err := CalibrationFromJSON([]byte(`{"model":"shisa-ai/shisa-de-2",
	    "readout_version":"de2-codebook-v1","serving_shape":"test","fitted_on":"now",
	    "temperatures":{"choice":2.0}}`), "test")
	if err != nil {
		t.Fatalf("CalibrationFromJSON: %v", err)
	}
	if record.Family != FamilyDE2 {
		t.Errorf("the family defaulted to %q, want de2", record.Family)
	}
	if record.Temperatures == nil || record.TestECE == nil || record.Withheld == nil {
		t.Error("a record with no tables has nil maps, which would panic on a read")
	}
	// An explicit family wins over the model id, which is how a record for a
	// checkpoint whose name says nothing is read.
	record, err = CalibrationFromJSON([]byte(`{"model":"my-checkpoint","family":"de2",
	    "readout_version":"de2-codebook-v1","serving_shape":"test","fitted_on":"now",
	    "temperatures":{"choice":2.0}}`), "test")
	if err != nil {
		t.Fatalf("CalibrationFromJSON: %v", err)
	}
	if record.Family != FamilyDE2 {
		t.Errorf("the explicit family was ignored: %q", record.Family)
	}

	// The provenance fields are required, as the Python loader requires them: a
	// record with no serving shape is a threshold whose provenance nobody can
	// check, and this port refuses it rather than defaulting it.
	for _, tc := range []struct{ name, body string }{
		{"not JSON", `{`},
		{"no model", `{"readout_version":"de2-codebook-v1"}`},
		{"no readout version", `{"model":"shisa-ai/shisa-de-2"}`},
		{"no serving shape", `{"model":"shisa-ai/shisa-de-2","readout_version":"de2-codebook-v1",
		    "fitted_on":"now","temperatures":{"choice":2.0}}`},
		{"no fitted_on", `{"model":"shisa-ai/shisa-de-2","readout_version":"de2-codebook-v1",
		    "serving_shape":"test","temperatures":{"choice":2.0}}`},
		{"no temperature table", `{"model":"shisa-ai/shisa-de-2",
		    "readout_version":"de2-codebook-v1","serving_shape":"test","fitted_on":"now"}`},
		{"a null temperature table", `{"model":"shisa-ai/shisa-de-2",
		    "readout_version":"de2-codebook-v1","serving_shape":"test","fitted_on":"now",
		    "temperatures":null}`},
		{"a null read table", `{"model":"shisa-ai/shisa-de-2",
		    "readout_version":"de2-codebook-v1","serving_shape":"test","fitted_on":"now",
		    "temperatures":{"choice":2.0},"reads":{"direct":null}}`},
		{"per-read fits and no table", `{"model":"shisa-ai/shisa-de-2",
		    "readout_version":"de2-codebook-v1","serving_shape":"test","fitted_on":"now",
		    "reads":{"direct":{"choice":2.0}}}`},
		{"a null temperature", `{"model":"shisa-ai/shisa-de-2",
		    "readout_version":"de2-codebook-v1","serving_shape":"test","fitted_on":"now",
		    "temperatures":{"choice":null}}`},
		{"a null per-read temperature", `{"model":"shisa-ai/shisa-de-2",
		    "readout_version":"de2-codebook-v1","serving_shape":"test","fitted_on":"now",
		    "reads":{"direct":{"choice":null}}}`},
		{"unknown family", `{"model":"x","readout_version":"de2-codebook-v1",
		    "serving_shape":"test","fitted_on":"now","temperatures":{"choice":2.0},
		    "family":"de3"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CalibrationFromJSON([]byte(tc.body), "test"); err == nil {
				t.Errorf("%s was accepted", tc.name)
			}
		})
	}
	// The shipped record carries both tables. A per-read table is the whole of
	// the record's applicability once it is there, as it is in the Python
	// client: a read the per-read table does not name gets 1.0, and the
	// top-level table is not consulted.
	record, err = CalibrationFromJSON([]byte(`{"model":"shisa-ai/shisa-de-2",
	    "readout_version":"de2-codebook-v1","serving_shape":"test","fitted_on":"now",
	    "temperatures":{"choice":1.89},"reads":{"direct":{"choice":2.0}}}`), "test")
	if err != nil {
		t.Fatalf("a record with both tables was refused: %v", err)
	}
	if got := record.TemperatureFor("choice", "direct"); got != 2.0 {
		t.Errorf("the per-read temperature is %v, want 2.0", got)
	}
	if got := record.TemperatureFor("choice", ""); got != 1.0 {
		t.Errorf("a read the per-read table does not name is %v, want 1.0", got)
	}
}

// TestLoadCalibrationUnknownName pins that a missing embedded record is an
// error rather than an empty record.
func TestLoadCalibrationUnknownName(t *testing.T) {
	if _, err := LoadCalibration("data/not-a-record.json"); err == nil {
		t.Error("LoadCalibration of an unknown name succeeded")
	}
	if _, err := LoadCalibrationFile("/nonexistent/record.json"); err == nil {
		t.Error("LoadCalibrationFile of a missing path succeeded")
	}
}

// TestReleasedCheckpointRecognisesReleasedNames pins the name rule that selects
// a checkpoint's record.
//
// A name that contains a released checkpoint's name as its own token is that
// checkpoint, which is what makes a quantized repo or a local directory work
// without passing the tokenizer. The rule is Python's, including its
// boundaries: `shisa-de-2.10` and `shisa-de-21` are not DE-2, because `.` and a
// digit continue a name rather than ending one.
func TestReleasedCheckpointRecognisesReleasedNames(t *testing.T) {
	cases := []struct {
		name       string
		checkpoint string
	}{
		{"shisa-ai/shisa-de-2.1", "shisa-ai/shisa-de-2.1"},
		{"shisa-ai/shisa-de-2.1-FP8-dynamic", "shisa-ai/shisa-de-2.1"},
		{"/models/shisa-de-2.1", "shisa-ai/shisa-de-2.1"},
		{"shisa-de-2.1", "shisa-ai/shisa-de-2.1"},
		{"SHISA-AI/Shisa-DE-2.1", "shisa-ai/shisa-de-2.1"},
		{"shisa-ai/shisa-de-2", "shisa-ai/shisa-de-2"},
		{"shisa-ai/shisa-de-2-FP8-dynamic", "shisa-ai/shisa-de-2"},
		// Not a released checkpoint: training arms, later versions, look-alikes.
		{"de2-v4-lr5e5-e3-s7", ""},
		{"de2-some-arm", ""},
		{"shisa-ai/shisa-de-2.2", ""},
		{"shisa-ai/shisa-de-21", ""},
		{"shisa-ai/shisa-de-2.10", ""},
		{"my-shisa-de-2.1x", ""},
		{"", ""},
		// The Python client ships a DE-1 record and so recognises DE-1 names.
		// This port has none and does not read the family, so a DE-1 name is not
		// a checkpoint it can select.
		{"shisa-ai/shisa-de-1-FP8-dynamic", ""},
	}
	for _, tc := range cases {
		if got := ReleasedCheckpoint(tc.name); got != tc.checkpoint {
			t.Errorf("ReleasedCheckpoint(%q) = %q, want %q", tc.name, got, tc.checkpoint)
		}
	}
}

// TestEachReleasedCheckpointGetsItsOwnRecord pins that every checkpoint in the
// table has a record whose model is that checkpoint and that the record reaches
// `checkpoint` level for it, which is what makes the client apply it.
func TestEachReleasedCheckpointGetsItsOwnRecord(t *testing.T) {
	for checkpoint, path := range CheckpointCalibrationFiles {
		record, err := LoadCalibration(path)
		if err != nil {
			t.Fatalf("LoadCalibration(%s): %v", path, err)
		}
		if strings.ToLower(record.Model) != checkpoint {
			t.Errorf("%s holds a record for %q, want %q", path, record.Model, checkpoint)
		}
		if got := ModelFamily(checkpoint); got != record.Family {
			t.Errorf("%s resolves to family %s, its record is %s", checkpoint, got, record.Family)
		}
		selected, err := CalibrationForCheckpoint(record.Family, checkpoint, "")
		if err != nil {
			t.Fatalf("CalibrationForCheckpoint(%s): %v", checkpoint, err)
		}
		if selected.Model != record.Model {
			t.Errorf("serving %s selected the record for %s", checkpoint, selected.Model)
		}
		level, reasons := record.Applicability(checkpoint, ReadoutVersionFor(record.Family),
			ApplicabilityOptions{Family: record.Family})
		if level != ApplicabilityCheckpoint || len(reasons) != 0 {
			t.Errorf("%s applies to its own checkpoint at %q, not %q (%v)",
				checkpoint, ApplicabilityCheckpoint, level, reasons)
		}
	}

	// The two DE-2 records are different fits. A test that only checked they
	// load would pass with the same file under both names.
	de21, err := CalibrationForCheckpoint(FamilyDE2, "shisa-ai/shisa-de-2.1", "")
	if err != nil {
		t.Fatalf("CalibrationForCheckpoint(2.1): %v", err)
	}
	de2, err := CalibrationForCheckpoint(FamilyDE2, "shisa-ai/shisa-de-2", "")
	if err != nil {
		t.Fatalf("CalibrationForCheckpoint(de2): %v", err)
	}
	if de21.Model == de2.Model {
		t.Fatalf("both DE-2 checkpoints selected the record for %s", de21.Model)
	}
	if de21.Reads["direct"]["noul"] == de2.Reads["direct"]["noul"] {
		t.Error("the two records hold the same direct noul temperature")
	}
	if got := de21.TemperatureFor("noul", "think"); got != 5.23 {
		t.Errorf("DE-2.1's think temperature is %v, want 5.23", got)
	}
	if de21.Temperatures["noul"] != de21.Reads["repeat2"]["noul"] {
		t.Error("the DE-2.1 record's temperatures are not its repeat2 entry")
	}
}

// TestCalibrationForCheckpointFallsBackToTheFamilyRecord pins the two ways a
// served model reaches a record: by its id, and by the tokenizer source when a
// server aliases the id.
func TestCalibrationForCheckpointFallsBackToTheFamilyRecord(t *testing.T) {
	// Another DE-2 checkpoint gets the family record, which names DE-2 and is
	// reported rather than applied to it.
	record, err := CalibrationForCheckpoint(FamilyDE2, "de2-some-arm", "")
	if err != nil {
		t.Fatalf("CalibrationForCheckpoint: %v", err)
	}
	if record.Model != "shisa-ai/shisa-de-2" {
		t.Errorf("an uncovered checkpoint selected the record for %s", record.Model)
	}
	level, reasons := record.Applicability("de2-some-arm", ReadoutVersionFor(FamilyDE2),
		ApplicabilityOptions{Family: FamilyDE2})
	if level != ApplicabilityFamily || len(reasons) == 0 {
		t.Errorf("an uncovered checkpoint is %q with %v, want %q with a reason",
			level, reasons, ApplicabilityFamily)
	}

	// A server that aliases the id is recognised by its tokenizer source.
	record, err = CalibrationForCheckpoint(FamilyDE2, "alias", "shisa-ai/shisa-de-2.1")
	if err != nil {
		t.Fatalf("CalibrationForCheckpoint(alias): %v", err)
	}
	if record.Model != "shisa-ai/shisa-de-2.1" {
		t.Errorf("an aliased DE-2.1 selected the record for %s", record.Model)
	}
	level, reasons = record.Applicability("alias", ReadoutVersionFor(FamilyDE2),
		ApplicabilityOptions{Family: FamilyDE2, Tokenizer: "shisa-ai/shisa-de-2.1"})
	if level != ApplicabilityCheckpoint || len(reasons) != 0 {
		t.Errorf("an aliased DE-2.1 is %q with %v, want %q", level, reasons, ApplicabilityCheckpoint)
	}

	// A checkpoint of another family does not lend its record.
	if record, err = CalibrationForCheckpoint(FamilyDE1, "shisa-ai/shisa-de-2.1", ""); err == nil {
		t.Errorf("a DE-1 request got the record for %s", record.Model)
	}
}

// TestResolveCalibrationAcceptsAReleasedCheckpoint pins the `--calibration`
// spellings, including a checkpoint id, which is how a caller reading an alias
// says which checkpoint is really behind it.
func TestResolveCalibrationAcceptsAReleasedCheckpoint(t *testing.T) {
	cases := []struct{ spec, model string }{
		{"shisa-ai/shisa-de-2.1", "shisa-ai/shisa-de-2.1"},
		{"shisa-ai/shisa-de-2.1-FP8-dynamic", "shisa-ai/shisa-de-2.1"},
		{"shisa-ai/shisa-de-2", "shisa-ai/shisa-de-2"},
		{"de2", "shisa-ai/shisa-de-2"},
		{"DE-2", "shisa-ai/shisa-de-2"},
	}
	for _, tc := range cases {
		record, ok, err := ResolveCalibration(tc.spec)
		if err != nil {
			t.Errorf("ResolveCalibration(%q): %v", tc.spec, err)
			continue
		}
		if !ok || record.Model != tc.model {
			t.Errorf("ResolveCalibration(%q) = %q, ok=%v; want %q", tc.spec, record.Model, ok, tc.model)
		}
	}
	if _, ok, err := ResolveCalibration("auto"); err != nil || ok {
		t.Errorf(`ResolveCalibration("auto") = ok=%v, err=%v; want ok=false`, ok, err)
	}
	// A version this package has no record for is an error that names what it
	// does ship, rather than silently falling back to DE-2's fit.
	_, _, err := ResolveCalibration("shisa-ai/shisa-de-2.2")
	if err == nil {
		t.Fatal("a checkpoint with no record was accepted")
	}
	if !strings.Contains(err.Error(), "shisa-ai/shisa-de-2.1") {
		t.Errorf("the error does not name the checkpoints that ship: %v", err)
	}
}

// TestReleasedCheckpointIsConservativeAboutCase pins that the fold is ASCII, so
// this port recognises a subset of what the Python client recognises and never a
// name it does not.
//
// Python's `str.lower()` maps U+0130 to `i` plus U+0307 and U+017F to `s`; Go's
// `strings.ToLower` maps U+0130 to `i` alone. Folding ASCII only means neither
// becomes part of a match here, which is the safe direction: an unrecognised
// name gets raw probabilities instead of a fit that may not describe it.
func TestReleasedCheckpointIsConservativeAboutCase(t *testing.T) {
	cases := []struct{ name, checkpoint string }{
		{"SHISA-DE-2.1", "shisa-ai/shisa-de-2.1"},
		// U+0130 is not `i`: it is a different letter, so the slug is not present.
		// (Python's `lower()` would make it `i` plus a combining dot, also no
		// match; `casefold()` is what maps it, and neither client folds with it.)
		{"SH\u0130SA-DE-2.1", ""},
		// U+017F is not `s` under `str.lower()` either, so neither client matches.
		{"SHI\u017fA-DE-2.1", ""},
		// Adjacent to the slug, a rune Python lowercases into `[a-z0-9.]` continues
		// the name, so the slug is part of a longer one and does not match.
		{"shisa-de-2.1\u0130", ""},
		{"\u212Ashisa-de-2.1", ""},
		// A multi-byte rune next to the slug is a boundary in both clients.
		{"\u65e5\u672c\u8a9eshisa-de-2.1", "shisa-ai/shisa-de-2.1"},
		{"shisa-de-2.1\u65e5\u672c\u8a9e", "shisa-ai/shisa-de-2.1"},
		// Both slugs present: the longer one wins, as in Python.
		{"shisa-de-2/shisa-de-2.1", "shisa-ai/shisa-de-2.1"},
		{"shisa-de-2.1/shisa-de-2", "shisa-ai/shisa-de-2.1"},
		// U+0130 before the longer slug is a boundary: Python expands it to `i`
		// plus U+0307, so the combining dot abuts the slug. Rejecting it there
		// would fall through to the shorter checkpoint and apply the wrong record.
		{"\u0130shisa-de-2.1/shisa-de-2", "shisa-ai/shisa-de-2.1"},
	}
	for _, tc := range cases {
		if got := ReleasedCheckpoint(tc.name); got != tc.checkpoint {
			t.Errorf("ReleasedCheckpoint(%q) = %q, want %q", tc.name, got, tc.checkpoint)
		}
	}
}

// TestEveryShippedRecordCarriesItsProvenance pins the artifact this package
// ships, not just the one it shipped first. A truncated copy of a record still
// loads -- `source`, `test_ece` and `note` are optional -- so the provenance is
// what a test has to hold.
func TestEveryShippedRecordCarriesItsProvenance(t *testing.T) {
	if len(checkpointRecords) != len(CheckpointCalibrationFiles) {
		t.Fatalf("the match order has %d entries and the table has %d",
			len(checkpointRecords), len(CheckpointCalibrationFiles))
	}
	for _, entry := range checkpointRecords {
		record, err := LoadCalibration(entry.path)
		if err != nil {
			t.Fatalf("LoadCalibration(%s): %v", entry.path, err)
		}
		if !record.Fitted() {
			t.Errorf("%s is not fitted", entry.path)
		}
		if record.FittedOn == "" || record.ServingShape == "" || record.Source == "" {
			t.Errorf("%s lost its provenance: fitted_on=%q serving_shape=%q source=%q",
				entry.path, record.FittedOn, record.ServingShape, record.Source)
		}
		if record.Note == "" {
			t.Errorf("%s has no note, which is where the fit's limits are recorded", entry.path)
		}
		if len(record.TestECE) == 0 {
			t.Errorf("%s carries no held-out measurements", entry.path)
		}
		if record.ReadoutVersion != ReadoutVersionFor(record.Family) {
			t.Errorf("%s was fitted against %q, the readout is %q",
				entry.path, record.ReadoutVersion, ReadoutVersionFor(record.Family))
		}
		for _, read := range []string{"direct", "repeat2", "think"} {
			if len(record.Reads[read]) == 0 {
				t.Errorf("%s has no temperatures for the %q read", entry.path, read)
			}
		}
		if record.Temperatures["noul"] != record.Reads["repeat2"]["noul"] {
			t.Errorf("%s: temperatures is not the repeat2 entry", entry.path)
		}
	}
}

// TestApplicabilityNormalisesTheRecordsOwnModel pins that both sides of the
// comparison use one rule, so a record fitted on a suffixed repo name reaches
// `checkpoint` level for that name instead of reporting "another checkpoint".
func TestApplicabilityNormalisesTheRecordsOwnModel(t *testing.T) {
	record, err := CalibrationForCheckpoint(FamilyDE2, "shisa-ai/shisa-de-2.1", "")
	if err != nil {
		t.Fatalf("CalibrationForCheckpoint: %v", err)
	}
	record.Model = "shisa-ai/shisa-de-2.1-FP8-dynamic"
	for _, served := range []string{
		"shisa-ai/shisa-de-2.1-FP8-dynamic",
		"shisa-ai/shisa-de-2.1",
	} {
		level, reasons := record.Applicability(served, ReadoutVersionFor(FamilyDE2),
			ApplicabilityOptions{Family: FamilyDE2})
		if level != ApplicabilityCheckpoint || len(reasons) != 0 {
			t.Errorf("a record fitted on a suffixed name is %q for %q (%v), want %q",
				level, served, reasons, ApplicabilityCheckpoint)
		}
	}
}

// TestResolveCalibrationLoadsAPathBeforeMatchingAName pins that an explicit file
// is opened even when a directory in its path is named after a checkpoint.
//
// The served-name matcher is permissive by design, so running it over a spec
// first would turn a deployment's own fit into the bundled record and never open
// the file the caller named.
func TestResolveCalibrationLoadsAPathBeforeMatchingAName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shisa-de-2.1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	embedded, err := LoadCalibration("data/calibration-de2.1.json")
	if err != nil {
		t.Fatalf("LoadCalibration: %v", err)
	}
	// A record that is not the bundled one, so a fallback is visible.
	raw, err := os.ReadFile("data/calibration-de2.1.json")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	record["fitted_on"] = "a deployment's own fit"
	record["serving_shape"] = "a-deployment"
	path := filepath.Join(dir, "deployment.json")
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	loaded, ok, err := ResolveCalibration(path)
	if err != nil {
		t.Fatalf("ResolveCalibration(%s): %v", path, err)
	}
	if !ok || loaded.FittedOn != "a deployment's own fit" {
		t.Errorf("the file under a checkpoint-named directory resolved to %q (ok=%v), want the file",
			loaded.FittedOn, ok)
	}

	// A missing file is an error, not a silent fallback to the bundled record --
	// with or without an extension, and whether the path is relative or rooted.
	for _, missing := range []string{
		filepath.Join(dir, "not-here.json"),
		filepath.Join(dir, "deployment"),
		"/fits/shisa-de-2.1/deployment",
		"./fits/shisa-de-2.1/deployment",
		"~/fits/shisa-de-2.1/deployment",
		// A relative path whose first segment is not an organisation the shipped
		// records live under, so it is a path and not a selector.
		"fits/shisa-de-2.1/deployment",
		"models/shisa-de-2.1/deployment",
	} {
		if got, _, err := ResolveCalibration(missing); err == nil {
			t.Errorf("%s resolved to %s instead of failing", missing, got.Model)
		}
	}
	// The bundled record is still reachable by its checkpoint id.
	if got, ok, err := ResolveCalibration("shisa-ai/shisa-de-2.1"); err != nil || !ok || got.Model != embedded.Model {
		t.Errorf("ResolveCalibration(checkpoint) = %q ok=%v err=%v", got.Model, ok, err)
	}
}
