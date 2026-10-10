#!/usr/bin/env python3
"""Generate what Python's `repr()` does with a string, for this port to match.

Two files come out of one run against one interpreter:

`data/python-printable.json` is the runtime table. `str.isprintable()` is the
predicate `repr()` uses to decide between emitting a character and escaping it,
and the table is that predicate over every code point, compacted to inclusive
ranges. Shipping it rather than using Go's `strconv.IsPrint` is the difference
between parity and near-parity: Go 1.25's tables are Unicode 15.0.0 and a
Python 3.14's are 16.0.0, so 5,812 code points assigned in 16.0.0 are printable
to Python and unprintable to Go. The table records the interpreter's Unicode
version so a later regeneration is a diff rather than a guess.

`testdata/python_repr.json` is the expectation set the Go test replays. It is a
sample rather than every code point, because every code point is 1.1M entries:
all of the Basic Multilingual Plane's first three pages, where the controls and
the separators live, every printable range's first and last code point with a
neighbour on each side, and a stride through the rest.

Usage:
    python3 scripts/gen_python_printable.py
"""

from __future__ import annotations

import datetime
import json
import pathlib
import sys
import unicodedata

ROOT = pathlib.Path(__file__).resolve().parent.parent
DATA = ROOT / "data" / "python-printable.json"
FIXTURE = ROOT / "testdata" / "python_repr.json"

# The first three pages of the BMP: the C0 and C1 controls, the Latin-1
# separators, the format characters, and the line and paragraph separators.
BMP_PREFIX = 0x3000
# A stride through everything else. It is a sample: a code point the table gets
# wrong is caught by the boundary set below when it sits on a range edge, and a
# stride that lands in a wrong range catches the rest.
STRIDE = 64
# The tag characters and the variation selectors, and the end of the planes.
TAIL_RANGES = [(0xE0000, 0xE01FF), (0x10FF00, 0x10FFFF)]


def printable_ranges() -> list[list[int]]:
    """`str.isprintable()` over every code point, as inclusive ranges."""
    ranges: list[list[int]] = []
    for code_point in range(sys.maxunicode + 1):
        # A lone surrogate has no `repr` outside a string and cannot appear in
        # a Go string either, so it is left out of the table rather than
        # answered with a rule neither language has.
        if 0xD800 <= code_point <= 0xDFFF:
            continue
        if not chr(code_point).isprintable():
            continue
        if ranges and ranges[-1][1] + 1 == code_point:
            ranges[-1][1] = code_point
        else:
            ranges.append([code_point, code_point])
    return ranges


def sampled_code_points(ranges: list[list[int]]) -> list[int]:
    """The code points the Go test replays, covering every range boundary."""
    wanted: set[int] = set(range(BMP_PREFIX))
    for start, end in ranges:
        for edge in (start - 1, start, start + 1, end - 1, end, end + 1):
            if 0 <= edge <= sys.maxunicode:
                wanted.add(edge)
    for start, end in TAIL_RANGES:
        wanted.update(range(start, end + 1))
    wanted.update(range(0, sys.maxunicode + 1, STRIDE))
    # A lone surrogate cannot be carried in a Go string; the table has no entry
    # for it and neither does the expectation set.
    return sorted(cp for cp in wanted if not 0xD800 <= cp <= 0xDFFF)


def main() -> int:
    ranges = printable_ranges()
    now = datetime.date.today().isoformat()

    # One range per line: 736 of them, and an indented pair per line is six
    # times the file for no reader's benefit.
    header = json.dumps({
        "python": sys.version.split()[0],
        "python_unicode": unicodedata.unidata_version,
        "generated": now,
        "source": "str.isprintable() over every code point, compacted to "
                  "inclusive [first, last] ranges; a lone surrogate is omitted",
    }, indent=1)
    body = ",\n  ".join(json.dumps(pair, separators=(",", ":")) for pair in ranges)
    DATA.write_text(header[:-2] + ',\n "printable": [\n  ' + body + "\n ]\n}\n")

    code_points = sampled_code_points(ranges)
    reprs = {chr(cp): repr(chr(cp)) for cp in code_points}
    FIXTURE.write_text(json.dumps({
        "python": sys.version.split()[0],
        "python_unicode": unicodedata.unidata_version,
        "generated": now,
        "source": "repr() of a one-character string, for the sampled code points",
        "code_points": len(code_points),
        "reprs": reprs,
    }, ensure_ascii=False, separators=(",", ":")) + "\n")

    print(f"{DATA}: {len(ranges)} printable ranges, "
          f"Python {sys.version.split()[0]} Unicode {unicodedata.unidata_version}")
    print(f"{FIXTURE}: {len(code_points)} sampled code points")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
