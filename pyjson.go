package shisade

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Member is one key and value of an Object.
type Member struct {
	Key   string
	Value any
}

// Object is a JSON object with a caller-chosen key order.
//
// A Go map has no order, but the order of the keys in the user turn decides the
// tokens the model sees, and therefore the answer. Use an Object when the order
// matters; a map is accepted too and has its keys sorted, which is at least
// stable.
//
// A key may appear only once. Python's json.dumps writes a dict, which cannot
// hold a repeated key, so an Object with one has no Python bytes to match and is
// an error rather than a guess. DecodeJSON already reduces a repeated key in a
// document to its last value at the first occurrence's position.
type Object []Member

// PythonJSON renders a value the way Python's
// `json.dumps(value, ensure_ascii=False)` does.
//
// The readout's prompt is the serialized user turn, so a byte of difference is a
// different prompt and a different answer. Go's encoding/json does not produce
// the same bytes: it writes `{"a":1}` where Python writes `{"a": 1}`, and it
// escapes `<`, `>` and `&` as `\u003c`, `\u003e` and `\u0026`, which Python
// leaves alone. Both would move the calibration away from the prompts it was
// fitted on.
//
// The values accepted are those a JSON payload is built from: nil, bool, string,
// every integer kind, float32 and float64, Object, slices and arrays, and maps
// with string keys. `json.Number` keeps the int-or-float distinction Python's
// parser makes, and must be a JSON number literal; `json.RawMessage` is inserted
// verbatim. Anything else is an error naming the type rather than a guess at its
// JSON form.
//
// Use DecodeJSON to get Python-equivalent numbers from a byte slice: a plain
// json.Unmarshal into `any` turns every number into a float64, and 3 would then
// render as 3.0 where Python renders 3.
//
// Two inputs Python can hold are errors here rather than a silent substitution:
// a string that is not valid UTF-8, and a value with no JSON form (a channel,
// a func, a map with non-string keys). Python's `str` is always valid, so the
// first has no Python bytes to match.
func PythonJSON(value any) (string, error) {
	return PythonJSONWith(value, PythonJSONOptions{})
}

// PythonJSONOptions are the `json.dumps` keyword arguments that change the
// bytes, for the callers that are not writing a prompt.
//
// The zero value is `json.dumps(value, ensure_ascii=False)`, which is what
// `PythonJSON` is and what the prompt needs. `indent` is a pointer because
// Python's own default is `None`: `indent=""` is the indented form with no
// indentation at all, which is a different output from the compact one.
type PythonJSONOptions struct {
	// Indent is `indent`. Nil is the compact form; a string is the indented
	// form, one level deeper per nesting level.
	Indent *string
	// EnsureASCII is `ensure_ascii`, which `json.dumps` defaults to true and
	// which escapes every code point outside `0x20..0x7e`. The prompt wants the
	// model to see the text, so this is false there.
	EnsureASCII bool
}

// PythonJSONWith is `json.dumps` with the options a caller asks for.
func PythonJSONWith(value any, options PythonJSONOptions) (string, error) {
	var buffer bytes.Buffer
	if err := appendPythonJSON(&buffer, value, 0, options); err != nil {
		return "", err
	}
	return buffer.String(), nil
}

