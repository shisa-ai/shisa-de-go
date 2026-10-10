#!/usr/bin/env python3
"""Offline tests for the parts of `live_parity.py` that decide what a run means.

    python3 -m unittest discover -s scripts -p 'test_*.py'

These need no network, no API key and no `shisa_de` checkout: they cover the
endpoint and model resolution, the tokenizer pin, and the drift arithmetic that
tells "the server moved" from "the port differs". The client and `httpx` are
imported lazily for exactly this reason, so `python3` and the standard library
are the whole dependency.

The rest of the recorder -- the requests, the recording, the provenance -- is
exercised by `TestLiveParityAgainstPython` in `live_test.go`, which runs the
script against a real endpoint and reads what it wrote.
"""

from __future__ import annotations

import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import live_parity  # noqa: E402

# The two variables both halves of the gate resolve. Set by a test, cleared by
# its teardown.
ENV_VARS = ("SHISA_DE_ENDPOINT", "SHISA_DE_MODEL", "HF_HUB_CACHE")


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


# -- the tokenizer pin --------------------------------------------------------


class TokenizerRevisionCase(EnvCase):
    def test_pins_the_one_cached_snapshot(self):
        with tempfile.TemporaryDirectory() as cache:
            self.setenv("HF_HUB_CACHE", cache)
            self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2")
            (pathlib.Path(cache) / "models--shisa-ai--shisa-de-2" / "snapshots"
             / "abc123").mkdir(parents=True)
            self.assertEqual(live_parity.tokenizer_revision(), "abc123")

    def test_says_when_there_is_nothing_cached(self):
        with tempfile.TemporaryDirectory() as cache:
            self.setenv("HF_HUB_CACHE", cache)
            self.assertEqual(live_parity.tokenizer_revision(), "")

    def test_reports_ambiguity_instead_of_pinning_one(self):
        """Two snapshots must not look like a revision.

        Picking one silently would pin a tokenizer the run may not have used, and
        an empty string would read as "not cached".
        """
        with tempfile.TemporaryDirectory() as cache:
            self.setenv("HF_HUB_CACHE", cache)
            self.setenv("SHISA_DE_MODEL", "shisa-ai/shisa-de-2")
            snapshots = pathlib.Path(cache) / "models--shisa-ai--shisa-de-2" / "snapshots"
            (snapshots / "abc123").mkdir(parents=True)
            (snapshots / "def456").mkdir()
            revision = live_parity.tokenizer_revision()
            self.assertNotIn(revision, ("abc123", "def456"))
            self.assertTrue(revision.startswith("ambiguous"), revision)

    def test_follows_the_model_it_is_pointed_at(self):
        with tempfile.TemporaryDirectory() as cache:
            self.setenv("HF_HUB_CACHE", cache)
            self.setenv("SHISA_DE_MODEL", "someone/other-de2")
            (pathlib.Path(cache) / "models--someone--other-de2" / "snapshots"
             / "zzz").mkdir(parents=True)
            self.assertEqual(live_parity.tokenizer_revision(), "zzz")


# -- the drift arithmetic -----------------------------------------------------


def answer(value, probabilities, strategy="repeat2", requests=2):
    return {"value": value, "probabilities": probabilities,
            "strategy": strategy, "requests": requests}


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
             "same_strategy", "same_requests"})

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


if __name__ == "__main__":
    unittest.main(verbosity=2)
