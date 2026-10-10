#!/usr/bin/env python3
"""Write the tokenizer parity vectors the Go tests check against.

The Go tokenizer is a reimplementation of Hugging Face's BPE. The only way to
know it is faithful is to compare it against Hugging Face on the same strings,
so this script records (text, token ids) pairs from `transformers` and the Go
test replays them.

    python scripts/gen_tokenizer_parity.py

Needs `transformers` (no torch) and network access to the Hub on first run. The
tokenizer is the pinned one: `shisa-ai/shisa-de-2`, snapshot
8f21734b27b3462471c4662d5638e52195b005a3, as recorded in
data/codebook-de2.json.

Output: testdata/tokenizer_parity.json, a list of {"text": ..., "ids": [...]}.
Regenerate when the pinned snapshot changes, and record the snapshot in the
commit that does.
"""

from __future__ import annotations

import json
import os
import pathlib
import random
import sys

from transformers import AutoTokenizer

MODEL = "shisa-ai/shisa-de-2"
HERE = pathlib.Path(__file__).resolve().parent
OUT = HERE.parent / "testdata" / "tokenizer_parity.json"
PY_REPO = pathlib.Path.home() / "data" / "dev" / "shisa" / "shisa-de"


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
    for name in ("README.md", "AGENTS.md", "docs/READOUT-DE2.md"):
        path = HERE.parent / name
        if path.exists():
            body = path.read_text(encoding="utf-8")
            for start in range(0, min(len(body), 24_000), 2_000):
                texts.append(body[start:start + 2_000])
    for name in ("shisa_de/readout.py", "shisa_de/client.py", "shisa_de/policy.py"):
        path = PY_REPO / name
        if path.exists():
            body = path.read_text(encoding="utf-8")
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


def main() -> int:
    tokenizer = AutoTokenizer.from_pretrained(MODEL)
    texts = corpus()
    vectors = [{"text": text, "ids": tokenizer.encode(text, add_special_tokens=False)}
               for text in texts]
    total_ids = sum(len(v["ids"]) for v in vectors)
    total_bytes = sum(len(v["text"]) for v in vectors)
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps(vectors, ensure_ascii=False) + "\n", encoding="utf-8")
    print(f"{OUT}: {len(vectors)} strings, {total_ids} ids, {total_bytes} bytes of text",
          file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
