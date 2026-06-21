//go:build !windows

package main

import "fmt"

func moveToRecycleBin(path string) error {
	return fmt.Errorf("Papierkorb nur auf Windows verfügbar")
}
