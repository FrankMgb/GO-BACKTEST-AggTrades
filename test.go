package main

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"
)

type ResultContainer struct {
	Times []float64
	Feats []float64
	Targs []float64
}

// Per-worker storage: [horizon][model] -> ResultContainer
type WorkerResults struct {
	Data [][]*ResultContainer
}

// RunTest now runs the full OOS pipeline for **all discovered symbols** under BaseDir.
// For each symbol, it calls RunTestForSymbol and writes a separate report file:
//
//	Continuous_Algo_Report_OOS_<SYMBOL>.txt
func RunTest() {
	startAll := time.Now()

	// Discover all symbols, same logic as RunProbe.
	var symbols []string
	for sym := range discoverSymbols() {
		symbols = append(symbols, sym)
	}
	if len(symbols) == 0 {
		fmt.Println("No symbols discovered under BaseDir.")
		return
	}
	sort.Strings(symbols)

	fmt.Printf(">>> CONTINUOUS-TIME ALGO DISCOVERY (OOS REPORT, ALL SYMBOLS) <<<\n")
	fmt.Printf("   Workers: %d | Symbols: %d\n\n", CPUThreads, len(symbols))

	for _, sym := range symbols {
		fmt.Printf("=== [%s] Starting OOS discovery ===\n", sym)
		RunTestForSymbol(sym)
		fmt.Printf("=== [%s] Finished OOS discovery ===\n\n", sym)
	}

	fmt.Printf("All symbols completed in %s\n", time.Since(startAll))
}

