#!/usr/bin/env python3
"""Record the DE-2 readout's request/response pairs and results.

The Go readout has to send the same requests and derive the same answers as the
Python client, and the only honest way to check that is to run the Python client
and keep what it did. This script drives `Readout.read_codes`, `Readout.think`
and the read after a thought against a vLLM-shaped stub, records every request
body and every response body in order, and records what the Python client made
of them.

The stub tokenizer is the one the Python test suite uses
(`tests/test_offline.py`, `CodeStubTokenizer`): one token per character, with the
two-letter codes and the answer terminator as single tokens. It is copied here
rather than imported because the test module needs pytest, which is not
installed. The prompt is therefore the Python client's render against that stub,
and the Go test uses the recorded prompt text verbatim; the real prompt bytes are
pinned separately by `testdata/render_parity.json`, which was generated with the
pinned checkpoint's tokenizer.

    python scripts/gen_readout_parity.py

Needs `httpx` and the `shisa-de` Python repository next to this one. Output:
testdata/readout_parity.json and testdata/readout_parity.meta.json.
"""

from __future__ import annotations

import hashlib
import json
import pathlib
import re
import subprocess
import sys

import httpx

HERE = pathlib.Path(__file__).resolve().parent
OUT = HERE.parent / "testdata" / "readout_parity.json"
META_OUT = HERE.parent / "testdata" / "readout_parity.meta.json"
PY_REPO = pathlib.Path.home() / "data" / "dev" / "shisa" / "shisa-de"

sys.path.insert(0, str(PY_REPO))

from shisa_de import Choice, Noul, Readout  # noqa: E402
from shisa_de.questions import MAX_OPTIONS  # noqa: E402
from shisa_de.readout import (  # noqa: E402
    ANSWER_PREFIX,
    DE2_READOUT_VERSION,
    MAX_CODES,
    THINK_SYSTEM,
    codebook,
)

# The Python test suite's ANSWER_TOKEN_ID.
ANSWER_TOKEN_ID = 101


# -- the stub server ----------------------------------------------------------


class CodeStubTokenizer:
    """One token per character, with the codes and the terminator as one token."""

    def __init__(self) -> None:
        words = [ANSWER_PREFIX] + [code for code in codebook() if len(code) > 1]
        self.ids = {word: 0x110000 + index for index, word in enumerate(words)}
        self.ids[ANSWER_PREFIX] = ANSWER_TOKEN_ID
        self.words = {value: word for word, value in self.ids.items()}
        self.pattern = re.compile(
            "|".join(re.escape(word) for word in sorted(words, key=len, reverse=True)))

    def apply_chat_template(self, messages, tokenize=False, add_generation_prompt=True,
                            enable_thinking=False):
        opened = f"<sys>{messages[0]['content']}<user>{messages[1]['content']}<model>"
        return opened if enable_thinking else opened + ANSWER_PREFIX

    def encode(self, text: str, add_special_tokens: bool = False) -> list[int]:
        ids, position = [], 0
        while position < len(text):
            match = self.pattern.match(text, position)
            if match:
                ids.append(self.ids[match.group()])
                position = match.end()
            else:
                ids.append(ord(text[position]))
                position += 1
        return ids

    def decode(self, ids) -> list[str]:
        return "".join(self.words.get(value) or chr(value) for value in ids)


