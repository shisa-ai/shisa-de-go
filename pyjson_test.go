package shisade

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPythonJSONGoValues(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"nil", nil, "null"},
		{"true", true, "true"},
		{"false", false, "false"},
		{"int", 3, "3"},
		{"negative int", int64(-17), "-17"},
		{"uint", uint(5), "5"},
		{"float is a float", float64(3), "3.0"},
		{"empty object", Object{}, "{}"},
		{"empty map", map[string]any{}, "{}"},
		{"empty slice", []any{}, "[]"},
		{"object keeps order", Object{{"b", 1}, {"a", 2}}, `{"b": 1, "a": 2}`},
		{"map sorts keys", map[string]any{"b": 1, "a": 2}, `{"a": 2, "b": 1}`},
		{"slice", []any{1, "a", nil}, `[1, "a", null]`},
		{"nested", Object{{"x", Object{{"y", []any{1, 2}}}}}, `{"x": {"y": [1, 2]}}`},
		{"string map", map[string]string{"k": "v"}, `{"k": "v"}`},

		// Python's float repr, which Go's strconv does not produce.
		{"integral float", float64(1), "1.0"},
		{"zero", 0.0, "0.0"},
		{"negative zero", math.Copysign(0, -1), "-0.0"},
		{"fixed notation at 1e15", 1e15, "1000000000000000.0"},
		{"scientific at 1e16", 1e16, "1e+16"},
		{"scientific at 1e21", 1e21, "1e+21"},
		{"small fixed", 1e-4, "0.0001"},
		{"small scientific", 1e-5, "1e-05"},
		{"negative exponent", 1.5e-9, "1.5e-09"},
		{"third", 1.0 / 3.0, "0.3333333333333333"},
		{"max float", math.MaxFloat64, "1.7976931348623157e+308"},
		{"subnormal", 1e-323, "1e-323"},
		{"float32 widens", float32(0.1), "0.10000000149011612"},
		{"nan", math.NaN(), "NaN"},
		{"infinity", math.Inf(1), "Infinity"},
		{"negative infinity", math.Inf(-1), "-Infinity"},

		// Python's json escaping: only ", \ and the control characters below
		// 0x20 are escaped, and only four of those get a short form.
		{"control characters", "\x00\x1f\b\f\n\r\t", `"\u0000\u001f\b\f\n\r\t"`},
		{"quote and backslash", "\"\\", `"\"\\"`},
		{"html is not escaped", "<&>", `"<&>"`},
		{"line separators are not escaped", "\u2028\u2029", "\"\u2028\u2029\""},
		{"del is not escaped", "\x7f", "\"\x7f\""},
		{"non-ascii is not escaped", "日本語", `"日本語"`},
		{"solidus is not escaped", "a/b", `"a/b"`},

		// Numbers from a JSON document keep Python's int-or-float distinction.
		{"json number int", json.Number("3"), "3"},
		{"json number float", json.Number("3.0"), "3.0"},
		{"json number exponent", json.Number("1e16"), "1e+16"},
		// Python parses -0 as the integer 0 and dumps "0".
		{"json number negative zero", json.Number("-0"), "0"},
		// Python parses an out-of-range literal as an infinity or a zero.
		{"json number overflow", json.Number("1e400"), "Infinity"},
		{"json number negative overflow", json.Number("-1e400"), "-Infinity"},
		{"json number underflow", json.Number("1e-400"), "0.0"},
		{"raw message is verbatim", json.RawMessage(`{"a":1}`), `{"a":1}`},
	}
	for _, tc := range cases {
		got, err := PythonJSON(tc.value)
		if err != nil {
			t.Errorf("%s: PythonJSON: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: PythonJSON = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestPythonJSONRejectsWhatItCannotRender(t *testing.T) {
	deep := any(1)
	for i := 0; i < maxJSONDepth+50; i++ {
		deep = []any{deep}
	}
	cases := []struct {
		name  string
		value any
	}{
		{"bytes", []byte("x")},
		{"non-string map key", map[int]any{1: "a"}},
		{"struct", struct{ A int }{A: 1}},
		{"nested too deep", deep},
		{"invalid raw message", json.RawMessage(`{`)},
		// json.Valid checks syntax only and accepts bytes above 0x80 inside a
		// string without checking that they are UTF-8.
		{"raw message with invalid utf-8", json.RawMessage("{\"t\":\"a\xffb\"}")},
		// Python's str is always valid UTF-8, so there is no Python output to
		// match; substituting U+FFFD would invent bytes.
		{"invalid utf-8", "a\xffb"},
		{"lone surrogate in cesu-8", "\xed\xa0\x80"},
		{"invalid utf-8 in an object key", Object{{"\xff", 1}}},
		{"invalid utf-8 in a json number", json.Number("1\xff")},
		// json.dumps writes a dict, which cannot hold a repeated key.
		{"repeated object key", Object{{"a", 1}, {"a", 2}}},
		{"repeated object key with the same value", Object{{"a", 1}, {"b", 2}, {"a", 1}}},
		// Python's json.dumps raises TypeError for bytes, array or slice.
		{"byte array", [4]byte{1, 2, 3, 4}},
		{"named byte array", [32]byte{}},
	}
	for _, tc := range cases {
		if _, err := PythonJSON(tc.value); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

// TestDecodeJSONKeepsTheLastOfARepeatedKey pins the one place a JSON object can
// hold a key twice. json.loads keeps the last value at the first occurrence's
// position, and the user turn is the serialized object, so the choice changes
// the prompt.
func TestDecodeJSONKeepsTheLastOfARepeatedKey(t *testing.T) {
	value, err := DecodeJSON([]byte(`{"a": 1, "b": 2, "a": 3}`))
	if err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	got, err := PythonJSON(value)
	if err != nil {
		t.Fatalf("PythonJSON: %v", err)
	}
	if want := `{"a": 3, "b": 2}`; got != want {
		t.Errorf("PythonJSON(DecodeJSON(...)) = %s, want %s", got, want)
	}
	object, ok := value.(Object)
	if !ok {
		t.Fatalf("DecodeJSON returned %T, want Object", value)
	}
	member, ok := object.Get("a")
	if !ok || member != json.Number("3") {
		t.Errorf("Object.Get(\"a\") = %v, %v; want the last value", member, ok)
	}
}

// TestDecodeJSONStaysStricterThanPythonOnBareNonFiniteLiterals documents the
// asymmetries: json.loads accepts NaN, Infinity and -Infinity by default, and
// this parser rejects them. PythonJSON still writes NaN and the infinities for a
// non-finite float, so that round trip is asymmetric.
func TestDecodeJSONStaysStricterThanPythonOnBareNonFiniteLiterals(t *testing.T) {
	for _, text := range []string{"NaN", "Infinity", "-Infinity"} {
		if _, err := DecodeJSON([]byte(text)); err == nil {
			t.Errorf("DecodeJSON(%q) should be rejected, matching encoding/json", text)
		}
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := PythonJSON(value); err != nil {
			t.Errorf("PythonJSON(%v) = %v, and Python writes these", value, err)
		}
	}
}

// TestDecodeJSONRejectsInvalidUTF8 pins the other asymmetry: the Go decoder
// would substitute U+FFFD for an invalid byte inside a string, and CPython's
// json.loads on those bytes raises UnicodeDecodeError instead. Laundering the
// value into a valid Go string would put bytes in the prompt that Python never
// produced, which the output-side check could no longer see.
func TestDecodeJSONRejectsInvalidUTF8(t *testing.T) {
	for _, text := range []string{
		"\"a\xffb\"",
		`{"t":"a\xffb"}`,
		`["ok","\xff"]`,
		"{\xff}",
	} {
		if _, err := DecodeJSON([]byte(text)); err == nil {
			t.Errorf("DecodeJSON(%q) should reject the invalid UTF-8", text)
		}
	}
}

// TestDecodeJSONCoercesALoneSurrogate pins the one place DecodeJSON is looser
// than json.loads. encoding/json turns a \u escape for a lone surrogate into
// U+FFFD, where Python keeps the surrogate and the client fails later when it
// encodes the prompt. A literal U+FFFD in the input looks the same after
// decoding, so this is documented rather than detected; the test exists so the
// behavior cannot change silently.
func TestDecodeJSONCoercesALoneSurrogate(t *testing.T) {
	value, err := DecodeJSON([]byte(`{"t":"\ud800"}`))
	if err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	got, err := PythonJSON(value)
	if err != nil {
		t.Fatalf("PythonJSON: %v", err)
	}
	if want := "{\"t\": \"\ufffd\"}"; got != want {
		t.Errorf("DecodeJSON then PythonJSON = %q, want %q (the documented coercion)", got, want)
	}
}

func TestDecodeJSONKeepsIntsAndFloatsApartAndKeyOrder(t *testing.T) {
	value, err := DecodeJSON([]byte(`{"i": 3, "f": 3.0, "e": 1e16, "s": "x"}`))
	if err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	got, err := PythonJSON(value)
	if err != nil {
		t.Fatalf("PythonJSON: %v", err)
	}
	want := `{"i": 3, "f": 3.0, "e": 1e+16, "s": "x"}`
	if got != want {
		t.Errorf("PythonJSON(DecodeJSON(...)) = %s, want %s", got, want)
	}
}

// TestPythonJSONFixedPoints replays the payloads and values the Python client
// produced, requiring this encoder to reproduce Python's bytes exactly.
func TestPythonJSONFixedPoints(t *testing.T) {
	fixture := loadRenderFixture(t)
	for _, entry := range fixture.Serialization {
		value, err := DecodeJSON([]byte(entry.JSON))
		if err != nil {
			t.Errorf("%s: DecodeJSON: %v", entry.Name, err)
			continue
		}
		got, err := PythonJSON(value)
		if err != nil {
			t.Errorf("%s: PythonJSON: %v", entry.Name, err)
			continue
		}
		if got != entry.JSON {
			t.Errorf("%s:\n got %s\nwant %s", entry.Name, truncate(got), truncate(entry.JSON))
		}
	}
}

func truncate(text string) string {
	if len(text) <= 300 {
		return text
	}
	return text[:300] + "..."
}

func TestQuestionValidation(t *testing.T) {
	blank := NewNoul("   ")
	var questionErr *QuestionError
	if err := blank.Validate(MaxOptions); !errors.As(err, &questionErr) {
		t.Errorf("a blank question returned %v, want *QuestionError", err)
	}
	single := NewChoice("Which?", []Option{{Key: "only", Description: "Only"}})
	if err := single.Validate(MaxOptions); !errors.As(err, &questionErr) {
		t.Errorf("a one-option choice returned %v, want *QuestionError", err)
	}
	wide := NewChoice("Which?", keyedOptions(MaxOptions+1))
	if err := wide.Validate(MaxOptions); !errors.As(err, &questionErr) {
		t.Errorf("a choice past the letter limit returned %v, want *QuestionError", err)
	}
	if err := wide.Validate(MaxCodes); err != nil {
		t.Errorf("the same choice within the codebook limit returned %v", err)
	}
	if err := NewScore("How much?", []any{"a", "b"}).Validate(MaxOptions); err != nil {
		t.Errorf("a two-level score returned %v", err)
	}
	if err := NewNoul("Is it?").Validate(MaxOptions); err != nil {
		t.Errorf("a noul returned %v", err)
	}
	// A non-positive limit is the letter limit, the same default Payload and
	// Render use, not a limit of zero.
	if err := NewNoul("Is it?").Validate(0); err != nil {
		t.Errorf("Validate(0) returned %v, want the letter-limit default", err)
	}
	if err := NewChoice("Which?", keyedOptions(MaxOptions)).Validate(0); err != nil {
		t.Errorf("Validate(0) on a 26-option choice returned %v", err)
	}
	if err := NewChoice("Which?", keyedOptions(MaxOptions+1)).Validate(0); err == nil {
		t.Error("Validate(0) should still apply the letter limit to 27 options")
	}
	if err := NewChoice("Which?", keyedOptions(MaxOptions+1)).Validate(-1); err == nil {
		t.Error("Validate(-1) should still apply the letter limit")
	}
}

// TestPythonSpaceIsPythonsStrip pins the one place Go's whitespace set differs
// from Python's: the U+001C-U+001F separators are whitespace to str.strip() and
// not to strings.TrimSpace.
func TestPythonSpaceIsPythonsStrip(t *testing.T) {
	for _, text := range []string{"\u001c", "\u001d", "\u001e", "\u001f", " \u001c\u001f "} {
		if trimmed := trimPythonSpace(text); trimmed != "" {
			t.Errorf("trimPythonSpace(%q) = %q, want empty", text, trimmed)
		}
	}
	// The rest of Python's set, for the same reason.
	for _, text := range []string{"\t", "\n", "\v", "\f", "\r", " ", "\u0085", "\u00a0", "\u2028", "\u3000"} {
		if trimmed := trimPythonSpace(text); trimmed != "" {
			t.Errorf("trimPythonSpace(%q) = %q, want empty", text, trimmed)
		}
	}
	if got := trimPythonSpace("\u200b"); got != "\u200b" {
		t.Errorf("a zero-width space is not whitespace to Python, got %q", got)
	}
	if got := trimPythonSpace(" \u001cspam\u001d "); got != "spam" {
		t.Errorf("trimPythonSpace = %q, want %q", got, "spam")
	}
	// The separator reaches the renderer: a description of "\u001c" is blank to
	// Python and is shown as the key.
	choice := NewChoice("\u001cWhich?\u001d", []Option{
		{Key: "alpha", Description: "\u001c"},
		{Key: "beta", Description: "Beta"},
	})
	if got := choice.Options()[0].Description; got != "alpha" {
		t.Errorf("a U+001C description rendered as %v, want the key", got)
	}
	if err := choice.Validate(MaxOptions); err != nil {
		t.Errorf("a U+001C-wrapped question is not blank to Python: %v", err)
	}
}

// TestRenderOptionTreatsATypedNilAsNone pins the Go-only case: a nil pointer in
// an `any` is not `== nil`, and without this it would reach the prompt as
// "null", a description the model cannot use. A nil map or slice is not absent:
// Python's {} and [] are real descriptions.
func TestRenderOptionTreatsATypedNilAsNone(t *testing.T) {
	var pointer *string
	if got := RenderOption("alpha", pointer); got != "alpha" {
		t.Errorf("a typed nil pointer rendered as %v, want the key", got)
	}
	var nothing any
	if got := RenderOption("alpha", nothing); got != "alpha" {
		t.Errorf("a nil interface rendered as %v, want the key", got)
	}
	if got := RenderOption("alpha", map[string]any(nil)); got == "alpha" {
		t.Error("a nil map is Python's {}, a real description, and must not fall back to the key")
	}
	if got := RenderOption("alpha", []any(nil)); got == "alpha" {
		t.Error("a nil slice is Python's [], a real description, and must not fall back to the key")
	}
	// The choice renders it the same way, and that is what reaches the prompt.
	choice := NewChoice("Which?", []Option{{Key: "alpha", Description: pointer}, {Key: "beta", Description: "B"}})
	if got := choice.Options()[0].Description; got != "alpha" {
		t.Errorf("a typed nil description reached Options as %v, want the key", got)
	}
	payload, _, err := Payload(map[string]any{"t": "x"}, choice, 0)
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	serialized, err := PythonJSON(payload)
	if err != nil {
		t.Fatalf("PythonJSON: %v", err)
	}
	if strings.Contains(serialized, "null") {
		t.Errorf("the typed nil reached the user turn as null: %s", truncate(serialized))
	}
}

func TestQuestionOptions(t *testing.T) {
	noul := NewNoul("Is it?")
	if got := noul.Options(); len(got) != 2 || got[0].Key != "yes" || got[1].Key != "no" {
		t.Errorf("Noul.Options() = %v", got)
	}
	choice := NewChoice("Which?", []Option{
		{Key: "alpha", Description: ""},
		{Key: "beta", Description: nil},
		{Key: "gamma", Description: "   "},
		{Key: "delta", Description: "a real description"},
	})
	got := choice.Options()
	want := []any{"alpha", "beta", "gamma", "a real description"}
	for i, option := range got {
		if option.Description != want[i] {
			t.Errorf("option %d (%s) is shown as %v, want %v", i, option.Key, option.Description, want[i])
		}
	}
	if got[0].Key != "alpha" || got[3].Key != "delta" {
		t.Errorf("the option order changed: %v", got)
	}
	// A score keeps a blank level: its key is the position, not a label.
	score := NewScore("How much?", []any{"", "  ", "High"})
	if desc := score.Options()[0].Description; desc != "" {
		t.Errorf("a blank level was rendered as %v; a score does not substitute its key", desc)
	}
	if key := score.Options()[2].Key; key != "2" {
		t.Errorf("the third level's key is %q, want \"2\"", key)
	}
}

// TestSoftmaxIsDeterministic requires bit-identical results across calls. The
// sum runs in codebook order rather than Go's randomized map order, so there is
// no tolerance to hide behind.
func TestSoftmaxIsDeterministic(t *testing.T) {
	first, err := Softmax(map[string]float64{"A": -1, "B": -2, "C": -3})
	if err != nil {
		t.Fatalf("Softmax: %v", err)
	}
	for i := 0; i < 50; i++ {
		second, err := Softmax(map[string]float64{"C": -3, "B": -2, "A": -1})
		if err != nil {
			t.Fatalf("Softmax: %v", err)
		}
		for key := range first {
			if first[key] != second[key] {
				t.Fatalf("iteration %d: %s is %v then %v", i, key, first[key], second[key])
			}
		}
	}
}

// TestSoftmaxSumsInCodebookOrder checks that the accumulation order is the one
// the Python client uses, which is the codebook's: shortest code first, then
// lexicographic. "Z" comes before "AA" in the codebook even though it sorts
// after it as a string, and a sum that ran in plain string order would differ in
// the last bits.
func TestSoftmaxSumsInCodebookOrder(t *testing.T) {
	logprobs := map[string]float64{"AA": -0.1, "Z": -0.2, "B": -0.3, "A": -0.4}
	got, err := Softmax(logprobs)
	if err != nil {
		t.Fatalf("Softmax: %v", err)
	}
	peak := -0.1
	total := 0.0
	for _, key := range []string{"A", "B", "Z", "AA"} {
		total += math.Exp(logprobs[key] - peak)
	}
	for _, key := range []string{"A", "B", "Z", "AA"} {
		want := math.Exp(logprobs[key]-peak) / total
		if got[key] != want {
			t.Errorf("%s = %v, want %v (codebook-order sum)", key, got[key], want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short"); got != "short" {
		t.Errorf("truncate(\"short\") = %q", got)
	}
	if got := truncate(strings.Repeat("x", 400)); !strings.HasSuffix(got, "...") || len(got) != 303 {
		t.Errorf("truncate of a long string is %d bytes", len(got))
	}
}

// TestPythonJSONDumpsOptions replays `json.dumps` for the `indent` and
// `ensure_ascii` combinations the command line uses.
//
// `PythonJSON` is the compact, `ensure_ascii=False` form the prompt needs. The
// command line prints reports, and `json.dumps(report, indent=2)` is not Go's
// `json.MarshalIndent`: the item separator is a bare comma followed by the
// indent's own newline, an empty container stays on one line, a float keeps
// Python's repr, and `ensure_ascii=True` escapes DEL and the C1 block and writes
// an astral code point as the surrogate pair UTF-16 would.
func TestPythonJSONDumpsOptions(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "pyjson_dumps.json"))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	var fixture struct {
		Python    string `json:"python"`
		Generated string `json:"generated"`
		Cases     []struct {
			Name        string  `json:"name"`
			Value       string  `json:"value"`
			Indent      *string `json:"indent"`
			EnsureASCII bool    `json:"ensure_ascii"`
			Output      string  `json:"output"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("the fixture is not JSON: %v", err)
	}
	metaRaw, err := os.ReadFile(filepath.Join("testdata", "pyjson_dumps.meta.json"))
	if err != nil {
		t.Fatalf("reading the provenance: %v", err)
	}
	var meta struct {
		Python    string `json:"python"`
		Generated string `json:"generated"`
		Cases     int    `json:"cases"`
		SHA256    string `json:"sha256"`
	}
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("the provenance is not JSON: %v", err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != meta.SHA256 {
		t.Errorf("the fixture is %s, the provenance records %s", got, meta.SHA256)
	}
	if len(fixture.Cases) != meta.Cases {
		t.Errorf("the fixture holds %d cases, the provenance records %d",
			len(fixture.Cases), meta.Cases)
	}
	if fixture.Python == "" || meta.Python == "" || fixture.Generated == "" {
		t.Error("the fixture does not say which interpreter produced it")
	}

	for _, tc := range fixture.Cases {
		value, err := DecodeJSON([]byte(tc.Value))
		if err != nil {
			t.Errorf("%s: DecodeJSON: %v", tc.Name, err)
			continue
		}
		got, err := PythonJSONWith(value, PythonJSONOptions{
			Indent: tc.Indent, EnsureASCII: tc.EnsureASCII,
		})
		if err != nil {
			t.Errorf("%s: PythonJSONWith: %v", tc.Name, err)
			continue
		}
		if got != tc.Output {
			t.Errorf("%s:\n got %q\nwant %q", tc.Name, got, tc.Output)
		}
	}
}

// TestPythonJSONIndentIsNotGoIndent pins the two shapes `json.MarshalIndent`
// would get wrong, so the test above cannot pass by accident on a fixture that
// happens to agree with Go.
func TestPythonJSONIndentIsNotGoIndent(t *testing.T) {
	indent := "  "
	value := map[string]any{"a": []any{}, "b": []any{1.0, 2.0}}
	got, err := PythonJSONWith(value, PythonJSONOptions{Indent: &indent})
	if err != nil {
		t.Fatalf("PythonJSONWith: %v", err)
	}
	want := "{\n  \"a\": [],\n  \"b\": [\n    1.0,\n    2.0\n  ]\n}"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Go's encoder would write the empty list as `[]` too, but it writes 1.0 as
	// 1 and an empty object's members differently; this is the check that the
	// fixture is comparing Python against Python.
	goShape, err := json.MarshalIndent(value, "", indent)
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	if string(goShape) == want {
		t.Error("Go's own indented encoder produces the same bytes, so the fixture " +
			"would not catch a swap")
	}
}