// RunTestForSymbol runs the original OOS pipeline for a single symbol.
func RunTestForSymbol(sym string) {
	start := time.Now()

	models := GetContinuousModels()
	modelNames := make([]string, len(models))
	for i, m := range models {
		modelNames[i] = m.Name()
	}

	fmt.Printf(">>> CONTINUOUS-TIME ALGO DISCOVERY (OOS REPORT) <<<\n")
	fmt.Printf("   Symbol: %s | Workers: %d | Models: %d\n", sym, CPUThreads, len(models))

	// Global results[horizon][model].
	results := make([][]*ResultContainer, len(HorizonLabels))
	for h := range results {
		results[h] = make([]*ResultContainer, len(models))
		for m := range results[h] {
			results[h][m] = &ResultContainer{}
		}
	}

	// Collect all (year,month,day) tasks for this symbol.
	tasks := make([]ofiTask, 0)
	for t := range discoverTasks(sym) {
		tasks = append(tasks, t)
	}

	if len(tasks) == 0 {
		fmt.Printf("[%s] No tasks discovered; nothing to do.\n", sym)
		return
	}

	// Sort tasks chronologically so workers process days in a sensible order.
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Year != tasks[j].Year {
			return tasks[i].Year < tasks[j].Year
		}
		if tasks[i].Month != tasks[j].Month {
			return tasks[i].Month < tasks[j].Month
		}
		return tasks[i].Day < tasks[j].Day
	})

	// Per-worker result storage.
	workerResults := make([]*WorkerResults, CPUThreads)
	for i := 0; i < CPUThreads; i++ {
		wr := &WorkerResults{
			Data: make([][]*ResultContainer, len(HorizonLabels)),
		}
		for h := range wr.Data {
			wr.Data[h] = make([]*ResultContainer, len(models))
			for m := range wr.Data[h] {
				wr.Data[h][m] = &ResultContainer{}
			}
		}
		workerResults[i] = wr
	}

	// Task channel and worker pool.
	taskCh := make(chan ofiTask, len(tasks))
	for _, t := range tasks {
		taskCh <- t
	}
	close(taskCh)

	var wg sync.WaitGroup
	var processed atomic.Int64

	for wID := 0; wID < CPUThreads; wID++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			localStore := workerResults[id]
			localModels := GetContinuousModels()

			cols := DayColumnPool.Get().(*DayColumns)
			defer DayColumnPool.Put(cols)

			var buf []byte

			for task := range taskCh {
				if !LoadGNCFile(BaseDir, sym, task, &buf) {
					continue
				}
				if _, err := InflateGNC(buf, cols); err != nil {
					continue
				}

				streamRes := RunStream(cols, localModels)
				if len(streamRes.Times) == 0 {
					continue
				}

				numSamples := len(streamRes.Times)
				numModels := streamRes.NumModels
				numHorizons := streamRes.NumHorizons

				// Append into thread-local storage.
				for s := 0; s < numSamples; s++ {
					t := float64(streamRes.Times[s])

					featBase := s * numModels
					targBase := s * numHorizons

					for mIdx := 0; mIdx < numModels; mIdx++ {
						featVal := streamRes.Features[featBase+mIdx]
						for hIdx := 0; hIdx < numHorizons; hIdx++ {
							targVal := streamRes.Targets[targBase+hIdx]

							rc := localStore.Data[hIdx][mIdx]
							rc.Times = append(rc.Times, t)
							rc.Feats = append(rc.Feats, featVal)
							rc.Targs = append(rc.Targs, targVal)
						}
					}
				}

				processed.Add(1)
			}
		}(wID)
	}
	wg.Wait()

	// Merge worker-local results into global results.
	for wID := 0; wID < CPUThreads; wID++ {
		wr := workerResults[wID]
		for hIdx := range HorizonLabels {
			for mIdx := range models {
				src := wr.Data[hIdx][mIdx]
				dst := results[hIdx][mIdx]

				if len(src.Times) == 0 {
					continue
				}

				dst.Times = append(dst.Times, src.Times...)
				dst.Feats = append(dst.Feats, src.Feats...)
				dst.Targs = append(dst.Targs, src.Targs...)
			}
		}
	}

	// ---------------------------------------------------------------------
	// Reporting phase (per symbol)
	// ---------------------------------------------------------------------

	// One report per symbol.
	filename := fmt.Sprintf("Continuous_Algo_Report_OOS_%s.txt", sym)
	f, err := os.Create(filename)
	if err != nil {
		fmt.Printf("[%s] ERROR: could not create report file %s: %v\n", sym, filename, err)
		return
	}
	defer f.Close()
	w := tabwriter.NewWriter(f, 0, 0, 1, ' ', 0)

	const trainFrac = 0.7 // 70% earliest samples train, 30% latest samples test

	// 1) Core OOS summary, per model × horizon
	fmt.Fprintf(w, "MODEL\tHORIZON\tTrainN\tTestN\tPearsonIC\tSpearmanIC\tHitRate\tHitZ\tSharpe\tSpread(bps)\tTopDecile(bps)\tBotDecile(bps)\tMI(bits)\tNMI\tΔLogLoss\n")
	fmt.Fprintf(w, "-----\t-------\t------\t-----\t---------\t-----------\t-------\t----\t------\t-----------\t--------------\t---------------\t--------\t---\t--------\n")

	for mIdx, name := range modelNames {
		for hIdx, hName := range HorizonLabels {
			data := results[hIdx][mIdx]
			if len(data.Feats) == 0 {
				continue
			}

			stats := AnalyzeFullSuiteOOS(data.Times, data.Feats, data.Targs, trainFrac)
			if stats.TestCount == 0 {
				continue
			}

			fmt.Fprintf(
				w,
				"%s\t%s\t%d\t%d\t%.4f\t%.4f\t%.3f\t%.2f\t%.3f\t%+.1f\t%+.1f\t%+.1f\t%.3f\t%.3f\t%.4f\n",
				name,
				hName,
				stats.TrainCount,
				stats.TestCount,
				stats.PearsonIC,
				stats.SpearmanIC,
				stats.HitRate,
				stats.HitRateZ,
				stats.Sharpe,
				stats.SpreadBps,
				stats.TopDecileRetBps,
				stats.BottomDecileRetBps,
				stats.MutualInfo,
				stats.NormalizedMI,
				stats.DeltaLogLoss,
			)
		}
		fmt.Fprintf(w, "\n")
	}

	// 2) Rolling OOS metrics on the test segment
	fmt.Fprintf(w, "\n\n# Rolling OOS metrics (test segment only)\n")
	fmt.Fprintf(w, "MODEL\tHORIZON\tWIN\tCount\tPearsonIC\tSpearmanIC\tHitRate\tSharpe\n")
	fmt.Fprintf(w, "-----\t-------\t---\t-----\t---------\t-----------\t-------\t------\n")

	const rollingWindows = 8

	for mIdx, name := range modelNames {
		for hIdx, hName := range HorizonLabels {
			data := results[hIdx][mIdx]
			if len(data.Feats) == 0 {
				continue
			}
			wins := RollingWindowMetricsOOS(data.Times, data.Feats, data.Targs, trainFrac, rollingWindows)
			for winIdx, wm := range wins {
				if wm.Count == 0 {
					continue
				}
				fmt.Fprintf(
					w,
					"%s\t%s\t%d\t%d\t%.4f\t%.4f\t%.3f\t%.3f\n",
					name,
					hName,
					winIdx,
					wm.Count,
					wm.PearsonIC,
					wm.SpearmanIC,
					wm.HitRate,
					wm.Sharpe,
				)
			}
		}
		fmt.Fprintf(w, "\n")
	}

	// 3) Volatility regime OOS metrics
	fmt.Fprintf(w, "\n\n# Volatility regime OOS metrics (test segment only)\n")
	fmt.Fprintf(w, "MODEL\tHORIZON\tREGIME\tCount\tPearsonIC\tSpearmanIC\tHitRate\tSharpe\n")
	fmt.Fprintf(w, "-----\t-------\t------\t-----\t---------\t-----------\t-------\t------\n")

	for mIdx, name := range modelNames {
		for hIdx, hName := range HorizonLabels {
			data := results[hIdx][mIdx]
			if len(data.Feats) == 0 {
				continue
			}
			regs := VolRegimeMetricsOOS(data.Times, data.Feats, data.Targs, trainFrac)
			for _, rm := range regs {
				if rm.Count == 0 {
					continue
				}
				fmt.Fprintf(
					w,
					"%s\t%s\t%s\t%d\t%.4f\t%.4f\t%.3f\t%.3f\n",
					name,
					hName,
					rm.Name,
					rm.Count,
					rm.PearsonIC,
					rm.SpearmanIC,
					rm.HitRate,
					rm.Sharpe,
				)
			}
		}
		fmt.Fprintf(w, "\n")
	}

	// 4) Time-of-day regime OOS metrics
	fmt.Fprintf(w, "\n\n# Time-of-day regime OOS metrics (test segment only)\n")
	fmt.Fprintf(w, "MODEL\tHORIZON\tREGIME\tCount\tPearsonIC\tSpearmanIC\tHitRate\tSharpe\n")
	fmt.Fprintf(w, "-----\t-------\t------\t-----\t---------\t-----------\t-------\t------\n")

	for mIdx, name := range modelNames {
		for hIdx, hName := range HorizonLabels {
			data := results[hIdx][mIdx]
			if len(data.Feats) == 0 {
				continue
			}
			regs := TimeOfDayRegimeMetricsOOS(data.Times, data.Feats, data.Targs, trainFrac)
			for _, rm := range regs {
				if rm.Count == 0 {
					continue
				}
				fmt.Fprintf(
					w,
					"%s\t%s\t%s\t%d\t%.4f\t%.4f\t%.3f\t%.3f\n",
					name,
					hName,
					rm.Name,
					rm.Count,
					rm.PearsonIC,
					rm.SpearmanIC,
					rm.HitRate,
					rm.Sharpe,
				)
			}
		}
		fmt.Fprintf(w, "\n")
	}

	w.Flush()
	fmt.Printf("Done. [%s] Processed %d days in %s. OOS report saved to %s\n", sym, processed.Load(), time.Since(start), filename)
}
