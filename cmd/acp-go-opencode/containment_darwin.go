//go:build darwin

package main

import "github.com/savid/acp-go-opencode/internal/opencode"

var diagnoseContainment = opencode.DiagnoseDarwinContainment

var cleanupContainment = opencode.CleanupDarwinContainment
