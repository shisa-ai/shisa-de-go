#!/usr/bin/env python3
"""Offline tests for the parts of `live_parity.py` that decide what a run means.

    python3 -m unittest discover -s scripts -p 'test_*.py'

These need no network, no API key and no `shisa_de` checkout: they cover the
endpoint and model resolution, the capability probe's classification, the
tokenizer pin, the calibration record option, and the drift arithmetic that tells
"the server moved" from "the port differs". The client and `httpx` are imported
lazily for exactly this reason, so `python3` and the standard library are the
whole dependency.

Two things are covered beyond those pure parts. The `--calibration` option is
followed all the way through `main`, with the client, the probe and the transport
replaced by fakes: the record has to reach every model, and the provenance has to
name the file and the digest of its bytes, because the Go side compares that
digest against its own copy. And a missing record has to abort before anything is
sent, since falling back to the client's own choice would record a run whose two
halves held different records.

The one place a request header is built sets a placeholder key rather than
reading the environment. That is deliberate: these tests passed on a machine
that happened to have a real `SHISA_API_KEY` and failed on the CI runner that
did not, so the suite now proves it runs with the key variables unset.

The rest of the recorder -- the requests, the recording, the provenance -- is
exercised by `TestLiveParityAgainstPython` in `live_test.go`, which runs the
script against a real endpoint and reads what it wrote.
"""
from __future__ import annotations

import base64
import contextlib
import hashlib
import io
import json
import pathlib
import sys
import tempfile
import types
import unittest
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import live_parity  # noqa: E402

# The two variables both halves of the gate resolve, plus the key variables a
# test that builds a request header has to set. Set by a test, cleared by its
# teardown: a test that leaks a key into the next one would hide exactly the
# failure the CI run found, where these tests passed on a machine that happened
# to have a key and failed on one that did not.
# `SHISA_DE_TOKENIZER` decides which tokenizer a run loads, `HF_HUB_CACHE` and
# `HF_HOME` are the two cache roots the clients read, and `HOME` is the third:
# a case that leaves any of them at the developer's value can be answered by their
# own cache instead of its fixture.
ENV_VARS = ("SHISA_DE_ENDPOINT", "SHISA_DE_MODEL", "SHISA_DE_TOKENIZER",
            "HF_HUB_CACHE", "HF_HOME", "HOME") + live_parity.API_KEY_ENVS


#: A record file's contents, in the shape both clients read. The values are not
#: a fit of anything: these cases are about which record reaches which model and
#: which bytes the provenance names, not about a temperature.
RECORD = {
    "temperatures": {"choice": 1.5, "noul": 1.25},
    "readout_version": "de2-codebook-v1",
    "model": "shisa-ai/shisa-de-2.1",
    "serving_shape": "test-shape",
    "fitted_on": "a test fixture",
    "reads": {"direct": {"choice": 1.5, "noul": 1.25}},
}


class _FakeRecord:
    """A faithful stub of the client's `calibration_from_dict`.

    It copies the document the way the client's loader does -- required keys
    indexed, temperature values copied without converting them, read tables built
    with `dict(...)` -- and raises the same exceptions for the same shapes. A case
    about an unusable record is then a case about the recorder's validation rather
    than about this fake being stricter than the real loader.
    """

    def __init__(self, raw: dict, origin: str = "") -> None:
        if not isinstance(raw, dict):
            raise TypeError("a calibration record is a JSON object")
        self.origin = origin
        self.model = raw["model"]
        self.readout_version = raw["readout_version"]
        self.serving_shape = raw["serving_shape"]
        self.temperatures = dict(raw["temperatures"])
        self.reads = {name: dict(values) for name, values in (raw.get("reads") or {}).items()}
        self.id = f"{raw['model']}|{raw['readout_version']}|{raw['serving_shape']}"


class _FakeAnswer:
    """One head's answer, with every field `record()` reads."""

    type = "noul"
    value = 0.8
    label = None
    probabilities = {"yes": 0.8, "no": 0.2}
    strategy = "direct"
    calibrated = True
    temperature = 1.25
    requests = 1
    confidence = 0.6
    level = None
    thought_tokens = 0
    thought_closed = None
    missing_from_top: list = []


class _FakeDecision:
    def __init__(self, head: str) -> None:
        self.answers = {head: _FakeAnswer()}
        self.raw = {head: {}}


class _FakeModel:
    """A `DecisionModel` that records its keyword arguments and answers nothing."""

    built: list = []

    def __init__(self, **kwargs) -> None:
        self.kwargs = kwargs
        type(self).built.append(self)

    def decide(self, evidence, questions, **kwargs):
        return _FakeDecision(next(iter(questions)))

    def close(self) -> None:
        self.closed = True


class _FakeRecorder:
    """The bit of `Recorder` `main` uses, with the transport it hands out."""

    instances: list = []

    def __init__(self) -> None:
        self.requests = 0
        self.served = {"shisa-ai/shisa-de-2.1"}
        self.http = types.SimpleNamespace(
            MockTransport=lambda handler: ("transport", handler))
        type(self).instances.append(self)

    def handler(self, request):
        raise AssertionError("a fake transport should not be called")

    def fingerprint(self) -> str:
        return "test-fingerprint"


class EnvCase(unittest.TestCase):
    """A test that sets environment variables and puts them back."""

    def setUp(self) -> None:
        self.saved = {name: sys.modules["os"].environ.get(name) for name in ENV_VARS}
        self.addCleanup(self.restore)

    def restore(self) -> None:
        import os
        for name, value in self.saved.items():
            if value is None:
                os.environ.pop(name, None)
            else:
                os.environ[name] = value

    def setenv(self, name: str, value: str) -> None:
        import os
        os.environ[name] = value

    def unsetenv(self, name: str) -> None:
        import os
        os.environ.pop(name, None)


# -- the endpoint and the model -----------------------------------------------


