# Quark upload/pre ambiguity: audit and safe experiment plan

Status: **DRAFT RESEARCH / NOT A RECOVERY IMPLEMENTATION**.

This follow-up to AlistGo/alist#9643 documents an observed reliability gap and
the evidence needed before changing `/file/upload/pre` replay policy. It changes
no production Go code, shared client configuration, credentials, workflows, or
deployed NAS. It is separate from the DELETE work in AlistGo/alist#9651.

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

If the running binary used the audited baseline helper, surfacing the exact
message-only `inner error, requestId ...` implies the final Resty response was
parsed as an error envelope strongly enough to populate `Resp.Message` and
trigger the helper's status/code check. Under that binary assumption, the six
logged events are not examples of the separate HTTP-200 provider-error, HTML, or
empty-JSON false-success shapes discussed below. The currently running NAS
binary has not been freshly fingerprinted, so this remains a bounded inference,
not an incident fact.

A nearby part-stage error was retried and followed by PUT 201. The later lock
DELETE was logged as 204. Neither proves that AlistGo/alist#9651's exact binary
was deployed. The AlistGo/alist#9643 description distinguishes the earlier
deployed combined candidate `26bc4f937a213f44437a5db9a8318e9d317fae53`
from its final upstream candidate [S2].

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
classification happens after `Execute`; the six message-only log events do not
prove that these internal transport retries occurred in those incidents.

**Finding:** a pre request whose response is lost can be replayed below the
driver. A future no-replay implementation must test production retry settings,
not only a test client with retries disabled. It must not temporarily mutate
the shared client's retry count or use a concurrency-unsafe shared-client
mutation as a per-request control.

Disabling Resty retries alone is not an exactly-once guarantee. The test plan
must also account for net/http redirect behavior, including POST replay on
307/308, 301/302/303 method conversion, custom RoundTrippers, and middleware
[S7] [S14]. No conclusion here is made about provider-side duplication in the
absence of a wire-level trace.

### A3. Error decoding can discard allocation evidence or create false PRE success

`requestWithCookie` registers `SetResult(&UpPreResp)` through its caller, but
registers `SetError(&Resp)` separately [S4]. For JSON/XML content types, Resty
v2.14.0 decodes 2xx responses into Result and responses >=400 into Error [S8].
Non-JSON/XML bodies are not decoded by that path, and an unmarshal failure while
decoding an error body is logged by Resty rather than returned as the request
error [S8]. `Resp` only has status/code/message, whereas `UpPreResp.Data` has
task_id/fid/upload_id and other upload fields [S9]. On a recognized provider
error the helper reduces the result to `errors.New(e.Message)`.

**Finding 1 - evidence loss:** even if a non-2xx raw response contained task
identifiers, those fields would not reach `upPreReliable` through
`UpPreResp.Data` on this path. Empty fields in the returned Go struct or current
logs are not evidence that the raw response lacked identifiers or that
allocation did not happen.

**Finding 2 - false PRE success:** the helper has no independent HTTP-status
rejection and does not validate the PRE success envelope. HTTP >=400 with `{}`,
HTML, malformed JSON, or an error body whose status/code do not populate `Resp`
can leave the helper's `e` zero and return `nil` error with a zero
`UpPreResp`. HTTP 200 with a provider-error envelope is decoded into Result, not
the separate Error object, so the helper's `e` check can also remain zero. 201,
204, and redirect edge shapes require explicit treatment rather than being
assumed to be valid PRE success.

That zero-valued PRE is not harmless. `Put` immediately calls hash with the
empty task ID. If the downstream hash response is also misclassified as a
nil-error non-finish result, `Put` uses `pre.Metadata.PartSize == 0` and reaches
`total / partSize`, causing an integer divide-by-zero panic [S12]. A later path
also slices `UploadUrl[7:]`, so an empty/short URL is another structural hazard
[S4]. An independent loopback review reproduced the divide-by-zero path with
synthetic responses; whether real Quark emits those shapes is not established.

The minimum structural PRE gate used by this research plan is therefore
stricter than merely having task_id/fid. Before the existing upload pipeline is
allowed to continue, a proposed validator must require an explicit HTTP 200,
provider status 200/code 0, non-empty task_id, fid, upload_id, obj_key, bucket,
non-empty auth_info, `part_size > 0`, and an upload_url structurally compatible
with the baseline's literal `UploadUrl[7:]` slice. The baseline then prepends
its own `https://<bucket>.` prefix [S4], so the research model requires a
seven-byte `<4-char-scheme>://` prefix followed by a non-empty host suffix and
rejects values that would leave a leading slash or path after the slice.

