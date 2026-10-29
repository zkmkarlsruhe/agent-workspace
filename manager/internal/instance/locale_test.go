// SPDX-License-Identifier: MIT

package instance

import (
	"strings"
	"testing"
	"time"
)

func TestTimezoneValidation(t *testing.T) {
	d, err := Load("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if d.Timezone != "UTC" {
		t.Fatalf("minimal timezone = %q", d.Timezone)
	}
	for _, zone := range []string{"UTC", "America/New_York", "Europe/Berlin"} {
		d.Timezone = zone
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
		previous := time.Local
		restore := Select(d)
		if Location().String() != zone || time.Local != Location() || Text("[[timezone]]") != zone {
			t.Fatalf("timezone not selected: %s", zone)
		}
		restore()
		if time.Local != previous {
			t.Fatal("local timezone not restored")
		}
	}
	for _, zone := range []string{"", "Local", "not/a-zone", "../UTC"} {
		d.Timezone = zone
		if err := d.Validate(); err == nil {
			t.Fatalf("accepted timezone %q", zone)
		}
	}
}

func TestFolderPromptDefaults(t *testing.T) {
	for _, name := range []string{"minimal", "example"} {
		d, err := Load("../../../instance/" + name + "/instance.yaml")
		if err != nil {
			t.Fatal(err)
		}
		language, zone := "English", "UTC"
		if name == "example" {
			language, zone = "German", "Europe/Berlin"
		}
		if d.Timezone != zone {
			t.Fatalf("%s timezone = %s, want %s", name, d.Timezone, zone)
		}
		want := "Suggest a short " + language + " folder name for the following task: lower case, words separated by hyphens, at most four words. Answer with the name only."
		for _, lang := range []string{"en", "de"} {
			if got := d.CopyText("folder_name_prompt", lang); got != want {
				t.Fatalf("%s/%s prompt = %q", name, lang, got)
			}
		}
		delete(d.Copy, "folder_name_prompt")
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "folder_name_prompt") {
			t.Fatal("missing prompt accepted", err)
		}
	}
}
