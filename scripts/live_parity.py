#!/usr/bin/env python3
"""Run the fixed live question set through the Python client and record it.

The M6 gate is that both clients agree on a fixed set of questions asked of the
same served checkpoint. This script is the Python half: it runs
`testdata/live_questions.json` through the Python `DecisionModel` against the
hosted DE-2 endpoint and writes the chosen value and the distribution per head.

    SHISA_API_KEY=... python scripts/live_parity.py

`SHISA_DE_API_KEY` and `SHISA_API_KEY` are read first, then a bare `API_KEY`,
which is the order the client itself reads them in.

`TestLiveParityAgainstPython` in `live_test.go` runs this script and then asks
the Go client the same questions, so both clients are measured in one session:

    SHISA_DE_LIVE=1 go test . -run TestLiveParityAgainstPython

It runs the set **twice** and records how far the two passes are apart. The
served checkpoint is deterministic within a session -- three back-to-back reads
of one prompt have come back bit-identical -- but it is not deterministic across
sessions, so a recording made yesterday is not an oracle for a run today. The
self-delta is what makes that measurable: when it is non-zero the server moved
during this run and the comparison against it is inconclusive rather than a
parity failure.

The endpoint and the model come from `SHISA_DE_ENDPOINT` and `SHISA_DE_MODEL`
first, exactly as the client and the Go suite resolve them, so pointing the suite
at another served checkpoint points both halves of the gate at it. A recorder
that read only its own defaults would compare the Go client's local server
against the Python client's hosted one and report the difference as a port
defect.

Output: testdata/live_python.json and testdata/live_python.meta.json by default,
or the path given to `--out`. The numbers are a measurement of a served
checkpoint, so the metadata carries the served `system_fingerprint`, the date,
the Python revision and the tokenizer revision; a number without its fingerprint
is not reproducible.

The exit status is 0 for a repeatable run, 3 when the passes disagreed on the
chosen value, and 4 when the endpoint's vision capability could not be measured;
for 0 and 3 the output and the metadata are written first, so a caller that
treats 3 as "recorded, but the server moved" can read them. The Go test does
exactly that.
"""

from __future__ import annotations

import argparse
import datetime
import functools
import hashlib
import json
import os
import pathlib
import subprocess
import sys
import types
from typing import Any

HERE = pathlib.Path(__file__).resolve().parent
QUESTIONS = HERE.parent / "testdata" / "live_questions.json"
DEFAULT_OUT = HERE.parent / "testdata" / "live_python.json"
PY_REPO = pathlib.Path(os.environ.get(
    "SHISA_DE_PYTHON_REPO", pathlib.Path.home() / "data" / "dev" / "shisa" / "shisa-de"))

sys.path.insert(0, str(PY_REPO))


@functools.cache
def client() -> types.SimpleNamespace:
    """The Python client's names, imported when they are first needed.

    Deferred so this module's pure parts -- the endpoint and model resolution,
    the drift arithmetic, the tokenizer pin -- can be imported and tested without
    a `shisa_de` checkout on `sys.path`.
    """
    from shisa_de import Choice, DecisionModel, Noul, Score
    from shisa_de.readout import READOUT_VERSIONS
    return types.SimpleNamespace(
        Choice=Choice, DecisionModel=DecisionModel, Noul=Noul, Score=Score,
        READOUT_VERSIONS=READOUT_VERSIONS)


@functools.cache
def http() -> types.ModuleType:
    """`httpx`, imported when something is first sent.

    Deferred for the same reason as `client`: nothing above the `Recorder` needs
    it, so importing this module does not.
    """
    import httpx
    return httpx

BASE_URL = "https://api.shisa.ai/openai"
MODEL = "shisa-ai/shisa-de-2"
API_KEY_ENVS = ("SHISA_DE_API_KEY", "SHISA_API_KEY", "API_KEY")

# How far two numbers may differ and still be the same answer. This is the gate's
# tolerance, not the server's: the two clients are `temperature: 0` against a
# deterministic server, so the only expected difference is float arithmetic, and
# the measured one is 1e-16 to 1e-19.
TOLERANCE = 1e-9


