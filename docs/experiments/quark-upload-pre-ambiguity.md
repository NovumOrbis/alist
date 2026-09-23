# Quark upload/pre ambiguity: audit and safe experiment plan

Status: **DRAFT RESEARCH / NOT A RECOVERY IMPLEMENTATION**.

This follow-up to #9643 documents an observed reliability gap and the evidence
needed before changing `/file/upload/pre` replay policy. It changes no production
Go code, shared client configuration, credentials, workflows, or deployed NAS.
It is separate from the DELETE work in #9651.

## 1. Frozen scope and evidence boundary

Source audit baseline: `AlistGo/alist@fb0731a6953012e7b72b89bf5473817caa4625f9`.
The baseline requires Go 1.25.0 and pins Resty v2.14.0 [S1].

The incident source is operator-supplied NAS log extracts, not a new provider
experiment. Six distinct request IDs identify six pre-stage failures, each
logged at both the operation and WebDAV layers. Do not count those as twelve:

| NAS local timestamp | Object category |
| --- | --- |
| 2026-09-07 17:11:10 | index |
| 2026-09-08 14:40:01 | bucket |
| 2026-09-08 14:44:38 | configuration index |
| 2026-09-10 00:37:52 | index |
| 2026-09-10 22:40:06 | bucket |
| 2026-09-23 10:44:42 | index |

All contain `quark upload stage pre: inner error, requestId ...`. The last
failure is associated with a WebDAV PUT returning 405. The supplied search of
the same object path found no later successful PUT in that searched log at
collection time. This is not proof that the provider did not allocate a task,
that no other log contains recovery, or that data is absent from the provider.
There is no denominator from which to estimate a failure rate.

A nearby part-stage error was retried and followed by PUT 201. The later lock
DELETE was logged as 204. Neither proves that #9651's exact binary was deployed.
The current NAS executable hash/build identity has not been freshly verified.
The #9643 description distinguishes earlier deployed combined candidate
`26bc4f937a213f44437a5db9a8318e9d317fae53` from its final upstream candidate [S2].

Raw operator logs are intentionally not committed: they include private paths,
host identifiers and provider request identifiers. The table is a minimized
summary of supplied evidence, not independently reproduced provider behavior.

## 2. Read-only code audit

### A1. Pre-stage recovery is intentionally excluded

`upPreReliable` calls `upPre` once and wraps errors with the `pre` stage. The
comment explicitly excludes additional driver-layer retries because a replay
can allocate a second task/FID [S3]. `Put` returns on that error before hash,
part, commit and finish. Therefore a narrow transient classifier existing for
other stages does not imply that pre is covered [S12].

**Finding:** the six logged aborts are consistent with an intentional safety
boundary. This does not establish that the provider rejection occurred before
allocation. An `inner error` message is not a non-allocation certificate.

### A2. One driver call is not necessarily one wire request

`upPre` uses the shared request helper. Its production client has
`RetryCount(3)` [S4] [S5]. Resty v2.14.0's backoff loop can execute an eligible
transport-error operation once plus three retries [S6]. Provider-envelope
classification happens after `Execute`; the six message-only log events do
not prove that these internal transport retries occurred in those incidents.

**Finding:** a pre request whose response is lost can be replayed below the
driver. A future no-replay implementation must test production retry settings,
not only a test client with retries disabled. It must not temporarily mutate
the shared client's retry count or call a concurrency-unsafe client clone.

Disabling Resty retries alone is not an exactly-once guarantee. The test plan
must also account for HTTP 307/308 redirects, custom RoundTrippers, middleware,
and idempotency-header-driven transport behavior [S7]. No conclusion here is
made about provider-side duplication in the absence of a wire-level trace.

### A3. The error path discards allocation evidence

`requestWithCookie` registers `SetResult(&UpPreResp)` through its caller, but
registers `SetError(&Resp)` separately [S4]. Resty v2.14.0 decodes 2xx into
Result and >=400 into Error [S8]. `Resp` only has status/code/message, whereas
`UpPreResp.Data` has task_id/fid/upload_id and other upload fields [S9]. On a
recognized provider error the helper returns `nil, errors.New(e.Message)`.