class EndpointCase(EnvCase):
    def test_prefers_the_environment(self):
        self.setenv("SHISA_DE_ENDPOINT", "http://127.0.0.1:8021")
        self.assertEqual(live_parity.endpoint(), "http://127.0.0.1:8021")

    def test_falls_back_to_the_hosted_one(self):
        self.unsetenv("SHISA_DE_ENDPOINT")
        self.assertEqual(live_parity.endpoint(), live_parity.BASE_URL)

    def test_ignores_an_empty_variable(self):
        self.setenv("SHISA_DE_ENDPOINT", "")
        self.assertEqual(live_parity.endpoint(), live_parity.BASE_URL)

    def test_model_prefers_the_environment(self):
        self.setenv("SHISA_DE_MODEL", "de2-v4-lr5e5-e3-s7")
        self.assertEqual(live_parity.model_id(), "de2-v4-lr5e5-e3-s7")

    def test_model_falls_back_to_the_hosted_one(self):
        self.unsetenv("SHISA_DE_MODEL")
        self.assertEqual(live_parity.model_id(), live_parity.MODEL)

    def test_the_gate_and_the_recorder_read_the_same_variables(self):
        """The gate is only a gate when both halves read the same two variables.

        The Go side resolves `SHISA_DE_ENDPOINT` and `SHISA_DE_MODEL`; a recorder
        that did not would compare the Go client's local server against the
        Python client's hosted one and report the difference as a port defect.
        """
        source = pathlib.Path(live_parity.__file__).read_text()
        self.assertIn('os.environ.get("SHISA_DE_ENDPOINT"', source)
        self.assertIn('os.environ.get("SHISA_DE_MODEL"', source)


class ApiUrlCase(EnvCase):
    """The capability probe's URL, which has to be the client's own.

    Appending `/chat/completions` to the base directly 404s against the default
    endpoint, whose base does not end in `/v1`. That 404 was reported as "the
    endpoint does not serve images", which is a false negative that reads as a
    verdict about the server.
    """

    def test_a_base_without_v1(self):
        self.setenv("SHISA_DE_ENDPOINT", "https://api.shisa.ai/openai")
        self.assertEqual(live_parity.api_url("/v1/chat/completions"),
                         "https://api.shisa.ai/openai/v1/chat/completions")

    def test_a_base_ending_in_v1_does_not_double_it(self):
        self.setenv("SHISA_DE_ENDPOINT", "http://127.0.0.1:8021/v1")
        self.assertEqual(live_parity.api_url("/v1/chat/completions"),
                         "http://127.0.0.1:8021/v1/chat/completions")

    def test_a_trailing_slash_is_dropped(self):
        self.setenv("SHISA_DE_ENDPOINT", "http://127.0.0.1:8021/")
        self.assertEqual(live_parity.api_url("/v1/models"),
                         "http://127.0.0.1:8021/v1/models")

    def test_the_default_endpoint_is_the_one_the_probe_hits(self):
        self.unsetenv("SHISA_DE_ENDPOINT")
        self.assertEqual(live_parity.api_url("/v1/chat/completions"),
                         live_parity.BASE_URL + "/v1/chat/completions")


# -- the calibration record both halves of the gate are given ------------------