class StubDE2Server:
    """Answers the three DE-2 request shapes the way vLLM does.

    `logprob(code)` scores a code; `thought` is the token ids a thinking request
    generates. `candidate_ids` chooses how the server treats `logprob_token_ids`:
    honoured, ignored (an older server), rejected with a 400, or late (honoured
    until `reject_after` requests have carried it, then rejected). The remaining
    knobs break one thing at a time, for the error scenarios.
    """

    def __init__(self, logprob=None, thought=None, candidate_ids="honoured", top_k=20,
                 after_thought=None, prompt_tokens_delta=0, omit_token_ids=False,
                 fallback_empty=False, text_keys=False, omit_text=False, topk_score=None,
                 reject_after=None):
        self.tokenizer = CodeStubTokenizer()
        self.logprob = logprob or (lambda code: 0.0 if code == "A" else -6.0)
        self.after_thought = after_thought or self.logprob
        self.thought = thought
        self.candidate_ids = candidate_ids
        self.top_k = top_k
        self.prompt_tokens_delta = prompt_tokens_delta
        self.omit_token_ids = omit_token_ids
        self.fallback_empty = fallback_empty
        self.text_keys = text_keys
        self.omit_text = omit_text
        # topk_score is what the top-k ranks by, when it differs from the
        # logprob_token_ids values: the case where a mid-read downgrade changes
        # an answer.
        self.topk_score = topk_score or self.logprob
        # reject_after is how many requests with logprob_token_ids to accept
        # before rejecting the field, so a downgrade can happen mid-read.
        self.reject_after = reject_after
        self.seen_candidate_requests = 0
        self.code_of = {self.tokenizer.encode(code)[0]: code for code in codebook()}

    def __call__(self, request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content)
        prompt = body["prompt"]
        ids = self.tokenizer.encode(prompt) if isinstance(prompt, str) else list(prompt)
        usage = {"prompt_tokens": len(ids) + self.prompt_tokens_delta, "completion_tokens": 1}
        if "stop_token_ids" in body:
            generated = list(self.thought or [])[:body["max_tokens"]]
            if ANSWER_TOKEN_ID in generated:
                generated = generated[:generated.index(ANSWER_TOKEN_ID) + 1]
            choice = {"text": "thinking"}
            if not self.omit_token_ids:
                choice["token_ids"] = generated
            return httpx.Response(200, json={"choices": [choice], "usage": usage})
        score = self.after_thought if isinstance(prompt, list) else self.logprob
        if "prompt_logprobs" in body:
            entry = {} if self.fallback_empty else {
                str(ids[-1]): {"logprob": score(self.code_of[ids[-1]]), "rank": 30,
                               "decoded_token": self.code_of[ids[-1]]}}
            return httpx.Response(200, json={"choices": [{"prompt_logprobs": [None, entry]}],
                                             "usage": usage})
        if "logprob_token_ids" in body and self.candidate_ids != "ignored":
            self.seen_candidate_requests += 1
            if self.candidate_ids == "rejected" or (
                    self.candidate_ids == "late" and self.reject_after is not None and
                    self.seen_candidate_requests > self.reject_after):
                return httpx.Response(
                    400, json={"error": {"message": "extra_forbidden: logprob_token_ids"}})
            assert len(body["logprob_token_ids"]) <= 128
            top = {f"token_id:{token}": score(self.code_of[token])
                   for token in body["logprob_token_ids"]}
        else:
            ranked = sorted(codebook(), key=lambda code: -self.topk_score(code))[:self.top_k]
            top = {code: self.topk_score(code) for code in ranked}
        if self.text_keys:
            # A server that ignores `return_tokens_as_token_ids` and keys the
            # logprobs by decoded text instead.
            top = {self.code_of[int(key.split(":")[1])] if key.startswith("token_id:") else key: value
                   for key, value in top.items()}
        best = max(codebook(), key=score)
        choice = {"logprobs": {"top_logprobs": [top]}}
        if not self.omit_text:
            choice["text"] = best
        return httpx.Response(200, json={"choices": [choice], "usage": usage})


# -- recording ----------------------------------------------------------------


class Recorder:
    """One Readout over one stub, keeping every request and response in order."""

    def __init__(self, server: StubDE2Server) -> None:
        self.server = server
        self.exchanges: list[dict] = []
        self.readout = Readout(base_url="http://replay.invalid", model="de2-test", api_key="",
                               transport=httpx.MockTransport(self._handle))
        self.readout._tokenizer = server.tokenizer

    def _handle(self, request: httpx.Request) -> httpx.Response:
        response = self.server(request)
        self.exchanges.append({
            "method": request.method,
            "path": request.url.path,
            "request": json.loads(request.content),
            "status": response.status_code,
            "response": response.json(),
        })
        return response

    def take(self) -> list[dict]:
        exchanges, self.exchanges = self.exchanges, []
        return exchanges


