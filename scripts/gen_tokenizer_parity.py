#!/usr/bin/env python3
"""Write the tokenizer parity vectors the Go tests check against.

The Go tokenizer is a reimplementation of Hugging Face's BPE. The only way to
know it is faithful is to compare it against Hugging Face on the same strings,
so this script records what `transformers` produces and the Go test replays it.

    python scripts/gen_tokenizer_parity.py

Needs `transformers` (no torch) and network access to the Hub on first run.

Three files are written:

  testdata/tokenizer_parity.json         a list of {"text", "ids"}
  testdata/tokenizer_decode_parity.json  a list of {"ids", "text"}
  testdata/tokenizer_parity.meta.json    the provenance: model, revision,
                                         library versions, counts

The model and revision are pinned below and passed to `from_pretrained`, so
regenerating cannot silently swap the oracle. The Go test reads the meta file
and asserts the counts, so a corpus that shrinks is a failure rather than a
smaller green run.
"""

from __future__ import annotations

import hashlib
import json
import os
import pathlib
import random
import sys

import tokenizers
import transformers
from transformers import AutoTokenizer

MODEL = "shisa-ai/shisa-de-2"
REVISION = "8f21734b27b3462471c4662d5638e52195b005a3"
HERE = pathlib.Path(__file__).resolve().parent
TESTDATA = HERE.parent / "testdata"
OUT = TESTDATA / "tokenizer_parity.json"
DECODE_OUT = TESTDATA / "tokenizer_decode_parity.json"
META_OUT = TESTDATA / "tokenizer_parity.meta.json"

# The Python reference checkout, used for real source text. Override with
# SHISA_DE_PY_REPO; the corpus fails loudly if a file it names is missing.
PY_REPO = pathlib.Path(os.environ.get("SHISA_DE_PY_REPO", pathlib.Path.home() / "data" / "dev" / "shisa" / "shisa-de"))
PY_FILES = ("shisa_de/readout.py", "shisa_de/client.py", "shisa_de/policy.py")
LOCAL_FILES = ("README.md", "AGENTS.md", "docs/READOUT-DE2.md")


def read_required(path: pathlib.Path) -> str:
    if not path.exists():
        raise SystemExit(f"corpus input missing: {path} (set SHISA_DE_PY_REPO for the Python checkout)")
    return path.read_text(encoding="utf-8")


