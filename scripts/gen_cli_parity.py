#!/usr/bin/env python3
"""Record the Python command line's output, and the requests it made.

Every other contract in this port is a recording replayed at test time. The
command line was the one that was not: `docs/PORT.md` says its comparisons "are
not fixtures and are not re-run at test time". This script makes them one. It
runs `shisa_de.cli.main` against a stub endpoint and a stub tokenizer, keeps
stdout, the exit status and every request in order, and the Go test replays each
recorded invocation through `run` and compares all three.

The stub tokenizer is the Go command-line test's, id for id: every codebook code
is 1000+index and the terminator is 101, with anything else encoded one rune at a
time. The ids reach the request bodies as `logprob_token_ids` and the response
keys as `token_id:<id>`, so a recording made against a different scheme could not
be replayed byte for byte. `cmd/shisa-de/main_test.go`'s `cliTokenizerFor` is the
other half of this pair; the two have to move together.

    python scripts/gen_cli_parity.py

Needs `httpx` and the `shisa-de` Python repository next to this one. Output:
testdata/cli_parity.json and testdata/cli_parity.meta.json.
"""

from __future__ import annotations

import contextlib
import hashlib
import io
import json
import pathlib
import re
import subprocess
import sys

import httpx

HERE = pathlib.Path(__file__).resolve().parent
OUT = HERE.parent / "testdata" / "cli_parity.json"
META_OUT = HERE.parent / "testdata" / "cli_parity.meta.json"
PY_REPO = pathlib.Path.home() / "data" / "dev" / "shisa" / "shisa-de"

# The endpoint every recorded invocation names. Nothing dials it: the transport
# is a `MockTransport`, and the Go replay replaces this text with its own stub's
# URL before comparing, because `doctor` prints the endpoint it was given.
STUB_BASE_URL = "http://cli-stub.invalid"

# The served ids the stub lists. The first is the model the command line asks
# for, so `model listed` is `yes`; the second makes the count non-trivial.
STUB_MODELS = ["shisa-ai/shisa-de-2", "something-else"]

# A 1x1 PNG as a data URL, which is what the Go test uses for the same reason:
# `prepare_image` accepts it without a file on disk.
IMAGE_URL = "data:image/png;base64,iVBORw0KGgo="

sys.path.insert(0, str(PY_REPO))

import shisa_de.cli  # noqa: E402
from shisa_de import Noul, Readout  # noqa: E402
from shisa_de.readout import ANSWER_PREFIX, MAX_CODES, codebook  # noqa: E402


# -- the stub -----------------------------------------------------------------


class CLIStubTokenizer:
    """The Go command-line test's stub tokenizer, id for id.

    `cmd/shisa-de/main_test.go`'s `cliTokenizerFor`: every codebook code is
    1000+index, the terminator is 101, and anything else is one rune. The
    encoding is greedy longest-match, which for this word list is the same as
    matching the two-letter codes before the one-letter ones.
    """

    def __init__(self) -> None:
        self.codes = list(codebook())
        self.ids = {code: 1000 + index for index, code in enumerate(self.codes)}
        self.ids[ANSWER_PREFIX] = 101
        self.by_id = {value: word for word, value in self.ids.items()}
        words = sorted(self.ids, key=lambda word: (-len(word), word))
        self.pattern = re.compile("|".join(re.escape(word) for word in words))
        self.code_of = {self.ids[code]: code for code in self.codes}

    def apply_chat_template(self, messages, tokenize=False, add_generation_prompt=True,
                            enable_thinking=False):
        """The pinned Gemma 4 template, spelled the way the Go port spells it.

        The Go command-line stub writes this same string by hand. `Readout`'s
        system-render comparison measures the server's prompt tokens against what
        the client would have rendered, so a stub with a different template would
        report `differs` for every server and the recording would pin that
        instead of the `string` verdict a faithful server gives.
        """
        system, user = messages[0]["content"], messages[1]["content"]
        out = f"<bos><|turn>system\n{system}<turn|>\n<|turn>user\n{user}<turn|>\n<|turn>model\n"
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

    def decode(self, ids) -> str:
        return "".join(self.by_id.get(value) or chr(value) for value in ids)


