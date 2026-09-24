#!/usr/bin/env python3
"""Offline counterexamples for upload/pre recovery design, NOT the AList driver.

Uses only synthetic data. No networking, credential discovery, provider calls,
filesystem writes, or runtime imports from AList. Run with Python >= 3.8:
    python3 docs/experiments/quark_upload_pre_lab.py

The ambiguity tests are illustrative counterexamples. They demonstrate why a
client-visible failure cannot establish provider allocation state; they do not
model or prove Quark's internal implementation.
"""
import json
import unittest
from dataclasses import dataclass, replace
from typing import Any, Dict, List, Optional, Tuple


MAX_BODY_BYTES = 65536
INT64_MIN = -(2 ** 63)
INT64_MAX = (2 ** 63) - 1


class PairObject(list):
    """JSON object represented as ordered key/value pairs, including duplicates."""


JsonPairs = List[Tuple[str, Any]]


def _pairs_object(pairs: JsonPairs) -> PairObject:
    return PairObject(pairs)


def _reject_json_constant(value: str) -> None:
    raise ValueError("non-standard JSON constant: " + value)


def _ascii_case_equal(candidate: str, key: str) -> bool:
    """Conservative case-insensitive match for the audited ASCII JSON tags.

    Go encoding/json uses Unicode simple folding for struct-field matching.
    This research projection intentionally models only exact matches plus ASCII
    case variants of the known lower-case tags. Non-ASCII folding remains an
    explicit approximation boundary rather than being over-accepted by Python's
    broader str.casefold().
    """
    return candidate == key or (
        candidate.isascii() and key.isascii() and candidate.lower() == key.lower()
    )


def _go_field(obj: PairObject, key: str) -> Tuple[bool, Any]:
    """Approximate encoding/json struct-field matching for audited fields.

    Later matching non-null values replace earlier values. For the non-pointer
    scalar/struct fields projected here, a later JSON null is modeled as a
    no-op when an earlier matching value already exists. Error propagation from
    incompatible JSON types is deliberately outside this shape-only model.
    """
    found = False
    value: Any = None
    for candidate, candidate_value in obj:
        if _ascii_case_equal(candidate, key):
            if candidate_value is None and found:
                continue
            found = True
            value = candidate_value
    return found, value


def _raw_key_state(obj: PairObject, key: str) -> Dict[str, Any]:
    matches = [candidate for candidate, _ in obj if _ascii_case_equal(candidate, key)]
    return {
        "present": bool(matches),
        "matching_key_count": len(matches),
        "case_variant": any(candidate != key for candidate in matches),
    }


def _integer_field(obj: PairObject, key: str) -> Dict[str, Any]:
    found, value = _go_field(obj, key)
    if not found:
        return {"state": "missing"}
    if value is None:
        return {"state": "null"}
    if type(value) is not int or not INT64_MIN <= value <= INT64_MAX:
        return {"state": "invalid"}
    return {"state": "integer", "value": value}


def _identifier_field(obj: PairObject, key: str) -> str:
    found, value = _go_field(obj, key)
    if not found:
        return "missing"
    if value is None:
        return "null"
    if not isinstance(value, str):
        return "invalid"
    return "nonempty" if len(value) > 0 else "empty"


