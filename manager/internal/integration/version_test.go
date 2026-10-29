// SPDX-License-Identifier: MIT

package integration

import "testing"

func TestVersionRequirements(t *testing.T) {
	for _, tt := range []struct {
		version, requirement string
		want, invalid        bool
	}{
		{"1.4.0", ">=1.4 <2", true, false},
		{"2.0.0", ">=1.4 <2", false, false},
		{"1.3.9", ">=1.4 <2", false, false},
		{"2.2.0", ">=2.2.0 <2.3", true, false},
		{"2.2.1", "=2.2.0", false, false},
		{"2.2.0", "2.2", true, false},
		{"2.2.0", "<=2.2", true, false},
		{"2.2.1", "<=2.2", false, false},
		{"2.2.0", ">2.2", false, false},
		{"2.2.1", ">2.2", true, false},
		{"2.2.0", "", true, false},
		{"2026.10.27.1", ">=2026.10.27 <2026.11", true, false},
		{"1.2.3", ">=2 <1", false, false},
		{"2.2", "", false, true},
		{"2.2.0-beta", "", false, true},
		{"v2.2.0", "", false, true},
		{"2.2.0", " ", false, true},
		{"2.2.0", ">= 2.2", false, true},
		{"2.2.0", "^2.2", false, true},
		{"2.2.0", "~2.2", false, true},
		{"2.2.0", "2.*", false, true},
		{"2.2.0", ">=2 || <3", false, true},
		{"2.2.0", "!=2.3", false, true},
		{"2.2.0", "==2.2", false, true},
		{"2.2.0", ">=2, <3", false, true},
		{"2.2.0", ">=02.2", false, true},
		{"2.2.0", ">=2.-1", false, true},
		{"2.2.0", ">=2.2.0.0.0", false, true},
		{"2.2.0", ">=18446744073709551616", false, true},
		{"2.2.0", "<1 >=broken", false, true},
	} {
		t.Run(tt.version+"/"+tt.requirement, func(t *testing.T) {
			got, err := satisfies(tt.version, tt.requirement)
			if got != tt.want || (err != nil) != tt.invalid {
				t.Fatalf("satisfies = %v, %v; want %v, invalid %v", got, err, tt.want, tt.invalid)
			}
		})
	}
}
