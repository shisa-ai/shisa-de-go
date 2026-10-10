package shisade

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Calibration and confidence for DE answers.
//
// Two numbers are kept apart on purpose:
//
//   - Confidence is the statistic TypeSafe returns on Choice and Score answers,
//     `(K * pMax - 1) / (K - 1)`, so the same code can read hosted Jev and DE.
//     It is a convenience, not a calibration guarantee.
//   - A calibrated probability is the tempered distribution. DE-1 arrives
//     near-calibrated and temperature scaling sharpens it further; the
//     temperatures shipped here were fitted on dev splits of the committed
//     suites.
//
// One record ships per family, because a temperature fitted on one checkpoint
// provides no evidence about another. Every answer says which calibration was
// applied, including the model it was fitted on, because a threshold fitted
// against one readout, one serving shape, or one checkpoint does not transfer to
// another.
//
// A record is applied on its own only to the checkpoint it was fitted on, read
// through a readout its fit still describes (`Calibration.Applicability`). The
// DE-2 record holds one fit per read, because the single read, the repeated read
// and the read after a thought are differently overconfident.

// CalibrationFiles names the shipped record for each family that has one.
//
// This port implements DE-2 only, so only the DE-2 record is embedded; asking
// for another family is an error rather than a silent fallback.
var CalibrationFiles = map[Family]string{
	FamilyDE2: "data/calibration-de2.json",
}

//go:embed data/calibration-de2.json
var calibrationFS embed.FS

// TemperBinary is binary temperature scaling: `p^(1/T) / (p^(1/T) + (1-p)^(1/T))`.
//
// A temperature at or below zero, or exactly 1, returns p unchanged, which is
// the Python client's convention for "no fit here".
func TemperBinary(p, temperature float64) float64 {
	if temperature <= 0 || temperature == 1 {
		return p
	}
	a := math.Pow(math.Max(p, 1e-12), 1.0/temperature)
	b := math.Pow(math.Max(1.0-p, 1e-12), 1.0/temperature)
	return a / (a + b)
}

// TemperDistribution is multiclass temperature scaling: `p_i^(1/T)` renormalized.
// The argmax is unchanged.
func TemperDistribution(probabilities map[string]float64, temperature float64) map[string]float64 {
	if temperature <= 0 || temperature == 1 {
		return copyFloatMap(probabilities)
	}
	powered := make(map[string]float64, len(probabilities))
	total := 0.0
	for key, value := range probabilities {
		weight := math.Pow(math.Max(value, 1e-12), 1.0/temperature)
		powered[key] = weight
		total += weight
	}
	if total == 0 {
		total = 1.0
	}
	tempered := make(map[string]float64, len(powered))
	for key, value := range powered {
		tempered[key] = value / total
	}
	return tempered
}

// Confidence is the confidence statistic hosted Jev returns, derived from the
// distribution.
//
// `(K * pMax - 1) / (K - 1)`, clipped to 0..1: 1.0 when all mass is on one
// option, 0 when the distribution is even. TypeSafe documents this formula in
// its confidence page and states that callers are not locked into it, which is
// why the raw probabilities are always returned beside it.
func Confidence(probabilities map[string]float64) float64 {
	if len(probabilities) == 0 {
		return 0.0
	}
	k := len(probabilities)
	if k < 2 {
		return 1.0
	}
	peak := math.Inf(-1)
	for _, value := range probabilities {
		peak = math.Max(peak, value)
	}
	return math.Max(0.0, math.Min(1.0, (float64(k)*peak-1.0)/float64(k-1)))
}

// ApplicabilityLevel is how far a calibration record applies to a served model.
type ApplicabilityLevel string

const (
	// ApplicabilityMismatch means the family differs, or the record was fitted
	// against a readout whose answers this one does not reproduce. Never applied
	// unless the caller passes the record explicitly, and `doctor` fails.
	ApplicabilityMismatch ApplicabilityLevel = "mismatch"
	// ApplicabilityFamily means the right family and readout, but fitted on
	// another checkpoint. Not applied unless the caller passes the record
	// explicitly.
	ApplicabilityFamily ApplicabilityLevel = "family"
	// ApplicabilityCheckpoint means fitted on this checkpoint. Applied.
	ApplicabilityCheckpoint ApplicabilityLevel = "checkpoint"
	// ApplicabilityUnfitted means the record holds no temperature, so there is
	// nothing to apply and nothing to mismatch on.
	ApplicabilityUnfitted ApplicabilityLevel = "unfitted"
)

// ApplicabilityOptions are the optional arguments of Calibration.Applicability.
type ApplicabilityOptions struct {
	// Family is the family the client resolved. The zero value means "read the
	// model id".
	Family Family
	// Tokenizer is the tokenizer source, which recognises a checkpoint a server
	// has aliased.
	Tokenizer string
}

