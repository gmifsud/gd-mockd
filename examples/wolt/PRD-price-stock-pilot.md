---
type: prd
project: gd-mockd
feature: Trustworthy Wolt simulator for one price-and-stock synchronisation flow
status: increment-a-implemented
approval_status: approved
approved: 2026-10-19
approved_decisions: [D1, D2, D3, D4, D5, D6]
implementation_authorised: true
owner: Gregory Mifsud
created: 2026-10-19
updated: 2026-10-19
version: "0.1"
repository: D:/Repos/Tools/gd-mockd
branch: claude/adoring-davinci-t3a2gd
head_commit: 0cb456d
consumer: D:/Repos/Integration (docs/wolt-simulator-fidelity-reference.md)
increments: [A-authentication, B-catalogue-effects, C-engine-acceptance, D-real-source]
current_increment: B (blocked on D7)
tags: [prd, mockd, wolt, oauth, simulator, contract-matrix]
---

# Trustworthy Wolt simulator: price-and-stock pilot

> **Target.** Two independent owners synchronise products into two simulated Wolt venues through the real Integration Engine and Wolt plugin. Tokens expire and rotate correctly, catalogue values change correctly, and ambiguous deliveries are never replayed automatically.

## 1. Decision requested

1. Approve the **contract matrix** (§5) as the acceptance boundary, including the provenance label on every rule.
2. Approve **Increment A** (§6): generic OAuth changes in `pkg/oauth`, a generic bearer guard in `pkg/engine`, and a strict Wolt profile in `examples/wolt/mockd.yaml`.
3. Accept two **corrections to the assessment** (§4). Wolt's documentation contradicts it on access-token invalidation and on refresh-token reuse.

Increments B–D are outlined so A is designed against them. They need their own sign-off.

## 2. Claim boundary

| Layer | Proves | Lives in | Status |
|---|---|---|---|
| L1 mockd unit tests | Token and state mechanisms behave correctly | `pkg/oauth`, `pkg/engine` | Partial; refresh is racy (§3, F2) |
| L2 Wolt simulator contract tests | Configured Wolt behaviour matches the documented subset | `examples/wolt/tests/` | 15 scenarios. No auth enforcement, no catalogue effect |
| L3 Integration E2E | Real connector and engine are correct against L2's subset | `Integration/…Emulation.Tests/MockdFlowTests.cs` | Green against **per-run matcher mocks with unsigned `alg=none` JWTs**, not the twin |
| L4 Wolt development environment | L2's assumptions hold against the actual provider | — | Not started |

Passing L3 does not establish L4. Every rule labelled *Simulation policy* in §5 is a candidate for L4 capture.

**Provenance labels.**
- **Documented:** an official Wolt reference supports it. The reference is cited.
- **Captured:** a sanitised real-provider exchange supports it. There are none yet.
- **Simulation policy:** deliberately chosen for testing. It makes no claim of provider equivalence and must never become a production requirement.

## 3. Current state (verified 2026-10-19)

Evidence comes from source at `0cb456d` and from a race probe run against an unmodified copy of `pkg/oauth` (`go test -race`, 50 trials).

