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

The two clients do not choose a calibration record the same way: this port reads
the checkpoint id the server reports in its responses, and the Python client
tempers with the record its configured model id names. Against a gateway that
answers `shisa-ai/shisa-de-2` with DE-2.1's weights they therefore temper with
different fits, and the probabilities differ by far more than the tolerance.
That difference is a real property of the two clients, not a port defect, so the
gate removes it: `--calibration PATH` hands both clients the same record file,
and the Go side loads that same file. `TestLiveParityAgainstPython` always passes
it; run by hand without it and the recording describes the client's own choice,
which is only comparable against a Go client making the same one.

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
the Python revision, and which tokenizer bytes produced the token counts -- a
cached snapshot's hash, or a digest of a local tokenizer's own file, resolved
from `SHISA_DE_TOKENIZER` when it is set and the model id otherwise. A number
without its fingerprint is not reproducible.

The exit status is 0 for a repeatable run, 3 when the passes disagreed on the
chosen value, 4 when the endpoint's vision capability could not be measured, and
5 when the record named by `--calibration` cannot be used -- missing, unreadable,
or not a usable calibration record (bad JSON, a missing provenance key, or a
temperature that is not a finite number); for 0 and 3 the output and the
metadata are written first, so a caller that treats 3 as "recorded, but the
server moved" can read them. The Go test does exactly that. An unusable record
aborts before anything is sent, because falling back to automatic selection
would compare two different records and report the difference as a port defect.
"""

from __future__ import annotations

import argparse
import dataclasses
import datetime
import functools
import hashlib
import json
import math
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
    from shisa_de.calibration import calibration_from_dict
    from shisa_de.readout import READOUT_VERSIONS
    return types.SimpleNamespace(
        Choice=Choice, DecisionModel=DecisionModel, Noul=Noul, Score=Score,
        READOUT_VERSIONS=READOUT_VERSIONS, calibration_from_dict=calibration_from_dict)


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


def tokenizer_source() -> str:
    """The tokenizer source the run loads, resolved the way the clients resolve it.

    `SHISA_DE_TOKENIZER` when it is set, and the model id otherwise. This is the
    value the Python client is handed as `tokenizer=`; resolving the pin from the
    model id instead described a tokenizer the run did not load. The DE-2.1
    deployment is exactly that case: the model id names DE-2.1 and the tokenizer
    is DE-2's, so the model id's cache directory holds snapshots this run never
    read.
    """
    return os.environ.get("SHISA_DE_TOKENIZER", "").strip() or model_id()


def hub_cache_roots() -> list[pathlib.Path]:
    """The hub caches the clients look in, in their order.

    `HF_HUB_CACHE`, then `HF_HOME/hub`, then the per-user default. The Go client's
    `Lookup` reads the same three, so a cache one client finds is one the other
    finds too.
    """
    roots: list[pathlib.Path] = []
    if cache := os.environ.get("HF_HUB_CACHE", "").strip():
        roots.append(pathlib.Path(cache))
    if home := os.environ.get("HF_HOME", "").strip():
        roots.append(pathlib.Path(home) / "hub")
    roots.append(pathlib.Path.home() / ".cache" / "huggingface" / "hub")
    return roots


def _snapshot_name(value: str) -> bool:
    """Whether a name looks like a hub commit hash rather than a branch name.

    The same rule as the Go client's `isSnapshot`: 7 to 64 lowercase hex digits.
    """
    return 7 <= len(value) <= 64 and all(character in "0123456789abcdef" for character in value)


def _usable_snapshot(snapshot: pathlib.Path) -> bool:
    """Whether a snapshot directory holds a tokenizer the clients could load.

    A snapshot directory with no `tokenizer.json`, or an empty one, is a partial
    download rather than a tokenizer: pinning it would name bytes the run cannot
    have read.
    """
    tokenizer = snapshot / "tokenizer.json"
    try:
        return tokenizer.is_file() and tokenizer.stat().st_size > 0
    except OSError:
        return False


def _main_ref(base: pathlib.Path) -> str:
    """The snapshot `refs/main` names, or "" when it names none.

    This is the revision an unpinned `from_pretrained(repo)` loads, so it decides
    when it is there. Its content is another path component, so it is validated
    as one rather than joined blindly.
    """
    try:
        value = (base / "refs" / "main").read_text().strip()
    except (OSError, UnicodeDecodeError):
        return ""
    if not _snapshot_name(value):
        return ""
    return value


def _local_provenance(source: pathlib.Path) -> str:
    """A local tokenizer's provenance: its own bytes, because no revision names it.

    A directory is the form `AutoTokenizer.from_pretrained` takes for a local
    tokenizer, so its `tokenizer.json` is what gets read; a file is read as itself.
    The value names the local file or directory it came from and digests the
    bytes, so it cannot be mistaken for a hub revision.
    """
    file = source
    if source.is_dir():
        file = source / "tokenizer.json"
        if not file.is_file():
            return f"local:{source.name}:no tokenizer.json"
    try:
        digest = hashlib.sha256(file.read_bytes()).hexdigest()
    except OSError as exc:
        return f"local:{source.name}:unreadable: {exc.strerror or exc}"
    return f"local:{source.name}:sha256:{digest}"


def _hub_provenance(repo: str) -> str:
    """The snapshot a named hub repository resolves to in the local cache.

    `refs/main` first, because that is what an unpinned load resolves; then the
    one usable snapshot; and an explicit "ambiguous" when there is more than one,
    because picking one would pin a tokenizer the run may not have used.
    """
    folder = "models--" + repo.replace("/", "--")
    for root in hub_cache_roots():
        base = root / folder
        ref = _main_ref(base)
        if ref and _usable_snapshot(base / "snapshots" / ref):
            return ref
        snapshots = base / "snapshots"
        if not snapshots.is_dir():
            continue
        usable = sorted(entry.name for entry in snapshots.iterdir()
                        if entry.is_dir() and _usable_snapshot(entry))
        if len(usable) == 1:
            return usable[0]
        if len(usable) > 1:
            return (f"ambiguous: {len(usable)} snapshots cached under {base} "
                    "and no refs/main to choose between them")
    return ""


def tokenizer_provenance(source: str) -> str:
    """Which tokenizer bytes a source names, as the meta records them.

    One of these forms, because a source can name several different things and
    only some of them are a hub revision:

    - `""` -- nothing on this machine matches the source, so there is no pin.
    - `<hash>` -- a cached hub snapshot, named by the directory that holds it.
      That includes a source that *is* the snapshot path, which is how a caller
      pins one snapshot of a repository that has several.
    - `local:<name>:sha256:<digest>` -- a local directory's `tokenizer.json`, or a
      local file, identified by its bytes because no revision names them.
    - `ambiguous: ...`, `local:<name>:no tokenizer.json` or
      `local:<name>:unreadable: ...` -- a source that cannot be pinned to one set
      of bytes, saying why rather than looking like a revision.
    """
    if not source:
        return ""
    target = pathlib.Path(source).expanduser()
    if target.is_dir():
        # A snapshot directory is named by its hash, which is the pin. A directory
        # anywhere else is a local tokenizer and has no revision.
        if target.parent.name == "snapshots" and _snapshot_name(target.name):
            return target.name if _usable_snapshot(target) else ""
        return _local_provenance(target)
    if target.is_file():
        in_snapshot = (target.name == "tokenizer.json"
                       and target.parent.parent.name == "snapshots"
                       and _snapshot_name(target.parent.name))
        if in_snapshot:
            return target.parent.name if _usable_snapshot(target.parent) else ""
        return _local_provenance(target)
    if target.parent.name == "snapshots" and _snapshot_name(target.name):
        # A snapshot named explicitly that is not on this machine.
        return ""
    return _hub_provenance(source)


def tokenizer_revision() -> str:
    """Which tokenizer bytes the run loads, or a reason there is none.

    The recorder passes `SHISA_DE_TOKENIZER` (or the model id) to the Python
    client as its tokenizer source, so that is what decides: a cached snapshot's
    hash, a digest of a local tokenizer, or a reason the pin is missing.
    This describes the Python recorder's source; the Go live configuration must
    independently use a source its resolver supports. An empty string means the
    source is not on this machine.
    """
    return tokenizer_provenance(tokenizer_source())


def record_path(spec: str | None) -> str:
    """The calibration record file both halves of the gate are given, or "".

    An empty spec means the client selects a record for itself, which is what a
    hand-run does. The gate always passes one: the two clients do not select the
    same record from an aliased id, so a comparison of tempered answers is only
    a comparison of the ports when both were handed the same bytes.
    """
    return (spec or "").strip()


class RecordError(Exception):
    """The calibration record the gate named cannot be used.

    Its own type so `main` can turn every unusable record -- missing, unreadable,
    not JSON, not a record -- into the documented exit status before a request is
    spent, instead of a traceback on the ones that are not a missing file.
    """


@dataclasses.dataclass(frozen=True)
class LoadedRecord:
    """The record to hand every model, where it came from, and its digest.

    The digest is of the bytes this process read, so it is taken here rather than
    when the metadata is written: a record that changed or vanished mid-run would
    otherwise produce a recording whose provenance is about different bytes than
    the run used.
    """

    record: Any
    path: str
    sha256: str

    @property
    def name(self) -> str:
        """The file's own name, for the metadata."""
        return pathlib.Path(self.path).name if self.path else ""