class RecordFileCase(EnvCase):
    """The `--calibration` argument, which is what makes the gate a gate.

    The two clients do not select the same record from an aliased model id, so a
    comparison of tempered answers only compares the ports when both were handed
    the same bytes. An empty spec keeps the client's own selection, which is what
    a hand-run wants; a path is read as a path.

    Every way a named record can be unusable is one exception type, because
    `main` has to turn all of them into the documented exit status: a missing
    file, an unreadable one, and one that is not a calibration record.
    """

    def setUp(self):
        super().setUp()
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = pathlib.Path(self.directory.name)

    def fake_client(self):
        """The loader's dict entry point, and no file entry point at all.

        `load_record` must build the record from the bytes it read, so a fake that
        offered `load_calibration_file` could hide a reopen; this one has only the
        API the recorder is supposed to use.
        """
        return types.SimpleNamespace(calibration_from_dict=_FakeRecord)

    def load(self, spec):
        with mock.patch.object(live_parity, "client", self.fake_client):
            return live_parity.load_record(spec)

    def test_no_option_means_the_client_chooses(self):
        self.assertEqual(live_parity.record_path(None), "")
        self.assertEqual(live_parity.record_path("  "), "")
        loaded = live_parity.load_record(None)
        self.assertIsNone(loaded.record)
        self.assertEqual(loaded.path, "")
        self.assertEqual(loaded.name, "")
        self.assertEqual(loaded.sha256, "")

    def test_a_missing_file_raises_rather_than_falling_back(self):
        """A fallback would record a run whose two clients held different records."""
        missing = self.root / "nope.json"
        with self.assertRaises(live_parity.RecordError) as caught:
            live_parity.load_record(str(missing))
        self.assertIn("no calibration record", str(caught.exception))

    def test_a_directory_is_not_a_record(self):
        directory = self.root / "records"
        directory.mkdir()
        with self.assertRaises(live_parity.RecordError) as caught:
            live_parity.load_record(str(directory))
        self.assertIn("no calibration record", str(caught.exception))

    def test_an_unreadable_file_is_a_record_error(self):
        path = self.root / "record.json"
        path.write_text(json.dumps(RECORD))
        denied = PermissionError(13, "Permission denied")
        with mock.patch.object(pathlib.Path, "read_bytes", side_effect=denied):
            with self.assertRaises(live_parity.RecordError) as caught:
                live_parity.load_record(str(path))
        self.assertIn("cannot read", str(caught.exception))
        self.assertIn("Permission denied", str(caught.exception))

    def test_a_file_that_is_not_json_is_a_record_error(self):
        """A traceback here would exit 1, which is not a status this script documents."""
        path = self.root / "broken.json"
        path.write_text("{not json")
        with self.assertRaises(live_parity.RecordError) as caught:
            self.load(str(path))
        self.assertIn("is not a calibration record", str(caught.exception))

    def test_a_document_missing_a_record_key_is_a_record_error(self):
        path = self.root / "partial.json"
        path.write_text(json.dumps({"readout_version": "de2-codebook-v1"}))
        with self.assertRaises(live_parity.RecordError) as caught:
            self.load(str(path))
        self.assertIn("is not a calibration record", str(caught.exception))

    def test_a_document_whose_fields_have_the_wrong_shape_is_a_record_error(self):
        path = self.root / "wrong-shape.json"
        path.write_text(json.dumps({"model": "shisa-ai/shisa-de-2.1", "temperatures": 3,
                                    "readout_version": "de2-codebook-v1",
                                    "serving_shape": "x", "fitted_on": "y"}))
        with self.assertRaises(live_parity.RecordError) as caught:
            self.load(str(path))
        self.assertIn("is not a calibration record", str(caught.exception))

    def test_an_existing_file_is_loaded_named_and_digested(self):
        path = self.root / "record.json"
        path.write_text(json.dumps(RECORD))
        loaded = self.load(str(path))
        self.assertEqual(loaded.record.id, _FakeRecord(RECORD).id)
        self.assertEqual(loaded.path, str(path))
        self.assertEqual(loaded.name, path.name)
        self.assertEqual(loaded.sha256, hashlib.sha256(path.read_bytes()).hexdigest())

    def test_the_option_is_offered_by_the_command_line(self):
        """The Go gate passes `--calibration`, so the parser has to accept it."""
        source = pathlib.Path(live_parity.__file__).read_text()
        self.assertIn('"--calibration"', source)

    def test_the_record_is_built_from_the_bytes_that_were_hashed(self):
        """One read, so the digest and the record cannot describe different bytes.

        A recorder that hashed one buffer and built the record from a second read
        of the path would still produce a plausible digest, and only a comparison
        against the file's current contents would notice. The count is the direct
        statement of the guarantee; the replacement afterwards shows what it buys.
        """
        path = self.root / "record.json"
        path.write_text(json.dumps(RECORD))
        original = path.read_bytes()
        reads = []
        real = pathlib.Path.read_bytes

        def counted(target):
            reads.append(target)
            return real(target)

        with mock.patch.object(pathlib.Path, "read_bytes", counted):
            loaded = self.load(str(path))
        self.assertEqual(len(reads), 1, "the record file was read more than once")
        self.assertEqual(loaded.sha256, hashlib.sha256(original).hexdigest())
        self.assertEqual(loaded.record.model, RECORD["model"])

        path.write_text(json.dumps(dict(RECORD, model="shisa-ai/shisa-de-2")))
        self.assertEqual(loaded.sha256, hashlib.sha256(original).hexdigest())
        self.assertEqual(loaded.record.model, RECORD["model"])

    def test_the_real_loader_does_not_reopen_the_file(self):
        """The client's own loader, driven from the buffer, with the file refused.

        The client's file entry point is the thing that would reopen the path, so
        both reads it could use raise here. This is the case a fake client cannot
        make: it proves the record came from the bytes that were hashed even when
        the loader is the real one.
        """
        if not (live_parity.PY_REPO / "shisa_de").is_dir():
            self.skipTest(f"no shisa_de checkout under {live_parity.PY_REPO}")
        path = self.root / "record.json"
        path.write_text(json.dumps(RECORD))
        raw = path.read_bytes()
        live_parity.client()  # import with a normal filesystem, before the refusal

        def refuse(*args, **kwargs):
            raise AssertionError(f"the record file was reopened: {args or kwargs}")

        # The recorder's own read goes through `Path.open`, so that one call is
        # allowed and every later one is refused; `read_text` is the loader's file
        # entry point and is refused outright.
        real_open = pathlib.Path.open
        opens = []

        def once(target, *args, **kwargs):
            opens.append(target)
            if len(opens) > 1:
                raise AssertionError(f"the record file was reopened: {target}")
            return real_open(target, *args, **kwargs)

        with mock.patch.object(pathlib.Path, "read_text", refuse), \
                mock.patch.object(pathlib.Path, "open", once):
            loaded = live_parity.load_record(str(path))
        self.assertEqual(len(opens), 1, "the record file was opened more than once")
        self.assertEqual(loaded.sha256, hashlib.sha256(raw).hexdigest())
        self.assertEqual(loaded.record.model, RECORD["model"])
        self.assertTrue(loaded.record.fitted, "the real loader did not build a usable record")

    def test_a_null_temperature_is_refused(self):
        """The loader copies a null through, and `fitted` then raises on it."""
        path = self.root / "null.json"
        path.write_text(json.dumps(dict(RECORD, temperatures={"noul": None})))
        with self.assertRaises(live_parity.RecordError) as caught:
            self.load(str(path))
        self.assertIn("non-numeric temperature for 'noul'", str(caught.exception))

    def test_a_null_temperature_in_a_read_table_is_refused(self):
        """`fitted` short-circuits past this one, so it would fail while scoring."""
        path = self.root / "null-read.json"
        path.write_text(json.dumps(dict(RECORD, reads={"direct": {"choice": 1.5, "noul": None}})))
        with self.assertRaises(live_parity.RecordError) as caught:
            self.load(str(path))
        self.assertIn("reads['direct'] has a non-numeric temperature for 'noul'",
                      str(caught.exception))

    def test_a_non_numeric_temperature_is_refused(self):
        for name, value in (("string", "hot"), ("boolean", True), ("list", [1.5])):
            with self.subTest(name):
                path = self.root / f"{name}.json"
                path.write_text(json.dumps(dict(RECORD, temperatures={"choice": value})))
                with self.assertRaises(live_parity.RecordError) as caught:
                    self.load(str(path))
                self.assertIn("non-numeric temperature for 'choice'", str(caught.exception))

    def test_a_non_finite_temperature_is_refused(self):
        """`json.loads` reads a `NaN` literal, so a record can carry one.

        The Go loader refuses the document as invalid JSON, so accepting it here
        would record a run the other half cannot replay.
        """
        for name, value in (("nan", float("nan")), ("infinity", float("inf"))):
            with self.subTest(name):
                path = self.root / f"{name}.json"
                path.write_text(json.dumps(dict(RECORD, temperatures={"noul": value})))
                with self.assertRaises(live_parity.RecordError) as caught:
                    self.load(str(path))
                self.assertIn("non-finite temperature for 'noul'", str(caught.exception))

    def test_a_read_table_that_is_not_a_table_is_refused(self):
        path = self.root / "read-not-a-table.json"
        path.write_text(json.dumps(dict(RECORD, reads={"direct": 3})))
        with self.assertRaises(live_parity.RecordError) as caught:
            self.load(str(path))
        self.assertIn("is not a calibration record", str(caught.exception))


class _FakeProbe:
    """The capability probe, counting how often it was asked.

    A run that cannot use its record must not spend a request finding out, so the
    count is what these cases assert as well as the exit status.
    """

    def __init__(self):
        self.calls = 0

    def __call__(self):
        self.calls += 1
        return live_parity.PROBE_SERVED, ""


