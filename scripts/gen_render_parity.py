#!/usr/bin/env python3
"""Write the render parity vectors the Go tests check against.

The Go client renders the DE-2 prompt itself instead of running the
checkpoint's Jinja template, and serializes the user turn itself instead of
calling `json.dumps`. Both have to be byte-identical to what the Python client
produces, because the prompt bytes are the tokens the model sees.

This script runs the Python client for real -- `Readout._payload` and
`Readout.render` -- over a fixture set and records the payload, the prompt, and
the token count. The Go test rebuilds each case and compares.

    python scripts/gen_render_parity.py

Needs `transformers` (no torch) and the `shisa-de` Python repository next to
this one. Output: testdata/render_parity.json and
 testdata/render_parity.meta.json.

The model and revision are pinned below and passed to `Readout` as
`tokenizer_revision`, so the prompts are rendered and tokenized by the same
snapshot the Go tokenizer is pinned to; the meta file records it. The Go test
reads the meta file and fails when the fixture's digest or counts move.
"""

from __future__ import annotations

import hashlib
import json
import pathlib
import sys

import tokenizers
import transformers

HERE = pathlib.Path(__file__).resolve().parent
OUT = HERE.parent / "testdata" / "render_parity.json"
META_OUT = HERE.parent / "testdata" / "render_parity.meta.json"
PY_REPO = pathlib.Path.home() / "data" / "dev" / "shisa" / "shisa-de"

# The same pin the Go tokenizer uses (tokenizer.PinnedRepo/PinnedSnapshot).
MODEL = "shisa-ai/shisa-de-2"
REVISION = "8f21734b27b3462471c4662d5638e52195b005a3"

# tokenizer.PinnedVocabSize and tokenizer.PinnedCloseID. Checked below and
# recorded in the meta, so a generation against a different model fails here
# rather than producing a fixture whose provenance looks right.
VOCAB_SIZE = 262144
CLOSE_ID = 101

sys.path.insert(0, str(PY_REPO))

from shisa_de import Choice, Noul, Readout, Score  # noqa: E402
from shisa_de.questions import MAX_OPTIONS  # noqa: E402
from shisa_de.readout import MAX_CODES  # noqa: E402

SPAM_STATE = {
    "sms": "WINNER!! You have won a $1000 gift card. Claim it now: bit.ly/xyz",
    "sender": "+1-555-0199",
}
SPAM_CRITERIA = {
    "spam": "Unsolicited bulk or scam message",
    "ham": "Ordinary message",
}


def choice_criteria(*pairs: tuple[str, object]) -> list[dict]:
    return [{"key": key, "description": description} for key, description in pairs]


def build(kind: str, instructions: str, criteria: list[dict] | None, levels: list | None):
    if kind == "noul":
        return Noul(instructions)
    if kind == "choice":
        return Choice(instructions, {c["key"]: c["description"] for c in criteria})
    if kind == "score":
        return Score(instructions, list(levels))
    raise ValueError(kind)


def cases() -> list[dict]:
    """Every shape the readout can render, with the state that makes it real."""
    out: list[dict] = []

    def add(name, kind, instructions, state, *, criteria=None, levels=None,
            repeat=2, thinking=False, option_limit=MAX_OPTIONS):
        out.append({
            "name": name, "kind": kind, "instructions": instructions,
            "criteria": criteria, "levels": levels, "state": state,
            "repeat": repeat, "thinking": thinking, "option_limit": option_limit,
        })

    # The two questions on the readout page, which fix the measured scaffold.
    add("spam-choice-repeat2", "choice", "Is this message spam?", SPAM_STATE,
        criteria=choice_criteria(*SPAM_CRITERIA.items()))
    add("spam-choice-repeat1", "choice", "Is this message spam?", SPAM_STATE,
        criteria=choice_criteria(*SPAM_CRITERIA.items()), repeat=1)
    add("spam-choice-direct-think", "choice", "Is this message spam?", SPAM_STATE,
        criteria=choice_criteria(*SPAM_CRITERIA.items()), repeat=1, thinking=True)
    add("spam-noul-repeat2", "noul",
        "Does this message ask the reader to click a link?", SPAM_STATE)
    add("spam-score-repeat2", "score", "How risky is acting on this message?", SPAM_STATE,
        levels=["Safe", "Suspicious", "Dangerous"])
    add("jp-choice-repeat2", "choice", "この文章はポジティブですか？",
        {"text": "私は昨日、東京の新しいカフェに行きました。コーヒーがとても美味しかったです。"},
        criteria=choice_criteria(("positive", "ポジティブ"), ("negative", "ネガティブ"),
                                 ("neutral", "中立")))

    # The empty-description rule.
    add("blank-descriptions", "choice", "Which label applies?", {"text": "hello"},
        criteria=choice_criteria(("alpha", ""), ("beta", None), ("gamma", "   "),
                                 ("delta", "a real description")))
    add("non-string-descriptions", "choice", "Which label applies?", {"text": "hello"},
        criteria=choice_criteria(("a", 1), ("b", 2.5), ("c", ["x", "y"]),
                                 ("d", {"k": "v"}), ("e", True), ("f", None)))
    add("key-only-labels", "choice", "Which label applies?", {"text": "hello"},
        criteria=[{"key": "only", "description": None}, {"key": "other", "description": None}])

    # Evidence shapes. Anything a caller can hand over has to serialize the way
    # Python serializes it.
    for name, state in [
        ("evidence-string", "just a string"),
        ("evidence-int", 42),
        ("evidence-float", 3.14159265358979),
        ("evidence-small-float", 1.5e-09),
        ("evidence-large-float", 1e16),
        ("evidence-integral-float", 1.0),
        ("evidence-bool", True),
        ("evidence-null", None),
        ("evidence-list", [1, "two", None, True, 2.5]),
        ("evidence-nested", {"a": {"b": {"c": [1, 2, {"d": None}]}}}),
        ("evidence-empty-object", {}),
        ("evidence-empty-list", []),
        ("evidence-html", {"t": "<b>&amp;</b> <script>x</script>"}),
        ("evidence-control", {"t": "line\n\ttab\r\bform\f\u0000null\u001funit"}),
        ("evidence-quotes", {"t": 'quote " backslash \\ slash / tilde ~'}),
        ("evidence-unicode", {"t": "日本語 🙃 é ü \u200b \u00a0 ﬁ ǅ"}),
        ("evidence-numbers", {"i": 0, "neg": -17, "f": -0.5, "exp": 6.02e23, "tiny": 1e-7}),
    ]:
        add(name, "choice", "Which label applies?", state,
            criteria=choice_criteria(("yes", "Yes"), ("no", "No")))

    # Width. 26 is the letter limit, 27 the first codebook code, 256 the codebook.
    for width in (2, 26, 27, 77, 256):
        add(f"wide-{width}", "choice", "Which queue applies?", {"text": "x"},
            criteria=choice_criteria(*[(f"queue_{i}", f"Support queue {i}") for i in range(width)]),
            option_limit=MAX_CODES)
    add("wide-256-direct", "choice", "Which queue applies?", {"text": "x"},
        criteria=choice_criteria(*[(f"queue_{i}", f"Support queue {i}") for i in range(256)]),
        repeat=1, option_limit=MAX_CODES)

    # Long evidence, which is where the prompt-token count and the tokenizer's
    # single-pre-token merge both matter.
    add("long-evidence", "choice", "Is this message spam?",
        {"text": "the quick brown fox jumps over the lazy dog. " * 40},
        criteria=choice_criteria(*SPAM_CRITERIA.items()))

    return out


