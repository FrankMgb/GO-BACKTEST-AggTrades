package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// Per-horizon accumulator for one feature
type FHAcc struct {
	IS          Moments
	OOS         Moments
	ISDailyICs  []float64
	OOSDailyICs []float64
}

// Per-feature accumulator across all horizons
type FeatureAcc struct {
	Name string
	H    []FHAcc // len(TimeHorizonsMS)
}

// Entry point for the streaming report.
// Uses raw GNC data -> Atoms -> Moments -> MetricStats, no features/*.bin.
func runReport() {
	outPath := "winning_math_report.txt"

	f, err := os.Create(outPath)
	if err != nil {
		fmt.Printf("[report] cannot create %s: %v\n", outPath, err)
		return
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	defer w.Flush()

	now := time.Now()

	fmt.Fprintln(w, "=== QuantDev Streaming Winning Math Report ===")
	fmt.Fprintf(w, "Generated: %s\n", now.Format(time.RFC3339))
	fmt.Fprintf(w, "BaseDir:  %s\n", BaseDir)
	fmt.Fprintf(w, "OOS Cut:  %s (YMD=%d)\n", OOSDateStr, oosBoundaryYMD)
	fmt.Fprintln(w)

	symbols := discoverReportSymbols()
	if len(symbols) == 0 {
		fmt.Fprintln(w, "[report] no symbols discovered under BaseDir")
		fmt.Printf("[report] no symbols discovered under %q\n", BaseDir)
		return
	}

	for _, sym := range symbols {
		reportSymbolStreaming(sym, w)
	}

	fmt.Printf("[report] wrote %s for %d symbols\n", outPath, len(symbols))
}

// Discover symbols directly from data/ (ignores features/, common/, dot dirs).
func discoverReportSymbols() []string {
	entries, err := os.ReadDir(BaseDir)
	if err != nil {
		fmt.Printf("[report] ReadDir(%s): %v\n", BaseDir, err)
		return nil
	}
	var syms []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "features" || name == "common" || len(name) == 0 || name[0] == '.' {
			continue
		}
		syms = append(syms, name)
	}
	sort.Strings(syms)
	return syms
}

// Discover all days for which we have an index entry for this symbol.
func discoverReportDays(sym string) []int {
	root := filepath.Join(BaseDir, sym)
	years, err := os.ReadDir(root)
	if err != nil {
		fmt.Printf("[report] ReadDir(%s): %v\n", root, err)
		return nil
	}

	var days []int
	for _, yDir := range years {
		if !yDir.IsDir() {
			continue
		}
		y, err := strconv.Atoi(yDir.Name())
		if err != nil {
			continue
		}
		yearPath := filepath.Join(root, yDir.Name())
		months, err := os.ReadDir(yearPath)
		if err != nil {
			continue
		}
		for _, mDir := range months {
			if !mDir.IsDir() {
				continue
			}
			m, err := strconv.Atoi(mDir.Name())
			if err != nil {
				continue
			}

			idxPath := filepath.Join(yearPath, mDir.Name(), "index.quantdev")
			f, err := os.Open(idxPath)
			if err != nil {
				continue
			}

			var hdr [16]byte
			if _, err := io.ReadFull(f, hdr[:]); err != nil {
				_ = f.Close()
				continue
			}
			if string(hdr[0:4]) != IdxMagic {
				_ = f.Close()
				continue
			}

			count := binary.LittleEndian.Uint64(hdr[8:])
			var row [26]byte
			for i := uint64(0); i < count; i++ {
				if _, err := io.ReadFull(f, row[:]); err != nil {
					break
				}
				d := int(binary.LittleEndian.Uint16(row[0:]))
				ymd := y*10000 + m*100 + d
				days = append(days, ymd)
			}
			_ = f.Close()
		}
	}

	sort.Ints(days)
	return days
}