// Calibration is a fitted temperature set, tied to a readout version and a
// serving shape.
type Calibration struct {
	// Temperatures is the record's single fit, or the default read's when Reads
	// is present.
	Temperatures map[string]float64
	// Reads holds temperatures per read for a family whose answers come from
	// more than one (`direct`, `repeat2`, `think` for DE-2). When present, a
	// read it does not name is left raw; Temperatures is then the default
	// read's, for reference.
	Reads          map[string]map[string]float64
	ReadoutVersion string
	Model          string
	ServingShape   string
	FittedOn       string
	TestECE        map[string]any
	Source         string
	Family         Family
	Note           string
	// Withheld is a fit that was made and deliberately not shipped, kept for
	// its provenance.
	Withheld map[string]any
}

// Fitted reports whether the record changes any probability. An unfitted record
// is all 1.0.
func (c Calibration) Fitted() bool {
	for _, table := range append([]map[string]float64{c.Temperatures}, readTables(c.Reads)...) {
		for _, value := range table {
			if value != 1.0 {
				return true
			}
		}
	}
	return false
}

// ID is a short identifier recorded on every answer.
//
// Carries the model and the readout version, not just the temperatures. The
// fitted model is the field that decides whether a record applies to the
// endpoint in front of it, so leaving it out of the identifier is how a DE-1
// record came to be applied to DE-2 answers without anyone seeing it. A reader
// of one answer can now tell which checkpoint the number beside it was fitted
// on.
func (c Calibration) ID() string {
	table := func(temperatures map[string]float64) string {
		keys := make([]string, 0, len(temperatures))
		for key := range temperatures {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+"="+pythonFloatString(temperatures[key]))
		}
		return strings.Join(parts, ",")
	}
	parts := table(c.Temperatures)
	if len(c.Reads) != 0 {
		reads := make([]string, 0, len(c.Reads))
		for read := range c.Reads {
			reads = append(reads, read)
		}
		sort.Strings(reads)
		entries := make([]string, 0, len(reads))
		for _, read := range reads {
			entries = append(entries, read+":"+table(c.Reads[read]))
		}
		parts = strings.Join(entries, ";")
	}
	return fmt.Sprintf("%s|%s|%s[%s]", c.Model, c.ReadoutVersion, c.ServingShape, parts)
}

// TemperatureFor is the temperature for a question type. Score distributions use
// the choice fit.
//
// read names the read the distribution came from. A record with per-read
// temperatures applies only the entry for that read, and 1.0 to a read it has no
// fit for; a record without them has one fit, for the direct read.
func (c Calibration) TemperatureFor(questionType, read string) float64 {
	key := "choice"
	if questionType == "noul" {
		key = "noul"
	}
	if len(c.Reads) != 0 {
		if value, ok := c.Reads[read][key]; ok {
			return value
		}
		return 1.0
	}
	if value, ok := c.Temperatures[key]; ok {
		return value
	}
	return 1.0
}

// Applicability reports how far this record applies to a served model under a
// readout.
//
// The level is one of ApplicabilityMismatch, ApplicabilityFamily,
// ApplicabilityCheckpoint and ApplicabilityUnfitted; reasons explains a mismatch
// or a same-family mismatch. The checkpoint is recognised by the served model id
// or, for a server that aliases it, by the tokenizer source. options.Family is
// the family the client resolved; the zero value means "read the model id".
func (c Calibration) Applicability(model, readoutVersion string, options ApplicabilityOptions) (ApplicabilityLevel, []string) {
	served := options.Family
	if served == "" {
		served = ModelFamily(model)
	}
	var reasons []string
	if c.Family != served {
		reasons = append(reasons, fmt.Sprintf(
			"fitted for family %q but %q is read as %q", string(c.Family), model, string(served)))
	}
	if !CalibrationCompatibleWith(c.ReadoutVersion, readoutVersion) {
		reasons = append(reasons, fmt.Sprintf(
			"fitted against readout %q, whose answers %q does not reproduce", c.ReadoutVersion, readoutVersion))
	}
	if len(reasons) != 0 {
		return ApplicabilityMismatch, reasons
	}
	if !c.Fitted() {
		return ApplicabilityUnfitted, nil
	}
	names := make([]string, 0, 2)
	for _, name := range []string{model, options.Tokenizer} {
		if name != "" {
			names = append(names, strings.ToLower(name))
		}
	}
	for _, name := range names {
		if strings.ToLower(c.Model) == name {
			return ApplicabilityCheckpoint, nil
		}
	}
	return ApplicabilityFamily, []string{fmt.Sprintf(
		"fitted on %q, not on %q: same family, another checkpoint", c.Model, model)}
}

// Matches reports whether this record can describe a served model at all:
// `(ok, reasons)`. False only for a mismatch; see Applicability for the finer
// levels.
func (c Calibration) Matches(model, readoutVersion string, options ApplicabilityOptions) (bool, []string) {
	level, reasons := c.Applicability(model, readoutVersion, options)
	if level == ApplicabilityMismatch {
		return false, reasons
	}
	return true, nil
}

