// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"strconv"
	"strings"
)

// Three numeric components cover releases; four also cover the repository's
// CalVer. Range bounds may omit trailing components, which then mean zero.
func parseVersion(s string, bound bool) ([4]uint64, error) {
	var out [4]uint64
	parts := strings.Split(s, ".")
	if len(parts) > 4 || (!bound && len(parts) < 3) {
		return out, fmt.Errorf("invalid version %q", s)
	}
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') || strings.IndexFunc(p, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return out, fmt.Errorf("invalid version %q", s)
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return out, err
		}
		out[i] = n
	}
	return out, nil
}

func satisfies(version, requirement string) (bool, error) {
	v, err := parseVersion(version, false)
	if err != nil {
		return false, err
	}
	if requirement != "" && strings.TrimSpace(requirement) == "" {
		return false, fmt.Errorf("empty version requirement")
	}
	matched := true
	for _, term := range strings.Fields(requirement) {
		op := "="
		for _, prefix := range []string{">=", "<=", ">", "<", "="} {
			if strings.HasPrefix(term, prefix) {
				op, term = prefix, strings.TrimPrefix(term, prefix)
				break
			}
		}
		want, err := parseVersion(term, true)
		if err != nil {
			return false, err
		}
		cmp := 0
		for i := range v {
			if v[i] < want[i] {
				cmp = -1
				break
			}
			if v[i] > want[i] {
				cmp = 1
				break
			}
		}
		switch op {
		case "=":
			matched = matched && cmp == 0
		case ">":
			matched = matched && cmp > 0
		case ">=":
			matched = matched && cmp >= 0
		case "<":
			matched = matched && cmp < 0
		case "<=":
			matched = matched && cmp <= 0
		}
	}
	return matched, nil
}