This is a defensive baseline-derived shape check, **not** a provider protocol
specification and not evidence that Quark guarantees a particular scheme. The
real provider's observed upload_url scheme remains an E1/E2 evidence question.
An over-strict validator is itself unsafe: if it rejects a legitimate PRE only
after the provider allocated a task, a higher-level upload retry can allocate
another orphan task. The validator therefore must not invent a stronger scheme
contract than the baseline/evidence supports. A structurally valid PRE still
does **not** prove upload completion.

### A4. Pre-call cancellation is not in-flight cancellation

`upPreReliable` checks `ctx.Err()` before calling `upPre`, but `upPre` takes no
context and sets only the body on the Resty request [S3] [S4]. Cancellation after
the check is therefore not attached to that HTTP request. The current comment
about cancellation must not be interpreted as an in-flight guarantee.

**Finding:** cancellation and evidence capture must be designed together. A
cancelled request is never proof that allocation did not occur. Do not retry
using a detached background context to work around cancellation.

### A5. Overwrite state makes false PRE success a data-safety concern

Quark sets `NoOverwriteUpload=true` [S10]. For a non-empty existing destination,
`op.Put` can rename the old object to `.alist_to_delete`, attempt the upload,
restore it on a returned upload error, or remove it after reported success
[S11]. That restoration/removal logic is inline after the driver call, not a
deferred rollback. Therefore a panic inside the driver can bypass the restore
step and leave the old object under the temporary name. The false-PRE-success
panic chain in A3 is a concrete synthetic example of this baseline risk.

Other overwrite states need separate tests rather than being collapsed into the
normal non-empty case:

- a zero-byte existing destination is deleted before upload and has no rename
  fallback to restore;
- a stale `<name>.alist_to_delete` from an earlier failed restore can make the
  next rename collide;
- stale cache state can change whether `op.Put` believes an old object exists;
- `Remove(tempPath)` can fail after a real upload success;
- rollback rename itself can fail and must remain visible as an unresolved
  safety event.

**Design constraint:** never report upload success merely because a same-name,
same-size, or same-hash object is visible after ambiguous PRE. Such an object
may predate this attempt. A PRE task/FID, even if recovered exactly, is not
terminal upload completion. Identity and completion are distinct predicates.

The baseline already has a narrower later-stage exception: after commit,
`upFinishReliable` can treat visibility of the exact pre-allocated FID as
success only after its bounded finish retry budget is exhausted [S3]. That
existing finish-stage heuristic does not generalize to ambiguous PRE, where the
client may not possess a trustworthy attempt identity at all.

### A6. same_path_reuse is a clue, not an approved mechanism

`upPre` contains a commented-out `same_path_reuse` field and creates fresh local
timestamps on each call [S4]. No current provider-issued idempotency contract
for this field, request ID, or a client-generated key was found in the audited
source and bounded public search. A more useful primary historical source is
AlistGo/alist#1604 (2022): its initial same-path overwrite claim was challenged
after merge; the author reported hash-dependent behavior, and the discussion
records the attempted fix being overwritten after it failed validation [S13].
These are old contributor and maintainer observations, not a current provider
guarantee. In particular, reuse/deduplication of a completed file does not prove
deduplication of an unfinished PRE task whose response was lost.

**Decision:** do not enable this field, invent an idempotency header, recover by
path, or assume the generic `/task` endpoint supports upload-session recovery.
Each requires independent evidence. Absence of documentation in this search is
not proof that a provider-supported recovery mechanism cannot exist.

## 3. Recovery decision boundary

These rules are proposed acceptance criteria, not installed driver behavior:

| Observation | Permitted conclusion / action |
| --- | --- |
| Explicit HTTP 200 + provider 200/0 PRE with all minimum structural fields valid | Continue the established upload protocol; **not** upload success |
| PRE missing any required structural field or `part_size <= 0` | Stop/fail closed; do not call hash/part and do not infer non-allocation |
| Error envelope or transport failure without identity | Allocation UNKNOWN; no automatic PRE replay |
| Error envelope containing task/FID | Candidate evidence only; no automatic resume/finish/delete |
| Bounded parent listing has no matching file | Allocation UNKNOWN; hidden tasks and stale listings remain possible |
| Same-name/size/hash match | Not proof of this attempt's identity or completion |
| Cancelled caller | Stop; no background retry or success inference |
| Documented non-allocation rejection or verified dedup protocol | Separately review a narrowly bounded recovery implementation |