def read_record(read) -> dict:
    return {
        "logprobs": read.logprobs,
        "probabilities": read.probabilities,
        "requests": read.requests,
        "prompt_tokens": read.prompt_tokens,
        "missing_from_top": read.missing_from_top,
        "ranks": read.ranks,
        "sampled": read.sampled,
        "prompt": read.prompt,
        "top_logprobs": read.top_logprobs,
    }


def thought_record(thought) -> dict:
    return {
        "prompt_ids": thought.prompt_ids,
        "token_ids": thought.token_ids,
        "closed": thought.closed,
        "prompt_tokens": thought.prompt_tokens,
        "text": thought.text,
    }


def error_record(exc: Exception, match: str) -> dict:
    """`match` is a short pattern the message must contain, the way the Python
    test suite matches errors: the full wording is not the contract, but the
    reason has to be the same one."""
    if not re.search(match, str(exc)):
        raise SystemExit(f"the scenario expected an error matching {match!r} and the "
                         f"Python client raised {type(exc).__name__}: {exc}")
    return {"type": type(exc).__name__, "message": str(exc), "match": match}


# -- scenarios ----------------------------------------------------------------


def render(recorder: Recorder, count: int, *, thinking: bool = False, repeat: int = 2) -> str:
    question = Choice("Is this message spam?", {f"k{index}": f"Option {index}"
                                                for index in range(max(count, 2))})
    return recorder.readout.render(
        {"sms": "WINNER!! You have won a $1000 gift card. Claim it now: bit.ly/xyz"},
        question, repeat=repeat, thinking=thinking, max_options=MAX_CODES)