class SharedRecordCase(EnvCase):
    """One run of `main` with the network and the client replaced.

    The record has to reach every model -- the thinking question is asked of a
    second model, built for its own gate -- and the provenance has to name the
    bytes it was read from. A recorder that loaded the record for itself and then
    built models without it would still write a plausible recording, and the Go
    side's hash comparison is the only thing that would notice.
    """

    PLACEHOLDER_KEY = "placeholder-key"

    def setUp(self):
        super().setUp()
        self.setenv("SHISA_DE_API_KEY", self.PLACEHOLDER_KEY)
        _FakeModel.built = []
        _FakeRecorder.instances = []
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = pathlib.Path(self.directory.name)
        self.setenv("HF_HUB_CACHE", str(self.root / "hub"))
        # A developer's own cache and home must not answer for a fixture.
        self.setenv("HF_HOME", str(self.root / "hf-home"))
        self.setenv("HOME", str(self.root / "home"))
        self.unsetenv("SHISA_DE_TOKENIZER")

    def fake_client(self):
        return types.SimpleNamespace(
            DecisionModel=_FakeModel,
            Noul=lambda instructions: ("noul", instructions),
            Choice=lambda instructions, options: ("choice", instructions, options),
            Score=lambda instructions, levels: ("score", instructions, levels),
            READOUT_VERSIONS={"de2": "de2-codebook-v1"},
            calibration_from_dict=_FakeRecord)

    def run_main(self, *arguments):
        self.probe = _FakeProbe()
        with mock.patch.object(live_parity, "client", self.fake_client), \
                mock.patch.object(live_parity, "probe_images", self.probe), \
                mock.patch.object(live_parity, "Recorder", _FakeRecorder):
            return live_parity.main(["--quiet", *arguments])

    def write_record(self, document=None) -> pathlib.Path:
        path = self.root / "calibration.json"
        path.write_text(json.dumps(document if document is not None else RECORD,
                                   indent=2) + "\n")
        return path

    def meta(self, out: pathlib.Path) -> dict:
        return json.loads(out.with_name(out.stem + ".meta.json").read_text())

    def assert_nothing_was_sent(self, out: pathlib.Path) -> None:
        self.assertEqual(self.probe.calls, 0, "the probe ran for an unusable record")
        self.assertEqual(_FakeModel.built, [])
        self.assertEqual(_FakeRecorder.instances, [])
        self.assertFalse(out.exists())
        self.assertFalse(out.with_name(out.stem + ".meta.json").exists())

    def test_the_record_reaches_every_model_and_the_provenance(self):
        path = self.write_record()
        out = self.root / "answers.json"
        self.assertEqual(self.run_main("--out", str(out), "--calibration", str(path)), 0)

        # Every model, not just the first: the thinking question is asked of a
        # second one built for its gate, and a model built without the record
        # would temper with its own selection.
        self.assertGreaterEqual(len(_FakeModel.built), 2)
        for model in _FakeModel.built:
            self.assertIsNotNone(model.kwargs["calibration"], model.kwargs)
            self.assertEqual(model.kwargs["calibration"].id, _FakeRecord(RECORD).id)

        meta = self.meta(out)
        self.assertEqual(meta["calibration"], _FakeRecord(RECORD).id)
        self.assertEqual(meta["calibration_model"], RECORD["model"])
        self.assertEqual(meta["calibration_file"], path.name)
        self.assertEqual(meta["calibration_sha256"],
                         hashlib.sha256(path.read_bytes()).hexdigest())
        self.assertEqual(meta["served_models"], ["shisa-ai/shisa-de-2.1"])

    def test_without_the_option_the_provenance_says_the_client_chose(self):
        out = self.root / "answers.json"
        self.assertEqual(self.run_main("--out", str(out)), 0)
        for model in _FakeModel.built:
            self.assertIsNone(model.kwargs["calibration"], model.kwargs)
        meta = self.meta(out)
        self.assertEqual(meta["calibration"], "")
        self.assertEqual(meta["calibration_model"], "")
        self.assertEqual(meta["calibration_file"], "")
        self.assertEqual(meta["calibration_sha256"], "")

    def test_the_provenance_names_the_tokenizer_source_it_resolved(self):
        """The meta has to say which source the pin came from, not only the pin.

        The two differ exactly where the pin was wrong before: a run whose model id
        and tokenizer source name different checkpoints.
        """
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2.1")
        self.setenv("SHISA_DE_TOKENIZER", "shisa-ai/shisa-de-2")
        out = self.root / "answers.json"
        self.assertEqual(self.run_main("--out", str(out)), 0)
        meta = self.meta(out)
        self.assertEqual(meta["tokenizer_source"], "shisa-ai/shisa-de-2")
        self.assertEqual(meta["model"], "shisa-ai/shisa-de-2.1")
        # Nothing is cached in this fixture, so the pin is empty rather than
        # borrowed from another repository.
        self.assertEqual(meta["tokenizer_revision"], "")

    def test_a_missing_record_stops_before_anything_is_sent(self):
        """Exit 5 and no output: a recording nobody can match is worse than none."""
        out = self.root / "answers.json"
        missing = self.root / "missing.json"
        with contextlib.redirect_stderr(io.StringIO()) as stderr:
            status = self.run_main("--out", str(out), "--calibration", str(missing))
        self.assertEqual(status, 5)
        self.assertIn("no calibration record", stderr.getvalue())
        self.assert_nothing_was_sent(out)

    def test_a_record_that_is_not_json_stops_before_anything_is_sent(self):
        path = self.root / "broken.json"
        path.write_text("{not json")
        out = self.root / "answers.json"
        with contextlib.redirect_stderr(io.StringIO()) as stderr:
            status = self.run_main("--out", str(out), "--calibration", str(path))
        self.assertEqual(status, 5)
        self.assertIn("is not a calibration record", stderr.getvalue())
        self.assert_nothing_was_sent(out)

    def test_a_record_missing_its_keys_stops_before_anything_is_sent(self):
        path = self.write_record({"readout_version": "de2-codebook-v1"})
        out = self.root / "answers.json"
        with contextlib.redirect_stderr(io.StringIO()) as stderr:
            status = self.run_main("--out", str(out), "--calibration", str(path))
        self.assertEqual(status, 5)
        self.assertIn("is not a calibration record", stderr.getvalue())
        self.assert_nothing_was_sent(out)

    def test_a_record_with_an_unusable_temperature_stops_before_anything_is_sent(self):
        """The client's loader accepts these, so only this check can refuse them.

        A null or a string reaches model construction, which spends the probe and
        then raises `TypeError` from `Calibration.fitted` -- an exit outside the
        documented statuses, after a request. Each case here asserts the status,
        the message, and that nothing was sent or written.
        """
        cases = (
            ("null", {"temperatures": {"noul": None}}),
            ("string", {"temperatures": {"choice": "hot"}}),
            ("null-read", {"reads": {"direct": {"noul": None}}}),
        )
        for name, override in cases:
            with self.subTest(name):
                _FakeModel.built = []
                path = self.write_record(dict(RECORD, **override))
                out = self.root / f"answers-{name}.json"
                with contextlib.redirect_stderr(io.StringIO()) as stderr:
                    status = self.run_main("--out", str(out), "--calibration", str(path))
                self.assertEqual(status, 5, stderr.getvalue())
                self.assertIn("temperature", stderr.getvalue())
                self.assert_nothing_was_sent(out)


