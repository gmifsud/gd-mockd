---
type: implementation-handoff
project: gd-mockd
increment: B
status: awaiting-wsl-verification
approval_status: approved
decision: D7
tags: [wolt, mockd, catalogue, testing]
---

# Increment B: catalogue effects

D7 approved: unknown identifiers are ignored and recorded, a schema-invalid member rejects
the entire batch, supported updates apply synchronously, and concurrent patches preserve
independent fields. These are simulation policies, not proof of live Wolt batch semantics.

## Implementation

- `pkg/config/patch_elements.go`: generic array-path, identifier and scalar-field configuration.
- `pkg/stateful/patch_elements.go`: validates the whole input, then locks the resource,
  copies its latest document, applies the batch and swaps the document once. Existing readers
  keep their original snapshot. The lock is resource-wide (all `menus` documents), not a new
  per-document lock system; appropriate for this bounded pilot, with no throughput claim.
- `pkg/stateful/executor.go`, `pkg/engine/config_loader.go`, schema: `patchElements` step
  registration, configuration propagation and preflight validation.
- Wolt's mappings and policies remain in `examples/wolt/mockd.yaml`. Generic packages have no
  venue, price, inventory or Wolt-specific branching.

## Supported contract

| Surface | Behaviour |
|---|---|
| Identifiers | Exactly one non-null, non-empty string: `external_id`, `gtin`, or `sku`. Leading zeros preserved. |
| Stored mappings | `external_data.external_id`, `gtin_barcode`, `merchant_sku`, respectively. |
| Item fields | `price` and `discounted_price`: integer; `vat_percentage`: finite number; `enabled`: boolean. |
| Dependency | `discounted_price` requires `price` in the same input item. |
| Inventory | Required non-null integer `inventory`; price fields cannot be changed through this endpoint. |
| Unsupported fields | Rejected with 400, including deferred `in_stock` and `disabled_until`. Do not interpret this bounded simulator policy as Wolt behaviour. |
| Catalogue layout | `menu.items`, `menu.categories[*].items`, and `menu.categories[*].subcategories[*].items`. Options and arbitrary deeper structures are outside this increment. |
| Unknown identifiers | 202; ignored, no product creation, per-input unmatched result recorded. |
| Repeated identifiers | Applied in input order; last supplied value for a field wins. Every matching occurrence in the configured item arrays is updated. This is simulator policy. |
| Evidence | Successful batches: `menu_events` with payload, venue, operation, accepted outcome and match results. Rejected batches: HTTP request journal; no accepted event. |

The catalogue is the stored `menu` itself, so exports reflect updates. The fixtures include
SKU `"000123"` in both venues, with independent starting prices and stock.

## Atomicity boundary

The catalogue batch is atomic, including under concurrent price/inventory PATCH requests.
The subsequent event-table insertion is a separate step, not a multi-table transaction. A
failure after catalogue commit must not be interpreted as proof that nothing was applied.
The existing `consistency: atomic` mode implements rollback without request isolation;
combining it with `patchElements` is rejected before execution because rollback could erase
another request's committed work. No changes were made to legacy operation semantics.

Concurrent full-menu replacement still replaces the full menu; this increment only guarantees
field preservation between `patchElements` operations. Persistence across simulator restart
and post-mutation transport faults remain Increment C work.

## Validation completed here

- Python test module parses.
- YAML parses and the complete Wolt configuration validates against the updated JSON schema.
- Fixture identifiers and stock values were checked.
- `git diff --check` passes.
- Go compilation, race tests and socket E2E tests were **not run**: this Windows session has
  no Go toolchain. Do not report the new tests as passing until the commands below succeed.

The existing two-file Increment A expiry correction remains uncommitted alongside B.

## WSL verification

Run from the copied repository root in WSL (Go version from `go.mod`: 1.26.2).
Copy the whole working tree, including new/untracked files; cloning committed HEAD alone
will omit this increment. Use a disposable simulator instance: the existing Python fixture
resets state and clears its request log. Leave `WOLT_TWIN_EXTERNAL` unset.

```bash
set -euo pipefail
go version
gofmt -w pkg/config/patch_elements.go pkg/config/types.go \
  pkg/stateful/patch_elements.go pkg/stateful/patch_elements_test.go pkg/stateful/executor.go \
  pkg/engine/config_loader.go pkg/engine/config_loader_patch_test.go \
  pkg/oauth/rotation.go pkg/oauth/pilot_test.go
git diff --check
go test -race -count=3 ./pkg/stateful -run PatchElements
go test ./pkg/engine -run TestConvertCustomOperation_PatchElements
go test -race -count=3 ./pkg/oauth -run TestRotate_ExpiryCheckedAtSnapshotAndCommit
go test ./pkg/config ./pkg/stateful ./pkg/engine ./pkg/oauth ./pkg/cli
go build -o ./mockd ./cmd/mockd
unset WOLT_TWIN_EXTERNAL
MOCKD_BIN="$PWD/mockd" python3 examples/wolt/tests/test_wolt_twin.py
```

Expected Python test count: 23. Missing binary/skipped tests are not acceptance.
The Go concurrency test updates 32 products with 64 simultaneous field patches; the HTTP test
runs ten synchronized price/inventory pairs and checks both final values after each pair.
All new checks must pass before advancing to Increment C; live Wolt remains unverified.