def endpoint() -> str:
    """The endpoint, resolved the way the client and the Go suite resolve it."""
    return os.environ.get("SHISA_DE_ENDPOINT", "").strip() or BASE_URL


def model_id() -> str:
    """The checkpoint, resolved the way the client and the Go suite resolve it."""
    return os.environ.get("SHISA_DE_MODEL", "").strip() or MODEL


def api_key() -> str:
    """The key, from the variables the client itself reads."""
    for name in API_KEY_ENVS:
        value = os.environ.get(name, "").strip()
        if value:
            return value
    raise SystemExit("set one of " + ", ".join(API_KEY_ENVS) + " before recording")


def api_url(path: str) -> str:
    """A URL on the endpoint, resolved the way the client resolves one.

    `DecisionModel` strips a trailing `/v1` from the base URL and then appends
    `/v1/...`, so a base given either way lands on the same path. A probe that
    appended `/chat/completions` to the base directly would 404 against the
    default endpoint -- whose base does not end in `/v1` -- and report a
    text-only server, which is a false negative that reads as a verdict.
    """
    base = endpoint().rstrip("/")
    if base.endswith("/v1"):
        base = base[:-3]
    return base + path


#: The probe's three outcomes. A capability and a failure are different things,
#: and only the server's own refusal is evidence of the first.
PROBE_SERVED = "served"
PROBE_REFUSED = "refused"
PROBE_UNKNOWN = "unknown"

#: Words a server uses when it refuses an image because it has no vision. A
#: refusal that does not name one of these is treated as unmeasured, not as a
#: capability: a 404 from a wrong path and a 400 from a malformed body are bugs,
#: not deployment facts.
_NO_VISION_WORDS = ("image", "vision", "multimodal", "modality")


def probe_images(client=None) -> tuple[str, str]:
    """Ask the endpoint for an image read, and classify what comes back.

    The image read is the only read that needs `/v1/chat/completions` with vision
    enabled, and a text-only vLLM is a documented deployment for this client. So
    the image question is asked when the endpoint can serve it and skipped when it
    cannot, rather than making the text gate depend on a capability it does not
    use.

    Three outcomes, because two would conflate a capability with a failure:

    - `PROBE_SERVED` -- the server answered the image request.
    - `PROBE_REFUSED` -- the server answered with an error that names images,
      vision or modality. That is the server's own statement that it cannot, and
      it is the only thing that may become a skip.
    - `PROBE_UNKNOWN` -- a transport error, a 401/403/429, a 5xx, or an error
      that says nothing about images. The caller fails on this rather than
      recording an unmeasured capability as an absent one.

    The probe is one request and it is deliberately the real shape: an image
    content part on a one-option question. It returns the reason as well as the
    verdict so the skip says what the server answered. `client` is injectable so
    the classification can be tested without a socket.
    """
    body = {
        "model": model_id(),
        "messages": [
            {"role": "system", "content": "Answer with one option letter."},
            {"role": "user", "content": [
                {"type": "image_url", "image_url": {"url": _PROBE_IMAGE}},
                {"type": "text", "text": '{"options": ["red"]}'},
            ]},
        ],
        "max_tokens": 1,
        "temperature": 0,
    }
    own = client is None
    if own:
        client = http().Client(timeout=120.0)
    try:
        try:
            response = client.post(
                api_url("/v1/chat/completions"),
                json=body, headers={"Authorization": f"Bearer {api_key()}"})
        except Exception as exc:  # noqa: BLE001 -- the reason is the point
            return PROBE_UNKNOWN, f"the probe could not be sent: {exc}"
    finally:
        if own:
            client.close()
    if response.status_code == 200:
        return PROBE_SERVED, ""
    detail = (f"the endpoint answered {response.status_code} to an image request: "
              f"{response.text[:200]}")
    if response.status_code < 500 and any(word in response.text.lower()
                                         for word in _NO_VISION_WORDS):
        return PROBE_REFUSED, detail
    return PROBE_UNKNOWN, detail


