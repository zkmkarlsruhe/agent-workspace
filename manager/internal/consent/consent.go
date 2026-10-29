// SPDX-License-Identifier: MIT

// Package consent renders the selected instance's versioned notice.
package consent

import "github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"

type Notice = instance.Notice

// Version is persisted on acceptance. Change the instance version when notice meaning changes.
func Version() string { return instance.Current().Consent.Version }

// These legacy aliases describe the embedded default; request paths use Text.
var (
	Title  = Text("de").Title
	Points = Text("de").Points
	Short  = Text("de").Short
)

func Text(lang string) Notice { return instance.Current().Notice(lang) }
