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
	"unicode"
	"unicode/utf8"
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
// One record ships per released checkpoint, because a temperature fitted on one
// checkpoint provides no evidence about another. A served model whose id (or
// tokenizer source) names a released checkpoint gets that checkpoint's record;
// any other model gets its family's record, which is reported but not applied.
// Every answer says which calibration was applied, including the model it was
// fitted on, because a threshold fitted against one readout, one serving shape,
// or one checkpoint does not transfer to another.
//
// A record is applied on its own only to the checkpoint it was fitted on, read
// through a readout its fit still describes (`Calibration.Applicability`). The
// DE-2 record holds one fit per read, because the single read, the repeated read
// and the read after a thought are differently overconfident.

// CalibrationFiles names the shipped record for each family that has one.
//
// This port implements DE-2 only, so only DE-2 records are embedded; asking for
// another family is an error rather than a silent fallback. A family record is
// what a served model gets when its id names no released checkpoint, and
// `Calibration.Applicability` reports it without applying it.
var CalibrationFiles = map[Family]string{
	FamilyDE2: "data/calibration-de2.json",
}

// checkpointRecords is the shipped records in match order: longest slug first,
// so `shisa-de-2.1` is tried before `shisa-de-2`.
//
// This slice is the source of truth. Python sorts its table by length, and its
// stable sort keeps table order among equal lengths; a Go map has no order, so
// the order is written down here instead of derived.
var checkpointRecords = []struct {
	checkpoint string
	path       string
}{
	{"shisa-ai/shisa-de-2.1", "data/calibration-de2.1.json"},
	{"shisa-ai/shisa-de-2", "data/calibration-de2.json"},
}

// CheckpointCalibrationFiles names the shipped record for each released
// checkpoint, keyed by its model id in lower case. It is derived from
// `checkpointRecords`, which holds the match order.
//
// The Python client's table also lists `shisa-ai/shisa-de-1`, which this port
// has no record for and does not read, so it is absent here rather than
// present and unusable.
var CheckpointCalibrationFiles = func() map[string]string {
	files := make(map[string]string, len(checkpointRecords))
	for _, record := range checkpointRecords {
		files[record.checkpoint] = record.path
	}
	return files
}()

//go:embed data/calibration-de2.json data/calibration-de2.1.json
var calibrationFS embed.FS

// ReleasedCheckpoint is the released checkpoint a model id or tokenizer name
// refers to, or "" when it names none.
//
// An exact id, or a name that contains a released checkpoint's name as its own
// token, is that checkpoint: `shisa-ai/shisa-de-2.1-FP8-dynamic` and a local
// `/models/shisa-de-2.1` are DE-2.1, and `shisa-de-2.1` is never read as
// `shisa-de-2`. A training arm such as `de2-v4-lr5e5-e3-s7` names no released
// checkpoint, and neither does a version this package has no record for
// (`shisa-de-2.2`, `shisa-de-2.10`).
//
// A name that extends a release is that release, so a fine-tune whose directory
// is named after its base is treated as the base. The alternative -- refusing a
// suffix -- would break the quantized repos the rule exists for.
func ReleasedCheckpoint(name string) string {
	text := asciiLower(name)
	if text == "" {
		return ""
	}
	if _, ok := CheckpointCalibrationFiles[text]; ok {
		return text
	}
	for _, record := range checkpointRecords {
		checkpoint := record.checkpoint
		slug := checkpoint[strings.Index(checkpoint, "/")+1:]
		for start := 0; start+len(slug) <= len(text); start++ {
			if text[start:start+len(slug)] != slug {
				continue
			}
			if start > 0 && continuesCheckpointName(text, prevRuneStart(text, start), true) {
				continue
			}
			if end := start + len(slug); end < len(text) && continuesCheckpointName(text, end, false) {
				continue
			}
			return checkpoint
		}
	}
	return ""
}