#: A 32x32 solid red PNG, the shape the Python live suite's image test builds.
_PROBE_IMAGE = ("data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAIAAAD8GO2jAAAA"
                "KElEQVR4nO3NsQ0AAAzCMP5/un0CNkuZ41wybXsHAAAAAAAAAAAAxR4yw/wuPL6Q"
                "kAAAAABJRU5ErkJggg==")


def tokenizer_revision() -> str:
    """The revision of the checkpoint's cached tokenizer, or a reason there is none.

    The Go side resolves its tokenizer from the hub the same way, so this pins
    which tokenizer bytes produced the recorded token counts. An empty string
    means the tokenizer is not cached here; anything else that is not a revision
    says why the pin is missing rather than silently looking like one.
    """
    cache = pathlib.Path(os.environ.get(
        "HF_HUB_CACHE", pathlib.Path.home() / ".cache" / "huggingface" / "hub"))
    snapshots = cache / ("models--" + model_id().replace("/", "--")) / "snapshots"
    if not snapshots.is_dir():
        return ""
    revisions = sorted(entry.name for entry in snapshots.iterdir() if entry.is_dir())
    if not revisions:
        return ""
    if len(revisions) > 1:
        return f"ambiguous: {len(revisions)} snapshots cached, none pinned"
    return revisions[0]


def python_revision() -> str:
    """The revision of the Python repository the answers came from."""
    try:
        return subprocess.run(
            ["git", "-C", str(PY_REPO), "rev-parse", "HEAD"],
            capture_output=True, text=True, check=True).stdout.strip()
    except (subprocess.CalledProcessError, FileNotFoundError):
        return ""


class Recorder:
    """A transport that forwards to the real endpoint and keeps the responses.

    Only the served `system_fingerprint` is kept from them; the bodies are not
    recorded, because the Go side asks the server itself rather than replaying.
    """

    def __init__(self) -> None:
        self.http = http()
        self.fingerprints: set[str] = set()
        self.requests = 0
        self.real = self.http.Client(timeout=600.0)

    def handler(self, request: Any) -> Any:
        response = self.real.send(
            self.real.build_request(request.method, request.url, content=request.content,
                                    headers=request.headers))
        self.requests += 1
        if response.status_code != 200:
            raise SystemExit(
                f"the endpoint answered {response.status_code} for {request.url.path}: "
                f"{response.text[:400]}")
        body = response.json()
        if body.get("system_fingerprint"):
            self.fingerprints.add(body["system_fingerprint"])
        return self.http.Response(response.status_code, json=body,
                                  headers={"content-type": "application/json"})

    def fingerprint(self) -> str:
        if len(self.fingerprints) != 1:
            raise SystemExit(
                f"the responses carried {len(self.fingerprints)} system fingerprints: "
                f"{sorted(self.fingerprints)}")
        return self.fingerprints.pop()


def build(spec: dict) -> object:
    """One question spec as the Python question object."""
    client_ = client()
    kind = spec["type"]
    if kind == "noul":
        return client_.Noul(spec["instructions"])
    if kind == "choice":
        return client_.Choice(spec["instructions"],
                              {option["key"]: option["description"] for option in spec["options"]})
    if kind == "score":
        return client_.Score(spec["instructions"], list(spec["levels"]))
    raise SystemExit(f"{spec['id']}: unknown question type {kind!r}")


