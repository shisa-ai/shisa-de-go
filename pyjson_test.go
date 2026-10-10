package shisade

import (
	"encoding/json"
	"errors"
	"math"
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
	}
	for _, tc := range cases {
		if _, err := PythonJSON(tc.value); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
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
	wide := NewChoice("Which?", make([]Option, MaxOptions+1))
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

func TestSoftmaxDoesNotDependOnMapOrder(t *testing.T) {
	first, err := Softmax(map[string]float64{"A": -1, "B": -2, "C": -3})
	if err != nil {
		t.Fatalf("Softmax: %v", err)
	}
	second, err := Softmax(map[string]float64{"C": -3, "B": -2, "A": -1})
	if err != nil {
		t.Fatalf("Softmax: %v", err)
	}
	for key := range first {
		if math.Abs(first[key]-second[key]) > 1e-15 {
			t.Errorf("%s differs by map order: %v against %v", key, first[key], second[key])
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