def summarize(http_status: Optional[int], body: bytes) -> Dict[str, Any]:
    """Shape-only evidence projection; never emit raw body or identifier values.

    The model inspects at most MAX_BODY_BYTES + 1 bytes before deciding whether
    the body is oversized. A real E2 implementation must enforce the equivalent
    bound while reading from the response stream, for example with an
    io.LimitReader-style primitive, rather than buffering an unbounded body.

    The projection records a raw-shape view and a targeted approximation of the
    baseline Go decoder view separately. It still cannot establish allocation.
    """
    valid_http = type(http_status) is int and 100 <= http_status <= 599
    result: Dict[str, Any] = {
        "http_status": http_status if valid_http else None,
        "json_state": "unparsed",
        "allocation_state": "UNKNOWN",
    }
    bounded = body[: MAX_BODY_BYTES + 1]
    if len(bounded) > MAX_BODY_BYTES:
        result["json_state"] = "oversized"
        return result
    try:
        obj = json.loads(
            bounded.decode("utf-8"),
            object_pairs_hook=_pairs_object,
            parse_constant=_reject_json_constant,
        )
    except (UnicodeError, ValueError, RecursionError):
        result["json_state"] = "invalid"
        return result
    if not isinstance(obj, PairObject):
        result["json_state"] = "not_object"
        return result

    result["json_state"] = "object"
    result["raw_shape"] = {
        "status": _raw_key_state(obj, "status"),
        "code": _raw_key_state(obj, "code"),
        "data": _raw_key_state(obj, "data"),
    }
    go_projection: Dict[str, Any] = {
        "status": _integer_field(obj, "status"),
        "code": _integer_field(obj, "code"),
    }

    data_found, data = _go_field(obj, "data")
    if not data_found:
        go_projection["data_state"] = "missing"
    elif data is None:
        go_projection["data_state"] = "null"
    elif not isinstance(data, PairObject):
        go_projection["data_state"] = "invalid"
    else:
        go_projection["data_state"] = "object"
        go_projection["identifiers"] = {
            key: _identifier_field(data, key)
            for key in ("task_id", "fid", "upload_id")
        }
    result["go_projection"] = go_projection
    return result


@dataclass(frozen=True)
class Observation:
    http_status: Optional[int]
    provider_status: Optional[int]
    code: Optional[int]
    task_id: Optional[str] = None
    fid: Optional[str] = None
    upload_id: Optional[str] = None
    obj_key: Optional[str] = None
    bucket: Optional[str] = None
    upload_url: Optional[str] = None
    auth_info: Optional[str] = None
    part_size: Optional[int] = None
    transport_error: bool = False
    cancelled: bool = False


def valid_pre_observation() -> Observation:
    return Observation(
        http_status=200,
        provider_status=200,
        code=0,
        task_id="synthetic-task",
        fid="synthetic-fid",
        upload_id="synthetic-upload",
        obj_key="synthetic-object-key",
        bucket="synthetic-bucket",
        upload_url="http://synthetic-upload.invalid",
        auth_info="synthetic-auth-info",
        part_size=1024,
    )


def _nonempty_string(value: Optional[str]) -> bool:
    return isinstance(value, str) and len(value) > 0


def _usable_upload_url(value: Optional[str]) -> bool:
    # The baseline later slices UploadUrl[7:] and prepends its own "https://".
    # This model therefore accepts only values structurally compatible with a
    # seven-byte "<4-char-scheme>://" prefix plus a non-empty host suffix.
    # It intentionally does NOT claim which real provider scheme is guaranteed;
    # E1/E2 must establish the observed scheme before a runtime validator exists.
    if not isinstance(value, str) or len(value) <= 7:
        return False
    prefix, suffix = value[:7], value[7:]
    return (
        len(prefix) == 7
        and prefix[4:] == "://"
        and suffix != ""
        and not suffix.startswith("/")
        and "/" not in suffix
        and not any(ch.isspace() for ch in suffix)
    )