def record(spec: dict, model: Any) -> dict:
    """One head's answer, as the Go side has to reproduce it.

    `image` is passed straight through, as the data URL the question set carries.
    Both clients send the same bytes that way, with no file either one has to see.

    `system_render` is recorded only for an image question, because it is the one
    thing the image read decides rather than follows: whether the server rendered
    the system turn as the string form or with the space vLLM's content-part
    template writes. The text read has no such field.
    """
    result = model.decide(
        spec["evidence"], {spec["head"]: build(spec)},
        policy=spec.get("policy"),
        probability=spec.get("probability", False),
        image=spec.get("image"))
    answer = result.answers[spec["head"]]
    recorded = {
        "type": answer.type,
        "value": answer.value,
        "label": answer.label,
        "probabilities": answer.probabilities,
        "strategy": answer.strategy,
        "calibrated": answer.calibrated,
        "temperature": answer.temperature,
        "requests": answer.requests,
        "confidence": answer.confidence,
        "level": answer.level,
        "thought_tokens": answer.thought_tokens,
        "thought_closed": answer.thought_closed,
        "missing_from_top": answer.missing_from_top,
    }
    if spec.get("image"):
        recorded["system_render"] = result.raw[spec["head"]].get("system_render")
    return recorded


def run_set(models: dict, questions: list[dict]) -> dict[str, dict]:
    """Every question once, in the order the file lists them."""
    return {spec["id"]: record(spec, models[spec.get("gate")]) for spec in questions}


def _numeric(value: object) -> float | None:
    """A value as a number, or None when it is not one."""
    return float(value) if isinstance(value, (int, float)) and not isinstance(value, bool) else None


def _movement(one: dict, other: dict) -> dict:
    """How far two passes of one question are apart."""
    deltas = [abs(one["probabilities"][key] - other["probabilities"].get(key, 0.0))
              for key in one["probabilities"]]
    left, right = _numeric(one["value"]), _numeric(other["value"])
    if left is not None and right is not None:
        value_delta: float | None = abs(left - right)
        same_value = value_delta <= TOLERANCE
    else:
        value_delta = None
        same_value = one["value"] == other["value"]
    return {
        "max_probability_delta": max(deltas) if deltas else 0.0,
        "value_delta": value_delta,
        "same_value": same_value,
        "same_strategy": one["strategy"] == other["strategy"],
        "same_requests": one["requests"] == other["requests"],
        # An image question's verdict on the system turn is not a measurement, so
        # it has no delta; a flip between two passes is still the server being
        # unstable, which is what the caller does with this.
        "same_system_render": one.get("system_render") == other.get("system_render"),
    }


