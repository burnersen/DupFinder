package main

import (
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strconv"
)

func parsePGM(data []byte) ([]byte, error) {
	if len(data) < 6 || data[0] != 'P' || data[1] != '5' {
		return nil, fmt.Errorf("kein P5 PGM")
	}
	idx := 2

	readToken := func() (string, error) {
		for idx < len(data) {
			c := data[idx]
			if c == '#' {
				for idx < len(data) && data[idx] != '\n' {
					idx++
				}
				continue
			}
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				idx++
				continue
			}
			break
		}
		start := idx
		for idx < len(data) {
			c := data[idx]
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				break
			}
			idx++
		}
		if start == idx {
			return "", fmt.Errorf("token erwartet")
		}
		return string(data[start:idx]), nil
	}

	wTok, err := readToken()
	if err != nil {
		return nil, err
	}
	hTok, err := readToken()
	if err != nil {
		return nil, err
	}
	mTok, err := readToken()
	if err != nil {
		return nil, err
	}
	w, _ := strconv.Atoi(wTok)
	h, _ := strconv.Atoi(hTok)
	maxv, _ := strconv.Atoi(mTok)

	if idx >= len(data) {
		return nil, fmt.Errorf("kein body")
	}
	idx++

	expected := w * h
	if maxv > 255 {
		expected *= 2
	}
	if idx+expected > len(data) {
		return nil, fmt.Errorf("body zu kurz")
	}
	return data[idx : idx+expected], nil
}

func computePHash(pixels []byte) uint64 {
	if len(pixels) != 32*32 {
		return 0
	}
	const N = 32
	src := make([]float64, N*N)
	for i, b := range pixels {
		src[i] = float64(b)
	}
	dct := dct2D(src, N)

	const M = 8
	coeffs := make([]float64, 0, M*M-1)
	for y := 0; y < M; y++ {
		for x := 0; x < M; x++ {
			if x == 0 && y == 0 {
				continue
			}
			coeffs = append(coeffs, dct[y*N+x])
		}
	}
	sorted := make([]float64, len(coeffs))
	copy(sorted, coeffs)
	sort.Float64s(sorted)
	median := sorted[len(sorted)/2]

	var hash uint64
	for i, c := range coeffs {
		if c > median {
			hash |= 1 << uint(i)
		}
	}
	return hash
}

// dctSize ist die Kantenlänge der Graustufen-Matrix, über die der pHash-DCT läuft.
const dctSize = 32

// dctCosTable hält die DCT-Cosinus-Koeffizienten für dctSize. Sie hängen NUR von
// der Größe ab, sind also für jeden Frame identisch und werden einmal beim
// Programmstart berechnet statt pro Frame neu — das spart in der Hash-Schleife
// jeweils dctSize*dctSize math.Cos-Aufrufe.
var dctCosTable = makeDCTCosTable(dctSize)

func makeDCTCosTable(n int) []float64 {
	cos := make([]float64, n*n)
	for k := 0; k < n; k++ {
		for x := 0; x < n; x++ {
			cos[k*n+x] = math.Cos(math.Pi * (float64(x) + 0.5) * float64(k) / float64(n))
		}
	}
	return cos
}

func dct2D(src []float64, N int) []float64 {
	// Schnellpfad: vorberechnete Tabelle für die übliche Größe. Andere Größen
	// (aktuell nicht genutzt) berechnen sich weiterhin ad hoc → bleibt robust.
	cos := dctCosTable
	if N != dctSize {
		cos = makeDCTCosTable(N)
	}
	tmp := make([]float64, N*N)
	for y := 0; y < N; y++ {
		for k := 0; k < N; k++ {
			var sum float64
			for n := 0; n < N; n++ {
				sum += src[y*N+n] * cos[k*N+n]
			}
			tmp[y*N+k] = sum
		}
	}
	out := make([]float64, N*N)
	for x := 0; x < N; x++ {
		for k := 0; k < N; k++ {
			var sum float64
			for n := 0; n < N; n++ {
				sum += tmp[n*N+x] * cos[k*N+n]
			}
			out[k*N+x] = sum
		}
	}
	return out
}

func hammingDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

func compareHashSets(a, b []uint64) (matches int, avgDist float64) {
	if len(a) != len(b) || len(a) == 0 {
		return 0, 64
	}
	var totalDist, valid int
	for i := range a {
		if a[i] == 0 || b[i] == 0 {
			continue
		}
		d := hammingDistance(a[i], b[i])
		totalDist += d
		valid++
		if d <= maxHashDistance {
			matches++
		}
	}
	if valid == 0 {
		return 0, 64
	}
	return matches, float64(totalDist) / float64(valid)
}