**Finding:** even if a non-2xx raw response contained task identifiers, that
information would not reach `upPreReliable` through `UpPreResp.Data` on this
path. Empty fields in the returned Go struct or current log are not evidence
that the raw response lacked identifiers or that allocation did not happen.

The helper also lacks independent HTTP-error rejection and explicit validation
of the 2xx provider envelope. HTTP 500 with `{}` or non-JSON can leave `e` zero;
HTTP 200 with a provider error populates Result instead of Error. `upPreReliable`
does not validate those fields before returning. This is a code-path finding,
not a claim that one of the six logged incidents used these wire shapes, nor
proof that the entire upload would subsequently return success.

### A4. Pre-call cancellation is not in-flight cancellation

`upPreReliable` checks `ctx.Err()` before calling `upPre`, but `upPre` takes no
context and sets only the body on the Resty request [S3] [S4]. Cancellation after
the check is therefore not attached to that HTTP request. The current comment
about cancellation must not be interpreted as an in-flight guarantee.

**Finding:** cancellation and evidence capture must be designed together. A
cancelled request is never proof that allocation did not occur. Do not retry
using a detached background context to work around cancellation.

### A5. Same-name matching can violate overwrite safety

Quark sets `NoOverwriteUpload=true` [S10]. `op.Put` can rename a nonempty old
object to `.alist_to_delete`, attempt the upload, restore it on failure, or
remove it on reported success [S11]. The rollback itself can fail; a zero-byte
existing object follows a separate delete-before-upload path.

**Design constraint:** never report upload success merely because a same-name,
same-size, or same-hash object is visible after ambiguous pre. Such an object
may predate this attempt. A false success can trigger removal of the old
fallback. A PRE task/FID, even if recovered exactly, is not terminal upload
completion. Identity and completion are distinct predicates.

### A6. same_path_reuse is a clue, not an approved mechanism

`upPre` contains a commented-out `same_path_reuse` field and creates fresh local
timestamps on each call [S4]. No idempotency contract for this field, request ID,
or a client-generated key was found in the audited source and bounded public
search. A more useful primary historical source is PR #1604 (2022): its initial
same-path overwrite claim was challenged after merge; the author reported
hash-dependent behavior, and the discussion records the attempted fix being
overwritten after it failed validation [S13]. These are old contributor and
maintainer observations, not a current provider-issued guarantee. In particular,
reuse/deduplication of a completed file does not prove deduplication of an
unfinished PRE task whose response was lost.

**Decision:** do not enable this field, invent an idempotency header, recover by
path, or assume the generic `/task` endpoint supports upload-session recovery.
Each requires independent evidence. Absence of documentation in this search is
not proof that a provider-supported recovery mechanism cannot exist.

## 3. Recovery decision boundary

These rules are proposed acceptance criteria, not installed driver behavior:

| Observation | Permitted conclusion / action |
| --- | --- |
| Explicit valid PRE response with usable task identity | Continue the established upload protocol; not upload success |
| Error envelope or transport failure without identity | Allocation UNKNOWN; no automatic PRE replay |
| Error envelope containing task/FID | Candidate evidence only; no automatic resume/finish/delete |
| Bounded parent listing has no matching file | Allocation UNKNOWN; hidden tasks and stale listings remain possible |
| Same-name/size/hash match | Not proof of this attempt's identity or completion |
| Cancelled caller | Stop; no background retry or success inference |
| Documented non-allocation rejection or verified dedup protocol | Separately review a narrowly bounded recovery implementation |

A single attempted PRE can still leave one unknown allocation if its response
is lost. No-replay limits additional allocations; it does **not** guarantee
zero orphan tasks. Finite experiments cannot prove an undocumented global
exactly-once property.

## 4. Experiments and authorization boundaries

### E0. Offline counterexample lab - executable now

Run only the adjacent standard-library model:

```sh
python3 docs/experiments/quark_upload_pre_lab.py
```

