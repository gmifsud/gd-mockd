package stateful

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/getmockd/mockd/pkg/config"
	"github.com/stretchr/testify/require"
)

func patchPolicy() *config.PatchElementsConfig {
	return &config.PatchElementsConfig{
		Paths: []string{"menu.items", "menu.categories.*.items"},
		Match: map[string]string{"sku": "merchant_sku", "external_id": "external_data.external_id", "gtin": "gtin_barcode"},
		Fields: map[string]string{"price": "integer", "inventory": "integer", "enabled": "boolean", "vat_percentage": "number", "discounted_price": "integer"},
		Requires: map[string]string{"discounted_price": "price"},
	}
}

func patchJSON(t *testing.T, text string) interface{} {
	t.Helper()
	var value interface{}
	require.NoError(t, json.Unmarshal([]byte(text), &value))
	return value
}

func patchStore(t *testing.T) (*StateStore, *StatefulResource) {
	t.Helper()
	store := NewStateStore()
	seed := patchJSON(t, `{"id":"a","menu":{"items":[{"merchant_sku":"000123","external_data":{"external_id":"ext"},"gtin_barcode":"0123","price":100,"inventory":5,"enabled":true}]}}`).(map[string]interface{})
	require.NoError(t, store.Register("", &ResourceConfig{Name: "menus", SeedData: []map[string]interface{}{seed}}))
	return store, store.Get("", "menus")
}

func firstPatchItem(r *StatefulResource) map[string]interface{} {
	return r.Get("a").Data["menu"].(map[string]interface{})["items"].([]interface{})[0].(map[string]interface{})
}

func TestPatchElements_EffectsIdentifiersAndImmutableSnapshot(t *testing.T) {
	_, resource := patchStore(t)
	before := resource.Get("a")
	result, err := resource.PatchElements("a", patchJSON(t, `[{"sku":"000123","price":250,"enabled":false},{"gtin":"0123","inventory":0},{"external_id":"ext","vat_percentage":13.5}]`), patchPolicy())
	require.NoError(t, err)
	require.Equal(t, 3, result["matched"])
	require.Equal(t, 0, result["unmatched"])
	item := firstPatchItem(resource)
	require.Equal(t, float64(250), item["price"])
	require.Equal(t, float64(0), item["inventory"])
	require.Equal(t, false, item["enabled"])
	require.Equal(t, 13.5, item["vat_percentage"])
	require.Equal(t, "000123", item["merchant_sku"])
	old := before.Data["menu"].(map[string]interface{})["items"].([]interface{})[0].(map[string]interface{})
	require.Equal(t, float64(100), old["price"], "old readers must not observe mutation")
}

func TestPatchElements_InvalidBatchNeverPublishesPartialChanges(t *testing.T) {
	for _, row := range []string{
		`{"sku":"000123","price":1.5}`, `{"sku":123,"price":2}`,
		`{"sku":"000123","inventory":null}`, `{"sku":"000123","enabled":"false"}`,
		`{"sku":"000123","discounted_price":50}`, `{"sku":"000123","price":"200"}`,
		`{"sku":"000123","external_id":"ext","price":2}`, `{"price":2}`,
		`{"sku":"000123","unexpected":2}`, `{"sku":"000123","price":true}`, `null`, `[]`,
	} {
		t.Run(row, func(t *testing.T) {
			_, resource := patchStore(t)
			before := resource.Get("a")
			_, err := resource.PatchElements("a", patchJSON(t, `[{"sku":"000123","price":999},`+row+`]`), patchPolicy())
			var validation *ValidationError
			require.ErrorAs(t, err, &validation)
			require.Same(t, before, resource.Get("a"))
		})
	}
}

func TestPatchElements_RequiredAndUnknownIdentifiers(t *testing.T) {
	_, resource := patchStore(t)
	policy := patchPolicy()
	policy.Required = []string{"inventory"}
	_, err := resource.PatchElements("a", patchJSON(t, `[{"sku":"000123"}]`), policy)
	require.Error(t, err)
	result, err := resource.PatchElements("a", patchJSON(t, `[{"sku":"missing","inventory":4},{"sku":"000123","inventory":8}]`), policy)
	require.NoError(t, err)
	require.Equal(t, 1, result["matched"])
	require.Equal(t, 1, result["unmatched"])
	rows := result["results"].([]interface{})
	require.Equal(t, "unmatched", rows[0].(map[string]interface{})["outcome"])
	require.Equal(t, float64(100), firstPatchItem(resource)["price"])
	require.Equal(t, float64(8), firstPatchItem(resource)["inventory"])
}