def scenarios() -> list[dict]:
    out: list[dict] = []

    def simple(name: str, count: int, server: StubDE2Server, *, debug: bool = False,
               error_match: str = "") -> dict:
        recorder = Recorder(server)
        prompt = render(recorder, count)
        slots = recorder.readout.code_slots(count)
        try:
            read = recorder.readout.read_codes(prompt, slots, debug=debug)
            result = {"result": read_record(read)}
        except Exception as exc:  # noqa: BLE001 -- recorded as the expected outcome
            if not error_match:
                raise
            result = {"error": error_record(exc, error_match)}
        return {"name": name, "steps": [{
            "op": "read-codes", "prompt": prompt, "codes": count, "debug": debug,
            "exchanges": recorder.take(), **result}]}

    # Every width, including the two that cross the 128-candidate block boundary.
    for count in (2, 26, 27, 128, 129, 256):
        out.append(simple(f"codes-{count}", count, StubDE2Server()))
    out.append(simple("codes-3-debug", 3, StubDE2Server(), debug=True))
    # A server that sends no `choices[0].text`, which is the one place the
    # Python client's `str | None` and Go's string differ.
    out.append(simple("codes-no-text", 2, StubDE2Server(omit_text=True)))
    # A server that ignores `return_tokens_as_token_ids` and keys the logprobs by
    # decoded text, which is the other half of the code lookup.
    out.append(simple("codes-text-keys", 26, StubDE2Server(
        logprob=lambda code: -0.1 * codebook().index(code), text_keys=True)))

    # A server that ignores the field, and one that rejects it. Both fall back to
    # the top-k and pay one request per code outside it.
    out.append(simple("codes-topk-ignored", 30, StubDE2Server(
        logprob=lambda code: -0.1 * codebook().index(code), candidate_ids="ignored")))
    rejected = StubDE2Server(logprob=lambda code: -0.1 * codebook().index(code),
                             candidate_ids="rejected")
    recorder = Recorder(rejected)
    prompt = render(recorder, 30)
    slots = recorder.readout.code_slots(30)
    steps = []
    for step in ("first", "second"):
        read = recorder.readout.read_codes(prompt, slots)
        steps.append({"op": "read-codes", "prompt": prompt, "codes": 30, "debug": False,
                      "note": step, "exchanges": recorder.take(),
                      "result": read_record(read)})
    out.append({"name": "codes-topk-rejected", "steps": steps})

    # A downgrade that happens after the first block was accepted. Python then
    # re-reads every code in one request without the field, so the retry's top-k
    # replaces the codes the first block already read; the top-k scores differ
    # from the candidate-id scores here so that replacement is visible.
    late = StubDE2Server(logprob=lambda code: -0.001 * codebook().index(code),
                         topk_score=lambda code: -0.5 - 0.001 * codebook().index(code),
                         candidate_ids="late", reject_after=1, top_k=256)
    recorder = Recorder(late)
    prompt = render(recorder, 129)
    slots = recorder.readout.code_slots(129)
    read = recorder.readout.read_codes(prompt, slots)
    out.append({"name": "codes-late-downgrade", "steps": [{
        "op": "read-codes", "prompt": prompt, "codes": 129, "debug": False,
        "exchanges": recorder.take(), "result": read_record(read)}]})

    # The thinking read, closed by the model and cut by the budget.
    for name, thought, budget in (
        ("think-closed", [ord(c) for c in " thinking3+4=7"] + [ANSWER_TOKEN_ID, ord("B")], 1024),
        ("think-budget", [ord("x")] * 50, 8),
        ("think-empty", [ANSWER_TOKEN_ID], 1024),
    ):
        server = StubDE2Server(logprob=lambda code: -1.0, thought=thought)
        recorder = Recorder(server)
        prompt = render(recorder, 3, thinking=True, repeat=1)
        try:
            recorded = recorder.readout.think(prompt, budget, debug=True)
            result = {"thought": thought_record(recorded)}
        except Exception as exc:  # noqa: BLE001
            result = {"error": error_record(exc, r"return_token_ids")}
        out.append({"name": name, "steps": [{
            "op": "think", "prompt": prompt, "budget": budget, "debug": True,
            "exchanges": recorder.take(), **result}]})

    # The read after a thought: the thinking prompt's ids, the thought's ids, and
    # one terminator, which is the construction policy.py uses.
    server = StubDE2Server(
        logprob=lambda code: {"A": -0.7, "B": -0.8}.get(code, -3.0),
        thought=[ord(c) for c in " thinking3+4=7"] + [ANSWER_TOKEN_ID, ord("B")],
        after_thought=lambda code: 0.0 if code == "B" else -7.0)
    recorder = Recorder(server)
    prompt = render(recorder, 3, thinking=True, repeat=1)
    slots = recorder.readout.code_slots(3)
    thought = recorder.readout.think(prompt, 1024, debug=True)
    think_exchanges = recorder.take()
    after = thought.prompt_ids + thought.token_ids + [recorder.readout.answer_prefix_id()]
    read = recorder.readout.read_codes(after, slots, debug=True)
    out.append({"name": "post-thought", "steps": [
        {"op": "think", "prompt": prompt, "budget": 1024, "debug": True,
         "exchanges": think_exchanges, "thought": thought_record(thought)},
        {"op": "read-codes-after-thought", "prompt_ids": after, "codes": 3, "debug": True,
         "exchanges": recorder.take(), "result": read_record(read)},
    ]})

    # The fallback after a thought, which is the one place the prompt is token
    # ids rather than text: the code is appended as its token id, not as text.
    server = StubDE2Server(
        logprob=lambda code: 0.0 if code == "A" else -6.0,
        candidate_ids="ignored", top_k=1,
        thought=[ord(c) for c in " thinking"] + [ANSWER_TOKEN_ID],
        after_thought=lambda code: 0.0 if code == "A" else -6.0)
    recorder = Recorder(server)
    prompt = render(recorder, 3, thinking=True, repeat=1)
    slots = recorder.readout.code_slots(3)
    thought = recorder.readout.think(prompt, 1024, debug=True)
    think_exchanges = recorder.take()
    after = thought.prompt_ids + thought.token_ids + [recorder.readout.answer_prefix_id()]
    read = recorder.readout.read_codes(after, slots, debug=True)
    out.append({"name": "post-thought-fallback", "steps": [
        {"op": "think", "prompt": prompt, "budget": 1024, "debug": True,
         "exchanges": think_exchanges, "thought": thought_record(thought)},
        {"op": "read-codes-after-thought", "prompt_ids": after, "codes": 3, "debug": True,
         "exchanges": recorder.take(), "result": read_record(read)},
    ]})

    # Error scenarios. Each breaks one thing.
    out.append(simple("error-prompt-tokens", 3, StubDE2Server(prompt_tokens_delta=1),
                      error_match=r"counted \d+ prompt tokens where the client tokenized \d+"))
    out.append(simple("error-fallback-empty", 30, StubDE2Server(
        logprob=lambda code: -0.1 * codebook().index(code), candidate_ids="ignored",
        fallback_empty=True), error_match=r"prompt_logprobs fallback returned nothing for it"))
    no_token_ids = StubDE2Server(logprob=lambda code: -1.0, thought=[ord("x")],
                                 omit_token_ids=True)
    recorder = Recorder(no_token_ids)
    prompt = render(recorder, 3, thinking=True, repeat=1)
    try:
        recorder.readout.think(prompt, 1024)
        result = {}
    except Exception as exc:  # noqa: BLE001
        result = {"error": error_record(exc, r"return_token_ids")}
    if not result:
        raise SystemExit("error-no-token-ids: the Python client accepted a response with no "
                         "token_ids, so there is nothing to record")
    out.append({"name": "error-no-token-ids", "steps": [{
        "op": "think", "prompt": prompt, "budget": 1024, "debug": False,
        "exchanges": recorder.take(), **result}]})

    # A prompt that does not end on the terminator fails before any request.
    recorder = Recorder(StubDE2Server())
    slots = recorder.readout.code_slots(2)
    try:
        recorder.readout.read_codes("<sys>x<user>y<model>", slots)
        result = {}
    except Exception as exc:  # noqa: BLE001
        result = {"error": error_record(exc, r"does not end on")}
    if not result:
        raise SystemExit("error-boundary: the Python client accepted a prompt that does not "
                         "end on the terminator, so there is nothing to record")
    out.append({"name": "error-boundary", "steps": [{
        "op": "read-codes", "prompt": "<sys>x<user>y<model>", "codes": 2, "debug": False,
        "exchanges": recorder.take(), **result}]})

    return out


