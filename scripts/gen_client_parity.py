#!/usr/bin/env python3
"""Record the friendly layer's live exchanges and answers.

The Go client has to ask the same questions, send the same requests, and derive
the same answers as the Python client. The M3 fixture checks the readout; this
one checks everything above it: the policy, the calibration, the answer shapes
and the usage accounting.

It drives the Python `DecisionModel` against the hosted DE-2 endpoint through a
transport that forwards every request to the real server and records the request
and response bodies. It then records what the Python client made of them. The Go
test replays those recorded responses from a stub and compares its own answer to
the recorded one.

Because the requests are recorded, the replay does not depend on the server being
deterministic, and the recorded exchanges are the only thing the Go client sees:
no network is used by the test.

    SHISA_DE_API_KEY=... python scripts/gen_client_parity.py

`SHISA_API_KEY` and a bare `API_KEY` are accepted too, in that order, which is
the order the client itself reads them in.

Needs `httpx`, the `shisa-de` Python repository next to this one, and a cached
tokenizer for the checkpoint. The tokenizer revision is read from the cache and
pinned in the recording, so a later run against a different snapshot fails rather
than silently replacing the measurements. Output: testdata/client_parity.json and
testdata/client_parity.meta.json.

Every recorded number comes from the hosted endpoint. The metadata records the
served `system_fingerprint` the responses carried, the date, and the tokenizer
revision. This is the only fixture in the repository whose numbers are a
measurement of a served checkpoint.
"""

from __future__ import annotations

import datetime
import hashlib
import json
import os
import pathlib
import sys

import httpx

HERE = pathlib.Path(__file__).resolve().parent
OUT = HERE.parent / "testdata" / "client_parity.json"
META_OUT = HERE.parent / "testdata" / "client_parity.meta.json"
PY_REPO = pathlib.Path.home() / "data" / "dev" / "shisa" / "shisa-de"

sys.path.insert(0, str(PY_REPO))

from shisa_de import Choice, DecisionModel, Noul, Score  # noqa: E402
from shisa_de.readout import READOUT_VERSIONS  # noqa: E402

BASE_URL = "https://api.shisa.ai/openai"
MODEL = "shisa-ai/shisa-de-2"
API_KEY_ENVS = ("SHISA_DE_API_KEY", "SHISA_API_KEY", "API_KEY")


def api_key() -> str:
    """The key, from the variables the client itself reads.

    `API_KEY` is last: it is the one the recording environment happens to carry,
    not one this package documents.
    """
    for name in API_KEY_ENVS:
        value = os.environ.get(name, "").strip()
        if value:
            return value
    raise SystemExit(
        "set one of " + ", ".join(API_KEY_ENVS) + " before recording")


def tokenizer_revision() -> str:
    """The revision of the checkpoint's cached tokenizer.

    The client is given this revision explicitly, so the recording says which
    tokenizer bytes produced its token counts instead of whatever the cache
    happened to hold.
    """
    cache = pathlib.Path(os.environ.get(
        "HF_HUB_CACHE", pathlib.Path.home() / ".cache" / "huggingface" / "hub"))
    snapshots = cache / ("models--" + MODEL.replace("/", "--")) / "snapshots"
    revisions = sorted(entry.name for entry in snapshots.iterdir() if entry.is_dir())
    if len(revisions) != 1:
        raise SystemExit(
            f"expected exactly one cached snapshot of {MODEL}, found {revisions}; "
            f"point HF_HUB_CACHE at the cache that holds it")
    return revisions[0]


def serving_fingerprint(recorded: list[dict]) -> str:
    """The `system_fingerprint` the recorded responses carried.

    It is the serving shape the numbers came from, which is what makes them
    reproducible; an endpoint label is not.
    """
    seen = set()
    for case in recorded:
        for exchange in case["exchanges"]:
            body = exchange.get("response") or {}
            if body.get("system_fingerprint"):
                seen.add(body["system_fingerprint"])
    if len(seen) != 1:
        raise SystemExit(f"the responses carried {len(seen)} system fingerprints: {sorted(seen)}")
    return seen.pop()

STATE = {"evidence": "The customer was charged twice for one order and asked for a refund."}
STATE_URGENT = {"evidence": "The customer says the site is down and they cannot take orders."}


class Recorder:
    """A transport that forwards to the real endpoint and keeps every exchange."""

    def __init__(self) -> None:
        self.exchanges: list[dict] = []
        self.real = httpx.Client(timeout=600.0)

    def handler(self, request: httpx.Request) -> httpx.Response:
        response = self.real.send(
            self.real.build_request(request.method, request.url, content=request.content,
                                    headers=request.headers))
        body = response.json()
        self.exchanges.append({
            "method": request.method,
            "path": request.url.path,
            "request": json.loads(request.content) if request.content else None,
            "status": response.status_code,
            "response": body,
        })
        return httpx.Response(response.status_code, json=body,
                              headers={"content-type": "application/json"})


