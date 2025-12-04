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

	// --- Warm-up to decide iteration count ---
	warmStats := benchStudy(sym, dayInt, variants, featRoot, 1)
	warm := warmStats.Total
	if warm <= 0 {
		warm = 2 * time.Millisecond
	}
	target := 500 * time.Millisecond
	iters := int(target / warm)
	if iters < 3 {
		iters = 3
	} else if iters > 2000 {
		iters = 2000
	}

	fmt.Printf("[bench] warm-up: %s per study, selecting %d iterations\n",
		warmStats.Total, iters)

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

	stats := benchStudy(sym, dayInt, variants, featRoot, iters)
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

	runPprofTop("bench_cpu.pprof", "cpu")
	runPprofTop("bench_mem.pprof", "heap")

	fmt.Println("=== BENCHMARK COMPLETE ===")
}

// benchStudy repeatedly runs processStudyDay for one symbol/day
func benchStudy(sym string, dayInt int, variants []string, featRoot string, iters int) benchStats {
	stats := benchStats{
		Name:  "StudyDay",
		Iters: iters,
	}

	if iters <= 0 {
		return stats
	}

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
		// FIXED: Removed doQuantiles bool argument
		res := processStudyDay(
			sym, dayInt, variants, featRoot,
			&sigBuf, &fileBuf, &retBuf, &retsPerHBuf, &gncBuf,
		)

		if i == 0 {
			rows := 0
			// V3 DayResult just has Metrics map[string][]Moments
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

func printBenchStats(bs benchStats) {
	if bs.Iters <= 0 || bs.Total <= 0 {
		fmt.Printf("[bench] %s: no data\n", bs.Name)
		return
	}

	nsPerOp := float64(bs.Total.Nanoseconds()) / float64(bs.Iters)
	totalRows := float64(bs.RowsPerIter * bs.Iters)
	totalBytes := float64(bs.BytesPerIter * bs.Iters)
	totalCells := totalRows * float64(bs.FeatPerIter)

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

func runPprofTop(profilePath, kind string) {
	if _, err := os.Stat(profilePath); err != nil {
		return
	}
	cmd := exec.Command("go", "tool", "pprof", "-top", profilePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("[bench] go tool pprof -top (%s) failed: %v\n", kind, err)
		return
	}
	fmt.Printf("\n[pprof-%s] go tool pprof -top %s\n", kind, profilePath)
	fmt.Println(string(out))
}

func findStudySample() (sym string, dayInt int, variants []string, featRoot string, ok bool) {
	// Re-using common.go/study.go logic implicitly via shared package
	// But we need to implement discovery here if not exported.
	// Since we are in main package, we can use discoverFeatureSymbols from study.go if exported?
	// No, discoverFeatureSymbols returns iter.Seq.
	// We'll just do a quick manual scan.

	featDir := filepath.Join(BaseDir, "features")
	entries, err := os.ReadDir(featDir)
	if err != nil {
		return "", 0, nil, "", false
	}

	for _, e := range entries {
		if !e.IsDir() || e.Name()[0] == '.' {
			continue
		}
		s := e.Name()

		featRoot = filepath.Join(featDir, s)
		vEntries, err := os.ReadDir(featRoot)
		if err != nil {
			continue
		}

		var vs []string
		for _, ve := range vEntries {
			if ve.IsDir() && ve.Name()[0] != '.' {
				vs = append(vs, ve.Name())
			}
		}
		if len(vs) == 0 {
			continue
		}

		// Find a day
		vDir := filepath.Join(featRoot, vs[0])
		files, _ := os.ReadDir(vDir)
		for _, f := range files {
			if len(f.Name()) > 4 && filepath.Ext(f.Name()) == ".bin" {
				// 20200101.bin
				base := f.Name()[:len(f.Name())-4]
				if d, err := parseDateInt(base); err == nil {
					return s, d, vs, featRoot, true
				}
			}
		}
	}
	return "", 0, nil, "", false
}

func parseDateInt(s string) (int, error) {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("bad int")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// featureBytesForDay sums the sizes of all variant feature files for this day.
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
		if fi.Mode().IsRegular() {
			if sz := fi.Size(); sz > 0 {
				total += int(sz)
			}
		}
	}
	return total
}
