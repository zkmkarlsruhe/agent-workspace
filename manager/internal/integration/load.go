// SPDX-License-Identifier: MIT

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Load reads exactly one JSON manifest. Unknown fields (including wrong case),
// duplicate keys, null contract fields and trailing documents are errors.
func Load(r io.Reader) (Manifest, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := strictJSON(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// LoadYAML accepts the same schema using the existing YAML dependency. Aliases,
// merge keys and custom tags are excluded so both formats have one meaning.
func LoadYAML(r io.Reader) (Manifest, error) {
	d := yaml.NewDecoder(r)
	var n yaml.Node
	if err := d.Decode(&n); err != nil {
		return Manifest{}, err
	}
	var extra yaml.Node
	if err := d.Decode(&extra); err != io.EOF {
		return Manifest{}, fmt.Errorf("manifest: expected one YAML document")
	}
	value, err := yamlValue(&n)
	if err != nil {
		return Manifest{}, err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return Manifest{}, err
	}
	return Load(bytes.NewReader(b))
}

func yamlValue(n *yaml.Node) (any, error) {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return nil, fmt.Errorf("manifest: YAML aliases/anchors are unsupported")
	}
	switch n.Tag {
	case "", "!!map", "!!seq", "!!str", "!!int", "!!float", "!!bool", "!!null":
	default:
		return nil, fmt.Errorf("manifest: unsupported YAML tag %q", n.Tag)
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return nil, fmt.Errorf("manifest: expected one YAML value")
		}
		return yamlValue(n.Content[0])
	case yaml.MappingNode:
		out := map[string]any{}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if _, seen := out[key.Value]; key.Tag != "!!str" || key.Anchor != "" || seen {
				return nil, fmt.Errorf("manifest: non-string, anchored or duplicate YAML key %q", key.Value)
			}
			value, err := yamlValue(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			out[key.Value] = value
		}
		return out, nil
	case yaml.SequenceNode:
		out := []any{}
		for _, child := range n.Content {
			value, err := yamlValue(child)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		return out, nil
	default:
		var value any
		if err := n.Decode(&value); err != nil {
			return nil, err
		}
		if f, ok := value.(float64); ok {
			// Keep YAML floats distinct from integers through the JSON decoder.
			s := strconv.FormatFloat(f, 'g', -1, 64)
			if !strings.ContainsAny(s, ".eE") {
				s += ".0"
			}
			value = json.Number(s)
		}
		return value, nil
	}
}

func strictJSON(b []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := jsonValue(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	if err := jsonShape(b, reflect.TypeOf(dst).Elem()); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}

// jsonValue checks duplicates before decoding could discard an earlier value.
func jsonValue(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return fmt.Errorf("duplicate or invalid JSON key %q", key)
			}
			seen[s] = true
		}
		if err := jsonValue(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

// encoding/json accepts case-insensitive field aliases and null scalars. The
// manifest contract accepts neither; raw settings schemas keep their own keys.
func jsonShape(b []byte, typ reflect.Type) error {
	if typ == reflect.TypeFor[json.RawMessage]() {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return fmt.Errorf("null contract field")
	}
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[MCP]() {
		return nil // its union decoder checks the remote object's shape
	}
	switch typ.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(b, &fields); err != nil {
			return err
		}
		known := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			known[strings.Split(f.Tag.Get("json"), ",")[0]] = f.Type
		}
		for key, value := range fields {
			ft, ok := known[key]
			if !ok {
				return fmt.Errorf("unknown field %q", key)
			}
			if err := jsonShape(value, ft); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(b, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := jsonShape(value, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
