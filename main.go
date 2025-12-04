package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

func main() {
	// Use all 7900X hardware threads.
	runtime.GOMAXPROCS(CPUThreads)

	// Hard memory limit: 24GB.
	const ramLimit = 24 * 1024 * 1024 * 1024
	debug.SetMemoryLimit(ramLimit)

	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	start := time.Now()

	fmt.Printf("%s | Env: %s/%s | Threads: %d | RAM Limit: 24GB | GOGC: %s | GOAMD64: %s\n",
		runtime.Version(),
		runtime.GOOS, runtime.GOARCH,
		runtime.GOMAXPROCS(0),
		os.Getenv("GOGC"),
		os.Getenv("GOAMD64"),
	)

	cmd := os.Args[1]

	switch cmd {
	case "data":
		runData()
	case "build":
		runBuild()
	case "study":
		runStudy()
	case "sanity":
		runSanity()
	case "bench":
		runBench()
	case "report":
		runReport()
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		printHelp()
		os.Exit(1)
	}

	fmt.Printf("\n[sys] Execution Time: %s | Mem: %s\n", time.Since(start), getMemUsage())
}

func printHelp() {
	fmt.Println("Usage: quant.exe [command]")
	fmt.Println("  data   - Download raw aggTrades")
	fmt.Println("  build  - Run TFI Primitives (RWVI, VAI, etc) -> features")
	fmt.Println("  study  - Run IS/OOS backtest on features")
	fmt.Println("  sanity - Check data integrity")
	fmt.Println("  bench  - Run decode benchmark + inline pprof summaries")
}

func getMemUsage() string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return fmt.Sprintf("%d MB", m.Alloc/1024/1024)
}
