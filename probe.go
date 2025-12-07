package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"
)

// RunProbe performs a fast diagnostic over all symbols under BaseDir.
// It samples up to 16 days per symbol, runs LoadGNCFile + InflateGNC,
// and reports which symbols have healthy blobs.
func RunProbe() {
	start := time.Now()

	fmt.Println(">>> GNC DATA PROBE <<<")
	fmt.Printf("BaseDir: %s\n\n", BaseDir)

	// Discover symbols from filesystem.
	var symbols []string
	for sym := range discoverSymbols() {
		symbols = append(symbols, sym)
	}
	if len(symbols) == 0 {
		fmt.Println("No symbols discovered under BaseDir.")
		return
	}
	sort.Strings(symbols)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SYMBOL\tIDX_DAYS\tSAMPLED\tOK\tFAIL\tFIRST_DAY\tLAST_DAY\tMIN_ROWS\tMAX_ROWS\tAVG_ROWS")
	fmt.Fprintln(w, "------\t--------\t-------\t--\t----\t---------\t--------\t--------\t--------\t--------")

	const samplePerSymbol = 16

	for _, sym := range symbols {
		// Collect all tasks (days) for this symbol.
		var tasks []ofiTask
		for t := range discoverTasks(sym) {
			tasks = append(tasks, t)
		}
		if len(tasks) == 0 {
			fmt.Fprintf(w, "%-8s\t0\t0\t0\t0\t-\t-\t0\t0\t0\n", sym)
			continue
		}

		// Sort tasks chronologically to get true FIRST_DAY / LAST_DAY.
		sort.Slice(tasks, func(i, j int) bool {
			if tasks[i].Year != tasks[j].Year {
				return tasks[i].Year < tasks[j].Year
			}
			if tasks[i].Month != tasks[j].Month {
				return tasks[i].Month < tasks[j].Month
			}
			return tasks[i].Day < tasks[j].Day
		})

		idxDays := len(tasks)
		first := tasks[0]
		last := tasks[len(tasks)-1]

		// Determine which indices to sample (spread across the history).
		sampled := samplePerSymbol
		if idxDays < sampled {
			sampled = idxDays
		}
		var sampleIdxs []int
		if sampled > 0 {
			step := idxDays / sampled
			if step < 1 {
				step = 1
			}
			for i, count := 0, 0; i < idxDays && count < sampled; i += step {
				sampleIdxs = append(sampleIdxs, i)
				count++
			}
			if len(sampleIdxs) == 0 {
				sampleIdxs = []int{0}
				sampled = 1
			} else {
				sampled = len(sampleIdxs)
			}
		}

		cols := DayColumnPool.Get().(*DayColumns)
		cols.Reset()
		var buf []byte

		okCount := 0
		failCount := 0
		var minRows, maxRows, totalRows int

		for _, idx := range sampleIdxs {
			t := tasks[idx]

			if !LoadGNCFile(BaseDir, sym, t, &buf) {
				failCount++
				fmt.Printf(
					"  [%s] %04d-%02d-%02d  STATUS=LOAD_FAIL   rows=0 reason=missing_or_unreadable_blob\n",
					sym, t.Year, t.Month, t.Day,
				)
				continue
			}
			rows, err := InflateGNC(buf, cols)
			if err != nil || rows <= 0 {
				failCount++
				fmt.Printf(
					"  [%s] %04d-%02d-%02d  STATUS=DECODE_FAIL rows=%d reason=%v\n",
					sym, t.Year, t.Month, t.Day, rows, err,
				)
				continue
			}

			okCount++
			if okCount == 1 {
				minRows, maxRows = rows, rows
			} else {
				if rows < minRows {
					minRows = rows
				}
				if rows > maxRows {
					maxRows = rows
				}
			}
			totalRows += rows
		}

		DayColumnPool.Put(cols)

		avgRows := 0
		if okCount > 0 {
			avgRows = totalRows / okCount
		}

		firstStr := fmt.Sprintf("%04d-%02d-%02d", first.Year, first.Month, first.Day)
		lastStr := fmt.Sprintf("%04d-%02d-%02d", last.Year, last.Month, last.Day)

		fmt.Fprintf(
			w,
			"%-8s\t%d\t%d\t%d\t%d\t%s\t%s\t%d\t%d\t%d\n",
			sym,
			idxDays,
			sampled,
			okCount,
			failCount,
			firstStr,
			lastStr,
			minRows,
			maxRows,
			avgRows,
		)
	}

	w.Flush()
	fmt.Printf("\n[probe] Finished in %s\n", time.Since(start))
}