// Streaming report for one symbol: raw GNC -> Atoms -> Moments -> Metrics.
func reportSymbolStreaming(sym string, w *bufio.Writer) {
	days := discoverReportDays(sym)
	if len(days) == 0 {
		fmt.Fprintf(w, "[report] no indexed days for %s\n\n", sym)
		return
	}

	fmt.Fprintln(w, "==================================================")
	fmt.Fprintf(w, "SYMBOL: %s\n", sym)
	fmt.Fprintln(w, "==================================================")
	fmt.Fprintln(w)

	atoms := GetActiveAtoms()
	numAtoms := len(atoms)
	if numAtoms == 0 {
		fmt.Fprintf(w, "[report] no atoms active for %s\n\n", sym)
		return
	}

	// Per-feature accumulators
	features := make(map[string]*FeatureAcc, numAtoms)
	for _, a := range atoms {
		name := a.Name()
		features[name] = &FeatureAcc{
			Name: name,
			H:    make([]FHAcc, len(TimeHorizonsMS)),
		}
	}

	colsAny := DayColumnPool.Get()
	cols := colsAny.(*DayColumns)
	defer DayColumnPool.Put(cols)

	var gncBuf []byte
	var sigs [][]float64 // [atom][row]
	var retsPerH [][]float64
	var retBuf []float64

	for _, ymd := range days {
		cols.Reset()
		y := ymd / 10000
		m := (ymd % 10000) / 100
		d := ymd % 100

		n, ok := loadDayColumns(sym, y, m, d, cols, &gncBuf)
		if !ok || n <= 1 {
			continue
		}

		prices := cols.Prices
		times := cols.Times
		qtys := cols.Qtys
		sides := cols.Sides

		// Compute returns for each horizon for this day
		if retsPerH == nil {
			retsPerH = make([][]float64, len(TimeHorizonsMS))
		}
		for hIdx, hMS := range TimeHorizonsMS {
			computeReturns(prices, times, n, hMS, &retBuf)
			if cap(retsPerH[hIdx]) < n {
				retsPerH[hIdx] = make([]float64, n+n/4)
			}
			retsPerH[hIdx] = retsPerH[hIdx][:n]
			copy(retsPerH[hIdx], retBuf[:n])
		}

		// Prepare per-atom signal buffers and reset atom state
		if sigs == nil || len(sigs) != numAtoms {
			sigs = make([][]float64, numAtoms)
		}
		for i := range atoms {
			if cap(sigs[i]) < n {
				sigs[i] = make([]float64, n+n/4)
			}
			sigs[i] = sigs[i][:n]
			atoms[i].Reset()
		}

		// Generate signals in one pass across trades
		prevTime := times[0]
		for i := 0; i < n; i++ {
			q := qtys[i]
			s := float64(sides[i])
			p := prices[i]
			ts := times[i]
			flow := q * s

			dtSec := 0.0
			if i > 0 {
				dtMs := float64(ts - prevTime)
				if dtMs < 0 {
					dtMs = 0
				}
				dtSec = dtMs / 1000.0
			}
			if dtSec < 1e-4 {
				dtSec = 1e-4
			}

			for aIdx, atom := range atoms {
				v := atom.Update(q, s, p, flow, dtSec)

				// Clamp to match build.go behavior
				if v > 50 {
					v = 50
				} else if v < -50 {
					v = -50
				}
				sigs[aIdx][i] = v
			}
			prevTime = ts
		}

		isOOS := ymd >= oosBoundaryYMD

		// For each feature & horizon, accumulate day-level Moments and daily IC
		for aIdx, atom := range atoms {
			name := atom.Name()
			fa := features[name]
			if fa == nil {
				// Shouldn't happen, but be defensive
				continue
			}
			sArr := sigs[aIdx][:n]

			for hIdx := range TimeHorizonsMS {
				moms := CalcMomentsVectors(sArr, retsPerH[hIdx][:n])
				ic := dailyICFromMoments(moms)
				fh := &fa.H[hIdx]

				if isOOS {
					fh.OOS.Add(moms)
					if ic != 0 && !math.IsNaN(ic) && !math.IsInf(ic, 0) {
						fh.OOSDailyICs = append(fh.OOSDailyICs, ic)
					}
				} else {
					fh.IS.Add(moms)
					if ic != 0 && !math.IsNaN(ic) && !math.IsInf(ic, 0) {
						fh.ISDailyICs = append(fh.ISDailyICs, ic)
					}
				}
			}
		}
	}

	// Prepare sorted feature list for printing
	featNames := make([]string, 0, len(features))
	for name := range features {
		featNames = append(featNames, name)
	}
	sort.Strings(featNames)

	// Output tables per horizon
	for hIdx, hMS := range TimeHorizonsMS {
		sec := float64(hMS) / 1000.0
		fmt.Fprintf(w, "-- %s | Horizon: %.3fs (%d ms) --\n", sym, sec, hMS)
		fmt.Fprintln(w, "FEATURE\tSET\tCOUNT\tIC\tIC_T\tSharpe\tHitRate\tB/E_Bps\tAutoCorr\tAutoCorrAbs\tAvgSeg\tMaxSeg\tMeanSig\tStdSig\tMeanRet\tStdRet\tMeanPnL\tStdPnL")

		for _, name := range featNames {
			fa := features[name]
			fh := &fa.H[hIdx]

			isStats := FinalizeMetrics(fh.IS, fh.ISDailyICs)
			if isStats.Count > 0 {
				printMetricsRow(w, name, "IS", isStats)
			}

			oosStats := FinalizeMetrics(fh.OOS, fh.OOSDailyICs)
			if oosStats.Count > 0 {
				printMetricsRow(w, name, "OOS", oosStats)
			}
		}
		fmt.Fprintln(w)
	}
}

// Compute daily IC from a Moments struct (same math as Pearson).
func dailyICFromMoments(m Moments) float64 {
	if m.Count <= 1 {
		return 0
	}
	num := m.Count*m.SumProd - m.SumSig*m.SumRet
	denX := m.Count*m.SumSqSig - m.SumSig*m.SumSig
	denY := m.Count*m.SumSqRet - m.SumRet*m.SumRet
	if denX <= 0 || denY <= 0 {
		return 0
	}
	return num / math.Sqrt(denX*denY)
}

// Pretty-print one MetricStats row to the report.
func printMetricsRow(w io.Writer, feature, set string, ms MetricStats) {
	if ms.Count == 0 {
		return
	}
	fmt.Fprintf(
		w,
		"%-16s\t%s\t%d\t%.4f\t%.3f\t%.3f\t%.3f\t%.2f\t%.3f\t%.3f\t%.2f\t%.0f\t%.4f\t%.4f\t%.6f\t%.6f\t%.6f\t%.6f\n",
		feature,
		set,
		ms.Count,
		ms.ICPearson,
		ms.IC_TStat,
		ms.Sharpe,
		ms.HitRate,
		ms.BreakevenBps,
		ms.AutoCorr,
		ms.AutoCorrAbs,
		ms.AvgSegLen,
		ms.MaxSegLen,
		ms.MeanSig,
		ms.StdSig,
		ms.MeanRet,
		ms.StdRet,
		ms.MeanPnL,
		ms.StdPnL,
	)
}