# -- the case table -----------------------------------------------------------
#
# Each case is a spec (which the Go test reads and rebuilds) plus a Python call.
# The spec is written by `python_*`, so the fixture is the single source for the
# Go side and the two cannot drift silently: a case the Go test builds
# differently sends a different request and the replay fails.


def noul(head: str, instructions: str) -> dict:
    return {"head": head, "type": "noul", "instructions": instructions}


def choice(head: str, instructions: str, options: list[tuple[str, str]]) -> dict:
    return {"head": head, "type": "choice", "instructions": instructions,
            "options": [{"key": key, "description": description} for key, description in options]}


def score(head: str, instructions: str, levels: list[str]) -> dict:
    return {"head": head, "type": "score", "instructions": instructions, "levels": levels}


def labels(head: str, names: list[str]) -> dict:
    """A bare list of labels: the classify shorthand with no instructions."""
    return {"head": head, "type": "labels", "labels": names}


def mapping(head: str, described: list[tuple[str, str]]) -> dict:
    """A bare label-to-description mapping: the classify shorthand."""
    return {"head": head, "type": "mapping",
            "options": [{"key": key, "description": description} for key, description in described]}


def levels_shorthand(head: str, levels: list[str]) -> dict:
    """The `{"levels": [...]}` classify shorthand."""
    return {"head": head, "type": "levels", "levels": levels}


def multi_label(head: str, instructions: str, names: list[str]) -> dict:
    return {"head": head, "type": "multi_label", "instructions": instructions,
            "options": [{"key": name} for name in names]}


def python_question(spec: dict):
    if spec["type"] == "noul":
        return Noul(spec["instructions"])
    if spec["type"] == "score":
        return Score(spec["instructions"], list(spec["levels"]))
    return Choice(spec["instructions"], {option["key"]: option["description"]
                                         for option in spec["options"]})


def python_heads(specs: list[dict]) -> dict:
    """Build the `labels` mapping `classify` takes, from the recorded specs."""
    heads = {}
    for spec in specs:
        kind = spec["type"]
        if kind == "labels":
            heads[spec["head"]] = list(spec["labels"])
        elif kind == "mapping":
            heads[spec["head"]] = {option["key"]: option["description"]
                                    for option in spec["options"]}
        elif kind == "levels":
            heads[spec["head"]] = {"levels": list(spec["levels"])}
        elif kind == "multi_label":
            heads[spec["head"]] = {"labels": [option["key"] for option in spec["options"]],
                                    "multi_label": True,
                                    "prompt": spec["instructions"]}
        else:
            heads[spec["head"]] = python_question(spec)
    return heads


def python_options(spec: dict, kind: str) -> dict:
    options = {}
    if spec.get("policy"):
        options["policy"] = spec["policy"]
    if spec.get("probability"):
        options["probability"] = True
    if spec.get("calibrated") is not None:
        options["calibrated"] = spec["calibrated"]
    if spec.get("debug"):
        options["debug"] = True
    if kind == "classify":
        # `decide` has no such arguments; only `classify` returns a view.
        if spec.get("include_confidence"):
            options["include_confidence"] = True
        if spec.get("include_probabilities"):
            options["include_probabilities"] = True
    return options


