#!/usr/bin/env python3
"""Offline counterexamples for upload/pre recovery design, NOT the AList driver.

Uses only synthetic data. No networking, credential discovery, provider calls,
filesystem writes, or runtime imports from AList. Run with Python >= 3.8:
    python3 docs/experiments/quark_upload_pre_lab.py
"""
import json
import unittest
from dataclasses import dataclass
from typing import Any, Dict, Optional


MAX_BODY_BYTES = 65536


def _unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def _integer_field(obj: Dict[str, Any], key: str) -> Dict[str, Any]:
    if key not in obj:
        return {"state": "missing"}
    value = obj[key]
    if value is None:
        return {"state": "null"}
    if type(value) is not int or not -(2 ** 31) <= value < 2 ** 31:
        return {"state": "invalid"}
    return {"state": "integer", "value": value}


def _identifier_field(obj: Dict[str, Any], key: str) -> str:
    if key not in obj:
        return "missing"
    value = obj[key]
    if value is None:
        return "null"
    if not isinstance(value, str):
        return "invalid"
    return "nonempty" if value.strip() else "empty"


def summarize(http_status: Optional[int], body: bytes) -> Dict[str, Any]:
    """Proposed shape-only evidence projection; never emit raw body or IDs.

    This helper is exercised only with fixtures by this file. It does not
    authorize capture of real responses or prove that a task was not allocated.
    """
    valid_http = type(http_status) is int and 100 <= http_status <= 599
    result = {"http_status": http_status if valid_http else None,
              "json_state": "unparsed", "allocation_state": "UNKNOWN"}
    if len(body) > MAX_BODY_BYTES:
        result["json_state"] = "oversized"
        return result
    try:
        obj = json.loads(body.decode("utf-8"), object_pairs_hook=_unique_object)
    except (UnicodeError, ValueError, RecursionError):
        result["json_state"] = "invalid"
        return result
    if not isinstance(obj, dict):
        result["json_state"] = "not_object"
        return result
    result["json_state"] = "object"
    result["status"] = _integer_field(obj, "status")
    result["code"] = _integer_field(obj, "code")
    data = obj.get("data")
    if "data" not in obj:
        result["data_state"] = "missing"
    elif data is None:
        result["data_state"] = "null"
    elif not isinstance(data, dict):
        result["data_state"] = "invalid"
    else:
        result["data_state"] = "object"
        result["identifiers"] = {key: _identifier_field(data, key)
                                 for key in ("task_id", "fid", "upload_id")}
    return result


@dataclass(frozen=True)
class Observation:
    http_status: Optional[int]
    provider_status: Optional[int]
    code: Optional[int]
    task_id: Optional[str] = None
    fid: Optional[str] = None
    transport_error: bool = False
    cancelled: bool = False


def proposed_next_action(obs: Observation) -> str:
    """Conservative model only; a PRE handle is NOT upload completion."""
    if obs.cancelled:
        return "STOP_CANCELLED"
    integers = all(type(v) is int for v in
                   (obs.http_status, obs.provider_status, obs.code))
    handles = all(isinstance(v, str) and bool(v.strip())
                  for v in (obs.task_id, obs.fid))
    if (not obs.transport_error and integers and handles and
            (obs.http_status, obs.provider_status, obs.code) == (200, 200, 0)):
        return "CONTINUE_EXISTING_TASK_NOT_UPLOAD_SUCCESS"
    return "STOP_ALLOCATION_UNKNOWN"


class SyntheticProvider:
    """Two possible histories, not a claim about Quark's actual internals."""
    def __init__(self, allocate_before_error: bool, drop_response: bool = False):
        self.allocate_before_error = allocate_before_error
        self.drop_response = drop_response
        self.calls = 0
        self.allocations = 0

    def pre(self) -> Observation:
        self.calls += 1
        if self.allocate_before_error:
            self.allocations += 1
        if self.drop_response:
            return Observation(None, None, None, transport_error=True)
        return Observation(500, 500, 50000)

    def visible_files(self):
        # A mock pending task need not be a visible completed file.
        return []