def main() -> int:
    readout = Readout(base_url="http://unused.invalid", model=MODEL,
                      tokenizer_revision=REVISION)
    tokenizer = readout.ensure_tokenizer()
    vocab_size = len(tokenizer)
    close_id = tokenizer.convert_tokens_to_ids("<channel|>")
    if vocab_size != VOCAB_SIZE or close_id != CLOSE_ID:
        raise SystemExit(f"{MODEL}@{REVISION} is not the pinned tokenizer: "
                         f"vocab {vocab_size}, <channel|> {close_id}")

    vectors = []
    for case in cases():
        question = build(case["kind"], case["instructions"], case["criteria"], case["levels"])
        limit = case["option_limit"]
        payload, _ = readout._payload(case["state"], question, limit)
        payload_json = json.dumps(payload, ensure_ascii=False)
        prompt = readout.render(case["state"], question, repeat=case["repeat"],
                                thinking=case["thinking"], max_options=limit)
        tokens = tokenizer.encode(prompt, add_special_tokens=False)
        vectors.append({**case, "payload_json": payload_json, "prompt": prompt, "tokens": tokens})

    # Serialization fixed points: Python's own output has to be what the Go
    # encoder produces from the value Python's parser would read back.
    serialization = []
    for case in vectors:
        serialization.append({"name": case["name"], "json": case["payload_json"]})
    for name, value in [
        ("scalars", {"a": 1, "b": 1.0, "c": True, "d": None, "e": "s"}),
        ("floats", [0.0, -0.0, 1.0, 1e15, 1e16, 1e21, 1e-4, 1e-5, 1.5e-9, 6.02e23,
                    -2.5, 1 / 3, 2 ** 53, 1e-323, 1.7976931348623157e308]),
        ("strings", ["", "a", "\n\t\r\b\f", "\u0000\u001f", '"\\', "<&>", "日本語",
                     "🙃", "\u2028\u2029", "é"]),
        ("nested", {"x": [{"y": [1, 2, 3]}, [], {}], "z": {"w": None}}),
    ]:
        serialization.append({"name": name, "json": json.dumps(value, ensure_ascii=False)})

    OUT.parent.mkdir(parents=True, exist_ok=True)
    body = json.dumps({"renders": vectors, "serialization": serialization},
                      ensure_ascii=False) + "\n"
    # newline="" so the bytes are the same on every platform: the default
    # translates \n to os.linesep, which would break the recorded digest.
    OUT.write_text(body, encoding="utf-8", newline="")
    total_tokens = sum(len(case["tokens"]) for case in vectors)
    meta = {
        "model": MODEL,
        "revision": REVISION,
        "transformers": transformers.__version__,
        "tokenizers": tokenizers.__version__,
        "vocab_size": vocab_size,
        "close_id": close_id,
        "renders": len(vectors),
        "serialization": len(serialization),
        "tokens": total_tokens,
        "sha256": hashlib.sha256(body.encode("utf-8")).hexdigest(),
    }
    META_OUT.write_text(json.dumps(meta, indent=2) + "\n", encoding="utf-8", newline="")
    print(f"{OUT}: {len(vectors)} renders, {len(serialization)} serialization cases; "
          f"{META_OUT}: {meta['sha256'][:12]}", file=sys.stderr)
    for case in vectors:
        if case["name"] in ("spam-choice-repeat2", "spam-choice-repeat1", "jp-choice-repeat2"):
            print(f"  {case['name']}: {len(case['tokens'])} tokens", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
