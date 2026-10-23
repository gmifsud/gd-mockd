---
type: implementation-handoff
project: gd-mockd
increment: A
status: verified
approval_status: approved
decisions: [D1, D2, D3, D4, D5, D6]
updated: 2026-10-20
branch: development-wsl
verified_commit: 44bd61b
implementation_commits: [75eafdf, 44bd61b]
prd: examples/wolt/PRD-price-stock-pilot.md
tags: [wolt, mockd, oauth, authentication, testing]
---

# Increment A: authentication and venue isolation

The twin's OAuth provider now issues Wolt-shaped tokens bound to a venue (`integration.venue_id`).
Refresh rotation is a single atomic commit, and Wolt's documented revocation rules apply. The
Phase 1 Menu endpoints reject any request without a valid, venue-matching token, before any
state changes. This removed the blocker for Increment C: the real `PersistentWoltConnections` /
`WoltTokenExchange` path onboards and refreshes against the twin without an emulator.

Rules marked *simulation policy* in PRD §5 are test behaviour. Passing them does not prove
live Wolt behaviour (L4).

## Supported contract

| ID | Behaviour | Provenance |
|---|---|---|
| T1–T3 | `POST /oauth2/token` with Basic auth; `authorization_code` requires `code` + `redirect_uri`; 200 `{access_token, token_type, expires_in: 3600, refresh_token, scope: offline}`; access JWT carries `integration.venue_id` | Documented |
| T2 | Codes are single-use and valid for 1 h; replay, unknown or expired → 400 `invalid_grant` | Documented; error body is policy |
| T4 | `GET /oauth2/authorize?…&venue_id=<v>` binds the venue to the code; missing, repeated, empty or non-matching (`[A-Za-z0-9_-]{1,64}`) → redirect `error=invalid_request`, no code | Simulation policy (stands in for WIO / merchant consent) |
| T6 | A code is consumed only by the client it was issued to | Simulation policy (RFC 6749 §4.1.3) |
| R2/R5 | Refresh tokens are single-use. N concurrent refreshes of one token → exactly one 200, N−1 × `invalid_grant` | Documented / linearisable reading |
| R3 | A refresh revokes the access token issued with the consumed refresh token | Documented |
| R4 | Re-presenting a consumed refresh token (same client) revokes the chain's current pair, so the venue must be onboarded again. Concurrent losers count as reuse | Documented |
| R6 | Refreshed tokens keep the bound `integration.venue_id` and get a new `jti` | Simulation policy (inferred; capture at L4) |
| R8/R9 | A different client cannot consume or revoke a chain. A local mint failure leaves the token unconsumed | Simulation policy |
| R1 | Refresh-token expiry is inclusive (`now >= expiresAt` is expired) and re-checked at commit, because minting runs outside the lock (amendment A.1) | Simulation policy |
| G1/G2 | Missing, malformed, `alg=none`, bad signature, wrong `iss`, missing `exp`, expired or revoked token → 401 `{"detail":"Unauthorized"}` | Simulation policy (Menu spec documents only 202/400/500) |
| G3/G5 | Valid token whose `integration.venue_id` equals `{venueId}` → request proceeds, including after refresh | Documented (T3 + path-scoped API) |
| G4 | Valid token for another venue → **403** `{"detail":"Forbidden"}`; target venue unchanged | Simulation policy; **L4 capture priority 1** |

Guarded routes: `PATCH /venues/{venueId}/items` and `PATCH /venues/{venueId}/items/inventory`
only. Seeded venues: `68656c6c6f76326f72646572` and `wolt-pilot-venue-b`.

## Implementation

Generic, with no Wolt concepts in Go:

- `pkg/oauth/claims.go`: `authorizeClaims`. Dotted claim paths with an anchored `pattern`;
  reserved, empty and conflicting paths are rejected at load.
- `pkg/oauth/rotation.go`: `RotateRefreshToken` runs as snapshot → mint (no lock) →
  compare-and-swap commit. Also tombstones and chain heads for reuse detection,
  `revokePreviousAccessToken`, and `ErrTokenMint`.
