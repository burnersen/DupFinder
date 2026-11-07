// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"fmt"
	"math"
	"testing"
)

// pgm builds a binary P5 image the way ffmpeg writes it.
func pgm(w, h int, pixel func(x, y int) byte) []byte {
	data := []byte(fmt.Sprintf("P5\n%d %d\n255\n", w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			data = append(data, pixel(x, y))
		}
	}
	return data
}

func gradient(x, y int) byte { return byte((x*7 + y*3) % 256) }
func stripes(x, y int) byte {
	if (x/4)%2 == 0 {
		return 230
	}
	return 20
}

func TestParsePGM(t *testing.T) {
	px, err := parsePGM(pgm(32, 32, gradient))
	if err != nil {
		t.Fatal(err)
	}
	if len(px) != 32*32 || px[33] != gradient(1, 1) {
		t.Fatalf("got %d pixels, px[33]=%d", len(px), px[33])
	}

	withComment := append([]byte("P5\n# made by ffmpeg\n2 1\n255\n"), 1, 2)
	if px, err := parsePGM(withComment); err != nil || len(px) != 2 {
		t.Fatalf("comment line: %v, %d pixels", err, len(px))
	}

	for name, bad := range map[string][]byte{
		"empty":      nil,
		"not P5":     []byte("P6\n1 1\n255\nxxx"),
		"short body": []byte("P5\n4 4\n255\nab"),
		"no body":    []byte("P5\n4 4\n255"),
	} {
		if _, err := parsePGM(bad); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestPHashSameImageSameHash(t *testing.T) {
	a, _ := parsePGM(pgm(32, 32, gradient))
	b, _ := parsePGM(pgm(32, 32, gradient))
	ha, hb := computePHash(a), computePHash(b)
	if ha == 0 || ha != hb {
		t.Fatalf("hashes %x and %x", ha, hb)
	}
}

func TestPHashToleratesSmallChanges(t *testing.T) {
	a, _ := parsePGM(pgm(32, 32, gradient))
	// Slightly brighter, like a re-encode with other levels.
	b, _ := parsePGM(pgm(32, 32, func(x, y int) byte { return gradient(x, y)/2 + 60 }))
	if d := hammingDistance(computePHash(a), computePHash(b)); d > maxHashDistance {
		t.Fatalf("distance %d, want at most %d", d, maxHashDistance)
	}
}

func TestPHashTellsDifferentImagesApart(t *testing.T) {
	a, _ := parsePGM(pgm(32, 32, gradient))
	b, _ := parsePGM(pgm(32, 32, stripes))
	if d := hammingDistance(computePHash(a), computePHash(b)); d <= maxHashDistance {
		t.Fatalf("distance %d, want more than %d", d, maxHashDistance)
	}
}

func TestPHashWrongSizeIsZero(t *testing.T) {
	if h := computePHash(make([]byte, 10)); h != 0 {
		t.Fatalf("got %x", h)
	}
}

func TestDCTTableMatchesDirectComputation(t *testing.T) {
	direct := makeDCTCosTable(dctSize)
	for i := range direct {
		if math.Abs(direct[i]-dctCosTable[i]) > 1e-12 {
			t.Fatalf("index %d differs", i)
		}
	}
	src := make([]float64, 8*8)
	for i := range src {
		src[i] = float64(i)
	}
	// Other sizes still work (computed on the fly).
	if out := dct2D(src, 8); len(out) != 64 || out[0] == 0 {
		t.Fatalf("dct2D(8) = %v", out[:2])
	}
}

func TestCompareHashSets(t *testing.T) {
	a := []uint64{1, 2, 3, 0xFF, 0, 6}
	b := []uint64{1, 2, 3, 0xFF00, 5, 6}
	matches, avg := compareHashSets(a, b)
	// Index 4 is skipped (0 means "no frame"), index 3 is 16 bits apart.
	if matches != 4 {
		t.Fatalf("matches = %d, want 4", matches)
	}
	if want := 16.0 / 5; math.Abs(avg-want) > 1e-9 {
		t.Fatalf("avg = %v, want %v", avg, want)
	}
	if m, d := compareHashSets(a, a[:3]); m != 0 || d != 64 {
		t.Fatalf("different lengths: %d, %v", m, d)
	}
	if m, d := compareHashSets([]uint64{0}, []uint64{0}); m != 0 || d != 64 {
		t.Fatalf("no valid frames: %d, %v", m, d)
	}
}
