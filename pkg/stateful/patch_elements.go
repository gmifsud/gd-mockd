package stateful

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/getmockd/mockd/pkg/config"
)

func (e *OperationExecutor) stepPatchElements(step Step, exprCtx map[string]interface{}, workspaceID string) error {
	id, err := e.evalExpr(step.ID, exprCtx)
	if err != nil {
		return fmt.Errorf("id expression failed: %w", err)
	}
	value, err := e.evalExpr(step.Value, exprCtx)
	if err != nil {
		return fmt.Errorf("patch expression failed: %w", err)
	}
	resource := e.store.Get(workspaceID, step.Resource)
	if resource == nil {
		return &NotFoundError{Resource: step.Resource}
	}
	outcome, err := resource.PatchElements(fmt.Sprint(id), value, step.PatchElements)
	if err == nil {
		exprCtx[step.As] = outcome
	}
	return err
}

// PatchElements validates the complete batch before publishing a copy of the
// latest document under the resource lock. Existing readers retain an immutable
// snapshot; disjoint concurrent field patches cannot replace each other's work.
// Unknown identifiers are reported without creating elements. Repeated updates
// to the same element are applied in request order (last supplied field wins).
func (r *StatefulResource) PatchElements(id string, value interface{}, cfg *config.PatchElementsConfig) (map[string]interface{}, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	batch, ok := value.([]interface{})
	if !ok || len(batch) == 0 {
		return nil, &ValidationError{Field: "data", Message: "must be a non-empty array"}
	}
	type patch struct {
		key, identifier string
		fields          map[string]interface{}
	}
	patches := make([]patch, len(batch))
	for i, raw := range batch {
		invalid := func(message string) (map[string]interface{}, error) {
			return nil, &ValidationError{Field: fmt.Sprintf("data[%d]", i), Message: message}
		}
		row, ok := raw.(map[string]interface{})
		if !ok {
			return invalid("must be an object")
		}
		p := patch{fields: map[string]interface{}{}}
		identifiers := 0
		for field, v := range row {
			if _, match := cfg.Match[field]; match {
				if v == nil {
					continue
				}
				identifier, ok := v.(string)
				if !ok || identifier == "" {
					return invalid("identifiers must be non-empty strings")
				}
				identifiers++
				p.key, p.identifier = field, identifier
				continue
			}
			kind, allowed := cfg.Fields[field]
			if !allowed || !validPatchScalar(v, kind) {
				return invalid("unsupported field or invalid value: " + field)
			}
			p.fields[field] = v
		}
		if identifiers != 1 {
			return invalid("exactly one identifier is required")
		}
		for _, required := range cfg.Required {
			if _, present := p.fields[required]; !present {
				return invalid("missing required field: " + required)
			}
		}
		for field, dependency := range cfg.Requires {
			if _, present := p.fields[field]; present {
				if _, provided := p.fields[dependency]; !provided {
					return invalid(field + " requires " + dependency)
				}
			}
		}
		patches[i] = p
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	existing := r.items[id]
	if existing == nil {
		return nil, &NotFoundError{Resource: r.name, ID: id}
	}
	// Copy containers without round-tripping numeric fields through float64.
	// Unlike a shallow map copy, this prevents mutations leaking to old readers.
	data := jsonCompatible(existing.Data).(map[string]interface{})
	var elements []map[string]interface{}
	for _, path := range cfg.Paths {
		if err := collectPatchElements(data, strings.Split(path, "."), &elements); err != nil {
			return nil, err
		}
	}
	results := make([]interface{}, 0, len(patches))
	matched, unmatched := 0, 0
	for _, p := range patches {
		count := 0
		for _, element := range elements {
			if identifier, ok := patchValueAt(element, cfg.Match[p.key]).(string); ok && identifier == p.identifier {
				for key, v := range p.fields {
					element[key] = v
				}
				count++
			}
		}
		outcome := "matched"
		if count == 0 {
			outcome = "unmatched"
			unmatched++
		} else {
			matched++
		}
		results = append(results, map[string]interface{}{"identifier": p.key, "value": p.identifier, "outcome": outcome, "elements": count})
	}
	if matched > 0 {
		r.items[id] = &ResourceItem{ID: existing.ID, CreatedAt: existing.CreatedAt, UpdatedAt: time.Now(), Data: data}
	}
	return map[string]interface{}{"matched": matched, "unmatched": unmatched, "results": results}, nil
}

func validPatchScalar(value interface{}, kind string) bool {
	switch kind {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer", "number":
		var n float64
		switch v := value.(type) {
		case float64:
			n = v
		case int:
			n = float64(v)
		case int64:
			n = float64(v)
		case json.Number:
			var err error
			n, err = v.Float64()
			if err != nil {
				return false
			}
		default:
			return false
		}
		return !math.IsNaN(n) && !math.IsInf(n, 0) && (kind == "number" || math.Trunc(n) == n)
	}
	return false
}

func patchValueAt(value interface{}, path string) interface{} {
	for _, key := range strings.Split(path, ".") {
		m, ok := value.(map[string]interface{})
		if !ok {
			return nil
		}
		value = m[key]
	}
	return value
}

func collectPatchElements(value interface{}, path []string, out *[]map[string]interface{}) error {
	if value == nil {
		return nil // An optional array may be absent.
	}
	if len(path) == 0 {
		rows, ok := value.([]interface{})
		if !ok {
			return fmt.Errorf("patchElements target must be an array")
		}
		for _, row := range rows {
			m, ok := row.(map[string]interface{})
			if !ok {
				return fmt.Errorf("patchElements target entries must be objects")
			}
			*out = append(*out, m)
		}
		return nil
	}
	if path[0] == "*" {
		rows, ok := value.([]interface{})
		if !ok {
			return fmt.Errorf("patchElements wildcard requires an array")
		}
		for _, row := range rows {
			if err := collectPatchElements(row, path[1:], out); err != nil {
				return err
			}
		}
		return nil
	}
	m, ok := value.(map[string]interface{})
	if !ok {
		return fmt.Errorf("patchElements path requires an object")
	}
	return collectPatchElements(m[path[0]], path[1:], out)
}