def model_hidden_transport_retry(provider: SyntheticProvider, retries: int):
    for _ in range(retries + 1):
        obs = provider.pre()
        if not obs.transport_error:
            return obs
    return obs


class AmbiguityTests(unittest.TestCase):
    def test_identical_errors_can_hide_different_allocations(self):
        before = SyntheticProvider(False)
        after = SyntheticProvider(True)
        self.assertEqual(before.pre(), after.pre())
        self.assertEqual((before.allocations, after.allocations), (0, 1))

    def test_empty_listing_does_not_resolve_hidden_task(self):
        before, after = SyntheticProvider(False), SyntheticProvider(True)
        before.pre()
        after.pre()
        self.assertEqual(before.visible_files(), after.visible_files())
        self.assertNotEqual(before.allocations, after.allocations)

    def test_blind_retry_can_allocate_twice(self):
        provider = SyntheticProvider(True)
        provider.pre()
        provider.pre()
        self.assertEqual(provider.allocations, 2)

    def test_three_hidden_retries_can_make_four_attempts(self):
        provider = SyntheticProvider(True, drop_response=True)
        model_hidden_transport_retry(provider, 3)
        self.assertEqual((provider.calls, provider.allocations), (4, 4))

    def test_no_replay_limits_new_allocations_but_not_orphans(self):
        provider = SyntheticProvider(True, drop_response=True)
        self.assertEqual(proposed_next_action(provider.pre()),
                         "STOP_ALLOCATION_UNKNOWN")
        self.assertEqual((provider.calls, provider.allocations), (1, 1))

    def test_provider_error_must_not_be_treated_as_nonallocation(self):
        self.assertEqual(proposed_next_action(Observation(500, 500, 50000)),
                         "STOP_ALLOCATION_UNKNOWN")

    def test_error_with_identifiers_is_not_permission_to_resume(self):
        self.assertEqual(proposed_next_action(Observation(
            500, 500, 50000, "synthetic-task", "synthetic-fid")),
            "STOP_ALLOCATION_UNKNOWN")

    def test_candidate_handle_is_not_upload_completion(self):
        self.assertEqual(proposed_next_action(Observation(
            200, 200, 0, "synthetic-task", "synthetic-fid")),
            "CONTINUE_EXISTING_TASK_NOT_UPLOAD_SUCCESS")

    def test_uncertain_success_shapes_stop(self):
        for values in ((201, 200, 0), (200, None, 0), (200, 200, None),
                       (200, 0, 0), (200, 200, False), (200, 200, "0"),
                       (204, 200, 0), (500, 200, 0)):
            with self.subTest(values=values):
                self.assertEqual(proposed_next_action(Observation(
                    *values, task_id="synthetic-task", fid="synthetic-fid")),
                    "STOP_ALLOCATION_UNKNOWN")

    def test_missing_handles_stop(self):
        for task, fid in ((None, None), ("", "f"), ("t", ""), ("t", " ")):
            with self.subTest(task=task, fid=fid):
                self.assertEqual(proposed_next_action(Observation(
                    200, 200, 0, task, fid)), "STOP_ALLOCATION_UNKNOWN")

    def test_cancelled_observation_never_resumes(self):
        self.assertEqual(proposed_next_action(Observation(
            200, 200, 0, "t", "f", cancelled=True)), "STOP_CANCELLED")

    def test_completed_file_dedup_does_not_prove_pending_task_dedup(self):
        completed = {("existing.bin", "same-hash"): "completed-fid"}
        pending_tasks = []

        def possible_pre(name, digest):
            if (name, digest) in completed:
                return completed[(name, digest)]
            task = "pending-" + str(len(pending_tasks) + 1)
            pending_tasks.append(task)
            return task

        self.assertEqual(possible_pre("existing.bin", "same-hash"),
                         possible_pre("existing.bin", "same-hash"))
        self.assertNotEqual(possible_pre("new.bin", "same-hash"),
                            possible_pre("new.bin", "same-hash"))

    def test_name_size_hash_equality_does_not_identify_allocation(self):
        old = {"fid": "old", "name": "test.bin", "size": 1, "hash": "same"}
        new = dict(old, fid="new")
        self.assertEqual({k: old[k] for k in ("name", "size", "hash")},
                         {k: new[k] for k in ("name", "size", "hash")})
        self.assertNotEqual(old["fid"], new["fid"])