#: What `load_record` returns when the run was not given a record.
NO_RECORD = LoadedRecord(None, "", "")


def _check_temperature_table(target: pathlib.Path, where: str, table: Any) -> None:
    """Refuse a temperature table whose values cannot temper a distribution.

    The client's loader copies these values without converting them, so a `null`
    or a string survives construction and fails later: `Calibration.fitted`
    raises on it, or -- worse -- skips it because another entry short-circuits
    the `any()` and the failure waits until an answer is scored, after the probe
    and the recording have already spent requests. A temperature has to be a real
    finite number to scale a logit, and that is all this checks; whether a number
    is a good fit is not the recorder's judgement.
    """
    if not isinstance(table, dict):
        raise RecordError(
            f"{target} is not a calibration record: {where} is not a table of temperatures")
    for name, value in table.items():
        # `bool` is an `int` in Python and a JSON `true` is not a temperature.
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            raise RecordError(
                f"{target} is not a calibration record: {where} has a non-numeric "
                f"temperature for {name!r} ({value!r})")
        if not math.isfinite(value):
            raise RecordError(
                f"{target} is not a calibration record: {where} has a non-finite "
                f"temperature for {name!r} ({value!r})")


def _check_temperatures(target: pathlib.Path, record: Any) -> None:
    """Refuse a record with any unusable temperature, before a request is spent.

    Every table is checked, not the one a particular read would use: the run asks
    for the direct, repeated and thinking reads, so a bad value in any of them is
    a bad record for this run.
    """
    _check_temperature_table(target, "temperatures", record.temperatures)
    reads = record.reads
    if not isinstance(reads, dict):
        raise RecordError(
            f"{target} is not a calibration record: reads is not a table of reads")
    for name, table in reads.items():
        _check_temperature_table(target, f"reads[{name!r}]", table)