Minimum structural fields for the first row are: non-empty task_id, fid,
upload_id, obj_key, bucket, auth_info; a positive integer part_size; and an
upload_url whose shape is compatible with the baseline's seven-byte
`UploadUrl[7:]` slice. The model does not declare the real provider scheme to
be a protocol guarantee. Callback semantics and other provider fields may need
further validation before a production validator is finalized.

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

It contains illustrative counterexamples with two histories that have identical
client-visible failures but allocation counts zero and one; empty-listing
ambiguity; extra allocation from blind replay; and a conservative PRE structure
gate. The evidence projection keeps a raw-shape summary and a targeted
Go-decoder projection separate, including duplicate keys and ASCII case
variants, missing/null fields, malformed UTF-8/JSON, bounded body inspection,
ambiguous IDs, cancellation, and secret exclusion. The projection deliberately
does not claim exact Unicode simple-fold equivalence with Go's encoding/json;
non-ASCII lookalike keys are kept outside the modeled match set.

The model has no networking or credential discovery. All identifiers are
synthetic. It is **not** an AList/Resty integration test, a provider probe, or
proof that any proposed production implementation is correct. Its ambiguity
fixtures are intentionally counterexamples, not a simulation of Quark's hidden
state machine.

### E1. Actual-driver fault injection - required before runtime changes

Use a disposable checkout of the pinned baseline with `httptest` or a recording
RoundTripper. No production storage configuration, cookies, NAS mounts, or
public-provider egress. Add a transport guard rejecting non-loopback targets.
Tests must restore globals and avoid parallel global-client mutation.

Required matrix:

1. Explicit normal PRE success: validate every minimum structural field, preserve
   request fields, and continue once. Invalid PRE must make **zero** hash/part
   calls, return an error without panic, and exercise overwrite rollback when
   `op.Put` already renamed an old object.
2. Explicit error before allocation and error after allocation: same envelope,
   different private server state. Neither permits an additional PRE.
3. Allocation followed by connection EOF/reset, client timeout, response loss,
   or 200 headers plus a partial body with a real `RetryCount=3` source client:
   count wire PRE calls, not wrapper calls.
4. HTTP 500 with empty, incomplete, HTML, malformed JSON and identifier-bearing
   bodies; HTTP 200 with provider rejection; HTTP 201/204; missing/null fields;
   duplicate and case-variant JSON keys; zero/negative part_size; empty/short
   upload_url; baseline-compatible `http://host`; and an `https://host` shape
   that the current `[7:]` slice would mis-handle. Assert fail-closed behavior
   before hash/part without rejecting the baseline-compatible shape merely
   because of an unproven provider-scheme assumption. Record the actually
   observed provider scheme as evidence rather than protocol law.
5. Cancel before request, while the server is blocked, and during retry backoff:
   prompt return, no hidden replay after cancellation; an earlier remote
   allocation remains UNKNOWN.
6. 307/308 redirect preserving POST/body and 301/302/303 method conversion,
   tested for same-origin and cross-origin targets. Prevent unobserved PRE replay
   without changing global redirect policy; count redirect hops separately from
   Resty Request.Attempt.
7. Custom transport/middleware, response-plus-error, cookie refresh, UA, timeout,
   proxy/TLS settings: no global mutation and no credential logging.
8. Overwrite cases: non-empty destination, zero-byte destination, stale
   `.alist_to_delete`, stale cache, rollback-rename failure, and temp-object
   remove failure after real success. On unproven PRE success never remove the
   old fallback; on panic-capable baseline shapes the regression must prove the
   proposed validator returns before the panic path.
9. Run the relevant PRE matrix under both Quark and UC conf values. Keep
   hash/part/commit/finish behavior and unrelated callers unchanged.

Run Go 1.25+ package vet/test/race/shuffle and internal/op + server/webdav tests
on the exact proposed runtime candidate, plus regression proof against baseline.
This document does not claim those tests have been executed for new runtime code.

### E2. Passive response evidence - requires a separate deployment decision

First fingerprint the *running* NAS executable, configuration source, UTC clock
and log timezone. Do not infer the binary from the DSM package label or GitHub
PR state. No restart, binary replacement or debugger attachment is included in
this Draft.

Before adding instrumentation, use the existing service capture to look for
Resty's retry logger lines such as `Attempt N` around known PRE incidents. Resty
can emit per-attempt errors through its own logger/stderr path, which may be
separate from AList's logrus file. Absence of such lines is not evidence of one
wire request if stderr was not retained. If diagnostic instrumentation later
records `Request.Attempt`, treat it as a Resty execution count only; redirect
hops must be counted separately.

