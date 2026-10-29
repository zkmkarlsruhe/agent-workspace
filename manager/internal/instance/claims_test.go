// SPDX-License-Identifier: MIT

package instance

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestInstancePricingCopy(t *testing.T) {
	for _, name := range []string{"minimal", "example"} {
		d, err := Load("../../../instance/" + name + "/instance.yaml")
		if err != nil {
			t.Fatal(err)
		}
		assets := d.Render()
		for _, file := range []string{"commands/help.md", "opencode.jsonc"} {
			text := string(assets[file])
			if name == "minimal" && strings.Contains(text, "1 EUR = 1.10 USD") {
				t.Errorf("%s %s: exchange rate claim", name, file)
			}
			if name == "minimal" && !strings.Contains(text, "Prices are operator-configured estimates.") {
				t.Errorf("%s: missing neutral prices", file)
			}
			if strings.Contains(text, "[[") {
				t.Errorf("%s: unresolved copy token", file)
			}
		}
		for _, rate := range []float64{0, 1.25, 1.234567} {
			d.Accounting.USDPerEUR = rate
			d.Accounting.UpstreamCostCurrency = "USD"
			if rate == 0 {
				d.Accounting.UpstreamCostCurrency = "none"
			}
			d.Accounting.Prices = map[string]TokenPrice{"standard": {Input: 2, Output: 4}, "fast": {}}
			if err := d.Validate(); err != nil {
				t.Fatal(err)
			}
			rendered := d.Render()
			for _, file := range []string{"commands/help.md", "opencode.jsonc"} {
				text := string(rendered[file])
				for _, want := range []string{"Upstream cost currency: " + d.Accounting.UpstreamCostCurrency,
					"Conversion rate: " + strconv.FormatFloat(rate, 'f', -1, 64) + " USD per EUR", "standard 2/4; fast 0/0"} {
					if name == "minimal" && !strings.Contains(text, want) {
						t.Errorf("%s %s: missing %q", name, file, want)
					}
				}
			}
			for _, lang := range []string{"en", "de"} {
				text := d.CopyText("pricing_method", lang)
				if !strings.Contains(text, strconv.FormatFloat(rate, 'f', -1, 64)) || !strings.Contains(text, "standard 2/4") {
					t.Errorf("%s: stale localized pricing: %s", lang, text)
				}
			}
			// The committed JSONC artifacts use whole-line comments only.
			var lines []string
			for _, line := range strings.Split(string(rendered["opencode.jsonc"]), "\n") {
				if !strings.HasPrefix(strings.TrimSpace(line), "//") {
					lines = append(lines, line)
				}
			}
			var cfg struct {
				Providers map[string]struct {
					Models map[string]struct{ Cost *TokenPrice }
				}
			}
			if err := json.Unmarshal([]byte(strings.Join(lines, "\n")), &cfg); err != nil {
				t.Fatal(err)
			}
			models := cfg.Providers["litellm"].Models
			for _, role := range []string{"standard", "fast", "thorough"} {
				price, exists := d.Accounting.Prices[role]
				cost := models[role].Cost
				if !exists || rate == 0 {
					if cost != nil {
						t.Errorf("%s %s: cost without rate/price", name, role)
					}
				} else if cost == nil || cost.Input != price.Input*rate || cost.Output != price.Output*rate {
					t.Errorf("%s %s: wrong USD cost: %+v", name, role, cost)
				}
			}
		}
		d.Copy["pricing"] = Localized{EN: "Local price policy for [[name]].", DE: "Lokale Preise für [[name]]."}
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(d.Render()["commands/help.md"]), "Local price policy for "+d.Values["name"]+".") {
			t.Fatal("command ignored configured copy")
		}
	}
}

func TestWorkspaceRuntimePricing(t *testing.T) {
	d, err := Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	assets := d.Render()
	for _, want := range []string{
		`"cost": { "input": 0.66, "output": 3.96 }`,
		`"cost": { "input": 0.11, "output": 0.33 }`,
		`"cost": { "input": 0.0154, "output": 1.254 }`,
	} {
		if !strings.Contains(string(assets["opencode.jsonc"]), want) {
			t.Errorf("missing configured USD price: %s", want)
		}
	}
	if !strings.Contains(string(assets["commands/help.md"]), "1 EUR = 1.10 USD") {
		t.Fatal("missing instance exchange-rate sentence")
	}
	if !strings.Contains(d.CopyText("pricing", "de"), "1 EUR = 1,10 USD") {
		t.Fatal("missing German instance exchange-rate sentence")
	}
}

func TestEstimateValidation(t *testing.T) {
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		d, err := Load("../../../instance/minimal/instance.yaml")
		if err != nil {
			t.Fatal(err)
		}
		d.FootprintAssumptions.GridGPerKWh = bad
		if d.Validate() == nil {
			t.Errorf("accepted grid %v", bad)
		}
	}
	d, _ := Load("../../../instance/minimal/instance.yaml")
	d.FootprintAssumptions.PUE = 0.9
	if d.Validate() == nil {
		t.Fatal("accepted PUE below one")
	}
	d, _ = Load("../../../instance/minimal/instance.yaml")
	d.Copy["pricing"] = Localized{EN: "[[copy.providers.en]]", DE: "Text"}
	if d.Validate() == nil {
		t.Fatal("accepted recursive copy")
	}
}
