// SPDX-License-Identifier: MIT

package instance

import (
	"fmt"
	"math"
)

// TokenPrice is an operator estimate in EUR per million tokens, not provider billing.
type TokenPrice struct {
	Input  float64 `yaml:"input"`
	Output float64 `yaml:"output"`
}

type Accounting struct {
	UpstreamCostCurrency   string                `yaml:"upstream_cost_currency"`
	USDPerEUR              float64               `yaml:"usd_per_eur"`
	Prices                 map[string]TokenPrice `yaml:"prices"`
	LargestContext         int64                 `yaml:"largest_context"`
	MaxOutputTokens        int                   `yaml:"max_output_tokens"`
	EmbeddingEURPerMillion float64               `yaml:"embedding_eur_per_million"`
	ImageEUR               float64               `yaml:"image_eur"`
	MCPEUR                 float64               `yaml:"mcp_eur"`
	ToolboxEUR             float64               `yaml:"toolbox_eur"`
	SearchEUR              float64               `yaml:"search_eur"`
	FetchEUR               float64               `yaml:"fetch_eur"`
}

func DefaultAccounting() Accounting {
	return Accounting{UpstreamCostCurrency: "none", LargestContext: 262144, MaxOutputTokens: 32768,
		ImageEUR: 0.10, MCPEUR: 0.01, ToolboxEUR: 0.02, SearchEUR: 0.005, FetchEUR: 0.002}
}

// Dearest uses the highest input and output prices independently so mixed prices remain conservative.
func (a Accounting) Dearest() TokenPrice {
	var p TokenPrice
	for _, v := range a.Prices {
		p.Input, p.Output = max(p.Input, v.Input), max(p.Output, v.Output)
	}
	return p
}

// ReserveEUR rounds up to a tenth of a euro; an empty price map disables chat estimates and holds.
func (a Accounting) ReserveEUR() float64 {
	p := a.Dearest()
	return math.Ceil((float64(a.LargestContext)*p.Input+float64(a.MaxOutputTokens)*p.Output)/1e5) / 10
}

func (a Accounting) Validate() error {
	if a.UpstreamCostCurrency != "none" && a.UpstreamCostCurrency != "EUR" && a.UpstreamCostCurrency != "USD" {
		return fmt.Errorf("accounting.upstream_cost_currency must be EUR, USD or none")
	}
	if a.LargestContext <= 0 || a.MaxOutputTokens <= 0 {
		return fmt.Errorf("accounting token limits must be positive")
	}
	values := []float64{a.USDPerEUR, a.EmbeddingEURPerMillion, a.ImageEUR, a.MCPEUR, a.ToolboxEUR, a.SearchEUR, a.FetchEUR}
	for role, p := range a.Prices {
		if role != "standard" && role != "fast" && role != "thorough" {
			return fmt.Errorf("accounting: unknown price role %s", role)
		}
		values = append(values, p.Input, p.Output, p.Input*a.USDPerEUR, p.Output*a.USDPerEUR)
	}
	for _, v := range values {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("accounting prices and rate must be finite and non-negative")
		}
	}
	if math.IsInf(a.ReserveEUR(), 0) {
		return fmt.Errorf("accounting reserve overflows")
	}
	return nil
}