def proposed_next_action(obs: Observation) -> str:
    """Conservative model only; valid PRE is NOT completed upload evidence."""
    if obs.cancelled:
        return "STOP_CANCELLED"
    integers = all(
        type(value) is int
        for value in (obs.http_status, obs.provider_status, obs.code, obs.part_size)
    )
    required_strings = all(
        _nonempty_string(value)
        for value in (
            obs.task_id,
            obs.fid,
            obs.upload_id,
            obs.obj_key,
            obs.bucket,
            obs.auth_info,
        )
    )
    envelope_ok = (
        obs.http_status,
        obs.provider_status,
        obs.code,
    ) == (200, 200, 0)
    structural_ok = (
        integers
        and required_strings
        and _usable_upload_url(obs.upload_url)
        and obs.part_size is not None
        and obs.part_size > 0
    )
    if not obs.transport_error and envelope_ok and structural_ok:
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
    """Illustrative counterexamples, not a provider implementation model."""

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
        self.assertEqual(
            proposed_next_action(provider.pre()), "STOP_ALLOCATION_UNKNOWN"
        )
        self.assertEqual((provider.calls, provider.allocations), (1, 1))

    def test_provider_error_must_not_be_treated_as_nonallocation(self):
        self.assertEqual(
            proposed_next_action(Observation(500, 500, 50000)),
            "STOP_ALLOCATION_UNKNOWN",
        )

    def test_error_with_identifiers_is_not_permission_to_resume(self):
        obs = replace(
            valid_pre_observation(),
            http_status=500,
            provider_status=500,
            code=50000,
        )
        self.assertEqual(proposed_next_action(obs), "STOP_ALLOCATION_UNKNOWN")

    def test_valid_pre_handle_is_not_upload_completion(self):
        self.assertEqual(
            proposed_next_action(valid_pre_observation()),
            "CONTINUE_EXISTING_TASK_NOT_UPLOAD_SUCCESS",
        )

    def test_uncertain_success_shapes_stop(self):
        base = valid_pre_observation()
        for changes in (
            {"http_status": 201},
            {"provider_status": None},
            {"code": None},
            {"provider_status": 0},
            {"code": False},
            {"code": "0"},
            {"http_status": 204},
            {"http_status": 500},
        ):
            with self.subTest(changes=changes):
                self.assertEqual(
                    proposed_next_action(replace(base, **changes)),
                    "STOP_ALLOCATION_UNKNOWN",
                )

    def test_each_required_pre_string_is_fail_closed(self):
        base = valid_pre_observation()
        for field in (
            "task_id",
            "fid",
            "upload_id",
            "obj_key",
            "bucket",
            "auth_info",
        ):
            for value in (None, ""):
                with self.subTest(field=field, value=value):
                    self.assertEqual(
                        proposed_next_action(replace(base, **{field: value})),
                        "STOP_ALLOCATION_UNKNOWN",
                    )

    def test_part_size_must_be_positive_integer(self):
        base = valid_pre_observation()
        for value in (None, 0, -1, False, "1024"):
            with self.subTest(value=value):
                self.assertEqual(
                    proposed_next_action(replace(base, part_size=value)),
                    "STOP_ALLOCATION_UNKNOWN",
                )

    def test_upload_url_must_match_baseline_slice_shape(self):
        base = valid_pre_observation()
        self.assertEqual(
            proposed_next_action(replace(base, upload_url="http://host")),
            "CONTINUE_EXISTING_TASK_NOT_UPLOAD_SUCCESS",
        )
        for value in (
            None,
            "",
            "http://",
            "https://host",
            "synthetic",
            "ftp://host",
            "http:///host",
            "http://host/path",
            "http://host name",
        ):
            with self.subTest(value=value):
                self.assertEqual(
                    proposed_next_action(replace(base, upload_url=value)),
                    "STOP_ALLOCATION_UNKNOWN",
                )

    def test_cancelled_observation_never_resumes(self):
        self.assertEqual(
            proposed_next_action(replace(valid_pre_observation(), cancelled=True)),
            "STOP_CANCELLED",
        )

    def test_transport_error_never_resumes_even_with_complete_fields(self):
        self.assertEqual(
            proposed_next_action(
                replace(valid_pre_observation(), transport_error=True)
            ),
            "STOP_ALLOCATION_UNKNOWN",
        )

    def test_completed_file_dedup_does_not_prove_pending_task_dedup(self):
        completed = {("existing.bin", "same-hash"): "completed-fid"}
        pending_tasks = []

        def possible_pre(name, digest):
            if (name, digest) in completed:
                return completed[(name, digest)]
            task = "pending-" + str(len(pending_tasks) + 1)
            pending_tasks.append(task)
            return task

        self.assertEqual(
            possible_pre("existing.bin", "same-hash"),
            possible_pre("existing.bin", "same-hash"),
        )
        self.assertNotEqual(
            possible_pre("new.bin", "same-hash"),
            possible_pre("new.bin", "same-hash"),
        )

    def test_name_size_hash_equality_does_not_identify_allocation(self):
        old = {"fid": "old", "name": "test.bin", "size": 1, "hash": "same"}
        new = dict(old, fid="new")
        self.assertEqual(
            {key: old[key] for key in ("name", "size", "hash")},
            {key: new[key] for key in ("name", "size", "hash")},
        )
        self.assertNotEqual(old["fid"], new["fid"])