class _FakeResponse:
    def __init__(self, status_code, text):
        self.status_code = status_code
        self.text = text


class _FakeClient:
    """The bit of `httpx.Client` the probe uses, with the answer chosen here."""

    def __init__(self, response=None, raises=None):
        self.response = response
        self.raises = raises
        self.closed = False
        self.sent = []

    def post(self, url, json=None, headers=None):
        self.sent.append((url, json, headers))
        if self.raises is not None:
            raise self.raises
        return self.response

    def close(self):
        self.closed = True


class ProbeImagesCase(EnvCase):
    """The probe's verdict, which is the only thing that may become a skip.

    Two outcomes are not enough. "The server refused an image" is a fact about
    the deployment; "I could not ask" is a fact about the run, and recording the
    second as the first drops the gate's image coverage while the run passes.
    Only a refusal that names images is allowed to be a verdict.

    The probe builds its `Authorization` header through `api_key()`, so these
    cases set a placeholder key. They passed on a machine that had a real one and
    failed on the CI runner that did not, which is the failure this class now
    cannot have.
    """

    #: Not a credential. It is here so `api_key()` has something to return, and
    #: the header assertion checks this exact value, which is what makes the
    #: dependency explicit rather than incidental.
    PLACEHOLDER_KEY = "placeholder-key"

    def setUp(self):
        super().setUp()
        self.setenv("SHISA_DE_API_KEY", self.PLACEHOLDER_KEY)

    def probe(self, response=None, raises=None):
        client = _FakeClient(response=response, raises=raises)
        return live_parity.probe_images(client), client

    def test_a_200_is_served(self):
        (verdict, reason), _ = self.probe(_FakeResponse(200, "ok"))
        self.assertEqual(verdict, live_parity.PROBE_SERVED)
        self.assertEqual(reason, "")

    def test_a_refusal_that_names_images_is_a_verdict(self):
        (verdict, reason), _ = self.probe(
            _FakeResponse(400, "the model does not support image inputs"))
        self.assertEqual(verdict, live_parity.PROBE_REFUSED)
        self.assertIn("400", reason)
        self.assertIn("image", reason)

    def test_a_transport_error_is_not_a_verdict(self):
        (verdict, reason), _ = self.probe(raises=RuntimeError("connection reset"))
        self.assertEqual(verdict, live_parity.PROBE_UNKNOWN)
        self.assertIn("connection reset", reason)

    def test_a_rate_limit_is_not_a_verdict(self):
        # 429 is the plausible one on the hosted endpoint, and it says nothing
        # about vision.
        (verdict, _), _ = self.probe(_FakeResponse(429, "rate limit exceeded"))
        self.assertEqual(verdict, live_parity.PROBE_UNKNOWN)

    def test_an_auth_failure_is_not_a_verdict(self):
        (verdict, _), _ = self.probe(_FakeResponse(401, "invalid api key"))
        self.assertEqual(verdict, live_parity.PROBE_UNKNOWN)

    def test_a_server_error_is_not_a_verdict(self):
        (verdict, _), _ = self.probe(_FakeResponse(503, "image backend unavailable"))
        self.assertEqual(verdict, live_parity.PROBE_UNKNOWN)

    def test_a_refusal_that_says_nothing_about_images_is_not_a_verdict(self):
        # A 404 from a wrong path and a 400 from a malformed body are bugs in the
        # probe, which is exactly the class the `api_url` fix came from.
        (verdict, _), _ = self.probe(_FakeResponse(404, "not found"))
        self.assertEqual(verdict, live_parity.PROBE_UNKNOWN)

    def test_the_probe_sends_an_image_content_part(self):
        _, client = self.probe(_FakeResponse(200, "ok"))
        _, body, headers = client.sent[0]
        parts = body["messages"][1]["content"]
        self.assertEqual(parts[0]["type"], "image_url")
        self.assertTrue(parts[0]["image_url"]["url"].startswith("data:image/png;base64,"))
        self.assertEqual(headers["Authorization"], f"Bearer {self.PLACEHOLDER_KEY}")

    def test_an_injected_client_is_not_closed(self):
        # The caller owns the client it passed in; the probe only closes its own.
        _, client = self.probe(_FakeResponse(200, "ok"))
        self.assertFalse(client.closed)


# -- the tokenizer pin --------------------------------------------------------


