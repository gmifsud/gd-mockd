package config

import (
	"errors"
	"fmt"
	"strings"
)

// PatchElementsConfig describes a bounded array patch within one document.
// Paths select arrays using dotted object keys and '*' for array traversal.
// Match maps request identifier keys to dotted paths on stored elements.
type PatchElementsConfig struct {
	Paths    []string          `json:"paths" yaml:"paths"`
	Match    map[string]string `json:"match" yaml:"match"`
	Fields   map[string]string `json:"fields" yaml:"fields"`
	Required []string          `json:"required,omitempty" yaml:"required,omitempty"`
	Requires map[string]string `json:"requires,omitempty" yaml:"requires,omitempty"`
}

func (p *PatchElementsConfig) Validate() error {
	if p == nil || len(p.Paths) == 0 || len(p.Match) == 0 || len(p.Fields) == 0 {
		return errors.New("patchElements requires paths, match and fields")
	}
	validPath := func(path string, wildcard bool) bool {
		for _, segment := range strings.Split(path, ".") {
			if segment == "" || (strings.Contains(segment, "*") && (!wildcard || segment != "*")) {
				return false
			}
		}
		return true
	}
	for _, path := range p.Paths {
		if !validPath(path, true) {
			return fmt.Errorf("invalid patchElements array path %q", path)
		}
	}
	for key, path := range p.Match {
		if key == "" || !validPath(path, false) {
			return fmt.Errorf("invalid patchElements identifier %q", key)
		}
		if _, exists := p.Fields[key]; exists {
			return fmt.Errorf("patchElements cannot modify identifier %q", key)
		}
	}
	for field, kind := range p.Fields {
		if field == "" || strings.ContainsAny(field, ".*") {
			return fmt.Errorf("invalid patchElements field %q", field)
		}
		switch kind {
		case "integer", "number", "boolean", "string":
		default:
			return fmt.Errorf("unsupported patchElements field type %q", kind)
		}
		for _, path := range p.Match {
			if strings.Split(path, ".")[0] == field {
				return fmt.Errorf("patchElements field %q would overwrite a stored identifier", field)
			}
		}
	}
	for _, field := range p.Required {
		if _, ok := p.Fields[field]; !ok {
			return fmt.Errorf("patchElements required field %q is not declared", field)
		}
	}
	for field, dependency := range p.Requires {
		if p.Fields[field] == "" || p.Fields[dependency] == "" {
			return errors.New("patchElements requires entries must name declared fields")
		}
	}
	return nil
}
