#!/usr/bin/env python3
"""Record what the Python client makes of every image reference this port accepts.

`PrepareImage` and `ValidateImageURL` turn a caller's image reference into the
`image_url` a request can carry, and they are the only part of the image readout
that runs without a server. They also have to produce the same *error messages*
as the Python client, not merely the same verdicts, because those messages are
what a caller reads when a path is wrong.

This script builds a tree of image files, runs `shisa_de.images.prepare_image`
and `shisa_de.images.validate_image_url` over a table of references, and records
each verdict. The Go test replays the table against the same tree.

Two values in an input are placeholders, because the temp directory and the home
directory differ between the two runs:

    {tmp}   the directory the script created, replaced with the Go test's own
    {home}  the expanded home directory, replaced with the Go test's own

    python scripts/gen_images_parity.py

Needs the `shisa-de` Python repository next to this one. Output:
testdata/images_parity.json and testdata/images_parity.meta.json.

The `suffix` cases pin one Python version's answer. `pathlib.Path.suffix` is
`name.lstrip('.').rfind('.')` on 3.14 and `0 < i < len(name) - 1` over the whole
name on 3.10 and 3.12, which disagree on `..png` and `a.`. Both shapes are in
the table, so regenerating this file under an older interpreter changes those two
rows; the meta records the interpreter that produced it.
"""

from __future__ import annotations

import base64
import hashlib
import json
import pathlib
import platform
import shutil
import struct
import sys
import tempfile
import zlib

HERE = pathlib.Path(__file__).resolve().parent
OUT = HERE.parent / "testdata" / "images_parity.json"
META_OUT = HERE.parent / "testdata" / "images_parity.meta.json"
PY_REPO = pathlib.Path.home() / "data" / "dev" / "shisa" / "shisa-de"

sys.path.insert(0, str(PY_REPO))

from shisa_de.images import prepare_image, validate_image_url  # noqa: E402


def make_png(size: int = 32, rgb: tuple[int, int, int] = (255, 0, 0)) -> bytes:
    """A real RGB PNG, built without an image library.

    The measured image readout used 32x32 solid PNGs, and this port's test needs
    a file whose bytes are actually a PNG rather than a header prefix.
    """

    def chunk(tag: bytes, data: bytes) -> bytes:
        return (
            struct.pack(">I", len(data))
            + tag
            + data
            + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        )

    raw = b"".join(b"\x00" + bytes(rgb) * size for _ in range(size))
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 2, 0, 0, 0))
        + chunk(b"IDAT", zlib.compress(raw))
        + chunk(b"IEND", b"")
    )


#: Files the script writes, relative to the tree root. The Go test writes the
#: same ones, from the same bytes.
FILES: dict[str, bytes] = {
    "red.png": make_png(),
    "green.png": make_png(rgb=(0, 255, 0)),
    "blue.png": make_png(rgb=(0, 0, 255)),
    "shot.jpg": b"\xff\xd8\xff\xe0" + b"pretend jpeg bytes",
    "shot.webp": b"RIFF\x00\x00\x00\x00WEBP" + b"pretend webp bytes",
    "mislabelled.jpg": b"\x89PNG\r\n\x1a\n" + b"png bytes under a jpg name",
    "upper.PNG": make_png(),
    "empty.png": b"",
    "fake.png": b"this is not an image",
    "a/b.png": make_png(),
    "a/nested/deep.png": make_png(),
    "dir.d/a": b"not an image",
    ".hidden.png": make_png(),
    "a..png": make_png(),
}

