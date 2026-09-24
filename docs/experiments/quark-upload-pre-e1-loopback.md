# Quark upload/pre E1 loopback characterization

Status: **DRAFT E1 / LOOPBACK-ONLY / NO PROVIDER MUTATION**.

Baseline:

`AlistGo/alist@fb0731a6953012e7b72b89bf5473817caa4625f9`

Research prerequisite:

`NovumOrbis/alist#8@70c495d75c176edb813faabadeba849615cfe437`

This branch starts the E1 phase from the accepted research baseline. It adds
actual-driver loopback characterization tests only. It does not yet change
production Go behavior and does not implement automatic PRE recovery.

## Safety boundary

The tests may use:

- `httptest`
- synthetic Resty/`net/http` transports
- loopback-only HTTP servers
- synthetic file metadata and provider envelopes

The tests must not:

- call real Quark or UC endpoints
- use cookies, tokens or production credentials
- access the NAS
- create provider upload tasks
- modify `.hbk` backup data
- add automatic `/file/upload/pre` retry
- enable `same_path_reuse`

## Initial characterization matrix

The first E1 commit exercises these exact baseline paths:

1. HTTP 500 + JSON `{}` can be returned as nil error with zero-valued PRE.
2. HTTP 500 + HTML can be returned as nil error with zero-valued PRE.
3. HTTP 200 + provider rejection can be decoded into `UpPreResp` while the
   separate error envelope remains zero, so the request helper can return nil.
4. A zero PRE followed by an equally misclassified hash response can reach the
   exact `runtime.Error` integer divide-by-zero panic in `Put`. The test owns
   a temporary directory explicitly and requires the loopback server to observe
   PRE followed by hash before accepting that panic.
5. The production `base.NewRestyClient()` retry settings, with only its
   transport replaced by a synthetic no-network RoundTripper, can issue four
   wire PRE attempts for a transport error even though `upPreReliable` has no
   explicit retry loop.
6. Caller cancellation after PRE starts does not propagate into the current
   PRE request context. The synthetic transport inspects `req.Context()` after
   cancellation and is context-aware, so this characterization is expected to
   fail once PRE correctly attaches the caller context.
7. HTTP 307 preserves and replays the PRE POST and body.
8. HTTP 302 converts the POST redirect follow-up into GET.
9. Explicit provider errors are staged as PRE failures under Quark and UC PRE
   configuration values. The loopback cases use the provider-specific `pr`
   and Referer values while replacing only the API origin with the test server.
10. Cancellation before PRE starts emits no request.

These are **baseline characterization assertions**. They intentionally describe
unsafe or ambiguous behavior that the next hardening candidate must reverse.
They are not intended as permanent regression expectations for a fixed runtime.

## Corrective review status

An independent read-only review of the preceding head
`d1d80e4061075785737b29242bdde50b8c4599a4` found two blocking test defects:

- the panic characterization could fail before HTTP because the package's
  relative temp directory did not exist, and it accepted any panic rather than
  the documented divide-by-zero;
- the in-flight cancellation transport ignored `req.Context()`, so the test
  would still pass after the intended context-propagation fix.

The current corrective changes make both tests independently diagnostic. They
also bind the 1+3 retry test to `base.NewRestyClient()` and exercise the
provider-specific Quark/UC PRE `pr` and Referer values on loopback.

This document does **not** claim the corrected exact head has passed the
targeted/repeat/race/shuffle/vet gate yet. That is the next independent gate.

## Next E1 step

After this characterization compiles and runs on the exact baseline:

- convert the unsafe observations into fail-closed regression expectations;
- introduce a narrow PRE-only no-replay request path without mutating the shared
  Resty client;
- attach the caller context in flight;
- expose and validate actual HTTP + provider envelope state;
- reject structurally unusable PRE before hash/part and before any panic path;
- preserve existing provider error attribution and cookie-refresh behavior;
- add redirect, timeout, partial-body, URL-shape and overwrite rollback cases;
- run the matrix for Quark and UC;
- run package vet/test/race/shuffle plus internal/op and server/webdav gates.

This E1 phase remains **observation/no-replay/fail-closed only**. It does not
authorize automatic recovery of an ambiguous PRE allocation.
