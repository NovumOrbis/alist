# Quark upload/pre E1 hardening candidate

Status: **DRAFT HARDENING / LOOPBACK-ONLY VALIDATION / NO AUTOMATIC PRE RECOVERY**.

Frozen characterization prerequisite:

- baseline: `AlistGo/alist@fb0731a6953012e7b72b89bf5473817caa4625f9`
- accepted characterization: `NovumOrbis/alist#9@5aa8b80bc5ad0a07775780f9c8a21fa2e6eb5274`
- research prerequisite: `NovumOrbis/alist#8@70c495d75c176edb813faabadeba849615cfe437`

The accepted characterization proved that the baseline could:

- return nil error for HTTP 500 + empty/HTML PRE responses;
- accept an HTTP 200 provider rejection;
- reach an integer divide-by-zero after a zero-valued PRE;
- replay PRE through Resty's configured transport retry budget;
- leave PRE detached from the caller context;
- follow 307 and 302 redirects, causing POST replay or method conversion.

This branch turns those observations into fail-closed runtime behavior and
post-fix regression expectations.

## Runtime scope

The hardening is intentionally limited to `/file/upload/pre`.

It:

1. executes PRE through a dedicated Resty wrapper with **RetryCount=0**;
2. reuses the selected client's underlying transport, cookie jar and timeout;
3. disables HTTP redirects for PRE with `http.ErrUseLastResponse`;
4. binds PRE to the caller context;
5. requires exact HTTP 200;
6. rejects provider status/code failures;
7. requires non-empty `task_id`, `fid`, `upload_id`, `obj_key`,
   `bucket` and `auth_info`;
8. requires positive `part_size`;
9. validates that `upload_url` is compatible with the existing
   `UploadUrl[7:]` target construction without asserting a real-provider
   scheme contract;
10. returns before hash/part if PRE is invalid;
11. preserves response-session cookie merging;
12. does not mutate the shared Resty client's retry or redirect settings.

The general `requestWithCookie` path keeps its existing retry behavior.
Hash/part/commit/finish reliability behavior is unchanged.

## Explicit non-goals

This candidate does **not**:

- retry an ambiguous PRE;
- infer that no provider allocation occurred;
- resume an unknown task;
- enable `same_path_reuse`;
- discover or delete orphan tasks;
- add provider cleanup;
- change shared Resty retry semantics;
- change overwrite behavior in `internal/op`;
- authorize a real-provider experiment or NAS mutation.

A failed PRE remains allocation-ambiguous. Fail-closed behavior prevents the
same driver call from making an additional PRE; it cannot prove that the first
request did not allocate remotely.

## Regression matrix

The post-fix test matrix covers:

- HTTP 500 + `{}` -> staged error, one PRE;
- HTTP 500 + HTML -> staged error, one PRE;
- HTTP 200 provider rejection -> staged provider error;
- invalid PRE -> `Put` returns before hash and cannot reach the prior
  divide-by-zero path;
- transport failure -> exactly one wire PRE even when the selected source client
  is `base.NewRestyClient()` with its normal RetryCount=3;
- caller cancellation -> in-flight PRE observes `context.Canceled`;
- 307 -> rejected without replay;
- 302 -> rejected without POST-to-GET follow-up;
- cross-origin redirect -> target server receives zero requests;
- provider-specific Quark/UC `pr` and Referer values remain staged correctly;
- cancellation before PRE -> zero requests;
- exact HTTP 200 requirement, including 201/204 rejection;
- required PRE structural fields and positive part size;
- baseline-compatible and incompatible upload URL shapes, including the prior
  N3 query/fragment/IPv6/control-character cases;
- partial response-body read failure -> error with one attempt;
- PRE response `__puus` merge preservation;
- source Resty retry/redirect configuration remains unmodified.

The accepted characterization's E1-05 timeout hygiene is also incorporated in
the post-fix cancellation test through bounded channel waits.

## Validation gate

Before this candidate is eligible for a clean submission branch, run on its
exact SHA:

```sh
go test ./drivers/quark_uc -run '^TestE1' -count=1 -v
go test ./drivers/quark_uc -run '^TestE1' -count=10
go test -race ./drivers/quark_uc -run '^TestE1' -count=1
go test -shuffle=on ./drivers/quark_uc -run '^TestE1' -count=1
go test ./drivers/quark_uc
go test -race -shuffle=on ./drivers/quark_uc
go vet ./drivers/quark_uc
git diff --check
```

Then run the relevant `internal/op` and `server/webdav` gates before an
upstream submission candidate is declared ready.

This document intentionally makes no claim that those gates have passed on the
current hardening head until they are independently executed.