It demonstrates two histories with identical client-visible failures but
allocation counts zero and one; empty listing ambiguity; extra allocation from
blind replay; and conservative evidence projection. It also tests missing/null
fields, malformed responses, ambiguous IDs, cancellation, and secret exclusion.

The model has no networking or credential discovery. All identifiers are
synthetic. It is **not** an AList/Resty integration test, a provider probe, or
proof that any proposed production implementation is correct.

### E1. Actual-driver fault injection - required before runtime changes

Use a disposable checkout of the pinned baseline with `httptest` or a recording
RoundTripper. No production storage configuration, cookies, NAS mounts, or
public-provider egress. Add a transport guard rejecting non-loopback targets.
Tests must restore globals and avoid parallel global-client mutation.

Required matrix:

1. Explicit normal PRE success: preserve request fields and continue once.
2. Explicit error before allocation and error after allocation: same envelope,
   different private server state. Neither permits an additional PRE.
3. Allocation followed by connection EOF/reset or a lost response with a real
   `RetryCount=3` source client: count wire PRE calls, not wrapper calls.
4. HTTP 500 with empty, incomplete, HTML, malformed JSON and identifier-bearing
   bodies; HTTP 200 with provider rejection; HTTP 204/201; missing/null fields.
5. Cancel before request and while the server is blocked: prompt return, no
   hidden replay; assert that an earlier remote allocation remains unknown.
6. 307/308 redirect preserving the POST, same-origin and cross-origin: prevent
   unobserved PRE replay without changing global redirect policy.
7. Custom transport/middleware, response-plus-error, cookie refresh, UA, timeout,
   proxy/TLS settings: no global mutation and no credential logging.
8. Failed overwrite with a pre-existing synthetic file: old FID and bytes remain
   recoverable; never clean `.alist_to_delete` on unproven success.
9. Keep hash/part/commit/finish behavior and unrelated Quark/UC callers unchanged.

Run Go 1.25+ package vet/test/race/shuffle and internal/op + server/webdav tests
on the exact proposed runtime candidate, plus regression proof against baseline.
This document does not claim those tests have been executed for new runtime code.

### E2. Passive response evidence - requires a separate deployment decision

First fingerprint the *running* NAS executable, configuration source, UTC
clock and log timezone. Do not infer the binary from the DSM package label or
GitHub PR state. No restart, binary replacement or debugger attachment is
included in this Draft.

A separately reviewed diagnostic change may observe an already-authorized
normal request **without adding a PRE**, before the helper discards its response.
Do not enable global HTTP debug logging or dump an entire response into a PR.
Do not induce repeated provider failures to obtain samples.

Public/exportable evidence uses an allowlist only:

- a random local event ID unrelated to the account; exact tested build/tree;
- HTTP status, content-type category, bounded body length, parse outcome;
- presence/type/value of integer provider status/code, preserving missing/null;
- presence/type/nonempty flags for task_id/fid/upload_id; no actual values;
- request count, stage, cancellation/transport-error category, elapsed time;
- `ALLOCATION_STATE=UNKNOWN` unless independently established.

Do not export cookies, Authorization, auth_info, signed URLs, OSS keys, callback
bodies, filenames, parent FIDs, full provider messages, or raw JSON. Capture
only at the PRE boundary, with a strict size cap, not a shared/global hook.
If exact IDs are necessary for local follow-up, keep a separate owner-only,
0600, time-limited private record; share only random/HMAC event labels. The key
and raw record stay local. An instrumentation failure must not change the
original request result or start another request.

### E3. Isolated real-provider experiment - NOT authorized by this Draft

Prerequisites: separate disposable account/storage, explicitly approved parent
FID outside every backup repository, small synthetic payloads (<=64 KiB), no
concurrent writers, a reviewed client with PRE retries and POST redirects
disabled, and a separate explicit authorization for creation and cleanup.

Default bound: at most **three PRE invocations total**, one per declared trial,
including retries and redirects in the count; stop immediately on any unknown
allocation or scope mismatch. Do not loop until an `inner error` appears.