def run_case(case: dict) -> dict:
    """Run one case against the real endpoint and record what came back."""
    recorder = Recorder()
    revision = tokenizer_revision()
    model_options: dict = {"base_url": BASE_URL, "model": MODEL,
                           "api_key": api_key(),
                           "tokenizer": MODEL, "tokenizer_revision": revision,
                           "local_files_only": True,
                           "transport": httpx.MockTransport(recorder.handler)}
    if case.get("declared_model"):
        # The readout still talks to the real checkpoint; the client's own model
        # id is what the calibration's applicability is decided on.
        from shisa_de.readout import Readout
        model_options["model"] = case["declared_model"]
        if case.get("declared_tokenizer"):
            model_options["tokenizer"] = case["declared_tokenizer"]
        model_options["readout"] = Readout(
            base_url=BASE_URL, model=MODEL, tokenizer=MODEL,
            tokenizer_revision=revision, local_files_only=True,
            api_key=api_key(), transport=httpx.MockTransport(recorder.handler))
    model = DecisionModel(**model_options)
    options = python_options(case["options"], case["kind"])
    if case["kind"] == "classify":
        heads = python_heads(case["heads"])
        result = model.classify(case["state"], heads, **options)
        spec = {"kind": "classify", "heads": case["heads"], "options": case["options"]}
    else:
        questions = {spec["head"]: python_question(spec) for spec in case["questions"]}
        result = model.decide(case["state"], questions, **options)
        spec = {"kind": "decide", "questions": case["questions"], "options": case["options"]}
    return {
        "name": case["name"],
        "state": case["state"],
        **spec,
        "model": {"id": MODEL,
                  "declared_model": case.get("declared_model"),
                  "declared_tokenizer": case.get("declared_tokenizer")},
        "exchanges": recorder.exchanges,
        "decision": {
            "values": json.loads(json.dumps(result)),
            # `to_dict` carries the value but not the level an ordered scale lands on,
            # which `classify` returns as its label, nor the legend beside it, so both
            # are added here. Everything else is `to_dict`'s own shape.
            "answers": {head: {**answer.to_dict(), "level": answer.level,
                               "legend": answer.legend, "wire": answer.to_wire()}
                        for head, answer in result.answers.items()},
            "wire": result.to_wire(),
            "raw": result.raw,
            "usage": result.usage,
            "meta": {key: value for key, value in result.meta.items()
                     if key != "wall_ms"},
            "calibration_level": model.calibration_level,
            "calibration_applied": model.calibration_applied,
            "family_source": model.family_source,
            "calibration_id": model.calibration.id,
        },
    }


def cases() -> list[dict]:
    queue = choice("route", "Which queue should handle this?",
                   [("billing", "Charges and refunds"), ("shipping", "Delivery and tracking"),
                    ("technical", "Site errors and outages")])
    wide = choice("route", "Which queue should handle this?",
                  [(f"q{index:02d}", f"Queue number {index}") for index in range(27)])
    levels = score("severity", "How severe is this?", ["none", "low", "medium", "high", "critical"])
    urgent = noul("urgent", "Does this need an answer within the hour?")
    # The repeated read's top probability is 0.43, below the 0.7 gate, so this
    # question thinks unless something stops it: the option cap, or
    # probability=True.
    vague = choice("kind", "Which of these best describes the request?",
                   [("question", "A question"), ("complaint", "A complaint"),
                    ("request", "A request"), ("feedback", "Feedback")])
    vague_state = {"evidence": "It is about the thing we discussed."}

    return [
        {"name": "decide-noul-sure", "kind": "decide", "state": STATE_URGENT,
         "questions": [noul("urgent", "Is the site down?")],
         "options": {"policy": "repeat-think"}},
        {"name": "decide-noul-near-gate", "kind": "decide", "state": STATE,
         "questions": [urgent], "options": {"policy": "repeat-think"}},
        {"name": "decide-choice-sure", "kind": "decide", "state": STATE,
         "questions": [queue], "options": {"policy": "repeat-think"}},
        {"name": "decide-choice-think", "kind": "decide", "state": vague_state,
         "questions": [vague], "options": {"policy": "repeat-think"}},
        {"name": "decide-choice-3-think", "kind": "decide", "state": vague_state,
         "questions": [choice("kind", "Which of these best describes the request?",
                               [("question", "A question"), ("complaint", "A complaint"),
                                ("request", "A request")])],
         "options": {"policy": "repeat-think"}},
        {"name": "decide-score", "kind": "decide", "state": STATE,
         "questions": [levels], "options": {"policy": "repeat-think"}},
        # The repeated read is below the gate and there are 27 options, so the
        # 26-option cap is the only reason this does not think.
        {"name": "decide-choice-27-above-cap", "kind": "decide", "state": STATE,
         "questions": [wide], "options": {"policy": "repeat-think"}},
        {"name": "decide-direct", "kind": "decide", "state": STATE,
         "questions": [queue], "options": {"policy": "direct"}},
        {"name": "decide-repeat", "kind": "decide", "state": STATE,
         "questions": [queue], "options": {"policy": "repeat"}},
        # probability=True is the only reason this does not think.
        {"name": "decide-probability", "kind": "decide", "state": vague_state,
         "questions": [vague], "options": {"probability": True}},
        {"name": "decide-uncalibrated", "kind": "decide", "state": STATE,
         "questions": [queue], "options": {"calibrated": False}},
        # The client's own model id is not the fitted checkpoint, but the
        # tokenizer source is, which is how a server that aliases the checkpoint
        # is recognised.
        {"name": "decide-aliased-checkpoint", "kind": "decide", "state": STATE,
         "declared_model": "de2-v4-lr5e5-e3-s7",
         "questions": [queue], "options": {"policy": "repeat-think"}},
        # Neither the model id nor the tokenizer source is the fitted
        # checkpoint, so the record is not applied and the answers stay raw.
        {"name": "decide-other-checkpoint", "kind": "decide", "state": STATE,
         "declared_model": "de2-candidate-lr5e5", "declared_tokenizer": "de2-candidate-lr5e5",
         "questions": [queue], "options": {"policy": "repeat-think"}},
        {"name": "decide-multi-head", "kind": "decide", "state": STATE,
         "questions": [queue, urgent, levels], "options": {"policy": "repeat-think"}},
        {"name": "decide-debug", "kind": "decide", "state": STATE,
         "questions": [urgent], "options": {"debug": True}},
        # A debug read that reaches the thought, so the recorded raw carries the
        # id-form read: its `prompt` is null and its `sampled` is whatever the
        # server sends for a token-id request.
        {"name": "decide-debug-think", "kind": "decide", "state": vague_state,
         "questions": [vague], "options": {"debug": True}},
        {"name": "classify-labels", "kind": "classify", "state": STATE,
         "heads": [choice("intent", "What does the customer want?",
                          [("refund", "A refund"), ("info", "Information"),
                           ("cancel", "Cancellation")])],
         "options": {}},
        {"name": "classify-levels", "kind": "classify", "state": STATE,
         "heads": [levels], "options": {"include_confidence": True}},
        {"name": "classify-multilabel", "kind": "classify", "state": STATE,
         "heads": [multi_label("topic", "Is {label} one of the topics?",
                               ["billing", "delivery", "outage"])],
         "options": {}},
        # The three classify shorthands, which have no instructions of their own
        # and use the client's default question for the head.
        {"name": "classify-shorthand-labels", "kind": "classify", "state": STATE,
         "heads": [labels("intent", ["refund", "info", "cancel"])],
         "options": {}},
        {"name": "classify-shorthand-mapping", "kind": "classify", "state": STATE,
         "heads": [mapping("intent", [("refund", "A refund"), ("info", "Information")])],
         "options": {}},
        {"name": "classify-shorthand-levels", "kind": "classify", "state": STATE,
         "heads": [levels_shorthand("severity", ["none", "low", "high"])],
         "options": {}},
        {"name": "classify-probabilities", "kind": "classify", "state": STATE,
         "heads": [queue], "options": {"include_probabilities": True}},
        {"name": "classify-mixed", "kind": "classify", "state": STATE,
         "heads": [queue, levels], "options": {"include_confidence": True}},
    ]