def corpus() -> list[str]:
    """Strings that exercise every branch of the pipeline."""
    texts: list[str] = []

    # Added tokens, alone and adjacent, including the prefix pairs <|tool> /
    # <|tool_call> / <|tool_response> where the longest match has to win.
    added = [
        "<pad>", "<eos>", "<bos>", "<unk>", "<mask>", "<|tool>", "<tool|>",
        "<|tool_call>", "<tool_call|>", "<|tool_response>", "<tool_response|>",
        '<|"|>', "<|think|>", "<|channel>", "<channel|>", "<|turn>", "<turn|>",
        "<|image>", "<|audio>", "<|image|>", "<|audio|>", "<image|>", "<audio|>",
        "<|video|>", "<|tool><|tool_call>", "<|tool_call><|tool_response>",
        "<|tool_response>x", "a<|turn>b", "<channel|>A", "<channel|>AA",
        # Adjacent <unk> is the case that catches fuse_unk applied to added
        # tokens: Hugging Face gives [3, 3], not [3].
        "<unk><unk>", "<unk><unk><unk>", "a<unk><unk>b", "<unk> <unk>",
        "<unk><|turn><unk>",
    ]
    texts.extend(added)

    # The rendered scaffold pieces, byte for byte from docs/READOUT-DE2.md.
    system = (
        "Apply the supplied criterion to the supplied evidence. Choose exactly one listed "
        "option. Respond with only its uppercase letter, with no explanation or reasoning."
    )
    think_system = (
        "Apply the supplied criterion to the supplied evidence. Choose exactly one listed "
        "option. Reason through the question step by step before you answer. When your "
        "reasoning is complete, respond with only its uppercase letter."
    )
    repeat = "\n\nRead the same input again before answering:\n"
    texts.extend([
        "<bos><|turn>system\n" + system + "<turn|>\n<|turn>user\n{}\n<turn|>\n<|turn>model\n<|channel>thought\n<channel|>",
        "<bos><|turn>system\n<|think|>\n" + think_system + "<turn|>\n<|turn>user\n{}\n<turn|>\n<|turn>model\n",
        repeat,
        "{}" + repeat + "{}",
        "<channel|>" + "A",
    ])

    # The DE-2 codebook: every code is one token, and the boundary check depends
    # on it.
    codebook = json.loads((HERE.parent / "data" / "codebook-de2.json").read_text())["codes"]
    texts.extend(codebook)
    texts.extend("<channel|>" + code for code in codebook)
    texts.extend("".join(codebook[i:i + 16]) for i in range(0, len(codebook), 16))

    # JSON payloads of the shape the client sends.
    texts.append(json.dumps({
        "evidence": {"sms": "WINNER!! You have won a $1000 gift card. Claim it now: bit.ly/xyz",
                     "sender": "+1-555-0199"},
        "criterion": "Is this message spam?",
        "options": [{"letter": "A", "description": "Unsolicited bulk or scam message"},
                    {"letter": "B", "description": "Ordinary message"}],
    }, ensure_ascii=False))
    texts.append(json.dumps({"evidence": {"text": "私は昨日、東京の新しいカフェに行きました。コーヒーがとても美味しかったです。"},
                             "criterion": "この文章はポジティブですか？",
                             "options": [{"letter": "A", "description": "ポジティブ"}]}, ensure_ascii=False))
    texts.append(json.dumps({"evidence": {"a": 1.5e-9, "b": None, "c": True}, "criterion": "x"}, ensure_ascii=False))

    # Whitespace and boundary shapes.
    texts.extend([
        "", " ", "  ", "   ", "\n", "\t", "\n\n\n", "\r\n", "\u00a0", "\u3000",
        "a", "ab", "a b", " a b ", "a  b", "▁", "▁▁", "a▁b", "▁a",
        "the quick brown fox jumps over the lazy dog",
        "x" * 300, "▁" * 300, "ab" * 200, " " * 200,
        "one two three four five six seven eight nine ten " * 40,
        "\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n",
    ])

    # Scripts, combining marks, and characters outside the base vocab.
    texts.extend([
        "日本語のテキストです。",
        "한국어 텍스트",
        "Ελληνικά",
        "العربية",
        "עברית",
        "🙂🙃🎉",
        "e\u0301", "a\u0301\u0302\u0303", "ñ", "ß", "ﬁ", "ǅ",
        "\u200b", "\ufeff", "\u0000", "\u001b[31mred\u001b[0m",
        "\U0001F600\U0001F601\U0001F602",
        "mixed 日本語 and English 123",
        "— “quoted” – … •",
    ])

    # Real text from the repository, which is what a caller will actually send.
    for name in LOCAL_FILES:
        body = read_required(HERE.parent / name)
        for start in range(0, min(len(body), 24_000), 2_000):
            texts.append(body[start:start + 2_000])
    for name in PY_FILES:
        body = read_required(PY_REPO / name)
        for start in range(0, min(len(body), 20_000), 2_000):
            texts.append(body[start:start + 2_000])

    # Random strings over an alphabet that mixes ASCII, punctuation, the
    # word-boundary marker, control characters and multi-byte scripts.
    rng = random.Random(20261010)
    alphabet = (
        list("abcXYZ 019\t\n.,!?-_'\"/\\{}[]:;()<|>") +
        ["▁", "\u00a0", "\u200b", "é", "ü", "日本", "語", "テ", "🙂", "🎉", "\u0301", "\u0000"]
    )
    for _ in range(400):
        length = rng.randint(1, 80)
        texts.append("".join(rng.choice(alphabet) for _ in range(length)))

    # A wide 256-option question, the widest prompt the readout can send, which
    # is also the tokenizer's performance case.
    options = [{"letter": code, "description": f"Support queue {i}"}
               for i, code in enumerate(codebook)]
    payload = {"evidence": {"text": "x"}, "criterion": "Which queue?", "options": options}
    user = json.dumps(payload, ensure_ascii=False)
    texts.append("<bos><|turn>system\n" + system + "<turn|>\n<|turn>user\n" +
                 repeat.join([user, user]) + "<turn|>\n<|turn>model\n<|channel>thought\n<channel|>")

    # Deduplicate, keeping the first occurrence's order.
    seen: set[str] = set()
    unique: list[str] = []
    for text in texts:
        if text not in seen:
            seen.add(text)
            unique.append(text)
    return unique