def load_record(spec: str | None) -> LoadedRecord:
    """The record to hand every model, the path it came from, and its digest.

    The file is read once. Its bytes are decoded, parsed and hashed, and the
    record is built from that parsed document through the client's own
    `calibration_from_dict`, so the digest and the record describe one set of
    bytes: reopening the path to build the record would let a replacement between
    the two reads produce a digest of contents the run did not use.

    A path is read as a path: a mistyped one raises rather than falling back to
    the client's own selection, because that fallback would record a run in which
    the two clients held different records and the caller would have no way to
    see it. A file that exists but is not a usable calibration record raises the
    same way -- bad JSON, a missing provenance key, a temperature that is not a
    finite number -- so a bad record cannot become a run whose halves are
    uncomparable, or fail after the first request.

    No client import happens for an empty spec or a missing file, so the argument
    handling can be tested without a `shisa_de` checkout.
    """
    path = record_path(spec)
    if not path:
        return NO_RECORD
    target = pathlib.Path(path).expanduser()
    if not target.is_file():
        raise RecordError(f"no calibration record at {target}")
    try:
        raw = target.read_bytes()
    except OSError as exc:
        raise RecordError(
            f"cannot read the calibration record at {target}: {exc.strerror or exc}") from exc
    try:
        # Decoded as UTF-8 and parsed from `raw`, which is the buffer that is
        # hashed below; `UnicodeDecodeError` and `JSONDecodeError` are both
        # `ValueError`.
        document = json.loads(raw.decode("utf-8"))
    except ValueError as exc:
        raise RecordError(f"{target} is not a calibration record: {exc}") from exc
    try:
        record = client().calibration_from_dict(document, origin=str(target))
    except (KeyError, TypeError, ValueError) as exc:
        # A missing required key raises KeyError, a field of the wrong shape or
        # type raises TypeError, and a value the loader converts raises
        # ValueError. All three mean the same thing here: not a record.
        raise RecordError(f"{target} is not a calibration record: {exc}") from exc
    _check_temperatures(target, record)
    return LoadedRecord(record=record, path=str(target),
                        sha256=hashlib.sha256(raw).hexdigest())


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

    Only the served `system_fingerprint` and the model ids the responses named are
    kept from them; the bodies are not recorded, because the Go side asks the
    server itself rather than replaying.
    """

    def __init__(self) -> None:
        self.http = http()
        self.fingerprints: set[str] = set()
        # The ids the responses named. Recorded because the checkpoint that
        # answered is what a calibration record has to be fitted on, and a
        # recording that does not say which one answered cannot be re-read
        # against the right fit.
        self.served: set[str] = set()
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
        if body.get("model"):
            self.served.add(body["model"])
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
    parser.add_argument("--calibration", default=None, metavar="PATH",
                        help="a calibration record file to hand every model; the Go gate "
                             "loads the same file (default: let the client choose)")
    parser.add_argument("--quiet", action="store_true", help="print nothing on success")
    args = parser.parse_args(argv)

    # One pass measures nothing: the whole point of the repeat is the drift
    # between passes, and a single pass would report a self-delta of 0 that was
    # never measured.
    if args.passes < 2:
        parser.error(f"--passes must be at least 2, got {args.passes}")

    # The record is resolved before the capability probe, so an unusable one costs
    # nothing rather than a request and a recording nobody can match.
    try:
        loaded = load_record(args.calibration)
    except RecordError as exc:
        print(f"error: {exc}", file=sys.stderr)
        print("       nothing was recorded; both clients have to be given the same record",
              file=sys.stderr)
        return 5

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
    # per-call, so they do not need a model each. The record is a constructor
    # setting too, and the same one goes to every model: a run whose heads were
    # tempered by different records would be comparable to nothing.
    models: dict[float | None, object] = {}
    try:
        for spec in questions:
            gate = spec.get("gate")
            if gate not in models:
                models[gate] = client().DecisionModel(
                    base_url=endpoint_, model=model_, api_key=api_key(),
                    tokenizer=os.environ.get("SHISA_DE_TOKENIZER") or None,
                    transport=transport, calibration=loaded.record,
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
        # Which tokenizer source the run loaded, and which bytes that source names.
        # The source is `SHISA_DE_TOKENIZER` or the model id; the pin is a snapshot
        # hash or, for a local tokenizer, a digest of its file.
        "tokenizer_source": tokenizer_source(),
        "tokenizer_revision": tokenizer_revision(),
        "questions": len(questions),
        "passes": len(passes),
        "requests": recorder.requests,
        "images": images,
        "images_reason": why_not,
        "self_delta": self_delta,
        "unstable": unstable,
        # Which record tempered these answers, and the bytes it was read from.
        # `calibration_sha256` is what makes "both clients were given the same
        # record" checkable rather than asserted: the Go gate hashes its own copy
        # of the file and compares. It is the digest of the bytes read before the
        # run, so a file that changed mid-run cannot be described as the one used.
        "calibration": loaded.record.id if loaded.record is not None else "",
        "calibration_model": loaded.record.model if loaded.record is not None else "",
        "calibration_file": loaded.name,
        "calibration_sha256": loaded.sha256,
        "served_models": sorted(recorder.served),
        "sha256": hashlib.sha256(out.read_bytes()).hexdigest(),
        "note": ("Live answers from the Python client to the fixed question set in "
                 "live_questions.json. The Go client asks the same questions of the same "
                 "endpoint in TestLiveParityAgainstPython and has to agree on the chosen "
                 "value and on the distribution within tolerance. `self_delta` is how far "
                 "the Python client's own two passes are apart, which is the server's "
                 "repeatability rather than the port's. `images` is whether the endpoint "
                 "answered an image request, and `images_reason` the server's own refusal "
                 "when it did not; the image questions are left out when it is false, and "
                 "a probe that failed for any other reason aborts the run instead. "
                 "`calibration` and `calibration_sha256` name the record both clients were "
                 "given and the bytes it was read from; an empty pair means this run let "
                 "the client choose, which is only comparable against a Go client that "
                 "chose the same one. `served_models` is what the responses said answered. "
                 "`tokenizer_source` is the source the client loaded and "
                 "`tokenizer_revision` which bytes it names: a cached snapshot's hash, a "
                 "`local:<name>:sha256:<digest>` for a local tokenizer, or a reason the "
                 "pin is missing."),
    }
    out.with_name(out.stem + ".meta.json").write_text(json.dumps(meta, indent=2) + "\n")

    if not args.quiet:
        print(f"wrote {out.name}: {len(questions)} questions, {len(passes)} passes, "
              f"{recorder.requests} requests, fingerprint {meta['serving_fingerprint']}")
        print(f"the Python client's own passes differ by at most {self_delta:.3g} in probability")
        if loaded.record is not None:
            print(f"both clients are to be given {loaded.record.id}")
        if meta["served_models"]:
            print(f"the responses named {', '.join(meta['served_models'])}")
        if unstable:
            print(f"and disagree for: {', '.join(unstable)}", file=sys.stderr)
    if unstable:
        return 3
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
