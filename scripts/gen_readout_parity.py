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
        """The pinned Gemma 4 template, spelled the way the Go port spells it.

        The Go readout has no Jinja engine: it renders this one template itself.
        `Readout._system_render` compares the server's prompt tokens against what
        the client would have rendered, so a stub with a different template could
        not check that comparison at all -- it would report `differs` for every
        server.
        """
        system, user = messages[0]["content"], messages[1]["content"]
        out = "<bos><|turn>system\n"
        if enable_thinking:
            out += f"<|think|>\n{system}"
        else:
            out += system
        out += f"<turn|>\n<|turn>user\n{user}<turn|>\n<|turn>model\n"
        if not enable_thinking:
            out += "<|channel>thought\n" + ANSWER_PREFIX
        return out

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
                 reject_after=None, chat_system="string", chat_drop_code="",
                 chat_short_prompt=False, chat_non_option_filler=True, omit_message=False,
                 malformed_top_entry=False):
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
        # chat_system is how the chat endpoint renders the system turn:
        # "string" as the text scaffold does, "space" the way Gemma 4's template
        # renders a system message that arrives as content parts, or "short" a
        # response too small to tell.
        self.chat_system = chat_system
        # chat_drop_code removes one code from the returned top-k, which is the
        # case the image readout has no fallback for.
        self.chat_drop_code = chat_drop_code
        # chat_short_prompt returns two prompt-logprob entries instead of the
        # prompt's own, so `system_render` cannot compare and says so.
        self.chat_short_prompt = chat_short_prompt
        # chat_non_option_filler pads the top-k with tokens that are not
        # options, which the readout has to discard.
        self.chat_non_option_filler = chat_non_option_filler
        self.omit_message = omit_message
        # malformed_top_entry sends a logprob as a string, which the Python
        # client refuses entry by entry rather than as a whole response.
        self.malformed_top_entry = malformed_top_entry
        self.seen_candidate_requests = 0
        self.code_of = {self.tokenizer.encode(code)[0]: code for code in codebook()}

    # -- the chat endpoint -------------------------------------------------

    def _local_render(self, body: dict) -> str:
        """The prompt the client would have rendered itself for this question.

        The image readout cannot render it, but `Readout._system_render`
        compares the server's prompt tokens against exactly this string, so the
        stub has to build the same one.
        """
        messages = body["messages"]
        user = messages[1]["content"]
        if isinstance(user, list):
            user = next(part["text"] for part in user if part["type"] == "text")
        return self.tokenizer.apply_chat_template(
            [{"role": "system", "content": messages[0]["content"]},
             {"role": "user", "content": user}],
            tokenize=False, add_generation_prompt=True, enable_thinking=False)

    def _chat(self, body: dict) -> httpx.Response:
        messages = body["messages"]
        system = messages[0]["content"]
        served = self._local_render(body)
        if self.chat_system == "space":
            # What Gemma 4's template does to a system message that arrives as
            # content parts: one extra space, between the system line and the
            # turn terminator.
            served = served.replace(f"{system}<turn|>", f"{system} <turn|>", 1)
        ids = self.tokenizer.encode(served)
        usage = {"prompt_tokens": len(ids) + self.prompt_tokens_delta,
                 "completion_tokens": 1}
        if self.chat_short_prompt:
            entries = [None, {str(ids[-1]): {"logprob": -0.01, "rank": 1,
                                            "decoded_token": ANSWER_PREFIX}}]
        else:
            entries = [None] + [
                {str(token): {"logprob": -0.01, "rank": 1,
                              "decoded_token": self.tokenizer.decode([token])}}
                for token in ids[1:]]
        response: dict = {"choices": [{}], "usage": usage, "prompt_logprobs": entries}
        if "logprobs" in body:
            top_k = body["top_logprobs"]
            ranked = sorted(codebook(), key=lambda code: -self.logprob(code))
            chosen = [code for code in ranked if code != self.chat_drop_code][:top_k]
            top = [{"token": code, "logprob": self.logprob(code)} for code in chosen]
            if self.malformed_top_entry:
                top[0]["logprob"] = "not a number"
            filler = 0
            while self.chat_non_option_filler and len(top) < top_k:
                top.append({"token": f"z{filler}", "logprob": -99.0 - filler})
                filler += 1
            response["choices"][0]["logprobs"] = {
                "content": [{"token": chosen[0], "logprob": self.logprob(chosen[0]),
                             "top_logprobs": top}]}
            if not self.omit_message:
                response["choices"][0]["message"] = {
                    "role": "assistant", "content": chosen[0]}
        return httpx.Response(200, json=response)

    def __call__(self, request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content)
        if request.url.path == "/v1/chat/completions":
            return self._chat(body)
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

    def __init__(self, server: StubDE2Server, api_key: str = "") -> None:
        self.server = server
        self.api_key = api_key
        self.exchanges: list[dict] = []
        self.readout = Readout(base_url="http://replay.invalid", model="de2-test", api_key=api_key,
                               transport=httpx.MockTransport(self._handle))
        self.readout._tokenizer = server.tokenizer

    def _handle(self, request: httpx.Request) -> httpx.Response:
        response = self.server(request)
        # Only the headers the readout sets itself: httpx adds its own
        # (accept, user-agent, content-length, ...) that say nothing about the
        # client under test.
        headers = {name.lower(): value for name, value in request.headers.items()
                   if name.lower() in ("content-type", "authorization")}
        self.exchanges.append({
            "method": request.method,
            "path": request.url.path,
            "headers": headers,
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
        "messages": read.messages,
        "system_render": read.system_render,
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

    def keyed(name: str, count: int, api_key: str) -> dict:
        """A scenario whose client sends a bearer token, so the recorded
        `Authorization` header is compared rather than assumed."""
        recorder = Recorder(StubDE2Server(), api_key=api_key)
        prompt = render(recorder, count)
        slots = recorder.readout.code_slots(count)
        read = recorder.readout.read_codes(prompt, slots)
        return {"name": name, "api_key": api_key, "steps": [{
            "op": "read-codes", "prompt": prompt, "codes": count, "debug": False,
            "exchanges": recorder.take(), "result": read_record(read)}]}

    # Every width, including the two that cross the 128-candidate block boundary.
    for count in (2, 26, 27, 128, 129, 256):
        out.append(simple(f"codes-{count}", count, StubDE2Server()))
    out.append(simple("codes-3-debug", 3, StubDE2Server(), debug=True))
    # A keyed read, so the recorded Authorization header is compared too.
    out.append(keyed("codes-api-key", 2, "test-key-not-a-secret"))
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

    # The image read. The prompt is not rendered here at all: the server does
    # that, which is why the scenario records the messages instead.
    def image(name: str, count: int, server: StubDE2Server, *, top_logprobs: int = 20,
              debug: bool = False, error_match: str = "",
              require_string_system: bool = False) -> dict:
        recorder = Recorder(server)
        question = Choice("What colour is this?",
                          {f"k{index}": f"Option {index}" for index in range(max(count, 2))})
        state = {"note": "one image"}
        try:
            read = recorder.readout.read_image(
                state, question, "data:image/png;base64,iVBORw0KGgo=",
                top_logprobs=top_logprobs, debug=debug,
                require_string_system=require_string_system)
            result = {"result": read_record(read)}
        except Exception as exc:  # noqa: BLE001 -- recorded as the expected outcome
            if not error_match:
                raise
            result = {"error": error_record(exc, error_match)}
        return {"name": name, "steps": [{
            "op": "read-image", "state": state, "codes": count, "debug": debug,
            "top_logprobs": top_logprobs, "image_url": "data:image/png;base64,iVBORw0KGgo=",
            "require_string_system": require_string_system,
            "exchanges": recorder.take(), **result}]}

    out.append(image("image-basic", 3, StubDE2Server(
        logprob=lambda code: {"A": -0.2, "B": -0.5, "C": -3.0}.get(code, -9.0))))
    out.append(image("image-debug", 3, StubDE2Server(
        logprob=lambda code: {"A": -0.2, "B": -0.5, "C": -3.0}.get(code, -9.0)),
        debug=True))
    # A stock vLLM: the system turn carries one extra space, and the read answers
    # anyway while recording that it did.
    out.append(image("image-system-space", 3, StubDE2Server(
        logprob=lambda code: -0.1 * codebook().index(code), chat_system="space")))
    out.append(image("image-system-space-strict", 3, StubDE2Server(
        logprob=lambda code: -0.1 * codebook().index(code), chat_system="space"),
        require_string_system=True, error_match=r"did not render the system turn"))
    # A response with too few prompt tokens to compare against.
    out.append(image("image-system-unverified", 3, StubDE2Server(
        logprob=lambda code: -0.1 * codebook().index(code), chat_short_prompt=True)))
    # A server that sends no assistant message, which is the one place the Python
    # client's `str | None` and Go's string differ.
    out.append(image("image-no-message", 3, StubDE2Server(
        logprob=lambda code: -0.1 * codebook().index(code), omit_message=True)))
    # A top-k with no non-option filler, so every returned token is an option.
    out.append(image("image-options-only", 3, StubDE2Server(
        logprob=lambda code: -0.1 * codebook().index(code),
        chat_non_option_filler=False), top_logprobs=3))
    # Errors. Each breaks one thing.
    out.append(image("image-error-missing-code", 3, StubDE2Server(
        logprob=lambda code: {"A": -0.2, "B": -0.5, "C": -3.0}.get(code, -9.0),
        chat_drop_code="C"), error_match=r"missing from the returned top"))
    out.append(image("image-error-too-many-options", 21, StubDE2Server(
        logprob=lambda code: -0.1 * codebook().index(code)), top_logprobs=20,
        error_match=r"cannot be read from a top-20 distribution"))
    out.append(image("image-error-malformed-top", 3, StubDE2Server(
        logprob=lambda code: -0.1 * codebook().index(code), malformed_top_entry=True),
        error_match=r"malformed top_logprobs entry"))

    # The chat probe `doctor --probe` reports.
    for name, server in (("probe-string", StubDE2Server()),
                         ("probe-space", StubDE2Server(chat_system="space")),
                         ("probe-short", StubDE2Server(chat_short_prompt=True))):
        recorder = Recorder(server)
        question = Noul("Is this a health check?")
        verdict = recorder.readout.probe_chat_render({"health": "check"}, question)
        out.append({"name": name, "steps": [{
            "op": "probe-chat-render", "state": {"health": "check"},
            "exchanges": recorder.take(), "system_render": verdict}]})

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
    keyed_scenarios = [case for case in recorded if case.get("api_key")]
    print(f"{OUT}: {meta['scenarios']} scenarios, {meta['exchanges']} exchanges, "
          f"{len(keyed_scenarios)} keyed; "
          f"{META_OUT}: {meta['sha256'][:12]}", file=sys.stderr)
    for case in recorded:
        for step in case["steps"]:
            outcome = "error" if "error" in step else "ok"
            print(f"  {case['name']}/{step['op']}: {len(step['exchanges'])} exchanges, {outcome}",
                  file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
