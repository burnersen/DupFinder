//go:build !windows

// DupFinder — Required Notice: Copyright (c) 2026 burnersen — DupFinder
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

import "fmt"

func moveToRecycleBin(path string) error {
	return fmt.Errorf("Papierkorb nur auf Windows verfügbar")
}