| # | Severity | Location | Finding | Evidence |
|---|---|---|---|---|
| F1 | **Critical** (blocker) | `pkg/oauth/handler.go` `handleAuthorizationCodeGrant`, `handleRefreshTokenGrant` | Access tokens carry `sub, client_id, scope, iss, iat, exp, jti`, and no `integration` claim. `WoltTokenExchange.ExchangeAsync` reads `integration.venue_id` on **both** exchange and refresh, so the real connector cannot onboard against the twin | Probe: claim keys printed, `integration` absent |
| F2 | **Critical** | `handleRefreshTokenGrant`: `ValidateRefreshToken` → `GenerateToken` (RSA sign) → `RevokeToken` → `StoreRefreshToken` | Check-then-act across four separately locked steps. RSA signing sits inside the window, so concurrent refresh **always** forks the chain | Probe: **50/50** trials produced two valid successor refresh tokens |
| F3 | High | Same | The previous access token stays valid after refresh, and a replayed refresh token has no side effect. Both contradict Wolt (§4) | Probe: old access valid = true; newest pair valid after replay = true |
| F4 | High | `examples/wolt/mockd.yaml` `wolt-expired-token` | Marketplace endpoints accept **any** bearer. "Expired" is the magic literal `Bearer expired-token`. No signature, expiry or venue check | Config |
| F5 | High | `examples/wolt/mockd.yaml` `UpdateMenuItems`, `UpdateMenuInventory` | Updates append to `menu_events` only. Catalogue state never changes, so price and stock outcomes cannot be asserted | Config |
| F6 | Medium | `pkg/oauth/handler.go` `HandleAuthorize` | Codes always bind to the first configured user. There is no per-venue binding. Code expiry is hard-coded to 10 min (Wolt: 1 h) | Source |
| F7 | Medium | `pkg/oauth/provider.go` `ValidateToken` | Checks signature method family and `exp` only. No issuer check, no `exp`-required check, and no injectable clock, so expiry cannot be tested without waiting | Source |
| F8 | Low | `pkg/oauth/provider.go` `ExchangeAuthorizationCode` | The code is deleted **before** the client and redirect checks, so a wrong client burns a legitimate client's code | Source |
| F9 | Medium (for B) | `pkg/stateful/executor.go` step types | `read/update/delete/create/set/list/validate`; no iteration step. A read-then-update step pair is not atomic per document, so concurrent PATCHes to one venue can lose updates | Source |

Already satisfied, needs a test only: access-token generations are distinguishable (`GenerateToken` mints a random 32-character `jti`). Authorization-code consumption is atomic (delete under `authCodesMu`).

## 4. Corrections to the assessment

