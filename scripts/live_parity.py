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

Output: testdata/live_python.json and testdata/live_python.meta.json by default,
or the path given to `--out`. The numbers are a measurement of a served
checkpoint, so the metadata carries the served `system_fingerprint`, the date,
the Python revision and the tokenizer revision; a number without its fingerprint
is not reproducible.
"""

from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import os
import pathlib
import subprocess
import sys

import httpx

HERE = pathlib.Path(__file__).resolve().parent
QUESTIONS = HERE.parent / "testdata" / "live_questions.json"
DEFAULT_OUT = HERE.parent / "testdata" / "live_python.json"
PY_REPO = pathlib.Path(os.environ.get(
    "SHISA_DE_PYTHON_REPO", pathlib.Path.home() / "data" / "dev" / "shisa" / "shisa-de"))

sys.path.insert(0, str(PY_REPO))

from shisa_de import Choice, DecisionModel, Noul, Score  # noqa: E402
from shisa_de.readout import READOUT_VERSIONS  # noqa: E402

BASE_URL = "https://api.shisa.ai/openai"
MODEL = "shisa-ai/shisa-de-2"
API_KEY_ENVS = ("SHISA_DE_API_KEY", "SHISA_API_KEY", "API_KEY")


def api_key() -> str:
    """The key, from the variables the client itself reads."""
    for name in API_KEY_ENVS:
        value = os.environ.get(name, "").strip()
        if value:
            return value
    raise SystemExit("set one of " + ", ".join(API_KEY_ENVS) + " before recording")


def tokenizer_revision() -> str:
    """The revision of the checkpoint's cached tokenizer, or "" when not cached.

    The Go side resolves its tokenizer from the hub the same way, so this pins
    which tokenizer bytes produced the recorded token counts.
    """
    cache = pathlib.Path(os.environ.get(
        "HF_HUB_CACHE", pathlib.Path.home() / ".cache" / "huggingface" / "hub"))
    snapshots = cache / ("models--" + MODEL.replace("/", "--")) / "snapshots"
    if not snapshots.is_dir():
        return ""
    revisions = sorted(entry.name for entry in snapshots.iterdir() if entry.is_dir())
    return revisions[0] if len(revisions) == 1 else ""


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
        self.fingerprints: set[str] = set()
        self.requests = 0
        self.real = httpx.Client(timeout=600.0)

    def handler(self, request: httpx.Request) -> httpx.Response:
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
        return httpx.Response(response.status_code, json=body,
                              headers={"content-type": "application/json"})

    def fingerprint(self) -> str:
        if len(self.fingerprints) != 1:
            raise SystemExit(
                f"the responses carried {len(self.fingerprints)} system fingerprints: "
                f"{sorted(self.fingerprints)}")
        return self.fingerprints.pop()


def build(spec: dict) -> object:
    """One question spec as the Python question object."""
    kind = spec["type"]
    if kind == "noul":
        return Noul(spec["instructions"])
    if kind == "choice":
        return Choice(spec["instructions"],
                      {option["key"]: option["description"] for option in spec["options"]})
    if kind == "score":
        return Score(spec["instructions"], list(spec["levels"]))
    raise SystemExit(f"{spec['id']}: unknown question type {kind!r}")


def record(spec: dict, model: DecisionModel) -> dict:
    """One head's answer, as the Go side has to reproduce it."""
    result = model.decide(spec["evidence"], {spec["head"]: build(spec)})
    answer = result.answers[spec["head"]]
    return {
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
        "missing_from_top": answer.missing_from_top,
    }


def run_set(models: dict, questions: list[dict]) -> dict[str, dict]:
    """Every question once, in the order the file lists them."""
    return {spec["id"]: record(spec, models[spec.get("gate")]) for spec in questions}


def drift(first: dict[str, dict], second: dict[str, dict]) -> dict[str, dict]:
    """How far the two passes are apart, per question.

    A chosen value that moved is reported as itself; the probability delta is
    the largest single-key difference.
    """
    report: dict[str, dict] = {}
    for name, one in first.items():
        other = second[name]
        deltas = [abs(one["probabilities"][key] - other["probabilities"].get(key, 0.0))
                  for key in one["probabilities"]]
        report[name] = {
            "max_probability_delta": max(deltas) if deltas else 0.0,
            "same_value": one["value"] == other["value"],
            "same_strategy": one["strategy"] == other["strategy"],
            "same_requests": one["requests"] == other["requests"],
        }
    return report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--out", default=str(DEFAULT_OUT),
                        help="where to write the answers (default: %(default)s)")
    parser.add_argument("--passes", type=int, default=2,
                        help="how many times to run the set (default: %(default)s)")
    parser.add_argument("--quiet", action="store_true", help="print nothing on success")
    args = parser.parse_args(argv)

    out = pathlib.Path(args.out)
    document = json.loads(QUESTIONS.read_text())
    questions = document["questions"]
    recorder = Recorder()
    transport = httpx.MockTransport(recorder.handler)

    # One model per distinct gate, because the gate is a constructor setting.
    # `None` is the shipped default.
    models: dict[float | None, DecisionModel] = {}
    try:
        for spec in questions:
            gate = spec.get("gate")
            if gate not in models:
                models[gate] = DecisionModel(
                    base_url=BASE_URL, model=MODEL, api_key=api_key(),
                    tokenizer=os.environ.get("SHISA_DE_TOKENIZER") or None,
                    transport=transport,
                    **({"think_gate": gate} if gate is not None else {}))
        passes = [run_set(models, questions) for _ in range(max(1, args.passes))]
    finally:
        for model in models.values():
            model.close()

    first = passes[0]
    movement = drift(first, passes[1]) if len(passes) > 1 else {}
    self_delta = max((entry["max_probability_delta"] for entry in movement.values()), default=0.0)
    unstable = [name for name, entry in movement.items()
                if not (entry["same_value"] and entry["same_strategy"] and entry["same_requests"])]

    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(
        {"questions": first, "drift": movement, "self_delta": self_delta},
        indent=2, ensure_ascii=False, sort_keys=True) + "\n")

    meta = {
        "endpoint": BASE_URL,
        "model": MODEL,
        "serving_fingerprint": recorder.fingerprint(),
        "recorded": datetime.date.today().isoformat(),
        "python_repo": python_revision(),
        "readout_version": READOUT_VERSIONS["de2"],
        "tokenizer_revision": tokenizer_revision(),
        "questions": len(questions),
        "passes": len(passes),
        "requests": recorder.requests,
        "self_delta": self_delta,
        "unstable": unstable,
        "sha256": hashlib.sha256(out.read_bytes()).hexdigest(),
        "note": ("Live answers from the Python client to the fixed question set in "
                 "live_questions.json. The Go client asks the same questions of the same "
                 "endpoint in TestLiveParityAgainstPython and has to agree on the chosen "
                 "value and on the distribution within tolerance. `self_delta` is how far "
                 "the Python client's own two passes are apart, which is the server's "
                 "repeatability rather than the port's."),
    }
    out.with_name(out.stem + ".meta.json").write_text(json.dumps(meta, indent=2) + "\n")

    if not args.quiet:
        print(f"wrote {out.name}: {len(questions)} questions, {len(passes)} passes, "
              f"{recorder.requests} requests, fingerprint {meta['serving_fingerprint']}")
        print(f"the Python client's own passes differ by at most {self_delta:.3g} in probability")
        if unstable:
            print(f"and disagree on the chosen value for: {', '.join(unstable)}", file=sys.stderr)
    if unstable:
        return 3
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
