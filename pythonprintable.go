package shisade

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
)

// pythonPrintableJSON is Python's `str.isprintable()`, over every code point.
//
// Go's `strconv.IsPrint` is the same predicate against Go's own Unicode tables,
// which are a different version from the interpreter the Python client runs on.
// A label spelled the way `repr()` spells it is what this port owes a caller, so
// the predicate is the interpreter's rather than the toolchain's.
//
//go:embed data/python-printable.json
var pythonPrintableJSON []byte

// pythonPrintableFile is the table and its provenance.
type pythonPrintableFile struct {
	Python        string  `json:"python"`
	PythonUnicode string  `json:"python_unicode"`
	Generated     string  `json:"generated"`
	Source        string  `json:"source"`
	Printable     [][]int `json:"printable"`
}

var (
	pythonPrintableOnce sync.Once
	pythonPrintableData pythonPrintableFile
	pythonPrintableErr  error
)

func loadPythonPrintable() {
	pythonPrintableOnce.Do(func() {
		if err := json.Unmarshal(pythonPrintableJSON, &pythonPrintableData); err != nil {
			pythonPrintableErr = fmt.Errorf("data/python-printable.json is not readable: %w", err)
			return
		}
		if len(pythonPrintableData.Printable) == 0 {
			pythonPrintableErr = fmt.Errorf("data/python-printable.json holds no ranges")
			return
		}
		// The ranges are searched as sorted and disjoint, so a file that is not
		// is refused here rather than answering a lookup from a table that
		// happens to be wrong in the middle.
		previous := -1
		for _, pair := range pythonPrintableData.Printable {
			if len(pair) != 2 || pair[0] > pair[1] || pair[0] <= previous {
				pythonPrintableErr = fmt.Errorf(
					"data/python-printable.json holds a malformed range %v", pair)
				return
			}
			previous = pair[1]
		}
	})
}

// isPythonPrintable reports whether `repr()` emits the rune rather than escaping
// it.
//
// A surrogate never reaches here: Go decodes one out of a string as U+FFFD,
// which is printable, so the table omits the surrogates and this answers from
// the ranges alone.
func isPythonPrintable(r rune) bool {
	loadPythonPrintable()
	if pythonPrintableErr != nil {
		// The table is this package's own build artifact, so a caller cannot
		// cause this and the test that loads it is the real check. Fall back to
		// the toolchain's tables rather than escaping every label.
		return strconv.IsPrint(r)
	}
	ranges := pythonPrintableData.Printable
	index := sort.Search(len(ranges), func(i int) bool { return ranges[i][1] >= int(r) })
	return index < len(ranges) && ranges[index][0] <= int(r)
}

// pythonPrintableTable is the loaded table, for the test that replays it.
func pythonPrintableTable() (pythonPrintableFile, error) {
	loadPythonPrintable()
	return pythonPrintableData, pythonPrintableErr
}
