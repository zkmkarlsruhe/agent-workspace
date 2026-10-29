// SPDX-License-Identifier: MIT

package instance

import (
	"fmt"
	"regexp"

	"go.yaml.in/yaml/v3"
)

// CapabilityDefault accepts all, none, or an explicit list of catalog IDs.
type CapabilityDefault struct {
	Mode string
	IDs  []string
}

func (d *CapabilityDefault) UnmarshalYAML(n *yaml.Node) error {
	*d = CapabilityDefault{}
	if n.Kind == yaml.SequenceNode {
		return n.Decode(&d.IDs)
	}
	if n.Kind != yaml.ScalarNode || (n.Value != "all" && n.Value != "none") {
		return fmt.Errorf("capabilities.project_default must be all, none, or a list of catalog IDs")
	}
	d.Mode = n.Value
	return nil
}

type Capabilities struct {
	Projects       bool              `yaml:"projects"`
	ProjectDefault CapabilityDefault `yaml:"project_default"`
	LegacyDefault  string            `yaml:"legacy_default"`
}

var capabilityID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func (c Capabilities) Validate() error {
	if c.LegacyDefault != "global" && c.LegacyDefault != "none" {
		return fmt.Errorf("capabilities.legacy_default must be global or none")
	}
	if c.ProjectDefault.Mode != "" && c.ProjectDefault.Mode != "all" && c.ProjectDefault.Mode != "none" {
		return fmt.Errorf("invalid capabilities.project_default")
	}
	seen := map[string]bool{}
	for _, id := range c.ProjectDefault.IDs {
		if !capabilityID.MatchString(id) || seen[id] {
			return fmt.Errorf("invalid or duplicate capabilities.project_default ID %q", id)
		}
		seen[id] = true
	}
	return nil
}