def decode_cases() -> list[list[int]]:
    """Id sequences whose decode is compared, including byte-fallback runs."""
    cases: list[list[int]] = []

    # Every code, alone and after the closing channel token.
    codebook = json.loads((HERE.parent / "data" / "codebook-de2.json").read_text())
    for code, ids in list(codebook["token_ids"].items())[:32]:
        cases.append([ids])
        cases.append([codebook["token_ids"]["A"], ids])

    # Byte-fallback runs: the pinned vocab's byte tokens are ids 238..493.
    def bytes_ids(values: list[int]) -> list[int]:
        return [238 + value for value in values]

    cases.extend([
        bytes_ids([0x80]),                       # one invalid byte
        bytes_ids([0x80, 0x81]),                 # two invalid bytes
        bytes_ids([0xFF]),                       # an invalid byte
        bytes_ids([0xE1, 0x80, 0xC2]),           # an invalid sequence
        bytes_ids([0xE6, 0x97]),                 # a truncated character
        bytes_ids([0xF0, 0x9F, 0x99]),           # a truncated emoji
        bytes_ids([0xF0, 0x9F, 0x99, 0x82]),     # a valid emoji
        bytes_ids([0xC2, 0xA9]),                 # a valid two-byte character
        bytes_ids([0xE1, 0x80, 0x80]),           # a valid three-byte character
        bytes_ids([0xE6, 0x97, 0xA5]),           # a valid CJK character
        bytes_ids([0xC2, 0xA9, 0xFF]),           # valid then invalid
        bytes_ids([0xFF, 0xC2, 0xA9]),           # invalid then valid
        bytes_ids([0xC2, 0xA9, 0x80, 0xC2, 0xA9]),  # a run split around an invalid byte
        bytes_ids([0xED, 0xA0, 0x80]),           # an encoded surrogate
    ])

    # A byte run broken by an ordinary token.
    a = 236776
    cases.extend([[a] + bytes_ids([0xE6, 0x97]), bytes_ids([0xE6, 0x97]) + [a]])
    return cases


def main() -> int:
    tokenizer = AutoTokenizer.from_pretrained(MODEL, revision=REVISION)
    texts = corpus()
    vectors = [{"text": text, "ids": tokenizer.encode(text, add_special_tokens=False)}
               for text in texts]
    decodes = [{"ids": ids, "text": tokenizer.decode(ids)} for ids in decode_cases()]

    TESTDATA.mkdir(parents=True, exist_ok=True)
    body = json.dumps(vectors, ensure_ascii=False) + "\n"
    OUT.write_text(body, encoding="utf-8")
    DECODE_OUT.write_text(json.dumps(decodes, ensure_ascii=False) + "\n", encoding="utf-8")

    total_ids = sum(len(vector["ids"]) for vector in vectors)
    meta = {
        "model": MODEL,
        "revision": REVISION,
        "transformers": transformers.__version__,
        "tokenizers": tokenizers.__version__,
        "strings": len(vectors),
        "ids": total_ids,
        "decode_cases": len(decodes),
        "sha256": hashlib.sha256(body.encode("utf-8")).hexdigest(),
    }
    META_OUT.write_text(json.dumps(meta, indent=2) + "\n", encoding="utf-8")
    print(f"{OUT}: {len(vectors)} strings, {total_ids} ids; "
          f"{DECODE_OUT}: {len(decodes)} cases; {META_OUT}: {meta['sha256'][:12]}",
          file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
