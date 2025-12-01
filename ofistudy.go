package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// --- Study Config ---
const (
	OOSDateStr   = "2024-01-01"
	StudyThreads = 24
	StudyMaxRows = 10_000_000
)

var TimeHorizonsSec = []int{10, 30, 60, 180, 300}
var oosBoundaryYMD int

func init() {
	oosBoundaryYMD = parseOOSBoundary(OOSDateStr)
}

// DayResult carries raw Moments instead of final Stats
type DayResult struct {
	YMD     int
	Moments [][]Moments // [Variant][Horizon]
}

func runStudy() {
	startT := time.Now()
	featRoot := filepath.Join(BaseDir, "features", Symbol)

	// 1. Discover Variants
	entries, err := os.ReadDir(featRoot)
	if err != nil {
		fmt.Printf("[err] reading feature dir: %v\n", err)
		return
	}
	var variants []string
	for _, e := range entries {
		if e.IsDir() {
			variants = append(variants, e.Name())
		}
	}
	slices.Sort(variants)
	if len(variants) == 0 {
		fmt.Println("[warn] No variants found.")
		return
	}

	fmt.Printf("--- OFI STUDY | %s | %d Variants | Split: %s ---\n", Symbol, len(variants), OOSDateStr)

	// 2. Discover Common Days
	tasks := discoverStudyDays(filepath.Join(featRoot, variants[0]))
	fmt.Printf("[job] Processing %d days using %d threads.\n", len(tasks), StudyThreads)

	// 3. Global Accumulators: [Variant][Horizon]
	isAcc := make([][]Moments, len(variants))
	oosAcc := make([][]Moments, len(variants))
	for i := range variants {
		isAcc[i] = make([]Moments, len(TimeHorizonsSec))
		oosAcc[i] = make([]Moments, len(TimeHorizonsSec))
	}

	// Track Day Counts for display
	isDays := 0
	oosDays := 0

	// 4. Parallel Pipeline
	resultsChan := make(chan DayResult, 64)
	jobsChan := make(chan int, len(tasks))
	var wg sync.WaitGroup

	for i := 0; i < StudyThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Thread-local allocation
			maxRows := StudyMaxRows
			prices := make([]float64, maxRows)
			times := make([]int64, maxRows)
			sigBuf := make([]float64, maxRows)
			scratchSig := make([]float64, 0, maxRows)
			scratchRet := make([]float64, 0, maxRows)
			fileBuf := make([]byte, maxRows*8)

			for idx := range jobsChan {
				dayInt := tasks[idx]
				res := processStudyDay(
					dayInt, variants, featRoot,
					prices, times, sigBuf, fileBuf,
					&scratchSig, &scratchRet,
				)
				resultsChan <- res
			}
		}()
	}

	for i := range tasks {
		jobsChan <- i
	}
	close(jobsChan)

	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// 5. Aggregation (Main Thread)
	for res := range resultsChan {
		if len(res.Moments) == 0 {
			continue
		}
		isOOS := res.YMD >= oosBoundaryYMD

		if isOOS {
			oosDays++
		} else {
			isDays++
		}

		for vIdx := range variants {
			for hIdx := range TimeHorizonsSec {
				m := res.Moments[vIdx][hIdx]
				if m.Count > 0 {
					if isOOS {
						oosAcc[vIdx][hIdx].Add(m)
					} else {
						isAcc[vIdx][hIdx].Add(m)
					}
				}
			}
		}
	}

	// 6. Reporting
	fmt.Println()
	approxIS := isDays
	approxOOS := oosDays

	for hIdx, sec := range TimeHorizonsSec {
		printHorizonTable(sec, variants, isAcc, oosAcc, hIdx, approxIS, approxOOS)
		fmt.Println()
	}

	fmt.Printf("[study] Complete in %s\n", time.Since(startT))
}

func processStudyDay(
	dayInt int,
	variants []string,
	featRoot string,
	prices []float64,
	times []int64,
	sigBuf []float64,
	fileBuf []byte,
	scratchSig, scratchRet *[]float64,
) DayResult {
	y, m, d := dayInt/10000, (dayInt%10000)/100, dayInt%100

	res := DayResult{
		YMD:     dayInt,
		Moments: make([][]Moments, len(variants)),
	}

	rawBytes, rowCount, ok := loadRawDay(y, m, d)
	if !ok || rowCount == 0 {
		return res
	}
	n := int(rowCount)

	// Buffer management
	if n > cap(prices) {
		newCap := n + n/4
		prices = make([]float64, newCap)
		times = make([]int64, newCap)
		sigBuf = make([]float64, newCap)
	}
	if n*8 > cap(fileBuf) {
		fileBuf = make([]byte, n*8+1024)
	}
	if n > cap(*scratchSig) {
		*scratchSig = make([]float64, 0, n)
		*scratchRet = make([]float64, 0, n)
	}

	prices = prices[:n]
	times = times[:n]
	sigBuf = sigBuf[:n]

	// Parse Raw (Vectorized)
	for i := 0; i < n; i++ {
		off := i * RowSize
		prices[i] = float64(binary.LittleEndian.Uint64(rawBytes[off+8:]))
		times[i] = int64(binary.LittleEndian.Uint64(rawBytes[off+38:]))
	}

	dStr := fmt.Sprintf("%04d%02d%02d", y, m, d)

	for vIdx, v := range variants {
		sigPath := filepath.Join(featRoot, v, dStr+".bin")
		loadedSigs, ok := fastLoadFloats(sigPath, fileBuf, sigBuf)

		res.Moments[vIdx] = make([]Moments, len(TimeHorizonsSec))

		if !ok || len(loadedSigs) != n {
			continue
		}

		for hIdx, sec := range TimeHorizonsSec {
			prepSig, prepRet := prepareVectors(loadedSigs, prices, times, sec*1000, scratchSig, scratchRet)
			res.Moments[vIdx][hIdx] = CalcMoments(prepSig, prepRet)
		}
	}
	return res
}