class TokenizerCase(EnvCase):
    """Which tokenizer bytes the recording is pinned to.

    The recorder hands the Python client `SHISA_DE_TOKENIZER` (or the model id) as
    its tokenizer source, so that is what the pin has to describe. It used to read
    the model id's cache directory unconditionally: a run with
    `SHISA_DE_MODEL=shisa-ai/shisa-de-2.1` and `SHISA_DE_TOKENIZER=shisa-ai/shisa-de-2`
    reported `ambiguous: 2 snapshots` for a source that has one, so the metadata
    named neither the tokenizer the run loaded nor a reason it could not.

    The cases cover the four forms a source takes: a named hub repository, a cached
    snapshot path, a local directory and a local file. The home directory is
    replaced as well as the two cache variables, so a developer's own cache cannot
    answer for a fixture -- the first version of these tests passed on a machine
    whose cache held the checkpoint and failed on one whose did not.
    """

    SNAPSHOT = "8f21734b27b3462471c4662d5638e52195b005a3"
    OTHER = "0123456789abcdef0123456789abcdef01234567"

    def setUp(self):
        super().setUp()
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = pathlib.Path(self.directory.name)
        self.cache = self.root / "hub"
        self.cache.mkdir()
        self.setenv("HF_HUB_CACHE", str(self.cache))
        self.setenv("HF_HOME", str(self.root / "hf-home"))
        self.setenv("HOME", str(self.root / "home"))
        self.unsetenv("SHISA_DE_TOKENIZER")

    def snapshot(self, repo, revision, *, tokenizer=True):
        """One cached snapshot, with a tokenizer.json unless a test wants a partial one."""
        path = self.cache / ("models--" + repo.replace("/", "--")) / "snapshots" / revision
        path.mkdir(parents=True, exist_ok=True)
        if tokenizer:
            (path / "tokenizer.json").write_text('{"model": {"vocab": {}}}')
        return path

    def ref(self, repo, revision, content=None):
        base = self.cache / ("models--" + repo.replace("/", "--"))
        (base / "refs").mkdir(parents=True, exist_ok=True)
        (base / "refs" / "main").write_text(revision if content is None else content)
        return base

    # -- a named hub source ---------------------------------------------------

    def test_the_tokenizer_variable_decides_over_the_model_id(self):
        """The reported failure: the model id's cache was read instead of the source's.

        DE-2.1 is served with DE-2's tokenizer, so the two names point at different
        cache directories and only one of them is the tokenizer the run loads.
        """
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2.1")
        self.setenv("SHISA_DE_TOKENIZER", "shisa-ai/shisa-de-2")
        self.snapshot("shisa-ai/shisa-de-2.1", self.OTHER)
        self.snapshot("shisa-ai/shisa-de-2.1", "fedcba9876543210fedcba9876543210fedcba98")
        self.snapshot("shisa-ai/shisa-de-2", self.SNAPSHOT)
        self.assertEqual(live_parity.tokenizer_source(), "shisa-ai/shisa-de-2")
        self.assertEqual(live_parity.tokenizer_revision(), self.SNAPSHOT)

    def test_the_model_id_is_the_source_when_the_variable_is_unset(self):
        self.setenv("SHISA_DE_MODEL", "someone/other-de2")
        self.snapshot("someone/other-de2", self.SNAPSHOT)
        self.assertEqual(live_parity.tokenizer_source(), "someone/other-de2")
        self.assertEqual(live_parity.tokenizer_revision(), self.SNAPSHOT)

    def test_pins_the_one_cached_snapshot(self):
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2")
        self.snapshot("shisa-ai/shisa-de-2", self.SNAPSHOT)
        self.assertEqual(live_parity.tokenizer_revision(), self.SNAPSHOT)

    def test_refs_main_chooses_between_snapshots(self):
        """`refs/main` is the revision an unpinned load resolves, so it decides."""
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2")
        self.snapshot("shisa-ai/shisa-de-2", self.OTHER)
        self.snapshot("shisa-ai/shisa-de-2", self.SNAPSHOT)
        self.ref("shisa-ai/shisa-de-2", self.SNAPSHOT + "\n")
        self.assertEqual(live_parity.tokenizer_revision(), self.SNAPSHOT)

    def test_a_ref_that_is_not_a_snapshot_does_not_pin_anything(self):
        """A ref's content is a path component, so it is validated rather than joined."""
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2")
        self.snapshot("shisa-ai/shisa-de-2", self.SNAPSHOT)
        self.ref("shisa-ai/shisa-de-2", "../../etc")
        self.assertEqual(live_parity.tokenizer_revision(), self.SNAPSHOT)

    def test_reports_ambiguity_instead_of_pinning_one(self):
        """Two usable snapshots and no ref must not look like a revision.

        Picking one silently would pin a tokenizer the run may not have used, and
        an empty string would read as "not cached".
        """
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2")
        self.snapshot("shisa-ai/shisa-de-2", self.SNAPSHOT)
        self.snapshot("shisa-ai/shisa-de-2", self.OTHER)
        revision = live_parity.tokenizer_revision()
        self.assertNotIn(revision, (self.SNAPSHOT, self.OTHER))
        self.assertTrue(revision.startswith("ambiguous"), revision)
        self.assertIn("no refs/main", revision)

    def test_a_snapshot_without_a_tokenizer_is_not_a_pin(self):
        """A partial download is not a tokenizer, so it cannot be the one that ran."""
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2")
        self.snapshot("shisa-ai/shisa-de-2", self.OTHER, tokenizer=False)
        self.snapshot("shisa-ai/shisa-de-2", self.SNAPSHOT)
        self.assertEqual(live_parity.tokenizer_revision(), self.SNAPSHOT)

    def test_only_an_empty_snapshot_is_no_pin_at_all(self):
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2")
        self.snapshot("shisa-ai/shisa-de-2", self.SNAPSHOT, tokenizer=False)
        self.assertEqual(live_parity.tokenizer_revision(), "")

    def test_says_when_there_is_nothing_cached(self):
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2")
        self.assertEqual(live_parity.tokenizer_revision(), "")

    def test_hf_home_is_searched_when_the_cache_variable_is_unset(self):
        """The second cache root the clients read, in their order."""
        self.unsetenv("HF_HUB_CACHE")
        home = pathlib.Path(self.root / "hf-home") / "hub"
        path = home / "models--shisa-ai--shisa-de-2" / "snapshots" / self.SNAPSHOT
        path.mkdir(parents=True)
        (path / "tokenizer.json").write_text("{}")
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2")
        self.assertEqual(live_parity.tokenizer_revision(), self.SNAPSHOT)

    def test_the_hub_cache_variable_wins_over_the_home_one(self):
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2")
        self.snapshot("shisa-ai/shisa-de-2", self.SNAPSHOT)
        path = pathlib.Path(self.root / "hf-home") / "hub" / "models--shisa-ai--shisa-de-2" / "snapshots" / self.OTHER
        path.mkdir(parents=True)
        (path / "tokenizer.json").write_text("{}")
        self.assertEqual(live_parity.tokenizer_revision(), self.SNAPSHOT)

    # -- an explicit cached snapshot -----------------------------------------

    def test_an_explicit_snapshot_directory_pins_its_hash(self):
        """How a caller pins one snapshot of a repository that has several."""
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2.1")
        chosen = self.snapshot("shisa-ai/shisa-de-2.1", self.SNAPSHOT)
        self.snapshot("shisa-ai/shisa-de-2.1", self.OTHER)
        self.setenv("SHISA_DE_TOKENIZER", str(chosen))
        self.assertEqual(live_parity.tokenizer_revision(), self.SNAPSHOT)

    def test_an_explicit_snapshot_tokenizer_json_pins_the_snapshot(self):
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2.1")
        chosen = self.snapshot("shisa-ai/shisa-de-2.1", self.SNAPSHOT)
        self.setenv("SHISA_DE_TOKENIZER", str(chosen / "tokenizer.json"))
        self.assertEqual(live_parity.tokenizer_revision(), self.SNAPSHOT)

    def test_an_explicit_snapshot_that_is_not_here_is_not_a_pin(self):
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2")
        self.setenv("SHISA_DE_TOKENIZER",
                    str(self.cache / "models--shisa-ai--shisa-de-2" / "snapshots" / self.SNAPSHOT))
        self.assertEqual(live_parity.tokenizer_revision(), "")

    def test_an_explicit_snapshot_with_an_empty_tokenizer_is_not_a_pin(self):
        """An empty file is a partial download, so it names no tokenizer bytes."""
        self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2.1")
        chosen = self.snapshot("shisa-ai/shisa-de-2.1", self.SNAPSHOT)
        (chosen / "tokenizer.json").write_text("")
        for source in (str(chosen), str(chosen / "tokenizer.json")):
            self.setenv("SHISA_DE_TOKENIZER", source)
            self.assertEqual(live_parity.tokenizer_revision(), "", source)

    # -- a local tokenizer ----------------------------------------------------

    def test_a_local_directory_is_pinned_by_the_digest_of_its_tokenizer(self):
        """No revision names a local directory's bytes, so the bytes are the pin."""
        local = self.root / "shisa-de-2-tok"
        local.mkdir()
        (local / "tokenizer.json").write_text('{"model": {"vocab": {"a": 0}}}')
        self.setenv("SHISA_DE_TOKENIZER", str(local))
        digest = hashlib.sha256((local / "tokenizer.json").read_bytes()).hexdigest()
        self.assertEqual(live_parity.tokenizer_revision(),
                         f"local:shisa-de-2-tok:sha256:{digest}")

    def test_two_local_directories_with_different_bytes_pin_differently(self):
        one, other = self.root / "one", self.root / "other"
        for directory, text in ((one, '{"a": 1}'), (other, '{"a": 2}')):
            directory.mkdir()
            (directory / "tokenizer.json").write_text(text)
        self.setenv("SHISA_DE_TOKENIZER", str(one))
        first = live_parity.tokenizer_revision()
        self.setenv("SHISA_DE_TOKENIZER", str(other))
        self.assertNotEqual(first, live_parity.tokenizer_revision())

    def test_a_local_directory_without_a_tokenizer_says_so(self):
        """A clear local provenance, not an empty string that reads as "not cached"."""
        local = self.root / "empty-tok"
        local.mkdir()
        self.setenv("SHISA_DE_TOKENIZER", str(local))
        self.assertEqual(live_parity.tokenizer_revision(),
                         "local:empty-tok:no tokenizer.json")

    def test_a_local_tokenizer_file_is_pinned_by_its_own_digest(self):
        """A file source is what the Go client loads directly."""
        path = self.root / "my-tokenizer.json"
        path.write_text('{"model": {"vocab": {}}}')
        self.setenv("SHISA_DE_TOKENIZER", str(path))
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        self.assertEqual(live_parity.tokenizer_revision(),
                         f"local:my-tokenizer.json:sha256:{digest}")

    def test_a_local_pin_never_looks_like_a_hub_revision(self):
        """The value goes into a meta field named for a revision, so it says it is not one."""
        local = self.root / self.SNAPSHOT
        local.mkdir()
        (local / "tokenizer.json").write_text("{}")
        self.setenv("SHISA_DE_TOKENIZER", str(local))
        revision = live_parity.tokenizer_revision()
        self.assertNotEqual(revision, self.SNAPSHOT)
        self.assertTrue(revision.startswith("local:"), revision)