- `pkg/oauth/provider.go`: `ValidateToken` requires RS256, the provider's issuer and an `exp`;
  injectable clock (`Now`/`SetClock`); code consumed only by its client; `authorizationCodeExpiry`.
- `pkg/oauth/handler.go`: claim capture at `/authorize`; claims carried through both grants;
  rotation path.
- `pkg/mock/auth.go`, `pkg/engine/handler_auth.go`: `http.auth` bearer guard (`provider`,
  `audience`, `claims[].equals|equalsPathParam`, `onUnauthenticated`, `onForbidden`,
  `mode: enforce|warn`). Runs after matching and before SSE, chunked, validation and stateful
  dispatch. Providers are registered by OAuth mock id.
- Wiring: `pkg/mock/types.go`, `validator.go`, `pkg/engine/handler.go`, `handler_protocol.go`,
  `mock_manager.go`, `pkg/config/types.go` (`extend[].auth`), `pkg/cli/imports.go`,
  `schema/mockd.schema.json`.

Wolt-specific, in config only (`examples/wolt/mockd.yaml`):

- `revokePreviousAccessToken`, `refreshTokenReuseDetection`, `authorizationCodeExpiry: 1h`, the
  `venue_id → integration.venue_id` binding, and the redirect URI
  `https://onboarding.mockd.invalid/callback` for Integration.
- `x-wolt-venue-bearer` anchor applied to the two Menu `extend` entries; second venue seed.

Docs: `CHANGELOG.md`, `docs/src/content/docs/protocols/oauth.md`, `examples/wolt/README.md`,
PRD §11.

### Amendment A.1 (in `44bd61b`)

`RotateRefreshToken` treated `now == expiresAt` as valid and did not re-check expiry after
minting. A token could therefore expire during the unlocked mint and still be rotated. Fixed:
the boundary is inclusive and expiry is re-checked inside the commit; a failed rotation leaves
the original access token valid. Covered by `TestRotate_ExpiryCheckedAtSnapshotAndCommit`
(snapshot and commit × exact boundary and +1 ns).

## Deviations from PRD §6

| # | Design | Implemented | Why |
|---|---|---|---|
| X1 | Remove the `wolt-expired-token` magic mock | Kept as a fixture for **unguarded** endpoints (Order, Venue, Drive) | It can only answer 401, and guarded endpoints reject any non-`wolt-auth` token themselves (asserted) |
| X2 | Unknown guard `provider` fails at config load | Fails closed per request (500, logged) | Registration order is not guaranteed; a load-time check would reject valid configs |

## Deliberate boundaries

- Each process has its own in-memory RSA key and OAuth state. Restarting the simulator
  invalidates every token and refresh chain; nothing is persisted.
- No clock control on a running binary. Expiry is proven only in unit tests via `SetClock`;
  Increment C moves Integration's local expiry instead. Wall-clock provider expiry is not proven.
- Guarding Order, Venue and Drive endpoints is deferred.
- The `ValidateToken` hardening also applies to `/userinfo` and `/introspect`. Tokens without
  `exp` or with a foreign `iss` are now rejected there too.
- Tombstones and revocation lists are bounded, in-memory and cleaned once a minute.

## Verification record

- **2026-10-19, pre-commit tree** (Linux VM, Go 1.26.2): `go test -race -count=3` passed for
  `pkg/oauth`, `pkg/mock`, `pkg/config`, and the `pkg/engine` guard and OAuth tests. The
  concurrency test ran 200 trials × N∈{2,16} × reuse on/off with one winner every time;
  baseline before the fix: 50/50 trials forked. `go test ./...` passed except `tests/e2e`
  (needs Docker) and `tests/performance` (port collision; passes alone). Twin suite 18/18.
- **2026-10-20, `origin/development-wsl` @ `44bd61b`** (fresh clone, Linux, Go 1.26.2):
  `go vet`, `gofmt -l` and `git diff --check` clean on A's files.
  `go test -race -count=3 ./pkg/oauth` passed (27 A tests, A.1 included).
  `go test -race -count=3 ./pkg/engine -run 'BearerGuard|HTTPAuthValidate'` passed (13).
  `pkg/cli` extend-auth tests passed (3).
  `go test ./pkg/oauth ./pkg/mock ./pkg/config ./pkg/engine ./pkg/cli ./pkg/stateful` passed.
  Twin suite with a freshly built binary: **23/23**; the 5 A-relevant tests are
  `test_oauth_authorization_code_and_single_use_refresh`,
  `test_authorize_requires_a_valid_venue`,
  `test_menu_updates_require_a_token_bound_to_the_venue`,
  `test_concurrent_refresh_has_exactly_one_winner` and `test_expired_token_is_rejected`.