func prepareVectors(
	sig, prices []float64,
	times []int64,
	horizonMs int,
	scratchSig, scratchRet *[]float64,
) ([]float64, []float64) {
	n := len(sig)
	vSig := (*scratchSig)[:0]
	vRet := (*scratchRet)[:0]

	j := 0
	hVal := int64(horizonMs)

	for i := 0; i < n; i++ {
		pStart := prices[i]
		s := sig[i]
		if pStart <= 0 || s == 0 {
			continue
		}

		tTarget := times[i] + hVal

		if j < i+1 {
			j = i + 1
		}
		for j < n && times[j] < tTarget {
			j++
		}
		if j >= n {
			break
		}

		pEnd := prices[j]
		if pEnd > 0 {
			vSig = append(vSig, s)
			vRet = append(vRet, (pEnd-pStart)/pStart)
		}
	}

	return vSig, vRet
}

func fastLoadFloats(path string, fileBuf []byte, outBuf []float64) ([]float64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, false
	}
	size := int(fi.Size())
	if size <= 0 || size%8 != 0 {
		return nil, false
	}
	count := size / 8

	if count > cap(outBuf) {
		outBuf = make([]float64, count)
	} else {
		outBuf = outBuf[:count]
	}

	if cap(fileBuf) < size {
		fileBuf = make([]byte, size)
	}
	fileBuf = fileBuf[:size]

	if _, err := io.ReadFull(f, fileBuf); err != nil {
		return nil, false
	}

	for i := 0; i < count; i++ {
		outBuf[i] = math.Float64frombits(binary.LittleEndian.Uint64(fileBuf[i*8:]))
	}

	return outBuf, true
}

func printHorizonTable(
	sec int,
	variants []string,
	isAcc, oosAcc [][]Moments,
	hIdx, isDays, oosDays int,
) {
	type row struct {
		Name string
		IS   MetricStats
		OOS  MetricStats
	}
	var rows []row

	for vIdx, v := range variants {
		rows = append(rows, row{
			Name: v,
			IS:   FinalizeMetrics(isAcc[vIdx][hIdx]),
			OOS:  FinalizeMetrics(oosAcc[vIdx][hIdx]),
		})
	}

	slices.SortFunc(rows, func(a, b row) int {
		if a.OOS.Sharpe > b.OOS.Sharpe {
			return -1
		}
		if a.OOS.Sharpe < b.OOS.Sharpe {
			return 1
		}
		return 0
	})

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// --- FIX: Use isDays and oosDays in the header ---
	fmt.Fprintf(w, "== Horizon %d sec [IS: %d days | OOS: %d days] ==\n", sec, isDays, oosDays)
	fmt.Fprintln(w, "VARIANT\tIS_IC\tOOS_IC\tIS_SR(Tr)\tOOS_SR(Tr)\tIS_HIT\tOOS_HIT\tIS_BE\tOOS_BE")
	fmt.Fprintln(w, "-------\t-----\t------\t---------\t----------\t------\t-------\t-----\t------")

	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%.4f\t%.4f\t%.4f\t%.4f\t%.1f%%\t%.1f%%\t%.1f\t%.1f\n",
			r.Name,
			r.IS.ICPearson, r.OOS.ICPearson,
			r.IS.Sharpe, r.OOS.Sharpe,
			r.IS.HitRate*100, r.OOS.HitRate*100,
			r.IS.BreakevenBps, r.OOS.BreakevenBps,
		)
	}
	w.Flush()
}

func discoverStudyDays(vDir string) []int {
	var days []int
	files, _ := os.ReadDir(vDir)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".bin") {
			name := strings.TrimSuffix(f.Name(), ".bin")
			if len(name) == 8 {
				if val := fastAtoi(name); val > 0 {
					days = append(days, val)
				}
			}
		}
	}
	sort.Ints(days)
	return days
}
func parseOOSBoundary(d string) int {
	return fastAtoi(d[0:4])*10000 + fastAtoi(d[5:7])*100 + fastAtoi(d[8:10])
}
func fastAtoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}
