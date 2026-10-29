// SPDX-License-Identifier: MIT

package instance

import (
	"strings"
	"testing"
)

func TestArchiveAfterDays(t *testing.T) {
	base := strings.Replace(string(builtin), "sessions:\n  archive_after_days: 30\n", "", 1)
	d, err := Parse([]byte(base))
	if err != nil || d.Sessions.ArchiveAfterDays != 0 {
		t.Fatalf("default: %+v %v", d, err)
	}
	for _, value := range []string{"0", "0.001", "1.5", "30", "106751", "0.000000001", "-1", ".nan", ".inf", "106752", "999999999999999999999", "off"} {
		t.Run(value, func(t *testing.T) {
			d, err := Parse([]byte(base + "\nsessions:\n  archive_after_days: " + value + "\n"))
			valid := value == "0" || value == "0.001" || value == "1.5" || value == "30" || value == "106751"
			if (err == nil) != valid {
				t.Fatalf("parsed %+v: %v", d, err)
			}
		})
	}
}
