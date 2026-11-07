// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestUserErrorKeepsCause(t *testing.T) {
	cause := os.ErrNotExist
	err := userError(cause, "Cannot open x", "Check the name")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cause lost")
	}
	var stdout, stderr bytes.Buffer
	finish(newOutput(&stdout, &stderr), slog.New(slog.DiscardHandler), "", err)
	if strings.Contains(stderr.String(), "file does not exist") {
		t.Fatalf("technical text shown as headline: %q", stderr.String())
	}
}