def git_head(path: pathlib.Path) -> str:
    try:
        return subprocess.run(["git", "-C", str(path), "rev-parse", "HEAD"],
                              capture_output=True, text=True, check=True).stdout.strip()
    except (subprocess.CalledProcessError, FileNotFoundError):
        return ""


def main() -> int:
    recorded = scenarios()
    tokenizer = CodeStubTokenizer()
    body = json.dumps({
        "tokenizer": {"words": {word: value for word, value in sorted(tokenizer.ids.items())}},
        "scenarios": recorded,
    }, ensure_ascii=False) + "\n"
    OUT.parent.mkdir(parents=True, exist_ok=True)
    # newline="" so the bytes are the same on every platform: the default
    # translates \n to os.linesep, which would break the recorded digest.
    OUT.write_text(body, encoding="utf-8", newline="")

    meta = {
        "python_repo": git_head(PY_REPO),
        "readout_version": DE2_READOUT_VERSION,
        "max_codes": MAX_CODES,
        "max_options": MAX_OPTIONS,
        "answer_token_id": ANSWER_TOKEN_ID,
        "think_system": THINK_SYSTEM,
        "scenarios": len(recorded),
        "exchanges": sum(len(step["exchanges"]) for case in recorded for step in case["steps"]),
        "sha256": hashlib.sha256(body.encode("utf-8")).hexdigest(),
    }
    META_OUT.write_text(json.dumps(meta, indent=2) + "\n", encoding="utf-8", newline="")
    print(f"{OUT}: {meta['scenarios']} scenarios, {meta['exchanges']} exchanges; "
          f"{META_OUT}: {meta['sha256'][:12]}", file=sys.stderr)
    for case in recorded:
        for step in case["steps"]:
            outcome = "error" if "error" in step else "ok"
            print(f"  {case['name']}/{step['op']}: {len(step['exchanges'])} exchanges, {outcome}",
                  file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