# -- the drift arithmetic -----------------------------------------------------


def answer(value, probabilities, strategy="repeat2", requests=2, **extra):
    """One recorded answer, plus anything a particular question adds to it."""
    return {"value": value, "probabilities": probabilities,
            "strategy": strategy, "requests": requests, **extra}


class DriftCase(unittest.TestCase):
    def test_identical_passes_are_zero(self):
        one = {"q": answer(0.8, {"yes": 0.8, "no": 0.2}, strategy="repeat2", requests=1)}
        report = live_parity.drift([one, one])
        self.assertEqual(report["q"]["max_probability_delta"], 0.0)
        self.assertEqual(report["q"]["value_delta"], 0.0)
        self.assertTrue(report["q"]["same_value"])
        self.assertTrue(report["q"]["same_strategy"])
        self.assertTrue(report["q"]["same_requests"])

    def test_a_non_numeric_value_has_no_delta(self):
        one = {"q": answer("a", {"a": 0.75, "b": 0.25})}
        report = live_parity.drift([one, one])
        self.assertIsNone(report["q"]["value_delta"])
        self.assertTrue(report["q"]["same_value"])

    def test_reports_the_largest_single_key_difference(self):
        first = {"q": answer("a", {"a": 0.75, "b": 0.25})}
        second = {"q": answer("a", {"a": 0.70, "b": 0.30})}
        report = live_parity.drift([first, second])
        self.assertAlmostEqual(report["q"]["max_probability_delta"], 0.05)
        self.assertTrue(report["q"]["same_value"])

    def test_notices_a_moved_value(self):
        first = {"q": answer("a", {"a": 0.75, "b": 0.25})}
        second = {"q": answer("b", {"a": 0.25, "b": 0.75})}
        self.assertFalse(live_parity.drift([first, second])["q"]["same_value"])

    def test_a_numeric_value_moves_only_beyond_the_tolerance(self):
        """A noul or a score answers with a float, so equality is not the test.

        A one-ULP jitter used to read as an unstable value, which turned a run
        the probability check would have accepted into a hard failure.
        """
        first = {"q": answer(0.8, {"yes": 0.8, "no": 0.2}, strategy="repeat2", requests=1)}
        second = {"q": answer(0.8 + 1e-16, {"yes": 0.8, "no": 0.2},
                              strategy="repeat2", requests=1)}
        report = live_parity.drift([first, second])
        self.assertTrue(report["q"]["same_value"])
        self.assertIsNotNone(report["q"]["value_delta"])
        self.assertLessEqual(report["q"]["value_delta"], live_parity.TOLERANCE)

    def test_a_numeric_value_that_really_moved_is_unstable(self):
        first = {"q": answer(0.8, {"yes": 0.8, "no": 0.2}, strategy="repeat2", requests=1)}
        second = {"q": answer(0.2, {"yes": 0.2, "no": 0.8},
                              strategy="repeat2", requests=1)}
        report = live_parity.drift([first, second])
        self.assertFalse(report["q"]["same_value"])
        self.assertAlmostEqual(report["q"]["value_delta"], 0.6)

    def test_a_non_numeric_value_is_compared_as_itself(self):
        first = {"q": answer("card_details", {"card_details": 0.9, "nothing": 0.1})}
        second = {"q": answer("nothing", {"card_details": 0.1, "nothing": 0.9})}
        report = live_parity.drift([first, second])
        self.assertFalse(report["q"]["same_value"])
        self.assertIsNone(report["q"]["value_delta"])

    def test_notices_a_moved_strategy_and_request_count(self):
        first = {"q": answer("a", {"a": 1.0}, strategy="repeat2", requests=2)}
        second = {"q": answer("a", {"a": 1.0}, strategy="direct", requests=1)}
        report = live_parity.drift([first, second])
        self.assertFalse(report["q"]["same_strategy"])
        self.assertFalse(report["q"]["same_requests"])

    def test_covers_every_consecutive_pair_not_just_the_first_two(self):
        """`--passes 4` measures more of the server, not less.

        Only the first pair used to be compared, so a server that moved on the
        third pass was reported as repeatable.
        """
        steady = {"q": answer("a", {"a": 0.75, "b": 0.25})}
        moved = {"q": answer("b", {"a": 0.25, "b": 0.75})}
        report = live_parity.drift([steady, steady, moved])
        self.assertFalse(report["q"]["same_value"])
        self.assertAlmostEqual(report["q"]["max_probability_delta"], 0.5)

    def test_keeps_the_worst_probability_delta_across_pairs(self):
        one = {"q": answer("a", {"a": 0.90, "b": 0.10})}
        two = {"q": answer("a", {"a": 0.80, "b": 0.20})}
        three = {"q": answer("a", {"a": 0.85, "b": 0.15})}
        report = live_parity.drift([one, two, three])
        self.assertAlmostEqual(report["q"]["max_probability_delta"], 0.10)

    def test_two_passes_report_the_same_shape_as_more(self):
        one = {"q": answer("a", {"a": 0.75, "b": 0.25})}
        self.assertEqual(
            set(live_parity.drift([one, one])["q"]),
            {"max_probability_delta", "value_delta", "same_value",
             "same_strategy", "same_requests", "same_system_render"})

    def test_notices_a_system_render_that_flipped(self):
        """An image question's verdict on the system turn, not a measurement.

        Two passes that disagree on it are the server being unstable in the one
        place the image read makes a judgement, so it belongs in the same report
        as a moved value even though it has no delta.
        """
        one = {"q": answer("a", {"a": 1.0}, system_render="differs")}
        two = {"q": answer("a", {"a": 1.0}, system_render="string")}
        report = live_parity.drift([one, two])
        self.assertFalse(report["q"]["same_system_render"])
        self.assertTrue(report["q"]["same_value"])
        # A text question has no such key on either side, and that is agreement.
        self.assertTrue(
            live_parity.drift([{"q": answer("a", {"a": 1.0})},
                               {"q": answer("a", {"a": 1.0})}])["q"]["same_system_render"])

    def test_handles_a_key_the_other_pass_lacks(self):
        first = {"q": answer("a", {"a": 0.75, "b": 0.25})}
        second = {"q": answer("a", {"a": 1.0})}
        self.assertAlmostEqual(
            live_parity.drift([first, second])["q"]["max_probability_delta"], 0.25)

    def test_three_passes_are_enough_for_a_report(self):
        """Two passes is the floor, and the guard is on the script, not here."""
        one = {"q": answer("a", {"a": 0.75, "b": 0.25})}
        report = live_parity.drift([one, one, one])
        self.assertEqual(report["q"]["max_probability_delta"], 0.0)