Source: [Wolt Authentication 2.0](https://developer.wolt.com/docs/authentication20), fetched 2026-10-19.

| Assessment said | Wolt documents | Consequence |
|---|---|---|
| §4: "Do not automatically invalidate an old access token… unless the provider contract establishes that behaviour" | "The previous access token and refresh token are invalidated and can no longer be used." | The contract establishes it, so the Wolt profile **must** revoke the previous access token on refresh (rule R3). |
| §3 test 5: "Refresh using the winning replacement and verify success" (after a concurrent loser) | "Reusing the same refresh token in multiple requests will immediately deactivate and revoke the newly issued access and refresh tokens." | A concurrent loser *is* a reuse. Under the Wolt profile the winner's chain is **revoked**, and the venue must be re-onboarded. The test splits into two: winner valid with reuse detection off (generic), and winner revoked with it on (Wolt). |
| §3 test 4: "Replay the consumed token and verify failure" | Same sentence | A replay fails **and** revokes the current pair. It must run *after* any assertion that the successor works. |

This strengthens the case for Integration's refresh fencing (`ExchangeClaim` + `DeliveryClaim` in `PersistentWoltConnections`). Against real Wolt, one double-refresh does not create a second chain. It **bricks the venue**. The simulator must reproduce that, or it would validate a broken connector.

## 5. Contract matrix

**Prefixes.** T = token exchange. R = refresh. G = guarded API access. I = items. V = inventory. **Inc** = the increment that implements the rule.

### 5.1 Token exchange: `POST /oauth2/token`, `grant_type=authorization_code`

| ID | Request shape | State effect | Response | Provenance | Test | Inc |
|---|---|---|---|---|---|---|
| T1 | Basic `client_id:client_secret`; form `grant_type, code, redirect_uri`, all required; `application/x-www-form-urlencoded` | Code consumed | 200 `{access_token, token_type:"bearer", expires_in:3600, refresh_token, scope:"offline"}` | Documented ([auth20](https://developer.wolt.com/docs/authentication20)) | `TestCodeExchange_IssuesBoundPair` | A |
| T2 | — | Code single-use, valid 1 h | Second use → error | Documented | `TestCodeExchange_ConsumedCode_InvalidGrant`, `TestCodeExchange_ExpiryConfigurable` | A |
| T3 | — | — | Access JWT payload carries `integration.venue_id` | Documented | `TestAuthorizeClaims_VenueA`, `…_VenueB` | A |
| T4 | `GET /oauth2/authorize?…&venue_id={v}` (pattern `^[A-Za-z0-9_-]{1,64}$`) | Code bound to `{integration.venue_id: v}` | 302 to `redirect_uri?code=…` | **Simulation policy.** Stands in for WIO and self-service merchant consent, which Wolt performs out of band | `TestAuthorizeClaims_MissingOrInvalidParam_Rejected` | A |
| T5 | Unknown, consumed or expired code | None | 400 `{"error":"invalid_grant"}` | **Simulation policy.** Wolt documents no error body; the RFC 6749 §5.2 shape is used | `TestCodeExchange_UnknownCode_InvalidGrant` | A |
| T6 | Valid code, wrong client | Code **not** consumed | 400/401, no tokens | **Simulation policy** (RFC 6749 §4.1.3) | `TestCodeExchange_WrongClientDoesNotConsume` | A (D6) |

### 5.2 Refresh: `POST /oauth2/token`, `grant_type=refresh_token`

| ID | Request shape | State effect | Response | Provenance | Test | Inc |
|---|---|---|---|---|---|---|
| R1 | Basic auth; form `grant_type, refresh_token` | — | 200, same shape as T1 | Documented | `TestRotate_IssuesNewPair` | A |
| R2 | — | Presented refresh token consumed; one replacement published | New `refresh_token` ≠ old | Documented ("single-use") | `TestRotate_Replay_InvalidGrant` | A |
| R3 | — | Previous **access** token revoked at the commit point | Old bearer → 401 on guarded APIs | Documented | `TestRotate_RevokesPreviousAccessToken` | A |
| R4 | Consumed refresh token presented again by the **same client** | Current head pair (refresh + access) revoked | 400 `invalid_grant` | Documented (reuse sentence); status is Simulation policy | `TestRotate_ReuseRevokesFamily` | A |
| R5 | N concurrent requests, same token | Exactly one commit | 1 × 200, (N−1) × 400 `invalid_grant` | **Simulation policy.** Linearisable reading of R2 | `TestRotate_ConcurrentSameToken_ExactlyOneWinner` (N ∈ {2,16}, 200 trials, `-race`) | A |
| R6 | — | Bound claims copied to the successor | New access JWT has the **same** `integration.venue_id`, new `jti` | **Simulation policy.** Inferred from T3; capture at L4 | `TestRefresh_PreservesBoundClaims`, `TestRefresh_NewJTIPerGeneration` | A |
| R7 | — | — | Access 1 h; refresh 30 d | Documented | Config assertion in `test_wolt_twin.py` | A |
| R8 | Valid token, wrong or invalid client | **No** consumption, **no** family revocation | 401 `invalid_client` | **Simulation policy** | `TestRotate_WrongClientCannotConsumeOrRevoke` | A |
| R9 | Local mint failure (signing or random) | **No** consumption | 500 `server_error`; old token still usable | **Simulation policy** | `TestRotate_MintFailureDoesNotConsume` | A |
| R10 | `rotateRefreshTokens: false` | Unchanged legacy behaviour | Echoes the presented token | mockd compatibility | Existing `provider_test.go` + `TestRotate_DisabledPreservesLegacy` | A |

### 5.3 Guarded Marketplace access: `/venues/{venueId}/items[/inventory]`

| ID | Request | State effect | Response | Provenance | Test | Inc |
|---|---|---|---|---|---|---|
| G1 | Missing or malformed `Authorization` | **None** (evaluated before validation and stateful dispatch) | 401 `{"detail":"Unauthorized"}` | **Simulation policy.** The Menu spec documents only 202/400/500 | `TestBearerGuard_Missing_NoMutation` | A |
| G2 | Bad signature, `alg=none`, wrong `iss`, missing `exp`, expired, revoked | None | 401 | **Simulation policy.** R3 is documented; the status code is not | `TestBearerGuard_{Tampered,AlgNone,WrongIssuer,Expired,Revoked}` | A |
| G3 | Valid token, `integration.venue_id == {venueId}` | Proceeds | Downstream response | Documented (T3 + path-scoped API) | `TestBearerGuard_VenueMatch_Passes` | A |
| G4 | Valid token for A, path venue B | None. B unchanged | **403** `{"detail":"Forbidden"}` | **Simulation policy, L4 capture priority 1.** 401 would make the connector invalidate and refresh in a loop; 403 maps to `wolt_permission_denied` | `TestBearerGuard_VenueMismatch_403_NoMutation` | A |
| G5 | Refreshed token (generation N+1), same venue | Proceeds | Downstream response | Follows from R6 | `TestBearerGuard_RefreshedToken_Passes` | A |
| G6 | Guard `mode: warn` | Proceeds; violation logged | Downstream response | mockd demo mode; **forbidden** in L3 | `TestBearerGuard_WarnMode` | A |

### 5.4 Items and inventory: catalogue effects (Increment B; policy approval needed now)

| ID | Request | State effect | Response | Provenance | Test | Inc |
|---|---|---|---|---|---|---|
| I1 | `PATCH /venues/{v}/items` `{data:[…]}`; each element has **exactly one** of `external_id \| gtin \| sku` | — | — | Documented (menu spec `update-items`) | Existing twin validation | B |
| I2 | Valid batch | Supported fields (`price`, `discounted_price`, `vat_percentage`, `enabled`) applied to the matching item **in venue v only** | **202**, no body | Documented (202 Accepted) | `test_price_update_leaves_inventory` | B |
| I3 | `discounted_price` without `price`; non-integer `price` | None | 400 | Documented | `test_items_invalid_400_no_mutation` | B |
| I4 | Unknown identifier in an otherwise valid batch | Matched items applied; unmatched recorded in the journal as `unmatched` | 202 | **Simulation policy.** The spec says only "when a match is found, the item is updated" | `test_unknown_identifier_ignored_and_journalled` | B |
| I5 | One schema-invalid element in a batch | **No** element applied | 400 | **Simulation policy.** Batch atomicity is undocumented | `test_mixed_validity_batch_atomic_reject` | B |
| I6 | — | Applied **synchronously** before the 202 | — | **Simulation policy.** Wolt processes asynchronously | Documented in README | B |
| V1 | `PATCH /venues/{v}/items/inventory` `{data:[{<one id>, inventory:int}]}`; `inventory` required | `inventory` set; `price` untouched | 202 | Documented (`required: [inventory]`) | `test_inventory_update_leaves_price` | B |
| V2 | `inventory: null` or missing | None | 400 | Documented (required) | Existing twin validation | B |
| V3 | `sku: "000123"` | Matched as a **string**; stored value stays `"000123"` | 202 | Documented (`type: string`) | `test_leading_zero_sku_stays_string` | B |
| V4 | Same SKU in A and B; update A | B unchanged | 202 | Documented (path-scoped) | `test_cross_venue_same_sku_isolated` | B |
| J1 | Any accepted, rejected or unmatched request | Journal row `{venue, op, payload, outcome}` kept **alongside** catalogue state | — | mockd invariant | Every B test asserts both | B |

Out of matrix (deferred): `in_stock` semantics, rate-limit enforcement (`venue_config.rate_limit_settings.itemsUpdate`), options updates, order and Drive APIs.

## 6. Increment A design

### 6.1 Architecture

```text
GET /oauth2/authorize?venue_id=v ──► Provider.authCodes{code → Claims{integration.venue_id:v}}
POST /oauth2/token (code)        ──► access JWT(+Claims) ; refreshTokens{rt → {Claims, AccessToken, Family}}
POST /oauth2/token (refresh)     ──► Provider.Rotate (CAS commit) ──► successor carries Claims; old access revoked
PATCH /venues/{v}/items          ──► matcher ─► BearerGuard(provider=wolt-auth, claim==pathParam) ─► validation ─► stateful
                                                   └─ 401/403 before any state change
```

Boundary rule: `pkg/oauth` and `pkg/engine` contain **no Wolt concepts**. `integration.venue_id`, `venueId`, `403` and the error body appear only in `examples/wolt/mockd.yaml`.

### 6.2 A1: claim binding at authorisation (fixes F1, F6)

Generic config:

```yaml
oauth:
  authorizationCodeExpiry: "1h"          # new; default 10m (unchanged)
  authorizeClaims:                       # new; values copied from /authorize params into the code
    - param: venue_id
      claim: integration.venue_id        # dotted path → nested object
      pattern: '^[A-Za-z0-9_-]{1,64}$'   # matches WoltVenueBinding.Validate
      required: true
```

- Load-time validation: `claim` may not target reserved claims (`iss, sub, aud, exp, nbf, iat, jti, client_id, scope`). `pattern` must compile.
- `AuthorizationCode.Claims` → `RefreshTokenData.Claims` → every successor access token. The handler merges claims after `DefaultClaims` and before the reserved claims, so reserved claims cannot be overridden.
- A missing or invalid required param redirects with `error=invalid_request` and issues no code.

| Option | Verdict |
|---|---|
| **a. `/authorize` param → bound claim** | **Adopt.** Dynamic, so it supports Integration's run-qualified venues (`wolt-{r}-…`). Generic. |
| b. One configured user per venue plus `login_hint` | Reject. Static; cannot express run-qualified venues without admin API changes. |
| c. Admin API to mint codes directly | Defer. Bypasses the real `/authorize` path. |
| d. One global default `integration.venue_id` claim | Reject (assessment §2). It hides cross-venue defects. |

### 6.3 A2: atomic rotation (fixes F2)

A new `Provider.RotateRefreshToken(presented, clientID string, mint MintFunc) (*Issued, error)`, used only when `rotateRefreshTokens: true`:

1. **Snapshot** under `RLock`: look up `presented`, check expiry and client, keep the `*RefreshTokenData` pointer `s`. Failure → `invalid_grant`; no state change except the existing expired-token deletion.
2. **Mint** with no lock held. Sign the access token from `s.Claims`; generate the replacement refresh string. Failure → 500 `server_error` with **no consumption** (R9).
3. **Commit** under `Lock`. If `refreshTokens[presented] != s`, the request lost the race → reuse path (A3) → `invalid_grant`. Otherwise, in one critical section: delete `presented`, tombstone it to its family, insert the successor, revoke `s.AccessToken` (R3).

The linearisation point is the commit's pointer compare-and-swap. ABA is impossible because tokens are 64-character random values and are never re-inserted. Lock order stays `refreshTokensMu → revokedTokensMu`, the same as `RevokeToken`, so it cannot deadlock. A loser's minted tokens are discarded unreturned. That is harmless, because a credential that is never disclosed cannot be used.

### 6.4 A3: Wolt revocation semantics (fixes F3)

Generic flags under `oauth`. Both default to `false`, so existing behaviour is unchanged:

| Flag | Effect | Wolt profile |
|---|---|---|
| `revokePreviousAccessToken` | Commit revokes the access token issued with the consumed refresh token | `true` (R3) |
| `refreshTokenReuseDetection` | A presented token found in the tombstones, from the **same client**, revokes the family head (refresh + access) | `true` (R4) |

Tombstones are `map[consumed]familyID` and `families` is `map[familyID]headToken`. Both are bounded like the existing maps (cap plus `refreshExpiry` TTL) and cleaned in `cleanupExpiredTokens`. A different client never triggers revocation (R8), so one tenant cannot revoke another's chain.

Integration cross-check: `PersistentWoltConnections` blocks token maintenance while a `DeliveryClaim` is held. Revoking the previous access token at refresh therefore cannot strand an in-flight delivery. Increment C asserts this.

### 6.5 A4: generic bearer guard (fixes F4, F7)

New `HTTPSpec.Auth`, also accepted on `extend[]` entries so imported operations can use it:

```yaml
x-wolt-venue-bearer: &wolt-venue-bearer
  bearer:
    provider: wolt-auth                    # OAuth mock id → key, issuer, revocation list
    claims:
      - { claim: integration.venue_id, equalsPathParam: venueId }
  onUnauthenticated: { status: 401, body: '{"detail":"Unauthorized"}' }
  onForbidden:       { status: 403, body: '{"detail":"Forbidden"}' }
  mode: enforce                             # enforce (default) | warn
extend:
  - { mock: menu.update-items, table: menus, action: custom, operation: UpdateMenuItems, response: *accepted, auth: *wolt-venue-bearer }
```

- The guard is evaluated in `pkg/engine/handler.go` **after** path parameters are resolved and **before** `Validation` and `StatefulBinding`. Rejection happens before state changes (G1–G4), and the request log records the outcome.
- Matching is structural (`equalsPathParam`, `equals`), not templated, to avoid an injection surface.
- `ValidateToken` is hardened: `jwt.WithValidMethods(["RS256"])`, `WithIssuer(config.Issuer)`, `WithExpirationRequired()`, and an injectable `now func() time.Time` on `Provider` for expiry tests (F7). This also rejects Integration's current unsigned `MockdJwt` tokens. That is intentional; see §7.
- An unknown `provider` id fails at config load.
- The `wolt-expired-token` magic mock is **removed**. Expiry is real.

### 6.6 A5: strict Wolt profile

Increment A applies `mode: enforce` to the Phase 1 surface only: `menu.update-items` and `menu.update-item-inventory`. All other operations are unchanged; guarding them is deferred. The README's manual flow gains a `/authorize?venue_id=…` step. A second venue (`wolt-pilot-venue-b`) is seeded so the two-venue tests run offline.

### 6.7 Files touched (A)

| Path | Change |
|---|---|
| `pkg/oauth/types.go` | `AuthorizeClaim`, `AuthorizationCodeExpiry`, `RevokePreviousAccessToken`, `RefreshTokenReuseDetection`; `Claims` on code and refresh data; `AccessToken` and `Family` on refresh data |
| `pkg/oauth/provider.go` | `RotateRefreshToken`, tombstones and families, hardened `ValidateToken`, clock injection, F8 reorder |
| `pkg/oauth/handler.go` | Claim capture in `HandleAuthorize`; claim merge in both grants; rotation via `RotateRefreshToken` |
| `pkg/mock/types.go`, `pkg/config/types.go`, `schema/mockd.schema.json` | `HTTPSpec.Auth`, `ExtendBinding.Auth` |
| `pkg/engine/handler.go` (+ new `handler_auth.go`) | Guard evaluation at the validation seam |
| `pkg/engine/mock_manager.go` | Resolve `provider` id → `*oauth.Provider`; fail on unknown |
| `pkg/oauth/*_test.go`, `pkg/engine/handler_auth_test.go` | §5 test names |
| `examples/wolt/mockd.yaml`, `README.md`, `tests/test_wolt_twin.py` | Strict profile, second venue, contract tests |
| `CHANGELOG.md`, `docs/src/content/docs/protocols/oauth.md` | Document flags |

### 6.8 Verification (A)

```bash
go test -race ./pkg/oauth/ ./pkg/engine/ -run 'Authorize|CodeExchange|Rotate|Refresh|BearerGuard' -count=3
go test ./...                                  # regression: existing OAuth and engine suites unchanged
mockd start -c examples/wolt/mockd.yaml --no-auth && pytest examples/wolt/tests -k "auth or venue"
```

**Exit criterion.** The real `PersistentWoltConnections` completes onboarding and refresh against the twin for venues A and B. Concurrent refresh cannot create two valid chains. Two venues share the simulator without sharing authority.

## 7. Increments B–D (outline only)

- **B: Catalogue effects.** Per-venue catalogue documents with explicit identifier maps, seeded with a leading-zero SKU and the same SKU in both venues. F9 needs a decision first. Either (i) add a generic, atomic `patchElements` step: match array elements by key, apply listed fields, all-or-nothing under the document lock; or (ii) rebuild the array in an expr `set` step plus a single `update`. That is not atomic across concurrent requests. **Recommend (i).** (ii) loses updates under the concurrency that C exercises.
- **C: Engine acceptance.** Replace `MockdScenario.TokenMocks()` and `MockdJwt` (unsigned `alg=none`) in Integration with the twin's real OAuth provider. Delivery mocks give way to the guarded, stateful endpoints. Fault injection **after mutation** (response-lost) needs a new mockd hook; `delay` and `chaos` act before processing and cannot prove "applied, response lost". Two owners, two venues, restart before dispatch, run 3× against the marked database.
- **D: Real source.** Swap only the source for Panama's read path, which stays read-only.

## 8. Risks and open questions

| Item | Treatment |
|---|---|
| G4 status (401 vs 403) for a wrong-venue token is unknown at Wolt | Simulation policy; first L4 capture. The connector behaves differently under each, so the choice is visible in the matrix |
| R6 (venue preserved on refresh) is inferred, not documented | Capture at L4 |
| Expiry scenarios in C: Integration rejects `expires_in < 60` | Unit tests use the injectable clock. For C, either a dedicated twin instance with `tokenExpiry: 60s`, or an admin clock-offset endpoint. Decide in C's PRD |
| The twin's RSA key is per process | A restart invalidates all tokens. Acceptable for tests; document it. Persistence is out of scope |
| Integration working tree has 86 uncommitted paths, including the Mockd test layer | C must start from a committed baseline; record the commit in the run report |
| The project copy of `wolt-mockd-transport-redirect-prd.md` is v0.2; the repo copy is v0.4 | Resync before C |

## 9. Sign-off gate

| ID | Decision | Proposed |
|---|---|---|
| D1 | Contract matrix §5 and provenance labels as the acceptance boundary | Accept |
| D2 | Adopt Wolt-documented R3/R4 (corrections in §4) in the Wolt profile; generic defaults off | Accept |
| D3 | Venue binding via `/authorize` param (option a) | Accept |
| D4 | Generic `auth.bearer` guard at the validation seam; structural claim matching | Accept |
| D5 | G4 = 403 as Simulation policy, pending L4 capture | Accept |
| D6 | Include F8 (wrong client does not burn a code); 3-line reorder | Accept |
| D7 | B policies I4 (ignore and journal), I5 (atomic reject), I6 (synchronous); B approach (i) `patchElements` | Accept (binds B's PRD) |

No execution code until D1–D6 are approved.

## 10. Decision and activity log

| Date | Entry | Status |
|---|---|---|
| 2026-10-19 | Assessment received: make gd-mockd trustworthy for one price-and-stock flow | Received |
| 2026-10-19 | Source traced: `pkg/oauth`, `pkg/engine/handler*.go`, `examples/wolt/mockd.yaml`; Integration `WoltTokenExchange`, `PersistentWoltConnections`, `WoltModule`, `Emulation.Tests/Mockd/*` | Done |
| 2026-10-19 | Race probe against an unmodified `pkg/oauth`: 50/50 forked chains; no `integration` claim; old access valid after refresh; replay has no side effect | Done |
| 2026-10-19 | Wolt auth20 reviewed: previous tokens invalidated on refresh; reuse revokes new pair; code valid 1 h | Done; assessment corrected (§4) |
| 2026-10-19 | v0.1 drafted | Done |
| 2026-10-19 | D1–D6 approved by Gregory Mifsud; D7 remains open (binds Increment B) | Approved |
| 2026-10-19 | Increment A implementation started (test-first) | Done |
| 2026-10-19 | Increment A implemented and verified (§11); two deviations from §6 recorded | Awaiting review |

## 11. Implementation record: Increment A (2026-10-19)

Base `0cb456d`, uncommitted on `claude/adoring-davinci-t3a2gd`. Normalised diff: 16 modified files (+529/−118) plus 7 new files.

### Delivered

| Design ref | Change | Files |
|---|---|---|
| A1 (§6.2) | `authorizeClaims` (anchored pattern; reserved, empty and conflicting paths rejected at load), `authorizationCodeExpiry`. Claims flow code → refresh data → every access token | `pkg/oauth/claims.go` (new), `types.go`, `handler.go` |
| A2 (§6.3) | `Provider.RotateRefreshToken`: snapshot → mint (no lock) → compare-and-swap commit. `ErrTokenMint` leaves the token unconsumed | `pkg/oauth/rotation.go` (new), `handler.go` |
| A3 (§6.4) | `revokePreviousAccessToken`, `refreshTokenReuseDetection` (tombstones and chain heads, same-client only). Both require `rotateRefreshTokens`, enforced in `NewProvider` | `rotation.go`, `provider.go` |
| F7 | `ValidateToken`: RS256 only, issuer, `exp` required, provider clock (`Now`/`SetClock`) | `provider.go` |
| F8 / D6 | A code is consumed only by the client it was issued to | `provider.go` |
| A4 (§6.5) | `http.auth` guard (`bearer.provider/audience/claims`, `onUnauthenticated`, `onForbidden`, `mode`), on mocks and `extend[]`. Evaluated before SSE, chunked, validation and stateful dispatch. Providers registered by OAuth mock id | `pkg/mock/auth.go` (new), `pkg/engine/handler_auth.go` (new), `handler.go`, `handler_protocol.go`, `mock_manager.go`, `pkg/mock/types.go`, `validator.go`, `pkg/config/types.go`, `pkg/cli/imports.go`, `schema/mockd.schema.json` |
| A5 (§6.6) | Wolt profile: Wolt revocation flags, 1 h codes, `venue_id` binding, Integration's redirect URI, `x-wolt-venue-bearer` on `menu.update-items` and `menu.update-item-inventory`, second venue `wolt-pilot-venue-b` | `examples/wolt/mockd.yaml`, `README.md` |
| Docs | Changelog (4 Added, 3 Fixed); OAuth protocol page | `CHANGELOG.md`, `docs/src/content/docs/protocols/oauth.md` |

### Evidence

| Check | Result |
|---|---|
| `go test -race -count=3 ./pkg/oauth/ ./pkg/mock/ ./pkg/config/` | Pass. Includes R5: 200 trials × N∈{2,16} × reuse on/off, exactly one winner every time (baseline before the fix: 50/50 forked) |
| `go test -race -count=3 ./pkg/engine/ -run 'BearerGuard\|HTTPAuth\|OAuth\|Stateful'` | Pass. G1–G6 plus fail-closed provider cases; rejected requests leave the stateful table empty |
| `go test ./pkg/cli/` | Pass. `extend[].auth` applied, invalid guard rejected |
| `go test ./...` | Pass, except two environment-only failures: `tests/e2e` (gRPC test needs Docker, absent on the test VM) and `tests/performance` (port collision under parallel packages; passes when run alone) |
| `python3 examples/wolt/tests/test_wolt_twin.py` (strict profile, real binary) | **18/18** (was 15). New: venue-bound menu updates across two venues (401/403/202 with no state change on rejection), `venue_id` validation at `/authorize`, 8-way concurrent refresh with one winner and the winner's chain revoked. The existing OAuth test now also asserts T3 and R4 |

Test names map to the contract IDs in §5 (`pkg/oauth/pilot_test.go`, `pkg/engine/handler_auth_test.go`, `pkg/cli/imports_auth_test.go`).

### Deviations from §6

| # | Design said | Implemented | Reason |
|---|---|---|---|
| X1 | §6.5: remove the `wolt-expired-token` magic mock | Retained, re-documented as a demo fixture for **unguarded** endpoints (Order, Venue, Drive) | Removing it now would leave those endpoints with no negative-auth fixture until they are guarded (deferred). It cannot weaken guarded endpoints: it can only answer 401, and the guard rejects any non-`wolt-auth` token itself (`test_menu_updates_require_a_token_bound_to_the_venue` covers `Bearer expired-token`) |
| X2 | §6.5: an unknown `provider` id fails at config load | Fails **closed at request time** (500, logged); no admission | Mock registration order is not guaranteed (HTTP mocks may register before their OAuth mock), so a load-time check would reject valid configs. Covered by `TestBearerGuard_UnregisteredProvider_FailsClosed` and `…_ProviderUnregisteredWithIssuer` |

Minor: step 1 of A2 takes the exclusive lock (not `RLock`) because it may delete an expired token. That does not change the linearisation argument.

### Not done (by design)

No commits: the user commits. Integration-side changes belong to Increment C. Increment B is blocked on D7.