A separately reviewed diagnostic change may observe an already-authorized
normal request **without adding a PRE**, before the helper discards its response.
Do not enable global HTTP debug logging or dump an entire response into a PR.
Do not induce repeated provider failures to obtain samples.

Public/exportable evidence uses an allowlist only:

- a random local event ID unrelated to the account; exact tested build/tree;
- HTTP status, content-type category, bounded body length, parse outcome;
- a raw-shape summary (presence, matching-key count, case-variant indicator) for
  known envelope keys, without arbitrary key/value export;
- a targeted Go-decoder projection for provider status/code and the
  presence/type/nonempty state of task_id/fid/upload_id, preserving
  missing/null/invalid distinctions and modeled duplicate/ASCII-case behavior;
- the upload_url scheme category and whether the baseline `[7:]` host suffix
  would be structurally usable, without exporting the host itself;
- request count, redirect-hop count, stage, cancellation/transport-error
  category, elapsed time;
- `ALLOCATION_STATE=UNKNOWN` unless independently established.

Do not export cookies, Authorization, auth_info, signed URLs, OSS keys, callback
bodies, filenames, parent FIDs, full provider messages, or raw JSON. Capture
only at the PRE boundary with a strict **read-time** size cap, not by reading an
unbounded body and truncating afterward, and not through a shared/global hook.
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
explicitly authorized subexperiment; it is not included in the three-call plan.
If authorized later, it **inherits E3's hard invocation budget, stop-on-unknown
rule, exact-ID ownership requirement, and cleanup accounting** unless a stricter
subexperiment budget is approved. Validate same key/same request, conflicting
content, concurrent callers, timeout/restart and retention-window cases before
considering production use. Even positive trial results need a defensible
provider contract.

Cleanup: only known synthetic objects/tasks with independently verified
ownership, separate confirmation, and documented provider semantics. Never
delete by filename, never delete a backup lock or `.hbk` object, and never
assume unknown tasks were cleaned. Record any residual allocation as unresolved.

## 5. Path from this Draft to an implementation

1. Agree on evidence schema and protocol questions; establish deployed identity.
2. Add actual-driver tests for strict PRE envelope/structure validation,
   in-flight context and no-replay transport handling before changing runtime
   behavior. Do not broaden shared request semantics or mutate shared Resty
   configuration.
3. The first runtime hardening candidate, if supported by E1, is limited to
   observation/no-replay/fail-closed PRE mechanics. It must reject invalid PRE
   before hash/part, avoid panic, preserve overwrite rollback, and avoid an
   over-strict upload_url scheme rule that would reject a baseline-compatible
   allocated PRE. The provider scheme must be resolved from E1/E2 evidence, not
   invented by the validator. This is not automatic PRE recovery.
4. Resolve non-allocation/idempotency/resume semantics in the isolated evidence
   phase. Returned IDs alone are not permission to continue a failed task.
5. Only then propose bounded recovery. Require exact attempt identity, upload
   completion proof, overwrite preservation, cleanup accounting, and no hidden
   extra PRE calls. Tests must fail on the old unsafe behavior, not mirror code.
6. Independent review + full exact-candidate Go gate + explicit deployment
   authorization precede production use. This Draft must not be marked ready as
   an incident fix merely because its offline model tests pass.

## 6. Validation performed for this research Draft

- Read-only source audit at the pinned baseline and Resty v2.14.0.
- Initial offline Python lab: 23 top-level tests passed.
- Second corrective offline Python lab after targeted re-audit: **34 top-level
  tests passed**, including baseline-compatible upload_url slicing, later-null
  preservation for modeled Go scalar fields, a non-ASCII full-casefold
  counterexample, stricter PRE structural fields, int64 boundaries, malformed
  constants, and bounded evidence parsing.
- `python3 -m py_compile docs/experiments/quark_upload_pre_lab.py`: passed for
  the corrective lab.
- Documentation and lab only; production Go tree unchanged.
- No real-provider experiment, NAS command, task allocation, upload or deletion
  was performed for this Draft or corrective.
- An independent read-only review used loopback-only synthetic fault injection
  to reproduce four-wire Resty retry cases and the zero-PRE divide-by-zero
  baseline path. Those scratch tests are review evidence, not committed AList
  regression tests and not a full Go gate.
- No claim of a new AList full Go test gate or upstream CI result is made for
  this research Draft.

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
[S14]: https://pkg.go.dev/net/http#Transport