- **Consumer evidence (Increment C handoff):** strict acceptance 3/3 on three successive runs
  through the real OAuth provider, including a 403 for a cross-venue bearer and concurrent
  two-owner refresh to generation 2.
- **Not run at `44bd61b`:** the full `go test ./...` and `tests/e2e`.

## WSL reproduce

From the repository root in Ubuntu-GS66 (`~/repos/gd-mockd`, branch `development-wsl`):

```bash
set -euo pipefail
go version                                   # go.mod requires 1.26.2
git diff --check
go vet ./pkg/oauth ./pkg/engine ./pkg/mock ./pkg/config ./pkg/cli
go test -race -count=3 ./pkg/oauth
go test -race -count=3 ./pkg/engine -run 'BearerGuard|HTTPAuthValidate'
go test ./pkg/cli -run 'ProcessTablesAndExtend_.*Auth'
go test ./pkg/oauth ./pkg/mock ./pkg/config ./pkg/engine ./pkg/cli ./pkg/stateful
go build -o ./mockd ./cmd/mockd
unset WOLT_TWIN_EXTERNAL
MOCKD_BIN="$PWD/mockd" python3 examples/wolt/tests/test_wolt_twin.py
```

Expected: every Go package `ok`; Python `Ran 23 tests … OK`. The Python fixture starts its own
simulator in a temporary `--data-dir`; do not point it at a shared instance. `./mockd` is a
build output: do not commit it.

## Using it from Integration

1. Onboard: `GET http://<twin>/oauth2/authorize?client_id=wolt-pilot-client&redirect_uri=https://onboarding.mockd.invalid/callback&response_type=code&venue_id=<venue>`.
   Read `code` from the `Location` header (do not follow it), then pass it to
   `ReceiveOnboardingAsync`. `Engine:Wolt:Credentials:Development` = `wolt-pilot-client` /
   `wolt-pilot-secret`.
2. Venue IDs must match `[A-Za-z0-9_-]{1,64}`. Run-qualified IDs work; nothing is preconfigured.
3. The strict profile owns `/oauth2/token`. Matcher-based tests (synthetic JWTs) need a blank
   instance with its own `--data-dir`; see the Increment C handoff.
4. Under R4, any connector double-refresh revokes the venue's credentials: expect a 401, then
   `invalid_grant`, then `Uncertain`. That is the intended failure signal.

## Environment findings

- **WSL git identity is unset.** `44bd61b` is authored as `you@example.com`, while `75eafdf`
  used the real address. Set `git config --global user.email` in Ubuntu-GS66.
- **`44bd61b`'s message is a pasted chat summary** that covers both A's status and B. It also
  carries amendment A.1. It is already pushed; rewording it would need a force-push, so that
  is your call.
- **`origin/development-wsl` may lag WSL.** The C handoff records gd-mockd at `f85f131`, which
  is not on origin (head `44bd61b`). Push it. If its range touches A's files, re-run the
  commands above: `git log 44bd61b..f85f131 -- pkg/oauth pkg/mock/auth.go pkg/engine/handler_auth.go examples/wolt/mockd.yaml`.
- **PRD frontmatter is stale** (`repository: D:/…`, `branch: claude/adoring-davinci-t3a2gd`,
  `head_commit: 75eafdf`). It should name `development-wsl`.
- The Cowork file bridge cannot reach WSL paths (UNC is refused). The `D:\Repos\Tools\gd-mockd`
  copy is on `development` @ `75eafdf` with B uncommitted. Treat WSL as the source of truth.

## Next

Increment A needs no further work. Open items it hands on:
- L4 capture of G4 (401 vs 403) and R6 against the Wolt development environment.
- Guarding the Order, Venue and Drive endpoints once Phase 1 needs them.
