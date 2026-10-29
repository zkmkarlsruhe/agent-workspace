// SPDX-License-Identifier: MIT

// Package co2 estimates energy and CO2 of one LLM call (docs/adr/README.md B.2.2,
// docs/adr/README.md §3.9). The gateway stores the result per event together with
// Version; old rows are never recomputed.
package co2

import "github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"

// Version is stored in usage_events.method_version. New coefficients mean a
// new version.
const Version = "2026-10-a"

const (
	PUE    = 1.2   // default assumption; runtime uses instance footprint_assumptions
	GridDE = 344.0 // default assumption in g CO2/kWh; retained for callers
)

// Model holds the per-token coefficients of docs/adr/README.md B.2.3. EOut includes
// PUE 1.2 (EcoLogits), EIn does not.
type Model struct {
	Alias, Name, Provider string
	EOut, EIn             float64 // J per output / input token
	LoF, HiF              float64 // extra factors on the range, 1 when coefficients are known
	Rough                 bool    // "sehr grobe Schätzung"
}

// Models resolves footprint metadata through the same model roles as runtime config.
// Conflicting or missing metadata on a shared alias is unknown; role order must not choose its footprint.
func Models() map[string]Model {
	d := instance.Current()
	out := make(map[string]Model, len(d.Footprint))
	unknown := map[string]bool{}
	for _, role := range []string{"default", "small", "thorough"} {
		alias := map[string]string{"default": d.Models.Default, "small": d.Models.Small, "thorough": d.Models.Thorough}[role]
		m, ok := d.Footprint[role]
		model := Model{Alias: alias, Name: m.Name, Provider: m.Provider, EOut: m.EOut, EIn: m.EIn, LoF: m.LoF, HiF: m.HiF, Rough: m.Rough}
		previous, exists := out[alias]
		if !ok || unknown[alias] || (exists && previous != model) {
			delete(out, alias)
			unknown[alias] = true
			continue
		}
		out[alias] = model
	}
	return out
}

// Estimate returns the energy range in Wh and the CO2 range in g. in and
// cached are prompt tokens (cached is part of in); out is completion tokens,
// which already contain reasoning tokens in OpenAI usage. ok is false for an
// unknown alias.
func Estimate(alias string, in, cached, out int64) (whLo, whHi, gLo, gHi float64, ok bool) {
	m, ok := Models()[alias]
	if !ok {
		return 0, 0, 0, 0, false
	}
	a := instance.Current().FootprintAssumptions
	o, i, c := float64(out), float64(in), float64(min(cached, in))
	// EOut already carries PUE; PUE × (EOut/PUE × out) = EOut × out
	jHi := m.EOut*o + a.PUE*m.EIn*i
	jLo := m.EOut/4*o + a.PUE*m.EIn*(i-c)
	whLo, whHi = jLo/3600*m.LoF, jHi/3600*m.HiF
	return whLo, whHi, whLo / 1000 * a.GridGPerKWh, whHi / 1000 * a.GridGPerKWh, true
}