Start with one successful synthetic PRE to record the actual envelope and task
lifecycle. A response-loss experiment may suppress delivery to the test caller
only after a local recorder retained the exact response; do not blind-drop a
response whose identity cannot be recovered for authorized cleanup. This tests
client behavior and a known allocation, not the provider's natural inner-error
semantics. No auto-resume is permitted merely because the recorder has an ID.

Natural provider errors require evidence on that actual error response. A
GET-only lookup is allowed only for a previously observed exact ID and an
endpoint whose read semantics have been validated for upload tasks. Listing
absence is inconclusive. Do not guess task IDs or infer that generic task
statuses mean terminal upload durability.

`same_path_reuse` duplicate-allocation/overwrite testing is a separate,
explicitly authorized subexperiment; it is not included in the three-call
plan. Validate same key/same request, conflicting content, concurrent callers,
timeout/restart and retention-window cases before considering production use.
Even positive trial results need a defensible provider contract.

Cleanup: only known synthetic objects/tasks with independently verified
ownership, separate confirmation, and documented provider semantics. Never
delete by filename, never delete a backup lock or `.hbk` object, and never
assume unknown tasks were cleaned. Record any residual allocation as unresolved.

## 5. Path from this Draft to an implementation

1. Agree on evidence schema and protocol questions; establish deployed identity.
2. Add narrowly scoped PRE response validation, in-flight context and no-replay
   transport handling only after actual-driver fault-injection tests exist.
   Do not broaden shared request semantics or mutate shared Resty configuration.
3. Resolve non-allocation/idempotency/resume semantics in the isolated evidence
   phase. Returned IDs alone are not permission to continue a failed task.
4. Only then propose bounded recovery. Require exact attempt identity, upload
   completion proof, overwrite preservation, cleanup accounting, and no hidden
   extra PRE calls. Tests must fail on the old unsafe behavior, not mirror code.
5. Independent review + full exact-candidate Go gate + explicit deployment
   authorization precede production use. This Draft must not be marked ready
   as an incident fix merely because its offline model tests pass.

## 6. Validation performed for this research Draft

- Read-only source audit at the pinned baseline and Resty v2.14.0.
- Offline Python lab: 23 top-level tests passed, including parameterized cases.
- Documentation and lab only; production Go tree unchanged.
- No real-provider experiment, NAS command, task allocation, upload or deletion.
- No claim of a new AList Go test gate or upstream CI result. The working
  environment could not resolve GitHub for a full checkout and has Go 1.23.2,
  older than the baseline requirement. Source was read using the GitHub connector.

## Sources

[S1]: https://github.com/AlistGo/alist/blob/fb0731a6953012e7b72b89bf5473817caa4625f9/go.mod
[S2]: https://github.com/AlistGo/alist/pull/9643
[S3]: https://github.com/AlistGo/alist/blob/fb0731a6953012e7b72b89bf5473817caa4625f9/drivers/quark_uc/upload_reliability.go
[S4]: https://github.com/AlistGo/alist/blob/fb0731a6953012e7b72b89bf5473817caa4625f9/drivers/quark_uc/util.go
[S5]: https://github.com/AlistGo/alist/blob/fb0731a6953012e7b72b89bf5473817caa4625f9/drivers/base/client.go
[S6]: https://github.com/go-resty/resty/blob/v2.14.0/retry.go
[S7]: https://pkg.go.dev/net/http#Client
[S8]: https://github.com/go-resty/resty/blob/v2.14.0/middleware.go
[S9]: https://github.com/AlistGo/alist/blob/fb0731a6953012e7b72b89bf5473817caa4625f9/drivers/quark_uc/types.go
[S10]: https://github.com/AlistGo/alist/blob/fb0731a6953012e7b72b89bf5473817caa4625f9/drivers/quark_uc/meta.go
[S11]: https://github.com/AlistGo/alist/blob/fb0731a6953012e7b72b89bf5473817caa4625f9/internal/op/fs.go

[S12]: https://github.com/AlistGo/alist/blob/fb0731a6953012e7b72b89bf5473817caa4625f9/drivers/quark_uc/driver.go
[S13]: https://github.com/AlistGo/alist/pull/1604#issuecomment-1238037447
