// SPDX-License-Identifier: MIT

package co2

import (
	"math"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// docs/adr/README.md B.2.4: 1.2 M input (80 % cached), 30 k output on reference model
// gives 16–68 Wh and 6–23 g there (PUE 1 on input); here PUE 1.2 on input too.
func TestWorkedExample(t *testing.T) {
	whLo, whHi, gLo, gHi, ok := Estimate("default", 1_200_000, 960_000, 30_000)
	if !ok || whLo < 15 || whLo > 19 || whHi < 65 || whHi > 75 || gLo < 5 || gLo > 7 || gHi < 22 || gHi > 27 {
		t.Fatalf("%.1f–%.1f Wh, %.1f–%.1f g", whLo, whHi, gLo, gHi)
	}
	_, _, sLo, sHi, _ := Estimate("thorough", 1_200_000, 960_000, 30_000)
	if math.Abs(sLo-gLo/2) > 1e-9 || math.Abs(sHi-gHi*2) > 1e-9 {
		t.Fatalf("strong %.1f–%.1f", sLo, sHi)
	}
	if _, _, _, _, ok := Estimate("workspace/unlisted-max", 1, 0, 1); ok {
		t.Fatal("unknown alias must not be estimated")
	}
}

func TestAliasIsolation(t *testing.T) {
	for _, mode := range []string{"distinct", "shared-conflict", "shared-missing", "shared-identical", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			d, err := instance.Load("../../../instance/example/instance.yaml")
			if err != nil {
				t.Fatal(err)
			}
			primary := d.Models.Default
			switch mode {
			case "distinct":
				delete(d.Footprint, "small")
			case "shared-conflict":
				d.Models.Small = primary
			case "shared-missing":
				d.Models.Small = primary
				delete(d.Footprint, "small")
			case "shared-identical":
				d.Models.Small = primary
				d.Footprint["small"] = d.Footprint["default"]
			case "disabled":
				d.Footprint = nil
			}
			defer instance.Select(d)()
			_, _, _, _, ok := Estimate(primary, 10, 0, 2)
			want := mode == "distinct" || mode == "shared-identical"
			if ok != want {
				t.Fatalf("primary known=%v want=%v", ok, want)
			}
			if mode == "distinct" {
				if _, _, _, _, ok := Estimate(d.Models.Small, 10, 0, 2); ok {
					t.Fatal("missing alias inherited primary coefficients")
				}
			}
			if _, _, _, _, ok := Estimate("unlisted-fallback", 10, 0, 2); ok {
				t.Fatal("fallback inherited primary coefficients")
			}
		})
	}
}

func TestConfiguredAssumptions(t *testing.T) {
	d, err := instance.Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d.FootprintAssumptions.PUE = 2
	d.FootprintAssumptions.GridGPerKWh = 100
	defer instance.Select(d)()
	m := Models()[d.Models.Default]
	lo, hi, glo, ghi, ok := Estimate(d.Models.Default, 100, 20, 10)
	wantHi := (m.EOut*10 + 2*m.EIn*100) / 3600 * m.HiF
	wantLo := (m.EOut/4*10 + 2*m.EIn*80) / 3600 * m.LoF
	if !ok || math.Abs(hi-wantHi) > 1e-12 || math.Abs(lo-wantLo) > 1e-12 || math.Abs(ghi-hi/10) > 1e-12 || math.Abs(glo-lo/10) > 1e-12 {
		t.Fatalf("%v %v %v %v", lo, hi, glo, ghi)
	}
}