#: References to judge, as (function, input) with `{tmp}` and `{home}` for the
#: two directories that differ between the runs.
CASES: list[tuple[str, str]] = [
    # A URL is already what the request carries.
    ("prepare_image", "https://example.com/cat.png"),
    ("prepare_image", "http://example.com/cat.jpg"),
    ("prepare_image", "data:image/webp;base64,AAAA"),
    ("prepare_image", "  https://example.com/cat.png  "),
    # A local file becomes a data URL, sent as what it is rather than what it is
    # called.
    ("prepare_image", "{tmp}/red.png"),
    ("prepare_image", "{tmp}/shot.jpg"),
    ("prepare_image", "{tmp}/shot.webp"),
    ("prepare_image", "{tmp}/mislabelled.jpg"),
    ("prepare_image", "{tmp}/upper.PNG"),
    ("prepare_image", "{tmp}/a/b.png"),
    ("prepare_image", "{tmp}/a/nested/deep.png"),
    ("prepare_image", "{tmp}/.hidden.png"),
    ("prepare_image", "{tmp}/a..png"),
    # pathlib cleans a path lexically before the extension is read.
    ("prepare_image", "{tmp}/a//b.png"),
    ("prepare_image", "{tmp}/a/./b.png"),
    ("prepare_image", "{tmp}/a/../b.png"),
    ("prepare_image", "{tmp}/a///b.png"),
    ("prepare_image", "{tmp}/a/b.png/"),
    ("prepare_image", "{tmp}/./red.png"),
    # The home directory is expanded before the file is looked for.
    ("prepare_image", "~/shisa-de-no-such-image.png"),
    ("prepare_image", "{home}/shisa-de-no-such-image.png"),
    # ...but only when the path has neither a drive nor a root, which is
    # pathlib's guard. A rooted `~` stays a literal component, and the file is
    # looked for under it.
    ("prepare_image", "/~/shisa-de-no-such-image.png"),
    ("prepare_image", "//~/shisa-de-no-such-image.png"),
    ("prepare_image", "/./~/shisa-de-no-such-image.png"),
    ("prepare_image", "a/~/shisa-de-no-such-image.png"),
    # Rejections, in the order the function tests for them.
    ("prepare_image", ""),
    ("prepare_image", "   "),
    ("prepare_image", "photo.gif"),
    ("prepare_image", "photo.bmp"),
    ("prepare_image", "ftp://example.com/cat.png"),
    ("prepare_image", "file:///tmp/cat.png"),
    ("prepare_image", "{tmp}/missing.png"),
    ("prepare_image", "{tmp}/empty.png"),
    ("prepare_image", "{tmp}/fake.png"),
    ("prepare_image", "{tmp}/dir.d/a"),
    ("prepare_image", "{tmp}/no-extension"),
    # The suffix shapes the two Python versions disagree about.
    ("prepare_image", "{tmp}/.png"),
    ("prepare_image", "{tmp}/..png"),
    ("prepare_image", "{tmp}/a."),
    ("prepare_image", "{tmp}/.bashrc"),
    # validate_image_url is the one that must never forward a local path.
    ("validate_image_url", "https://example.com/cat.png"),
    ("validate_image_url", "http://example.com/cat.jpg"),
    ("validate_image_url", "data:image/png;base64,AAAA"),
    ("validate_image_url", "  data:image/png;base64,AAAA  "),
    ("validate_image_url", "{tmp}/red.png"),
    ("validate_image_url", "red.png"),
    ("validate_image_url", ""),
    ("validate_image_url", "   "),
    ("validate_image_url", "data:text/plain;base64,AAAA"),
    ("validate_image_url", "data:image/png;base64,"),
    ("validate_image_url", "data:image/png;base64"),
    ("validate_image_url", "https://"),
    ("validate_image_url", "http://"),
    ("validate_image_url", "ftp://example.com/cat.png"),
    ("validate_image_url", "file:///tmp/cat.png"),
]


def main() -> int:
    home = str(pathlib.Path("~").expanduser())
    tree = pathlib.Path(tempfile.mkdtemp(prefix="shisa-de-images-"))
    try:
        for relative, payload in FILES.items():
            target = tree / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(payload)

        functions = {"prepare_image": prepare_image, "validate_image_url": validate_image_url}

        def restore(text: str) -> str:
            """Put the placeholders back, so the fixture does not carry a temp path.

            Both the returned value and the error message can name the directory
            the file was looked for in, and that directory is different on every
            run.
            """
            return text.replace(str(tree), "{tmp}").replace(home, "{home}")

        rows = []
        for name, template in CASES:
            value = template.replace("{tmp}", str(tree)).replace("{home}", home)
            row: dict[str, object] = {"func": name, "input": template}
            try:
                row["result"] = restore(functions[name](value))
            except Exception as exc:  # noqa: BLE001 -- the message is the record
                row["error"] = restore(str(exc))
            rows.append(row)

        payload = {
            "files": {
                relative: base64.b64encode(data).decode("ascii")
                for relative, data in FILES.items()
            },
            "cases": rows,
        }
        text = json.dumps(payload, indent=2, ensure_ascii=False) + "\n"
        OUT.write_text(text)
        META_OUT.write_text(json.dumps({
            "python_repo": subprocess_revision(),
            "python_version": platform.python_version(),
            "cases": len(rows),
            "files": len(FILES),
            "sha256": hashlib.sha256(text.encode("utf-8")).hexdigest(),
        }, indent=2) + "\n")
        print(f"wrote {OUT.name} and {META_OUT.name}: {len(rows)} cases, {len(FILES)} files")
    finally:
        shutil.rmtree(tree, ignore_errors=True)
    return 0


def subprocess_revision() -> str:
    """The Python repository's commit, so a fixture records what produced it."""
    import subprocess

    try:
        return subprocess.run(
            ["git", "-C", str(PY_REPO), "rev-parse", "HEAD"],
            capture_output=True, text=True, check=True,
        ).stdout.strip()
    except Exception:  # noqa: BLE001 -- provenance, not a result
        return "unknown"


if __name__ == "__main__":
    raise SystemExit(main())
