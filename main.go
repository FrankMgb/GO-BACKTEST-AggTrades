package main

import (
	"fmt"
	"os"
	"runtime/debug"
)

func main() {
	// Slightly laxer GC; this is CPU-heavy research code.
	debug.SetGCPercent(200)

	if len(os.Args) < 2 {
		fmt.Println("Usage: go run . [test|probe]")
		return
	}

	switch os.Args[1] {
	case "test":
		// Full OOS research run (writes Continuous_Algo_Report_OOS.txt).
		RunTest()
	case "probe":
		// Structural sanity check of data under BaseDir.
		RunProbe()
	default:
		fmt.Println("Unknown command. Use 'test' or 'probe'")
	}
}
