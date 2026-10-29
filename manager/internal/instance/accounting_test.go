// SPDX-License-Identifier: MIT

package instance

import (
	"math"
	"testing"
)

func TestAccountingReserve(t *testing.T) {
	a := DefaultAccounting()
	a.Prices = map[string]TokenPrice{"standard": {Input: .6, Output: 3.6}, "fast": {Input: .1, Output: .3}}
	if a.ReserveEUR() != .3 {
		t.Fatal(a.ReserveEUR())
	}
	a.LargestContext = 524288
	if a.ReserveEUR() != .5 {
		t.Fatal(a.ReserveEUR())
	}
	a.Prices["thorough"] = TokenPrice{Input: 2, Output: 1}
	if a.ReserveEUR() != 1.2 {
		t.Fatal(a.ReserveEUR())
	}
	a.Prices = map[string]TokenPrice{}
	if a.ReserveEUR() != 0 {
		t.Fatal(a.ReserveEUR())
	}
}

func TestAccountingValidation(t *testing.T) {
	for name, edit := range map[string]func(*Accounting){
		"unit":     func(a *Accounting) { a.UpstreamCostCurrency = "GBP" },
		"nan rate": func(a *Accounting) { a.USDPerEUR = math.NaN() },
		"negative": func(a *Accounting) { a.ImageEUR = -1 },
		"infinite": func(a *Accounting) { a.EmbeddingEURPerMillion = math.Inf(1) },
		"role":     func(a *Accounting) { a.Prices = map[string]TokenPrice{"typo": {}} },
		"price":    func(a *Accounting) { a.Prices = map[string]TokenPrice{"fast": {Input: -1}} },
		"runtime price overflow": func(a *Accounting) {
			a.USDPerEUR = math.MaxFloat64
			a.Prices = map[string]TokenPrice{"standard": {Input: 2}}
		},
		"context": func(a *Accounting) { a.LargestContext = 0 },
		"output":  func(a *Accounting) { a.MaxOutputTokens = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			a := DefaultAccounting()
			edit(&a)
			if a.Validate() == nil {
				t.Fatal("invalid accounting accepted")
			}
		})
	}
}