def drift(passes: list[dict[str, dict]]) -> dict[str, dict]:
    """How far the passes are apart, per question, over every consecutive pair.

    A chosen value that moved is reported as itself, with the numeric distance
    when it is a number; the probability delta is the largest single-key
    difference. With more than two passes the worst pair is what is kept, so
    `--passes 4` measures more of the server rather than less.
    """
    report: dict[str, dict] = {}
    for index in range(len(passes) - 1):
        first, second = passes[index], passes[index + 1]
        for name, one in first.items():
            here = _movement(one, second[name])
            seen = report.get(name)
            if seen is None:
                report[name] = here
                continue
            seen["max_probability_delta"] = max(
                seen["max_probability_delta"], here["max_probability_delta"])
            if here["value_delta"] is not None:
                seen["value_delta"] = max(seen["value_delta"] or 0.0, here["value_delta"])
            seen["same_value"] = seen["same_value"] and here["same_value"]
            seen["same_strategy"] = seen["same_strategy"] and here["same_strategy"]
            seen["same_requests"] = seen["same_requests"] and here["same_requests"]
            seen["same_system_render"] = (
                seen["same_system_render"] and here["same_system_render"])
    return report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--out", default=str(DEFAULT_OUT),
                        help="where to write the answers (default: %(default)s)")
    parser.add_argument("--passes", type=int, default=2,
                        help="how many times to run the set, at least 2 (default: %(default)s)")
    parser.add_argument("--quiet", action="store_true", help="print nothing on success")
    args = parser.parse_args(argv)

    # One pass measures nothing: the whole point of the repeat is the drift
    # between passes, and a single pass would report a self-delta of 0 that was
    # never measured.
    if args.passes < 2:
        parser.error(f"--passes must be at least 2, got {args.passes}")

    endpoint_, model_ = endpoint(), model_id()
    out = pathlib.Path(args.out)
    document = json.loads(QUESTIONS.read_text())
    questions = document["questions"]

    # An image question needs a server with vision enabled, and a text-only vLLM is
    # a documented deployment. The capability is measured rather than declared, and
    # the meta carries it so the Go side can skip the same case for the same
    # reason instead of failing on it. Only the server's own refusal is a verdict:
    # a probe that failed for any other reason aborts the recording, so a
    # deployment fact is never inferred from a bug or a bad minute.
    verdict, why_not = probe_images()
    if verdict == PROBE_UNKNOWN:
        print(f"error: could not measure whether the endpoint serves images: {why_not}",
              file=sys.stderr)
        print("       nothing was recorded; re-run, or fix the endpoint or the probe",
              file=sys.stderr)
        return 4
    images = verdict == PROBE_SERVED
    if not images:
        dropped = [spec["id"] for spec in questions if spec.get("image")]
        questions = [spec for spec in questions if not spec.get("image")]
        print(f"not recording {', '.join(dropped) or 'nothing'}: {why_not}", file=sys.stderr)

    recorder = Recorder()
    transport = recorder.http.MockTransport(recorder.handler)

    # One model per distinct gate, because the gate is a constructor setting.
    # `None` is the shipped default. The policy and the probability view are
    # per-call, so they do not need a model each.
    models: dict[float | None, object] = {}
    try:
        for spec in questions:
            gate = spec.get("gate")
            if gate not in models:
                models[gate] = client().DecisionModel(
                    base_url=endpoint_, model=model_, api_key=api_key(),
                    tokenizer=os.environ.get("SHISA_DE_TOKENIZER") or None,
                    transport=transport,
                    **({"think_gate": gate} if gate is not None else {}))
        passes = [run_set(models, questions) for _ in range(args.passes)]
    finally:
        for model in models.values():
            model.close()

    first = passes[0]
    movement = drift(passes)
    self_delta = max((entry["max_probability_delta"] for entry in movement.values()), default=0.0)
    unstable = [name for name, entry in movement.items()
                if not (entry["same_value"] and entry["same_strategy"]
                        and entry["same_requests"] and entry["same_system_render"])]

    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(
        {"questions": first, "drift": movement, "self_delta": self_delta},
        indent=2, ensure_ascii=False, sort_keys=True) + "\n")

    meta = {
        "endpoint": endpoint_,
        "model": model_,
        "serving_fingerprint": recorder.fingerprint(),
        "recorded": datetime.date.today().isoformat(),
        "python_repo": python_revision(),
        "readout_version": client().READOUT_VERSIONS["de2"],
        "tokenizer_revision": tokenizer_revision(),
        "questions": len(questions),
        "passes": len(passes),
        "requests": recorder.requests,
        "images": images,
        "images_reason": why_not,
        "self_delta": self_delta,
        "unstable": unstable,
        "sha256": hashlib.sha256(out.read_bytes()).hexdigest(),
        "note": ("Live answers from the Python client to the fixed question set in "
                 "live_questions.json. The Go client asks the same questions of the same "
                 "endpoint in TestLiveParityAgainstPython and has to agree on the chosen "
                 "value and on the distribution within tolerance. `self_delta` is how far "
                 "the Python client's own two passes are apart, which is the server's "
                 "repeatability rather than the port's. `images` is whether the endpoint "
                 "answered an image request, and `images_reason` the server's own refusal "
                 "when it did not; the image questions are left out when it is false, and "
                 "a probe that failed for any other reason aborts the run instead."),
    }
    out.with_name(out.stem + ".meta.json").write_text(json.dumps(meta, indent=2) + "\n")

    if not args.quiet:
        print(f"wrote {out.name}: {len(questions)} questions, {len(passes)} passes, "
              f"{recorder.requests} requests, fingerprint {meta['serving_fingerprint']}")
        print(f"the Python client's own passes differ by at most {self_delta:.3g} in probability")
        if unstable:
            print(f"and disagree for: {', '.join(unstable)}", file=sys.stderr)
    if unstable:
        return 3
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
