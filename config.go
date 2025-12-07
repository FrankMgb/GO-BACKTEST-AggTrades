package main

import (
	"runtime"
	"sort"
)

// This is the shared data root produced by the downloader project.
// It MUST be the directory that directly contains BTCUSDT/, ETHUSDT/, etc.
//
//	Z:\DATA\data\BTCUSDT\2020\01\...
//	Z:\DATA\data\ETHUSDT\2020\01\...
const BaseDir = `Z:\DATA\data`

// SamplingRateSec: How often we "snapshot" the continuous physics.
const SamplingRateSec = 60

// Horizon definitions for the regression targets.
var HorizonLabels = []string{"15m", "30m", "1h"}
var HorizonDelays = []int64{
	15 * 60 * 1000, // 15 min in ms
	30 * 60 * 1000, // 30 min in ms
	60 * 60 * 1000, // 60 min in ms
}

// System tuning for Ryzen 9 7900X (leave 2 cores free for OS/other work).
var CPUThreads = func() int {
	n := runtime.GOMAXPROCS(0)
	if n > 4 {
		return n - 2
	}
	return n
}()

// Symbol selects which symbol to run research/OOS on.
func Symbol() string {
	// Preference order among discovered symbols.
	preferred := []string{
		"BTCUSDT",
		"ETHUSDT",
		"BNBUSDT",
		"SOLUSDT",
		"XRPUSDT",
		"DOGEUSDT",
		"HYPEUSDT",
		"ASTERUSDT",
	}

	var symbols []string
	symbolSet := make(map[string]struct{})

	for sym := range discoverSymbols() {
		symbols = append(symbols, sym)
		symbolSet[sym] = struct{}{}
	}

	if len(symbols) == 0 {
		// Hard fallback if directory scan fails.
		return "BTCUSDT"
	}

	// Try preferred list in order.
	for _, p := range preferred {
		if _, ok := symbolSet[p]; ok {
			return p
		}
	}

	// Fallback: alphabetical first.
	sort.Strings(symbols)
	return symbols[0]
}