# -- the question set ---------------------------------------------------------


class QuestionSetCase(unittest.TestCase):
    def document(self):
        return json.loads(live_parity.QUESTIONS.read_text())

    def test_every_question_has_the_keys_the_recorder_reads(self):
        for spec in self.document()["questions"]:
            self.assertTrue(spec["id"] and spec["head"] and spec["type"], spec)
            self.assertIn("evidence", spec)
            if spec["type"] == "choice":
                self.assertTrue(spec["options"], spec)
            if spec["type"] == "score":
                self.assertTrue(spec["levels"], spec)

    def test_the_set_covers_more_than_the_default_read(self):
        """The gate is the only live check of the policy and probability options.

        Without these, a `direct` read or the one-read view is never compared
        against the endpoint by anything.
        """
        questions = self.document()["questions"]
        policies = {spec.get("policy") for spec in questions}
        self.assertIn("direct", policies)
        self.assertTrue(any(spec.get("probability") for spec in questions))
        self.assertTrue(any(spec.get("gate") for spec in questions))

    def test_every_question_id_is_unique(self):
        ids = [spec["id"] for spec in self.document()["questions"]]
        self.assertEqual(len(ids), len(set(ids)), ids)

    def test_every_type_the_recorder_builds_is_used(self):
        kinds = {spec["type"] for spec in self.document()["questions"]}
        self.assertEqual(kinds, {"noul", "choice", "score"})

    def test_the_set_covers_the_image_read(self):
        """The image read is the one read that needs a different endpoint path.

        Without a question carrying `image`, nothing in the live suite ever sends
        an image, and a change to the chat request would be compared against
        nothing.
        """
        images = [spec for spec in self.document()["questions"] if spec.get("image")]
        self.assertTrue(images, "no question in the set carries an image")
        for spec in images:
            # A data URL, so both clients send the same bytes with no shared file.
            self.assertTrue(spec["image"].startswith("data:image/png;base64,"), spec["id"])
            payload = base64.b64decode(spec["image"].split(",", 1)[1])
            self.assertTrue(payload.startswith(b"\x89PNG\r\n\x1a\n"),
                            f"{spec['id']} is not a PNG")
            # Two options, so the answer carries a distribution to compare rather
            # than a single code whose top-k has nothing to disagree about.
            self.assertEqual(len(spec["options"]), 2, spec["id"])

    def test_the_image_probe_is_a_png(self):
        """The capability probe has to carry a real image, or a text-only server
        would answer it 200 and the gate would report vision it does not have."""
        payload = base64.b64decode(live_parity._PROBE_IMAGE.split(",", 1)[1])
        self.assertTrue(payload.startswith(b"\x89PNG\r\n\x1a\n"))
        self.assertEqual(live_parity._PROBE_IMAGE.split(",", 1)[0], "data:image/png;base64")


if __name__ == "__main__":
    unittest.main(verbosity=2)