class EvidenceProjectionTests(unittest.TestCase):
    def test_secrets_and_identifiers_are_not_emitted(self):
        raw = {"status": 500, "code": 50000,
               "message": "DO_NOT_EXPORT_SECRET",
               "Cookie": "DO_NOT_EXPORT_SECRET", "Authorization": "DO_NOT_EXPORT_SECRET",
               "data": {"task_id": "DO_NOT_EXPORT_SECRET", "fid": "DO_NOT_EXPORT_SECRET",
                        "upload_id": "DO_NOT_EXPORT_SECRET", "auth_info": "DO_NOT_EXPORT_SECRET",
                        "obj_key": "DO_NOT_EXPORT_SECRET", "callback": "DO_NOT_EXPORT_SECRET"}}
        out = summarize(500, json.dumps(raw).encode())
        self.assertNotIn("DO_NOT_EXPORT_SECRET", json.dumps(out))
        self.assertEqual(out["identifiers"]["task_id"], "nonempty")
        self.assertEqual(out["allocation_state"], "UNKNOWN")

    def test_absent_ids_do_not_mean_no_allocation(self):
        self.assertEqual(summarize(500, b'{"status":500,"code":50000}')
                         ["allocation_state"], "UNKNOWN")

    def test_missing_null_empty_and_invalid_are_distinct(self):
        for data, state in (({}, "missing"), ({"fid": None}, "null"),
                            ({"fid": ""}, "empty"), ({"fid": 7}, "invalid")):
            with self.subTest(state=state):
                out = summarize(500, json.dumps({"data": data}).encode())
                self.assertEqual(out["identifiers"]["fid"], state)

    def test_provider_zero_is_not_missing(self):
        self.assertEqual(summarize(200, b'{"code":0}')["code"],
                         {"state": "integer", "value": 0})
        self.assertEqual(summarize(200, b'{}')["code"], {"state": "missing"})
        self.assertEqual(summarize(200, b'{"code":null}')["code"], {"state": "null"})

    def test_invalid_integer_types(self):
        for value in (False, "0", 0.0, 2 ** 64, [], {}):
            with self.subTest(value=value):
                out = summarize(200, json.dumps({"code": value}).encode())
                self.assertEqual(out["code"], {"state": "invalid"})

    def test_untrusted_body_is_bounded(self):
        self.assertEqual(summarize(502, b'x' * (MAX_BODY_BYTES + 1))["json_state"],
                         "oversized")

    def test_html_malformed_unicode_and_duplicate_keys(self):
        for body in (b'<html>DO_NOT_EXPORT_SECRET</html>', b'{', b'\xff',
                     b'{"status":200,"status":500}',
                     b'{"data":{"fid":"a","fid":"b"}}'):
            with self.subTest(body=body):
                out = summarize(502, body)
                self.assertEqual(out["json_state"], "invalid")
                self.assertNotIn("DO_NOT_EXPORT_SECRET", json.dumps(out))

    def test_nonobject_json(self):
        for body in (b'null', b'[]', b'1', b'"DO_NOT_EXPORT_SECRET"'):
            with self.subTest(body=body):
                self.assertEqual(summarize(500, body)["json_state"], "not_object")

    def test_partial_error_ids_remain_unknown(self):
        out = summarize(500, b'{"status":500,"code":50000,"data":{"task_id":"t"}}')
        self.assertEqual(out["identifiers"],
                         {"task_id": "nonempty", "fid": "missing", "upload_id": "missing"})
        self.assertEqual(out["allocation_state"], "UNKNOWN")

    def test_http_status_not_coerced(self):
        for status in (None, True, "200", 0, 600):
            with self.subTest(status=status):
                self.assertIsNone(summarize(status, b'{}')["http_status"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
