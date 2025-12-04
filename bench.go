package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"time"
)

// benchStats holds per-benchmark summary for the study path.
type benchStats struct {
	Name            string
	Iters           int
	RowsPerIter     int // number of time steps (rows) per day
	FeatPerIter     int // number of feature series per day (variants * dims)
	BytesPerIter    int // approximate feature bytes read per day
	Total           time.Duration
	AllocBytesPerOp uint64
	MallocsPerOp    uint64
}

// runBench is called from main when you do: go run . bench
// It benchmarks the STUDY pipeline on a single day:
//
//	loadDayColumns + feature decode + returns + moments + quantiles.
func runBench() {
	fmt.Println("=== BENCHMARK: QuantDev STUDY (processStudyDay) ===")
	fmt.Printf("Go: %s | GOOS/GOARCH: %s/%s | Threads: %d\n",
		runtime.Version(),
		runtime.GOOS, runtime.GOARCH,
		runtime.GOMAXPROCS(0),
	)

	sym, dayInt, variants, featRoot, ok := findStudySample()
	if !ok {
		fmt.Printf("[bench] no feature sets found under %q\n", filepath.Join(BaseDir, "features"))
		return
	}
	y := dayInt / 10000
	m := (dayInt % 10000) / 100
	d := dayInt % 100

	fmt.Printf("[bench] Sample symbol: %s | day: %04d-%02d-%02d\n", sym, y, m, d)
	fmt.Printf("[bench] Variants: %v\n", variants)

	featureBytes := featureBytesForDay(featRoot, variants, dayInt)
	if featureBytes > 0 {
		fmt.Printf("[bench] Approx feature bytes/day: %d\n", featureBytes)
	}

	// Quantiles are the expensive part; mimic real logic but keep worst-case feel.
	doQuantiles := dayInt < oosBoundaryYMD

	// --- Warm-up to decide iteration count ---
	warmStats := benchStudy(sym, dayInt, variants, featRoot, 1, doQuantiles)
	warm := warmStats.Total
	if warm <= 0 {
		// Clock weirdness / too fast — assume a tiny but non-zero duration.
		warm = 2 * time.Millisecond
	}
	target := 500 * time.Millisecond
	iters := int(target / warm)
	if iters < 3 {
		iters = 3
	} else if iters > 2000 {
		iters = 2000
	}

	fmt.Printf("[bench] warm-up: %s per study, selecting %d iterations (fallback=%v)\n",
		warmStats.Total, iters, warmStats.Total <= 0)

	// --- CPU profile + real benchmark ---
	var cpuFile *os.File
	var err error
	cpuFile, err = os.Create("bench_cpu.pprof")
	if err != nil {
		fmt.Printf("[bench] cannot create CPU profile: %v\n", err)
	} else {
		if err := pprof.StartCPUProfile(cpuFile); err != nil {
			fmt.Printf("[bench] cannot start CPU profile: %v\n", err)
			cpuFile.Close()
			cpuFile = nil
		} else {
			fmt.Println("[bench] CPU profiling: ON")
		}
	}

	stats := benchStudy(sym, dayInt, variants, featRoot, iters, doQuantiles)
	stats.BytesPerIter = featureBytes

	if cpuFile != nil {
		pprof.StopCPUProfile()
		cpuFile.Close()
		fmt.Println("[bench] CPU profile written to bench_cpu.pprof")
	}

	printBenchStats(stats)

	// --- Heap profile snapshot ---
	memFile, err := os.Create("bench_mem.pprof")
	if err != nil {
		fmt.Printf("[bench] cannot create heap profile: %v\n", err)
	} else {
		runtime.GC()
		if err := pprof.WriteHeapProfile(memFile); err != nil {
			fmt.Printf("[bench] cannot write heap profile: %v\n", err)
		} else {
			fmt.Println("[bench] Heap profile written to bench_mem.pprof")
		}
		memFile.Close()
	}

	// --- Inline pprof -top summaries (best-effort) ---
	runPprofTop("bench_cpu.pprof", "cpu")
	runPprofTop("bench_mem.pprof", "heap")

	fmt.Println("=== BENCHMARK COMPLETE ===")
}

// benchStudy repeatedly runs processStudyDay for one symbol/day
// and measures time + allocations. This hits:
//
//   - loadDayColumns (GNC decompress)
//   - computeReturns for each horizon
//   - feature decode for each variant/dim
//   - CalcMomentsVectors
//   - ComputeQuantilesStrided (if doQuantiles)
//
// i.e. the "mega compute" path.
func benchStudy(sym string, dayInt int, variants []string, featRoot string, iters int, doQuantiles bool) benchStats {
	stats := benchStats{
		Name:  "StudyDay",
		Iters: iters,
	}

	if iters <= 0 {
		return stats
	}

	// Prepare worker-like buffers (same pattern as runStudy workers).
	var sigBuf []float64
	var fileBuf []byte
	var retBuf []float64
	retsPerHBuf := make([][]float64, len(TimeHorizonsMS))
	var gncBuf []byte

	runtime.GC()

	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)

	start := time.Now()
	for i := 0; i < iters; i++ {
		res := processStudyDay(
			sym, dayInt, variants, featRoot,
			&sigBuf, &fileBuf, &retBuf, &retsPerHBuf, &gncBuf,
			doQuantiles,
		)

		// On first iter, infer rows and feature count from Moments.
		if i == 0 {
			rows := 0
			for _, momsSlice := range res.Metrics {
				if len(momsSlice) > 0 {
					rows = int(momsSlice[0].Count)
					break
				}
			}
			stats.RowsPerIter = rows
			stats.FeatPerIter = len(res.Metrics)
		}
	}
	stats.Total = time.Since(start)

	runtime.ReadMemStats(&m1)
	allocBytes := m1.TotalAlloc - m0.TotalAlloc
	mallocs := m1.Mallocs - m0.Mallocs

	if iters > 0 {
		stats.AllocBytesPerOp = allocBytes / uint64(iters)
		stats.MallocsPerOp = mallocs / uint64(iters)
	}

	return stats
}