// prevRuneStart is the offset of the rune that ends at `at`, so a boundary can
// be tested on a whole rune rather than on a continuation byte.
func prevRuneStart(text string, at int) int {
	start := at - 1
	for start > 0 && text[start]&0xC0 == 0x80 {
		start--
	}
	return start
}

// asciiLower folds A-Z to a-z and leaves every other byte alone.
//
// Python's `str.lower()` applies the full Unicode mapping, and Go's
// `strings.ToLower` applies the simple one, so the two disagree for a few code
// points: U+0130 lowercases to `i` in Go and to `i` plus U+0307 in Python.
// Folding ASCII only means a name matches only when the slug appears literally,
// so this port recognises a subset of what Python recognises and never a name
// Python does not. The slugs are ASCII, so the cost is a name that needs
// non-ASCII case folding to match at all, which gets raw probabilities instead of
// a fit.
func asciiLower(text string) string {
	folded := []byte(text)
	for i, c := range folded {
		if c >= 'A' && c <= 'Z' {
			folded[i] = c + ('a' - 'A')
		}
	}
	return string(folded)
}

// continuesCheckpointName reports whether b would make a matched checkpoint
// name part of a longer one.
//
// Python writes this as the lookaround pair `(?<![a-z0-9.])` and
// `(?![a-z0-9.])`, applied to a fully lowercased string. Folding ASCII only is
// not enough to reproduce it: Python lowercases `İ` (U+0130) to `i` and `K`
// (U+212A) to `k`, so both continue a name there, while their bytes here are at
// or above 0x80. The byte test alone would therefore match a name Python does
// not. Decoding the adjacent rune and lowercasing it closes that gap, so the
// names this port recognises are a subset of Python's.
func continuesCheckpointName(text string, at int, preceding bool) bool {
	if at < 0 || at >= len(text) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(text[at:])
	if r == utf8.RuneError {
		// An invalid byte is not part of a name; Python keeps it as a lone
		// surrogate, which is not in `[a-z0-9.]` either.
		return false
	}
	if preceding && r == '\u0130' {
		// Python expands U+0130 to `i` plus U+0307, so the character adjacent to
		// the slug is the combining dot, which is not in the class: the slug
		// starts a name there. The following side sees the `i` instead, so this
		// test is directional on purpose.
		return false
	}
	lowered := unicode.ToLower(r)
	return lowered == '.' || (lowered >= 'a' && lowered <= 'z') || (lowered >= '0' && lowered <= '9')
}

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
			"fitted for family %s but %s is read as %s",
			pythonQuote(string(c.Family)), pythonQuote(model), pythonQuote(string(served))))
	}
	if !CalibrationCompatibleWith(c.ReadoutVersion, readoutVersion) {
		reasons = append(reasons, fmt.Sprintf(
			"fitted against readout %s, whose answers %s does not reproduce",
			pythonQuote(c.ReadoutVersion), pythonQuote(readoutVersion)))
	}
	if len(reasons) != 0 {
		return ApplicabilityMismatch, reasons
	}
	if !c.Fitted() {
		return ApplicabilityUnfitted, nil
	}
	names := make([]string, 0, 2)
	for _, name := range []string{model, options.Tokenizer} {
		if name == "" {
			continue
		}
		// A quantized repo or a local directory named after a release is that
		// checkpoint, so it reaches the record fitted on it.
		if checkpoint := ReleasedCheckpoint(name); checkpoint != "" {
			names = append(names, checkpoint)
			continue
		}
		// Python lowercases the raw name here. Folding ASCII only keeps this side
		// of the comparison from inventing a match Python would not make, the same
		// reason ReleasedCheckpoint folds ASCII only.
		names = append(names, asciiLower(name))
	}
	// Both sides of the comparison use one rule: a record fitted on a suffixed
	// repo name is still that checkpoint's record.
	recordModel := ReleasedCheckpoint(c.Model)
	if recordModel == "" {
		recordModel = asciiLower(c.Model)
	}
	for _, name := range names {
		if recordModel == name {
			return ApplicabilityCheckpoint, nil
		}
	}
	return ApplicabilityFamily, []string{fmt.Sprintf(
		"fitted on %s, not on %s: same family, another checkpoint", pythonQuote(c.Model), pythonQuote(model))}
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
//
// The required keys are the Python loader's: `model`, `readout_version`,
// `serving_shape`, `fitted_on` and `temperatures`. A missing provenance field is
// refused rather than defaulted, because the provenance is what decides whether
// the record applies to a checkpoint. A `null` temperature is refused too:
// Python's `float(None)` raises, and reading the null as zero would produce raw
// probabilities reported as calibrated.
func CalibrationFromJSON(data []byte, origin string) (Calibration, error) {
	var raw struct {
		// Pointers, because JSON null decodes into a float64 as 0. A null
		// temperature would then read as "freeze this option" and, since
		// TemperDistribution passes a non-positive temperature through, produce
		// raw probabilities reported as calibrated.
		Temperatures   map[string]*float64            `json:"temperatures"`
		Reads          map[string]map[string]*float64 `json:"reads"`
		ReadoutVersion string                         `json:"readout_version"`
		Model          string                         `json:"model"`
		ServingShape   string                         `json:"serving_shape"`
		FittedOn       string                         `json:"fitted_on"`
		TestECE        map[string]any                 `json:"test_ece"`
		Source         string                         `json:"source"`
		Family         string                         `json:"family"`
		Note           string                         `json:"note"`
		Withheld       map[string]any                 `json:"withheld"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Calibration{}, fmt.Errorf("calibration record %s is not valid JSON: %w", origin, err)
	}
	for _, required := range []struct{ name, value string }{
		{"model", raw.Model},
		{"readout_version", raw.ReadoutVersion},
		{"serving_shape", raw.ServingShape},
		{"fitted_on", raw.FittedOn},
	} {
		if required.value == "" {
			return Calibration{}, fmt.Errorf(
				"calibration record %s has no %s", origin, required.name)
		}
	}
	if raw.Temperatures == nil {
		return Calibration{}, fmt.Errorf(
			"calibration record %s has no temperatures; the Python loader indexes it "+
				"unconditionally, and a record without one has no fit to apply", origin)
	}
	temperatures, err := flattenTemperatures(raw.Temperatures, origin)
	if err != nil {
		return Calibration{}, err
	}
	reads := make(map[string]map[string]float64, len(raw.Reads))
	for name, values := range raw.Reads {
		if values == nil {
			return Calibration{}, fmt.Errorf(
				"calibration record %s has a null temperature table for the %q read",
				origin, name)
		}
		flattened, err := flattenTemperatures(values, origin+" read "+name)
		if err != nil {
			return Calibration{}, err
		}
		reads[name] = flattened
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
		Temperatures:   orEmptyFloatMap(temperatures),
		Reads:          reads,
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

// flattenTemperatures turns a decoded temperature table into its values,
// refusing a null. The Python loader converts each entry with `float()` and
// raises on None rather than reading it as zero.
func flattenTemperatures(values map[string]*float64, origin string) (map[string]float64, error) {
	if values == nil {
		return nil, nil
	}
	flattened := make(map[string]float64, len(values))
	for name, value := range values {
		if value == nil {
			return nil, fmt.Errorf(
				"calibration record %s has a null temperature for %q; a null is not a "+
					"temperature, and reading it as zero would report raw probabilities as "+
					"calibrated", origin, name)
		}
		flattened[name] = *value
	}
	return flattened, nil
}

// CalibrationFor is the shipped record for a family, which is what a served
// model whose id names no released checkpoint gets.
//
// Use CalibrationForCheckpoint to select the record of the checkpoint actually
// served.
func CalibrationFor(family Family) (Calibration, error) {
	name, ok := CalibrationFiles[family]
	if !ok {
		return Calibration{}, &ReadoutError{Message: fmt.Sprintf(
			"no calibration record ships for family %q; this package ships %s",
			string(family), calibrationFamilyNames())}
	}
	return LoadCalibration(name)
}

// CalibrationForCheckpoint is the shipped record for a served model.
//
// The record of the released checkpoint that model, or else tokenizer, names,
// when that checkpoint is of family; otherwise the family's record, which
// `Calibration.Applicability` reports and does not apply.
func CalibrationForCheckpoint(family Family, model, tokenizer string) (Calibration, error) {
	for _, name := range []string{model, tokenizer} {
		path, ok := CheckpointCalibrationFiles[ReleasedCheckpoint(name)]
		if !ok {
			continue
		}
		record, err := LoadCalibration(path)
		if err != nil {
			return Calibration{}, err
		}
		if record.Family == family {
			return record, nil
		}
	}
	return CalibrationFor(family)
}

// ResolveCalibration resolves a `--calibration` argument.
//
// Accepts a family name in any common spelling (`de1`, `DE-2`), a released
// checkpoint id (`shisa-ai/shisa-de-2.1`) or a path to a record. An empty spec
// or "auto" means "let the client pick from the served model", and returns
// ok=false.
//
// A path is loaded as a path. The served-name matcher is permissive by design --
// it matches a checkpoint anywhere in a name -- so running it over a spec first
// would turn `/fits/shisa-de-2.1/deployment.json` into the bundled DE-2.1 record
// and never open the file the caller named.
func ResolveCalibration(spec string) (Calibration, bool, error) {
	if spec == "" || spec == "auto" {
		return Calibration{}, false, nil
	}
	if looksLikeRecordPath(spec) {
		record, err := LoadCalibrationFile(spec)
		if err != nil {
			return Calibration{}, false, calibrationSpecError(spec, err)
		}
		return record, true, nil
	}
	if path, ok := CheckpointCalibrationFiles[ReleasedCheckpoint(spec)]; ok {
		record, err := LoadCalibration(path)
		return record, true, err
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
		return Calibration{}, false, calibrationSpecError(spec, err)
	}
	return record, true, nil
}

// looksLikeRecordPath reports whether a spec names a file rather than a family
// or a checkpoint.
//
// A `.json` suffix is a path even when the file is missing, so a typo in a path
// is an error instead of a silent fallback to a bundled record. A checkpoint id
// never ends in `.json`, so the two spellings cannot collide.
func looksLikeRecordPath(spec string) bool {
	lowered := strings.ToLower(spec)
	if strings.HasSuffix(lowered, ".json") {
		return true
	}
	// A rooted path is a path even without an extension, so a misspelt one is an
	// error rather than a fallback to a record whose name appears in a directory
	// along the way.
	for _, prefix := range []string{"/", "./", "../", "~"} {
		if strings.HasPrefix(spec, prefix) {
			return true
		}
	}
	if info, err := os.Stat(expandUser(spec)); err == nil && !info.IsDir() {
		return true
	}
	// A name with a separator is a checkpoint selector only when it starts with an
	// organisation the shipped records live under. Anything else with a separator
	// is a path the caller meant: `fits/shisa-de-2.1/deployment` must not become
	// the bundled DE-2.1 record because a directory along the way is named after
	// it.
	if first, _, ok := strings.Cut(lowered, "/"); ok {
		for _, record := range checkpointRecords {
			if org, _, _ := strings.Cut(record.checkpoint, "/"); org == first {
				return false
			}
		}
		return true
	}
	return false
}

// calibrationSpecError reports a spec that resolved to nothing.
func calibrationSpecError(spec string, cause error) error {
	return fmt.Errorf(
		"calibration %q is neither a family (%s), a released checkpoint (%s) nor a record file: %w",
		spec, familyNames(), checkpointNames(), cause)
}

// checkpointNames lists the released checkpoints this package has a record for.
func checkpointNames() string {
	names := make([]string, 0, len(CheckpointCalibrationFiles))
	for checkpoint := range CheckpointCalibrationFiles {
		names = append(names, checkpoint)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
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