func TestPatchElements_EmptyMalformedAndNestedTargets(t *testing.T) {
	_, resource := patchStore(t)
	before := resource.Get("a")
	for _, text := range []string{`null`, `[]`, `{}`, `"items"`} {
		_, err := resource.PatchElements("a", patchJSON(t, text), patchPolicy())
		require.Error(t, err)
		require.Same(t, before, resource.Get("a"))
	}
	_, err := resource.Update("a", patchJSON(t, `{"menu":{"categories":[{"items":[{"merchant_sku":"000123","price":100,"inventory":5}]}]}}`).(map[string]interface{}))
	require.NoError(t, err)
	result, err := resource.PatchElements("a", patchJSON(t, `[{"sku":"000123","price":201},{"sku":"000123","price":202}]`), patchPolicy())
	require.NoError(t, err)
	require.Equal(t, 2, result["matched"])
	menu := resource.Get("a").Data["menu"].(map[string]interface{})
	category := menu["categories"].([]interface{})[0].(map[string]interface{})
	item := category["items"].([]interface{})[0].(map[string]interface{})
	require.Equal(t, float64(202), item["price"])
	require.Equal(t, float64(5), item["inventory"])
	_, err = resource.Update("a", map[string]interface{}{"menu": map[string]interface{}{"items": "malformed"}})
	require.NoError(t, err)
	before = resource.Get("a")
	_, err = resource.PatchElements("a", patchJSON(t, `[{"sku":"000123","price":202}]`), patchPolicy())
	require.Error(t, err)
	require.Same(t, before, resource.Get("a"))
}

func TestPatchElements_ConcurrentIndependentFieldsAndVenueIsolation(t *testing.T) {
	store, resource := patchStore(t)
	var rows []interface{}
	for i := 0; i < 32; i++ {
		rows = append(rows, map[string]interface{}{"merchant_sku": fmt.Sprint(i), "price": float64(0), "inventory": float64(0)})
	}
	_, err := resource.Update("a", map[string]interface{}{"menu": map[string]interface{}{"items": rows}})
	require.NoError(t, err)
	require.NoError(t, store.Register("other-workspace", &ResourceConfig{Name: "menus", SeedData: []map[string]interface{}{{"id": "a", "menu": map[string]interface{}{"items": rows}}}}))
	other := store.Get("other-workspace", "menus").Get("a")
	var wg sync.WaitGroup
	errors := make(chan error, 64)
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		for _, field := range []string{"price", "inventory"} {
			wg.Add(1)
			go func(i int, field string) {
				defer wg.Done()
				<-start
				_, err := resource.PatchElements("a", []interface{}{map[string]interface{}{"sku": fmt.Sprint(i), field: float64(i + 1)}}, patchPolicy())
				errors <- err
			}(i, field)
		}
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	items := resource.Get("a").Data["menu"].(map[string]interface{})["items"].([]interface{})
	for i, row := range items {
		item := row.(map[string]interface{})
		require.Equal(t, float64(i+1), item["price"])
		require.Equal(t, float64(i+1), item["inventory"])
	}
	require.Same(t, other, store.Get("other-workspace", "menus").Get("a"))
	require.Equal(t, float64(0), rows[0].(map[string]interface{})["price"])
}

func TestPatchElements_ExecutorAndRollbackPreflight(t *testing.T) {
	store, resource := patchStore(t)
	executor := NewOperationExecutor(store)
	op := &CustomOperation{Name: "patch", Steps: []Step{{Type: StepPatchElements, Resource: "menus", ID: "input.id", Value: "input.data", As: "applied", PatchElements: patchPolicy()}}, Response: map[string]string{"matched": "applied.matched"}}
	req := &OperationRequest{Data: map[string]interface{}{"id": "a", "data": patchJSON(t, `[{"sku":"000123","price":222}]`)}}
	result := executor.Execute(context.Background(), op, req)
	require.Nil(t, result.Error)
	require.Equal(t, 1, result.Item.Data["matched"])
	before := resource.Get("a")
	op.Consistency = ConsistencyAtomic
	result = executor.Execute(context.Background(), op, req)
	require.NotNil(t, result.Error, "rollback of a committed snapshot could erase concurrent changes")
	require.Same(t, before, resource.Get("a"))
}