class CLIStubServer:
    """The three endpoints the command line reaches, answering like vLLM.

    `cmd/shisa-de/main_test.go`'s `cliServer` is the same stub in Go. The two
    have to answer identically or the recording pins this one's shape rather
    than the client's behaviour.
    """

    def __init__(self) -> None:
        self.tokenizer = CLIStubTokenizer()

    # -- the code reads ------------------------------------------------------

    def _score(self, code: str) -> float:
        """A steep spread: the first code wins by a margin no gate can cross.

        The policy stops at the repeated read, so a `doctor --probe` spends one
        completion request. A thought would need a stub that answers
        `token_ids`, which this one does not.
        """
        index = self.tokenizer.codes.index(code)
        return -0.1 if index == 0 else -8.0 - index

    def _completions(self, body: dict) -> httpx.Response:
        prompt = body["prompt"]
        ids = self.tokenizer.encode(prompt) if isinstance(prompt, str) else list(prompt)
        usage = {"prompt_tokens": len(ids), "completion_tokens": 1}
        wanted = body.get("logprob_token_ids") or []
        ranked = sorted(self.tokenizer.codes, key=lambda code: -self._score(code))
        top = {f"token_id:{self.tokenizer.ids[code]}": self._score(code) for code in ranked}
        if wanted:
            # The server honours `logprob_token_ids`: the answer is read from
            # exactly the codes the client named.
            top = {f"token_id:{token}": self._score(self.tokenizer.code_of[token])
                   for token in wanted}
        best = ranked[0]
        return httpx.Response(200, json={
            "choices": [{"text": best, "logprobs": {"top_logprobs": [top]}}],
            "usage": usage,
        })

    # -- the chat endpoint ---------------------------------------------------

    def _local_render(self, body: dict) -> str:
        """The prompt the client would have rendered itself for this question.

        The image readout cannot render it, but `Readout._system_render` compares
        the server's prompt tokens against exactly this string, so the stub has
        to build the same one.
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
        ids = self.tokenizer.encode(self._local_render(body))
        # The first prompt position carries no logprob, which is what
        # `prompt_logprobs: 0` returns.
        entries = [None] + [
            {str(token): {"logprob": -0.01, "rank": 1,
                          "decoded_token": self.tokenizer.decode([token])}}
            for token in ids[1:]]
        response: dict = {
            "choices": [{}],
            "usage": {"prompt_tokens": len(ids), "completion_tokens": 1},
            "prompt_logprobs": entries,
        }
        if "logprobs" in body:
            # An image read, not the text-only probe: it carries a top-k and an
            # assistant message.
            top_k = int(body["top_logprobs"])
            ranked = sorted(self.tokenizer.codes, key=lambda code: -self._score(code))[:top_k]
            response["choices"] = [{
                "message": {"role": "assistant", "content": ranked[0]},
                "logprobs": {"content": [{
                    "token": ranked[0], "logprob": self._score(ranked[0]),
                    "top_logprobs": [{"token": code, "logprob": self._score(code)}
                                     for code in ranked]}]},
            }]
        return httpx.Response(200, json=response)

    def __call__(self, request: httpx.Request) -> httpx.Response:
        if request.url.path.endswith("/models"):
            return httpx.Response(200, json={"data": [{"id": model} for model in STUB_MODELS]})
        body = json.loads(request.content)
        if request.url.path.endswith("/chat/completions"):
            return self._chat(body)
        return self._completions(body)


# -- recording ----------------------------------------------------------------


class Recorder:
    """One command line over one stub, keeping every request in order."""

    def __init__(self) -> None:
        self.server = CLIStubServer()
        self.requests: list[dict] = []
        self.transport = httpx.MockTransport(self._handle)

    def _handle(self, request: httpx.Request) -> httpx.Response:
        response = self.server(request)
        self.requests.append({
            "method": request.method,
            "path": request.url.path,
            "body": json.loads(request.content) if request.content else None,
        })
        return response

    def run(self, argv: list[str]) -> tuple[int, str, str, str]:
        """`shisa_de.cli.main(argv)` with the stub readout in place.

        `main` builds its own `DecisionModel`, so the injection is a replacement
        of the name it imported. The readout is then handed the stub tokenizer
        directly: the real one is a download, and the ids have to be this
        script's or the recorded request bodies could not be replayed.

        Returns the status, both streams, and how the command ended. An escaping
        exception is `uncaught`: the Python command line has no handler around
        the command body, so a refusal from the library leaves as a traceback
        where this port reports it. The status is 1 either way, which is what
        the replay compares.
        """
        real = shisa_de.cli.DecisionModel
        server = self.server

        def factory(*args, **kwargs):
            kwargs["transport"] = self.transport
            model = real(*args, **kwargs)
            model.readout._tokenizer = server.tokenizer
            return model

        stdout, stderr = io.StringIO(), io.StringIO()
        ended = "returned"
        shisa_de.cli.DecisionModel = factory
        try:
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                try:
                    status = shisa_de.cli.main(argv)
                except SystemExit as exc:  # argparse's error path
                    status = exc.code if isinstance(exc.code, int) else 2
                    ended = "systemexit"
                except BaseException:  # noqa: BLE001 -- recorded, not handled
                    status = 1
                    ended = "uncaught"
        finally:
            shisa_de.cli.DecisionModel = real
        return status, stdout.getvalue(), stderr.getvalue(), ended

    def take(self) -> list[dict]:
        requests, self.requests = self.requests, []
        return requests


def image_state() -> str:
    return json.dumps({"note": "one image"})


def cases() -> list[dict]:
    """The invocations to record, in the order the fixture holds them.

    The scope is the command line's image surface and the doctor line the image
    path added: `--image`, `--image-top-logprobs`, the image-only state rule, and
    `chat system` from `chat_system_render`. The text `ask` is here for contrast,
    so a change that broke both paths is not recorded as an image-only change.
    """
    common = ["--model", "shisa-ai/shisa-de-2", "--base-url", STUB_BASE_URL]
    return [
        {"name": "doctor", "argv": ["doctor", *common]},
        {"name": "doctor-json", "argv": ["doctor", *common, "--json"]},
        {"name": "doctor-probe", "argv": ["doctor", *common, "--probe"]},
        {"name": "doctor-probe-json", "argv": ["doctor", *common, "--probe", "--json"]},
        {"name": "ask-text", "argv": ["ask", *common, "--labels", "billing,outage"]},
        {"name": "ask-image", "argv": ["ask", *common, "--image", IMAGE_URL,
                                       "--labels", "billing,outage"]},
        {"name": "ask-image-top-logprobs",
         "argv": ["ask", *common, "--image", IMAGE_URL, "--image-top-logprobs", "30",
                  "--labels", "billing,outage"]},
        {"name": "ask-image-explicit-state",
         "argv": ["ask", *common, "--image", IMAGE_URL, "--state", image_state(),
                  "--labels", "billing,outage"]},
        {"name": "ask-image-probabilities",
         "argv": ["ask", *common, "--image", IMAGE_URL, "--labels", "billing,outage",
                  "--probabilities", "--confidence"]},
        # Labels in an order that is not alphabetical, so the distribution's key
        # order is visible: Python writes a dict in the order the options were
        # listed, and a Go map would write them sorted.
        {"name": "ask-image-probabilities-unsorted",
         "argv": ["ask", *common, "--image", IMAGE_URL, "--labels", "outage,billing",
                  "--probabilities"]},
        # The one case whose stderr is not comparable: the Python command line
        # lets the `ImageError` escape as a traceback, where this port reports
        # it. Only the status is recorded; the difference is in docs/PORT.md.
        {"name": "ask-image-empty", "argv": ["ask", *common, "--image", "",
                                             "--labels", "billing,outage"],
         "stdout_only": True},    ]


def git_head(repo: pathlib.Path) -> str:
    return subprocess.run(["git", "-C", str(repo), "rev-parse", "HEAD"],
                          capture_output=True, text=True, check=True).stdout.strip()


def main() -> int:
    # A key in the environment would add an Authorization header to every
    # request, which says nothing about the command line.
    import os
    os.environ.pop("SHISA_API_KEY", None)

    recorded = []
    for case in cases():
        recorder = Recorder()
        status, stdout, stderr, ended = recorder.run(case["argv"])
        entry = {
            "name": case["name"],
            "argv": case["argv"],
            "status": status,
            "ended": ended,
            "stdout": stdout.replace(STUB_BASE_URL, "{{base_url}}"),
            "requests": recorder.take(),
        }
        if not case.get("stdout_only"):
            entry["stderr"] = stderr
        recorded.append(entry)
        print(f"  {case['name']}: exit {status} ({ended}), "
              f"{len(entry['requests'])} requests", file=sys.stderr)

    body = json.dumps({"cases": recorded}, indent=2, ensure_ascii=False) + "\n"
    OUT.parent.mkdir(parents=True, exist_ok=True)
    # newline="" so the bytes are the same on every platform: the default
    # translates \n to os.linesep, which would break the recorded digest.
    OUT.write_text(body, encoding="utf-8", newline="")

    meta = {
        "python_repo": git_head(PY_REPO),
        "stub_base_url": STUB_BASE_URL,
        "stub_models": STUB_MODELS,
        "cases": len(recorded),
        "requests": sum(len(case["requests"]) for case in recorded),
        "sha256": hashlib.sha256(body.encode("utf-8")).hexdigest(),
    }
    META_OUT.write_text(json.dumps(meta, indent=2) + "\n", encoding="utf-8", newline="")
    print(f"{OUT}: {meta['cases']} cases, {meta['requests']} requests; "
          f"{META_OUT}: {meta['sha256'][:12]}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
