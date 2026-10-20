package engine

import (
	"testing"

	"github.com/getmockd/mockd/pkg/config"
	"github.com/getmockd/mockd/pkg/stateful"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestConvertCustomOperation_PatchElements(t *testing.T) {
	var cfg config.CustomOperationConfig
	require.NoError(t, yaml.Unmarshal([]byte(`
name: Patch
steps:
  - type: patchElements
    resource: menus
    id: input.id
    value: input.data
    as: applied
    patchElements:
      paths: [menu.items]
      match: {sku: merchant_sku}
      fields: {inventory: integer}
      required: [inventory]
`), &cfg))
	op, err := convertCustomOperation(&cfg)
	require.NoError(t, err)
	require.Equal(t, stateful.StepPatchElements, op.Steps[0].Type)
	require.Equal(t, "merchant_sku", op.Steps[0].PatchElements.Match["sku"])
	require.Equal(t, []string{"inventory"}, op.Steps[0].PatchElements.Required)
	cfg.Consistency = "atomic"
	_, err = convertCustomOperation(&cfg)
	require.Error(t, err)
	cfg.Consistency = "best_effort"
	cfg.Steps[0].PatchElements.Fields["inventory"] = "unknown"
	_, err = convertCustomOperation(&cfg)
	require.Error(t, err)
}