// DecodeJSON parses JSON the way Python's json.loads does: integers and floats
// stay apart, and an object's key order is kept.
//
// Order is kept because it reaches the prompt: the user turn is the serialized
// object, so reordering the keys of `evidence` changes the tokens the model
// sees. Go maps have no order, which is why this returns Object rather than
// map[string]any.
//
// A repeated object key keeps the last value at the first occurrence's position,
// which is what json.loads and a Python dict do. Writing such an Object back is
// an error rather than a guess: json.dumps iterates a dict and can never emit a
// repeated key, so there is no Python output to match.
//
// It is stricter than json.loads in three places. The bare literals NaN,
// Infinity and -Infinity are rejected, where Python accepts them by default, and
// PythonJSON still writes them for a NaN or infinite float, so a round trip
// through this pair is asymmetric for those three values. Input that is not
// valid UTF-8 is rejected, where CPython's json.loads on bytes raises
// UnicodeDecodeError. And a number literal with more digits than CPython's
// int-string limit (4,300 by default since 3.11) is accepted here, where
// json.loads raises ValueError.
//
// It is looser in one place: a `\u` escape for a lone surrogate is coerced to
// U+FFFD, which is what encoding/json does, where json.loads keeps the
// surrogate in the str and the Python client fails later when it encodes the
// prompt. A literal U+FFFD in the input is indistinguishable from the coerced
// one, so this is documented rather than detected.
func DecodeJSON(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("shisade: decoding JSON: the input is not valid UTF-8, " +
			"which Python's json.loads cannot decode either")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeValue(decoder)
	if err != nil {
		return nil, fmt.Errorf("shisade: decoding JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("shisade: decoding JSON: trailing data after the value")
	}
	return value, nil
}

// MarshalJSON writes the object with its keys in order, which is what an Object
// is.
//
// Without this, `encoding/json` would see the underlying []Member and write a
// JSON array of {"Key":...,"Value":...} objects -- a shape no Python dict has.
// The chat request bodies carry messages and content parts as Objects so their
// keys come out in the order the Python client writes them.
func (o Object) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for index, member := range o {
		if index != 0 {
			buffer.WriteByte(',')
		}
		key, err := json.Marshal(member.Key)
		if err != nil {
			return nil, err
		}
		buffer.Write(key)
		buffer.WriteByte(':')
		value, err := json.Marshal(member.Value)
		if err != nil {
			return nil, err
		}
		buffer.Write(value)
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

// Get returns a member of an object by key.
func (o Object) Get(key string) (any, bool) {
	for _, member := range o {
		if member.Key == key {
			return member.Value, true
		}
	}
	return nil, false
}

func decodeValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return token, nil
	}
	switch delim {
	case '{':
		object := Object{}
		// Positions by key, so a repeated key updates in place in constant time
		// instead of rescanning the members decoded so far.
		positions := make(map[string]int)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("an object key is %T, not a string", keyToken)
			}
			value, err := decodeValue(decoder)
			if err != nil {
				return nil, err
			}
			// A repeated key keeps the last value, at the first occurrence's
			// position: that is what json.loads and a Python dict do.
			if at, seen := positions[key]; seen {
				object[at].Value = value
			} else {
				positions[key] = len(object)
				object = append(object, Member{Key: key, Value: value})
			}
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return object, nil
	case '[':
		array := []any{}
		for decoder.More() {
			value, err := decodeValue(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected delimiter %q", delim)
	}
}

// maxJSONDepth bounds recursion so a value that contains itself fails instead of
// hanging.
const maxJSONDepth = 200

func appendPythonJSON(buffer *bytes.Buffer, value any, depth int, options PythonJSONOptions) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("shisade: the value nests deeper than %d levels", maxJSONDepth)
	}
	if value == nil {
		buffer.WriteString("null")
		return nil
	}
	switch typed := value.(type) {
	case bool:
		buffer.WriteString(strconv.FormatBool(typed))
		return nil
	case string:
		return appendPythonString(buffer, typed, options.EnsureASCII)
	case Object:
		return appendPythonObject(buffer, typed, depth, options)
	case json.Number:
		return appendJSONNumber(buffer, string(typed))
	case json.RawMessage:
		if !json.Valid(typed) {
			return fmt.Errorf("shisade: a json.RawMessage is not valid JSON")
		}
		// json.Valid checks syntax only: it accepts bytes above 0x80 inside a
		// string without checking that they are UTF-8, and writing those
		// verbatim would put bytes in the prompt that Python cannot hold.
		if !utf8.Valid(typed) {
			return fmt.Errorf("shisade: a json.RawMessage is not valid UTF-8, and Python cannot hold one")
		}
		buffer.Write(typed)
		return nil
	case []byte:
		return fmt.Errorf("shisade: []byte has no JSON form here; pass a string or a base64 string")
	}

	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.String:
		return appendPythonString(buffer, reflected.String(), options.EnsureASCII)
	case reflect.Bool:
		buffer.WriteString(strconv.FormatBool(reflected.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		buffer.WriteString(strconv.FormatInt(reflected.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		buffer.WriteString(strconv.FormatUint(reflected.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		appendPythonFloat(buffer, reflected.Float())
	case reflect.Slice, reflect.Array:
		// Python's json.dumps raises TypeError for bytes, and a byte array
		// ([32]byte from a hash, [16]byte from a UUID) is the same thing.
		if reflected.Type().Elem().Kind() == reflect.Uint8 {
			return fmt.Errorf("shisade: %s has no JSON form here; pass a string", reflected.Type())
		}
		buffer.WriteByte('[')
		for i := 0; i < reflected.Len(); i++ {
			if i > 0 {
				appendPythonItemSeparator(buffer, options)
			}
			appendPythonIndent(buffer, depth+1, options.Indent)
			if err := appendPythonJSON(buffer, reflected.Index(i).Interface(), depth+1, options); err != nil {
				return err
			}
		}
		if reflected.Len() > 0 {
			appendPythonIndent(buffer, depth, options.Indent)
		}
		buffer.WriteByte(']')
	case reflect.Map:
		if reflected.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("shisade: %s has a non-string key, which JSON objects cannot carry", reflected.Type())
		}
		keys := reflected.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		members := make(Object, 0, len(keys))
		for _, key := range keys {
			members = append(members, Member{Key: key.String(), Value: reflected.MapIndex(key).Interface()})
		}
		return appendPythonObject(buffer, members, depth, options)
	case reflect.Pointer, reflect.Interface:
		if reflected.IsNil() {
			buffer.WriteString("null")
			return nil
		}
		return appendPythonJSON(buffer, reflected.Elem().Interface(), depth+1, options)
	default:
		return fmt.Errorf("shisade: %s has no JSON form here; use shisade.Object, a map with string keys, or a JSON type",
			reflected.Type())
	}
	return nil
}

func appendPythonObject(buffer *bytes.Buffer, object Object, depth int, options PythonJSONOptions) error {
	// A dict with no members is written inline: `json.dumps` only breaks a line
	// when there is something to put on it.
	if len(object) == 0 {
		buffer.WriteString("{}")
		return nil
	}
	buffer.WriteByte('{')
	seen := make(map[string]struct{}, len(object))
	for i, member := range object {
		if _, repeated := seen[member.Key]; repeated {
			return fmt.Errorf("shisade: the object has the key %q twice, and Python's json.dumps "+
				"writes a dict, which cannot", member.Key)
		}
		seen[member.Key] = struct{}{}
		if i > 0 {
			appendPythonItemSeparator(buffer, options)
		}
		appendPythonIndent(buffer, depth+1, options.Indent)
		if err := appendPythonString(buffer, member.Key, options.EnsureASCII); err != nil {
			return err
		}
		buffer.WriteString(": ")
		if err := appendPythonJSON(buffer, member.Value, depth+1, options); err != nil {
			return err
		}
	}
	appendPythonIndent(buffer, depth, options.Indent)
	buffer.WriteByte('}')
	return nil
}

// appendPythonItemSeparator writes what sits between two members of a container.
//
// `json.dumps` uses `", "` when it is not indenting and `","` followed by a
// newline when it is, because the newline is the indent's own.
func appendPythonItemSeparator(buffer *bytes.Buffer, options PythonJSONOptions) {
	if options.Indent == nil {
		buffer.WriteString(", ")
		return
	}
	buffer.WriteByte(',')
}

// appendPythonIndent starts the line a member sits on. It writes nothing when
// the output is compact.
func appendPythonIndent(buffer *bytes.Buffer, depth int, indent *string) {
	if indent == nil {
		return
	}
	buffer.WriteByte('\n')
	for i := 0; i < depth; i++ {
		buffer.WriteString(*indent)
	}
}

// appendJSONNumber renders a number the way Python's parser and dumper would:
// an integer stays an integer, and anything with a fraction or an exponent is a
// float and gets Python's float repr.
//
// The text has to be a JSON number literal. A json.Number is a string type, so a
// caller can hand over anything, and writing it back unquoted would emit a bare
// non-number; Python's parser only ever produces literals.
func appendJSONNumber(buffer *bytes.Buffer, text string) error {
	if !isJSONNumberLiteral(text) {
		return fmt.Errorf("shisade: json.Number %q is not a JSON number literal", text)
	}
	if !strings.ContainsAny(text, ".eE") {
		// Python parses -0 as the integer 0 and dumps "0".
		if text == "-0" {
			text = "0"
		}
		buffer.WriteString(text)
		return nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		// Python parses an out-of-range literal as an infinity (1e400) or a
		// zero (1e-400), and ParseFloat already returns that value with the
		// range error, so the value is what Python would dump.
		var numberError *strconv.NumError
		if errors.As(err, &numberError) && errors.Is(numberError.Err, strconv.ErrRange) {
			appendPythonFloat(buffer, value)
			return nil
		}
		return fmt.Errorf("shisade: json.Number %q is not a number: %w", text, err)
	}
	appendPythonFloat(buffer, value)
	return nil
}

// isJSONNumberLiteral reports whether text is a JSON number literal:
// `-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?`. It is what JSON's grammar
// allows, and therefore what Python's parser can have produced.
func isJSONNumberLiteral(text string) bool {
	i := 0
	if i < len(text) && text[i] == '-' {
		i++
	}
	start := i
	for i < len(text) && isDigit(text[i]) {
		i++
	}
	if i == start || (text[start] == '0' && i-start > 1) {
		return false
	}
	if i < len(text) && text[i] == '.' {
		i++
		start = i
		for i < len(text) && isDigit(text[i]) {
			i++
		}
		if i == start {
			return false
		}
	}
	if i < len(text) && (text[i] == 'e' || text[i] == 'E') {
		i++
		if i < len(text) && (text[i] == '+' || text[i] == '-') {
			i++
		}
		start = i
		for i < len(text) && isDigit(text[i]) {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(text)
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// appendPythonString writes a string with Python's json escaping: the short
// forms for the control characters that have them, \u00xx for the rest, and no
// escaping of non-ASCII, <, >, &, / or the line separators.
//
// A string that is not valid UTF-8 is an error. Python's str is always valid,
// so there is no Python output to match; substituting U+FFFD would put bytes in
// the prompt that neither the caller nor Python produced.
func appendPythonString(buffer *bytes.Buffer, text string, ensureASCII bool) error {
	if !utf8.ValidString(text) {
		return fmt.Errorf("shisade: a string is not valid UTF-8, and Python cannot hold one")
	}
	buffer.WriteByte('"')
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch r {
		case '"':
			buffer.WriteString(`\"`)
		case '\\':
			buffer.WriteString(`\\`)
		case '\b':
			buffer.WriteString(`\b`)
		case '\f':
			buffer.WriteString(`\f`)
		case '\n':
			buffer.WriteString(`\n`)
		case '\r':
			buffer.WriteString(`\r`)
		case '\t':
			buffer.WriteString(`\t`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(buffer, `\u%04x`, r)
			case !ensureASCII || r <= 0x7e:
				// `ensure_ascii=False` writes the text as UTF-8. With it true,
				// `0x7f` and everything above is escaped, which is what the
				// ASCII encoder's `[^\x20-\x7e]` matches.
				buffer.WriteRune(r)
			case r <= 0xffff:
				fmt.Fprintf(buffer, `\u%04x`, r)
			default:
				// An astral code point is written as the surrogate pair UTF-16
				// would, because Python's ASCII encoder is a UTF-16 one.
				offset := r - 0x10000
				fmt.Fprintf(buffer, `\u%04x\u%04x`, 0xd800+(offset>>10), 0xdc00+(offset&0x3ff))
			}
		}
		i += size
	}
	buffer.WriteByte('"')
	return nil
}

// appendPythonFloat writes a float with Python's repr.
//
// Python uses fixed notation when the decimal point sits after at most 16
// digits and after at least 4 leading zeros, and scientific notation otherwise,
// with a two-digit exponent. Go's strconv formats a different range that way, so
// 1e15 is "1e+15" in Go and "1000000000000000.0" in Python, and 1.0 is "1" in
// Go and "1.0" in Python. Both differences would change the prompt.
func appendPythonFloat(buffer *bytes.Buffer, value float64) {
	switch {
	case math.IsNaN(value):
		buffer.WriteString("NaN")
		return
	case math.IsInf(value, 1):
		buffer.WriteString("Infinity")
		return
	case math.IsInf(value, -1):
		buffer.WriteString("-Infinity")
		return
	}
	// strconv's shortest round-trip form, which is the same digit sequence
	// Python's repr uses.
	text := strconv.FormatFloat(value, 'e', -1, 64)
	mantissa, exponentText, found := strings.Cut(text, "e")
	if !found {
		buffer.WriteString(text)
		return
	}
	exponent, err := strconv.Atoi(exponentText)
	if err != nil {
		buffer.WriteString(text)
		return
	}
	negative := strings.HasPrefix(mantissa, "-")
	mantissa = strings.TrimPrefix(mantissa, "-")
	digits := strings.Replace(mantissa, ".", "", 1)
	point := exponent + 1

	var out string
	switch {
	case point <= -4 || point > 16:
		out = digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		sign := "+"
		if exponent < 0 {
			sign = "-"
			exponent = -exponent
		}
		out += "e" + sign
		if exponent < 10 {
			out += "0"
		}
		out += strconv.Itoa(exponent)
	case point <= 0:
		out = "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		out = digits + strings.Repeat("0", point-len(digits)) + ".0"
	default:
		out = digits[:point] + "." + digits[point:]
	}
	if negative {
		out = "-" + out
	}
	buffer.WriteString(out)
}