def main() -> None:
    # Resolved before the first request, so a missing key fails without spending
    # anything, and the revision is pinned before the client loads the tokenizer.
    api_key()
    revision = tokenizer_revision()
    recorded = []
    for case in cases():
        print(f"  {case['name']} ...", end="", flush=True)
        result = run_case(case)
        recorded.append(result)
        exchanges = len(result["exchanges"])
        print(f" {exchanges} exchanges, ok")

    payload = {"endpoint": BASE_URL, "model": MODEL, "cases": recorded}
    text = json.dumps(payload, ensure_ascii=False, indent=1, sort_keys=False)
    OUT.write_text(text, encoding="utf-8")
    meta = {
        "endpoint": BASE_URL,
        "model": MODEL,
        # The serving shape the responses came from, not an endpoint label.
        "serving_fingerprint": serving_fingerprint(recorded),
        "recorded": datetime.date.today().isoformat(),
        "python_repo": subprocess_revision(),
        "readout_version": READOUT_VERSIONS["de2"],
        "tokenizer_revision": revision,
        "cases": len(recorded),
        "exchanges": sum(len(case["exchanges"]) for case in recorded),
        "sha256": hashlib.sha256(text.encode("utf-8")).hexdigest(),
        "note": ("Live exchanges against the hosted endpoint, recorded through a "
                 "forwarding transport. The responses are the server's; the answers are "
                 "the Python client's. The Go test replays the recorded responses and "
                 "compares its own answer, so it uses no network."),
    }
    META_OUT.write_text(json.dumps(meta, indent=2) + "\n", encoding="utf-8")
    print(f"{OUT}: {meta['cases']} cases, {meta['exchanges']} exchanges; "
          f"{META_OUT}: {meta['sha256'][:12]}")


def subprocess_revision() -> str:
    import subprocess
    try:
        return subprocess.run(["git", "-C", str(PY_REPO), "rev-parse", "HEAD"],
                              capture_output=True, text=True, check=True).stdout.strip()
    except Exception:
        return ""


if __name__ == "__main__":
    main()
