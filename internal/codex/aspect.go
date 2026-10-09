package codex

import (
	"math"
	"strconv"
)

// commonRatios are snapped to when the requested ratio is within aspectTolerance.
var commonRatios = [][2]int{
	{1, 1},
	{5, 4}, {4, 5},
	{4, 3}, {3, 4},
	{3, 2}, {2, 3},
	{16, 10}, {10, 16},
	{16, 9}, {9, 16},
	{2, 1}, {1, 2},
	{21, 9}, {9, 21},
}

const (
	aspectTolerance = 0.05
	maxRatioTerm    = 25
	// maxAspect bounds the ratio to 1:3 .. 3:1, the range the image model can produce.
	maxAspect = 3
)

// AspectRatio approximates width:height as a ratio like "16:9".
// A common ratio within 5% wins (the closest one); otherwise the reduced ratio
// is used, or the closest ratio whose terms are both at most 25.
// Ratios beyond 3:1 or 1:3 are clamped to 3:1 or 1:3.
func AspectRatio(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	r := float64(width) / float64(height)
	switch {
	case r >= maxAspect:
		return formatRatio(maxAspect, 1)
	case r <= 1.0/maxAspect:
		return formatRatio(1, maxAspect)
	}
	relErr := func(p, q int) float64 {
		c := float64(p) / float64(q)
		return math.Abs(r-c) / c
	}

	best, bestErr := [2]int{}, math.Inf(1)
	for _, c := range commonRatios {
		if e := relErr(c[0], c[1]); e <= aspectTolerance && e < bestErr {
			best, bestErr = c, e
		}
	}
	if bestErr <= aspectTolerance {
		return formatRatio(best[0], best[1])
	}

	g := gcd(width, height)
	if p, q := width/g, height/g; p <= maxRatioTerm && q <= maxRatioTerm {
		return formatRatio(p, q)
	}
	// Smallest terms win ties because a multiple is never strictly better.
	for q := 1; q <= maxRatioTerm; q++ {
		for p := 1; p <= maxRatioTerm; p++ {
			if e := relErr(p, q); e < bestErr {
				best, bestErr = [2]int{p, q}, e
			}
		}
	}
	return formatRatio(best[0], best[1])
}

func formatRatio(p, q int) string {
	return strconv.Itoa(p) + ":" + strconv.Itoa(q)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