// printBenchStats pretty-prints the stats in a human-friendly way.
func printBenchStats(bs benchStats) {
	if bs.Iters <= 0 || bs.Total <= 0 {
		fmt.Printf("[bench] %s: no data\n", bs.Name)
		return
	}

	nsPerOp := float64(bs.Total.Nanoseconds()) / float64(bs.Iters)
	totalRows := float64(bs.RowsPerIter * bs.Iters)
	totalBytes := float64(bs.BytesPerIter * bs.Iters)
	totalCells := totalRows * float64(bs.FeatPerIter) // rows × features

	secs := bs.Total.Seconds()
	rowsPerSec := 0.0
	bytesPerSec := 0.0
	cellsPerSec := 0.0
	if secs > 0 {
		rowsPerSec = totalRows / secs
		bytesPerSec = totalBytes / secs
		cellsPerSec = totalCells / secs
	}

	fmt.Printf("\n[bench] %s\n", bs.Name)
	fmt.Printf("  iters:         %d\n", bs.Iters)
	fmt.Printf("  rows/iter:     %d\n", bs.RowsPerIter)
	fmt.Printf("  features/iter: %d\n", bs.FeatPerIter)
	if bs.BytesPerIter > 0 {
		fmt.Printf("  bytes/iter:    %d (feature files)\n", bs.BytesPerIter)
	}
	fmt.Printf("  total time:    %s\n", bs.Total)
	fmt.Printf("  ns/op:         %.0f\n", nsPerOp)

	fmt.Printf("  throughput:    %.3f krows/s", rowsPerSec/1e3)
	if bs.FeatPerIter > 0 {
		fmt.Printf(", %.3f Mcells/s", cellsPerSec/1e6)
	}
	if bs.BytesPerIter > 0 {
		fmt.Printf(", %.3f MB/s (features)\n", bytesPerSec/(1024*1024))
	} else {
		fmt.Println()
	}

	fmt.Printf("  allocs/op:     %d mallocs/op, %d B/op\n",
		bs.MallocsPerOp, bs.AllocBytesPerOp)
}

// runPprofTop runs "go tool pprof -top <profile>" and prints its output.
// Best-effort: if 'go' isn't on PATH or anything fails, it just logs and returns.
func runPprofTop(profilePath, kind string) {
	if _, err := os.Stat(profilePath); err != nil {
		// No profile file; nothing to do.
		return
	}

	cmd := exec.Command("go", "tool", "pprof", "-top", profilePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("[bench] go tool pprof -top (%s) failed: %v\n", kind, err)
		if len(out) > 0 {
			fmt.Printf("[pprof-%s]\n%s\n", kind, string(out))
		}
		return
	}

	fmt.Printf("\n[pprof-%s] go tool pprof -top %s\n", kind, profilePath)
	fmt.Println(string(out))
}

// findStudySample locates the first symbol/day that has feature files
// so we can benchmark a realistic study workload.
func findStudySample() (sym string, dayInt int, variants []string, featRoot string, ok bool) {
	syms := discoverFeatureSymbols()
	if syms == nil {
		return "", 0, nil, "", false
	}

	for s := range syms {
		featRoot = filepath.Join(BaseDir, "features", s)
		entries, err := os.ReadDir(featRoot)
		if err != nil {
			continue
		}

		var vs []string
		for _, e := range entries {
			if e.IsDir() && !isDotDir(e.Name()) {
				vs = append(vs, e.Name())
			}
		}
		if len(vs) == 0 {
			continue
		}

		// Use the first variant (same as runStudy) to discover days.
		baseVariantDir := filepath.Join(featRoot, vs[0])

		var days []int
		for d := range discoverStudyDays(baseVariantDir) {
			days = append(days, d)
		}
		if len(days) == 0 {
			continue
		}

		// Pick a mid-day (roughly typical load).
		dayInt = days[len(days)/2]
		return s, dayInt, vs, featRoot, true
	}
	return "", 0, nil, "", false
}

func isDotDir(name string) bool {
	return len(name) > 0 && name[0] == '.'
}

// featureBytesForDay sums the sizes of all variant feature files for this day.
// It's an approximation for "bytes/iter" to give a sense of memory bandwidth.
func featureBytesForDay(featRoot string, variants []string, dayInt int) int {
	y := dayInt / 10000
	m := (dayInt % 10000) / 100
	d := dayInt % 100
	dStr := fmt.Sprintf("%04d%02d%02d", y, m, d)

	total := 0
	for _, v := range variants {
		path := filepath.Join(featRoot, v, dStr+".bin")
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		// Only count regular files.
		if fi.Mode().IsRegular() {
			if sz := fi.Size(); sz > 0 {
				total += int(sz)
			}
		}
	}
	return total
}