class EvidenceProjectionTests(unittest.TestCase):
    def test_secrets_and_identifiers_are_not_emitted(self):
        raw = {
            "status": 500,
            "code": 50000,
            "message": "DO_NOT_EXPORT_SECRET",
            "Cookie": "DO_NOT_EXPORT_SECRET",
            "Authorization": "DO_NOT_EXPORT_SECRET",
            "data": {
                "task_id": "DO_NOT_EXPORT_SECRET",
                "fid": "DO_NOT_EXPORT_SECRET",
                "upload_id": "DO_NOT_EXPORT_SECRET",
                "auth_info": "DO_NOT_EXPORT_SECRET",
                "obj_key": "DO_NOT_EXPORT_SECRET",
                "callback": "DO_NOT_EXPORT_SECRET",
            },
        }
        out = summarize(500, json.dumps(raw).encode())
        self.assertNotIn("DO_NOT_EXPORT_SECRET", json.dumps(out))
        self.assertEqual(
            out["go_projection"]["identifiers"]["task_id"], "nonempty"
        )
        self.assertEqual(out["allocation_state"], "UNKNOWN")

    def test_absent_ids_do_not_mean_no_allocation(self):
        out = summarize(500, b'{"status":500,"code":50000}')
        self.assertEqual(out["allocation_state"], "UNKNOWN")

    def test_missing_null_empty_and_invalid_are_distinct(self):
        for data, state in (
            ({}, "missing"),
            ({"fid": None}, "null"),
            ({"fid": ""}, "empty"),
            ({"fid": 7}, "invalid"),
        ):
            with self.subTest(state=state):
                out = summarize(500, json.dumps({"data": data}).encode())
                self.assertEqual(
                    out["go_projection"]["identifiers"]["fid"], state
                )

    def test_whitespace_identifier_matches_go_nonempty_shape(self):
        out = summarize(500, b'{"data":{"fid":" "}}')
        self.assertEqual(
            out["go_projection"]["identifiers"]["fid"], "nonempty"
        )

    def test_provider_zero_is_not_missing(self):
        self.assertEqual(
            summarize(200, b'{"code":0}')["go_projection"]["code"],
            {"state": "integer", "value": 0},
        )
        self.assertEqual(
            summarize(200, b'{}')["go_projection"]["code"],
            {"state": "missing"},
        )
        self.assertEqual(
            summarize(200, b'{"code":null}')["go_projection"]["code"],
            {"state": "null"},
        )

    def test_invalid_integer_types(self):
        for value in (False, "0", 0.0, 2 ** 64, [], {}):
            with self.subTest(value=value):
                out = summarize(200, json.dumps({"code": value}).encode())
                self.assertEqual(
                    out["go_projection"]["code"], {"state": "invalid"}
                )

    def test_int64_range_is_modelled(self):
        self.assertEqual(
            summarize(200, json.dumps({"code": INT64_MAX}).encode())[
                "go_projection"
            ]["code"],
            {"state": "integer", "value": INT64_MAX},
        )
        self.assertEqual(
            summarize(200, json.dumps({"code": INT64_MAX + 1}).encode())[
                "go_projection"
            ]["code"],
            {"state": "invalid"},
        )

    def test_untrusted_body_is_bounded_before_json_parse(self):
        self.assertEqual(
            summarize(502, b"x" * (MAX_BODY_BYTES + 1))["json_state"],
            "oversized",
        )

    def test_html_malformed_unicode_and_nan_are_invalid(self):
        for body in (
            b"<html>DO_NOT_EXPORT_SECRET</html>",
            b"{",
            b"\xff",
            b'{"code":NaN}',
        ):
            with self.subTest(body=body):
                out = summarize(502, body)
                self.assertEqual(out["json_state"], "invalid")
                self.assertNotIn("DO_NOT_EXPORT_SECRET", json.dumps(out))

    def test_duplicate_exact_key_models_go_last_value(self):
        out = summarize(500, b'{"code":0,"code":31001}')
        self.assertEqual(
            out["go_projection"]["code"],
            {"state": "integer", "value": 31001},
        )
        self.assertEqual(out["raw_shape"]["code"]["matching_key_count"], 2)

    def test_case_insensitive_key_models_go_last_matching_value(self):
        out = summarize(500, b'{"status":200,"code":0,"CODE":31001}')
        self.assertEqual(
            out["go_projection"]["code"],
            {"state": "integer", "value": 31001},
        )
        self.assertTrue(out["raw_shape"]["code"]["case_variant"])

    def test_duplicate_identifier_models_go_last_value_without_exporting_it(self):
        out = summarize(500, b'{"data":{"fid":"a","FID":"b"}}')
        self.assertEqual(
            out["go_projection"]["identifiers"]["fid"], "nonempty"
        )
        self.assertNotIn('"a"', json.dumps(out))
        self.assertNotIn('"b"', json.dumps(out))

    def test_later_null_does_not_replace_prior_integer_in_go_projection(self):
        out = summarize(500, b'{"code":31001,"code":null}')
        self.assertEqual(
            out["go_projection"]["code"],
            {"state": "integer", "value": 31001},
        )

    def test_later_null_does_not_replace_prior_identifier_in_go_projection(self):
        out = summarize(500, b'{"data":{"fid":"a","fid":null}}')
        self.assertEqual(
            out["go_projection"]["identifiers"]["fid"], "nonempty"
        )
        self.assertNotIn('"a"', json.dumps(out))

    def test_unicode_full_casefold_lookalike_is_not_modeled_as_go_match(self):
        out = summarize(500, '{"data":{"fid":"a","ﬁd":"b"}}'.encode("utf-8"))
        self.assertEqual(
            out["go_projection"]["identifiers"]["fid"], "nonempty"
        )
        self.assertEqual(out["raw_shape"]["data"]["matching_key_count"], 1)
        self.assertNotIn('"a"', json.dumps(out))
        self.assertNotIn('"b"', json.dumps(out))

    def test_nonobject_json(self):
        for body in (b"null", b"[]", b"1", b'"DO_NOT_EXPORT_SECRET"'):
            with self.subTest(body=body):
                self.assertEqual(summarize(500, body)["json_state"], "not_object")

    def test_partial_error_ids_remain_unknown(self):
        out = summarize(
            500,
            b'{"status":500,"code":50000,"data":{"task_id":"t"}}',
        )
        self.assertEqual(
            out["go_projection"]["identifiers"],
            {
                "task_id": "nonempty",
                "fid": "missing",
                "upload_id": "missing",
            },
        )
        self.assertEqual(out["allocation_state"], "UNKNOWN")

    def test_http_status_not_coerced(self):
        for status in (None, True, "200", 0, 600):
            with self.subTest(status=status):
                self.assertIsNone(summarize(status, b"{}")["http_status"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