// LoadCalibration loads a calibration record shipped inside the package.
func LoadCalibration(name string) (Calibration, error) {
	data, err := calibrationFS.ReadFile(name)
	if err != nil {
		return Calibration{}, &ReadoutError{Message: fmt.Sprintf(
			"no calibration record %q is shipped with this package", name)}
	}
	return CalibrationFromJSON(data, name)
}

// LoadCalibrationFile loads a calibration record from an arbitrary filesystem path.
func LoadCalibrationFile(path string) (Calibration, error) {
	target := expandUser(path)
	data, err := os.ReadFile(target)
	if err != nil {
		return Calibration{}, fmt.Errorf("no calibration record at %s: %w", target, err)
	}
	return CalibrationFromJSON(data, target)
}

// CalibrationFromJSON builds a record from its JSON form, defaulting the family
// from the model.
func CalibrationFromJSON(data []byte, origin string) (Calibration, error) {
	var raw struct {
		Temperatures   map[string]float64            `json:"temperatures"`
		Reads          map[string]map[string]float64 `json:"reads"`
		ReadoutVersion string                        `json:"readout_version"`
		Model          string                        `json:"model"`
		ServingShape   string                        `json:"serving_shape"`
		FittedOn       string                        `json:"fitted_on"`
		TestECE        map[string]any                `json:"test_ece"`
		Source         string                        `json:"source"`
		Family         string                        `json:"family"`
		Note           string                        `json:"note"`
		Withheld       map[string]any                `json:"withheld"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Calibration{}, fmt.Errorf("calibration record %s is not valid JSON: %w", origin, err)
	}
	if raw.Model == "" || raw.ReadoutVersion == "" {
		return Calibration{}, fmt.Errorf(
			"calibration record %s has no model or no readout_version", origin)
	}
	family := FamilyDE1
	if raw.Family != "" {
		normalized, err := NormalizeFamily(raw.Family)
		if err != nil {
			return Calibration{}, fmt.Errorf("calibration record %s: %w", origin, err)
		}
		family = normalized
	} else {
		family = ModelFamily(raw.Model)
	}
	return Calibration{
		Temperatures:   orEmptyFloatMap(raw.Temperatures),
		Reads:          raw.Reads,
		ReadoutVersion: raw.ReadoutVersion,
		Model:          raw.Model,
		ServingShape:   raw.ServingShape,
		FittedOn:       raw.FittedOn,
		TestECE:        orEmptyAnyMap(raw.TestECE),
		Source:         raw.Source,
		Family:         family,
		Note:           raw.Note,
		Withheld:       orEmptyAnyMap(raw.Withheld),
	}, nil
}

// CalibrationFor is the shipped record for a family.
func CalibrationFor(family Family) (Calibration, error) {
	name, ok := CalibrationFiles[family]
	if !ok {
		return Calibration{}, &ReadoutError{Message: fmt.Sprintf(
			"no calibration record ships for family %q; this package ships %s",
			string(family), calibrationFamilyNames())}
	}
	return LoadCalibration(name)
}

// ResolveCalibration resolves a `--calibration` argument.
//
// Accepts a family name in any common spelling (`de1`, `DE-2`) or a path to a
// record. An empty spec or "auto" means "let the client pick from the served
// model", and returns ok=false.
func ResolveCalibration(spec string) (Calibration, bool, error) {
	if spec == "" || spec == "auto" {
		return Calibration{}, false, nil
	}
	if family, err := NormalizeFamily(spec); err == nil {
		record, err := CalibrationFor(family)
		if err != nil {
			return Calibration{}, false, err
		}
		return record, true, nil
	}
	record, err := LoadCalibrationFile(spec)
	if err != nil {
		return Calibration{}, false, fmt.Errorf(
			"calibration %q is neither a family (%s) nor a record file: %w",
			spec, familyNames(), err)
	}
	return record, true, nil
}

func familyNames() string {
	names := make([]string, 0, len(Families))
	for _, family := range Families {
		names = append(names, string(family))
	}
	return strings.Join(names, ", ")
}

func calibrationFamilyNames() string {
	names := make([]string, 0, len(CalibrationFiles))
	for family := range CalibrationFiles {
		names = append(names, string(family))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func readTables(reads map[string]map[string]float64) []map[string]float64 {
	tables := make([]map[string]float64, 0, len(reads))
	for _, table := range reads {
		tables = append(tables, table)
	}
	return tables
}

func copyFloatMap(values map[string]float64) map[string]float64 {
	copied := make(map[string]float64, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func orEmptyFloatMap(values map[string]float64) map[string]float64 {
	if values == nil {
		return map[string]float64{}
	}
	return values
}

func orEmptyAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	return values
}

func expandUser(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// pythonFloatString is Python's `str(float)`, which is its repr.
func pythonFloatString(value float64) string {
	var buffer bytes.Buffer
	appendPythonFloat(&buffer, value)
	return buffer.String()
}
