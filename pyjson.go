package shisade

import (
	"bytes"
	"encoding/json"
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
// parser makes; `json.RawMessage` is inserted verbatim. Anything else is an
// error naming the type rather than a guess at its JSON form.
//
// Use DecodeJSON to get Python-equivalent numbers from a byte slice: a plain
// json.Unmarshal into `any` turns every number into a float64, and 3 would then
// render as 3.0 where Python renders 3.
func PythonJSON(value any) (string, error) {
	var buffer bytes.Buffer
	if err := appendPythonJSON(&buffer, value, 0); err != nil {
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
func DecodeJSON(data []byte) (any, error) {
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
			object = append(object, Member{Key: key, Value: value})
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

func appendPythonJSON(buffer *bytes.Buffer, value any, depth int) error {
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
		appendPythonString(buffer, typed)
		return nil
	case Object:
		return appendPythonObject(buffer, typed, depth)
	case json.Number:
		return appendJSONNumber(buffer, string(typed))
	case json.RawMessage:
		if !json.Valid(typed) {
			return fmt.Errorf("shisade: a json.RawMessage is not valid JSON")
		}
		buffer.Write(typed)
		return nil
	case []byte:
		return fmt.Errorf("shisade: []byte has no JSON form here; pass a string or a base64 string")
	}

	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.String:
		appendPythonString(buffer, reflected.String())
	case reflect.Bool:
		buffer.WriteString(strconv.FormatBool(reflected.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		buffer.WriteString(strconv.FormatInt(reflected.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		buffer.WriteString(strconv.FormatUint(reflected.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		appendPythonFloat(buffer, reflected.Float())
	case reflect.Slice, reflect.Array:
		if reflected.Kind() == reflect.Slice && reflected.Type().Elem().Kind() == reflect.Uint8 {
			return fmt.Errorf("shisade: %s has no JSON form here; pass a string", reflected.Type())
		}
		buffer.WriteByte('[')
		for i := 0; i < reflected.Len(); i++ {
			if i > 0 {
				buffer.WriteString(", ")
			}
			if err := appendPythonJSON(buffer, reflected.Index(i).Interface(), depth+1); err != nil {
				return err
			}
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
		return appendPythonObject(buffer, members, depth)
	case reflect.Pointer, reflect.Interface:
		if reflected.IsNil() {
			buffer.WriteString("null")
			return nil
		}
		return appendPythonJSON(buffer, reflected.Elem().Interface(), depth+1)
	default:
		return fmt.Errorf("shisade: %s has no JSON form here; use shisade.Object, a map with string keys, or a JSON type",
			reflected.Type())
	}
	return nil
}

func appendPythonObject(buffer *bytes.Buffer, object Object, depth int) error {
	buffer.WriteByte('{')
	for i, member := range object {
		if i > 0 {
			buffer.WriteString(", ")
		}
		appendPythonString(buffer, member.Key)
		buffer.WriteString(": ")
		if err := appendPythonJSON(buffer, member.Value, depth+1); err != nil {
			return err
		}
	}
	buffer.WriteByte('}')
	return nil
}

// appendJSONNumber renders a number the way Python's parser and dumper would:
// an integer stays an integer, and anything with a fraction or an exponent is a
// float and gets Python's float repr.
func appendJSONNumber(buffer *bytes.Buffer, text string) error {
	if text == "" {
		return fmt.Errorf("shisade: an empty json.Number")
	}
	if !strings.ContainsAny(text, ".eE") {
		buffer.WriteString(text)
		return nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return fmt.Errorf("shisade: json.Number %q is not a number: %w", text, err)
	}
	appendPythonFloat(buffer, value)
	return nil
}

// appendPythonString writes a string with Python's json escaping: the short
// forms for the control characters that have them, \u00xx for the rest, and no
// escaping of non-ASCII, <, >, &, / or the line separators.
func appendPythonString(buffer *bytes.Buffer, text string) {
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
			if r < 0x20 {
				fmt.Fprintf(buffer, `\u%04x`, r)
			} else {
				buffer.WriteRune(r)
			}
		}
		i += size
	}
	buffer.WriteByte('"')
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
