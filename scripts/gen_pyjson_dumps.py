#!/usr/bin/env python3
"""Write the `json.dumps` vectors the Go encoder's options are checked against.

`PythonJSON` is `json.dumps(value, ensure_ascii=False)` because that is what the
prompt needs. The command line is the other caller: `doctor --json` prints
`json.dumps(report, indent=2)` and `ask` prints
`json.dumps(values, indent=2, ensure_ascii=False)`, so the encoder has to carry
`indent` and `ensure_ascii` too, and carry them the way CPython does rather than
the way Go's `encoding/json` does.

Every case is a value, the two options, and CPython's output. The values are
written as `ensure_ascii=False` JSON so the Go test can decode them with
`DecodeJSON`, which keeps integers and floats apart the way Python's parser does.

    python3 scripts/gen_pyjson_dumps.py

Output: testdata/pyjson_dumps.json and testdata/pyjson_dumps.meta.json.
"""

from __future__ import annotations

import datetime
import hashlib
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
OUT = ROOT / "testdata" / "pyjson_dumps.json"
META_OUT = ROOT / "testdata" / "pyjson_dumps.meta.json"

# The values. Between them they carry an empty container, a nested one, both
# number kinds, the escapes the encoder special-cases, and the code point ranges
# `ensure_ascii` treats differently: DEL, the C1 block, the BMP, a lone astral
# character, and the surrogate pair one is written as.
VALUES: list[tuple[str, object]] = [
    ("empty object", {}),
    ("empty array", []),
    ("empty nested", {"a": [], "b": {}}),
    ("scalars", {"t": True, "f": False, "n": None, "i": 3, "neg": -7, "zero": 0}),
    ("floats", {"one": 1.0, "half": 0.5, "big": 1e15, "small": 1e-5, "exp": 1.5e300}),
    ("one element", [1]),
    ("two elements", [1, 2]),
    ("nested array", [1, [2, [3]]]),
    ("nested object", {"a": {"b": {"c": 1}}}),
    ("object of arrays", {"a": [1, 2], "b": [[3]]}),
    ("escapes", {"s": "quote \" backslash \\ newline \n tab \t return \r"}),
    ("controls", {"s": "\x00\x01\x1f"}),
    ("del and c1", {"s": "\x7f\x80\x9f"}),
    ("latin", {"s": "café"}),
    ("bmp", {"s": "中文"}),
    ("astral", {"s": "😀"}),
    ("astral pair", {"s": "😀😁"}),
    ("mixed text", {"s": "café 中文 😀 \x7f"}),
    ("line separator", {"s": "\u2028\u2029"}),
    ("key escapes", {"a\nb": 1, "c\"d": 2}),
    ("non-ascii key", {"中": 1}),
    ("deep", {"a": {"b": [{"c": [1, {"d": 2}]}]}}),
]

# `None` is `json.dumps`' own default and a different output from `""`. An
# integer is the number of spaces, which is what the fixture records: the Go
# option is the string itself.
INDENTS: list[object] = [None, "", 0, 1, 2, 4]


def effective_indent(indent: object) -> str | None:
    """`json.dumps`' indent argument as the string it stands for."""
    if indent is None:
        return None
    if isinstance(indent, int):
        return " " * indent
    return indent
ENSURE_ASCII: list[bool] = [True, False]


def main() -> int:
    cases = []
    for name, value in VALUES:
        for indent in INDENTS:
            for ensure_ascii in ENSURE_ASCII:
                cases.append({
                    "name": f"{name} indent={indent!r} ensure_ascii={ensure_ascii}",
                    "value": json.dumps(value, ensure_ascii=False),
                    "indent": effective_indent(indent),
                    "ensure_ascii": ensure_ascii,
                    "output": json.dumps(value, indent=indent, ensure_ascii=ensure_ascii),
                })
    fixture = {
        "python": sys.version.split()[0],
        "generated": datetime.date.today().isoformat(),
        "source": "json.dumps(value, indent=..., ensure_ascii=...)",
        "cases": cases,
    }
    text = json.dumps(fixture, ensure_ascii=False, indent=1) + "\n"
    OUT.write_text(text)
    digest = hashlib.sha256(text.encode()).hexdigest()
    META_OUT.write_text(json.dumps({
        "python": fixture["python"],
        "generated": fixture["generated"],
        "cases": len(cases),
        "sha256": digest,
    }, indent=1) + "\n")
    print(f"{OUT}: {len(cases)} cases, sha256 {digest[:16]}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
