package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"
	"unsafe"
)

// --- Configuration ---

const (
	OOSDateStr     = "2024-01-01"
	NumBuckets     = 5
	QuantileStride = 10
)

var TimeHorizonsMS = []int{500, 1000, 2000, 5000, 10000}
var oosBoundaryYMD int

func init() {
	oosBoundaryYMD = parseOOSBoundary(OOSDateStr)
}

type DayResult struct {
	YMD       int
	Metrics   map[string][]Moments
	Quantiles map[string]map[int][]BucketResult
}

// --- Main Logic ---

func runStudy() {
	startT := time.Now()

	found := false
	for sym := range discoverFeatureSymbols() {
		found = true
		studySymbol(sym)
	}

	if !found {
		fmt.Printf("[study] No features found in %s/features\n", BaseDir)
	} else {
		fmt.Printf("[study] ALL COMPLETE in %s\n", time.Since(startT))
	}
}

func discoverFeatureSymbols() iter.Seq[string] {
	return func(yield func(string) bool) {
		featDir := filepath.Join(BaseDir, "features")
		entries, err := os.ReadDir(featDir)
		if err != nil {
			fmt.Printf("[study] ReadDir(%s): %v\n", featDir, err)
			return
		}
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				if !yield(e.Name()) {
					return
				}
			}
		}
	}
}

func studySymbol(sym string) {
	fmt.Printf("\n>>> STUDY: %s <<<\n", sym)
	featRoot := filepath.Join(BaseDir, "features", sym)

	entries, err := os.ReadDir(featRoot)
	if err != nil {
		return
	}
	var variants []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			variants = append(variants, e.Name())
		}
	}
	slices.Sort(variants)
	if len(variants) == 0 {
		return
	}

	var tasks []int
	for d := range discoverStudyDays(filepath.Join(featRoot, variants[0])) {
		tasks = append(tasks, d)
	}
	totalTasks := len(tasks)
	fmt.Printf("Variants: %d | Days: %d\n", len(variants), totalTasks)

	isAcc := make(map[string][]Moments)
	oosAcc := make(map[string][]Moments)
	isDailyIC := make(map[string]map[int][]float64)
	oosDailyIC := make(map[string]map[int][]float64)
	isBuckets := make(map[string]map[int][]BucketAgg)
	oosBuckets := make(map[string]map[int][]BucketAgg)

	var accMu sync.Mutex
	resultsChan := make(chan DayResult, 64)
	jobsChan := make(chan int, len(tasks))
	var wg sync.WaitGroup

	var completed atomic.Int64
	doneChan := make(chan bool)

	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		start := time.Now()
		for {
			select {
			case <-doneChan:
				printProgress(int(completed.Load()), totalTasks, start)
				fmt.Println()
				return
			case <-ticker.C:
				printProgress(int(completed.Load()), totalTasks, start)
			}
		}
	}()

	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var sigBuf []float64
			var fileBuf []byte
			var retBuf []float64
			retsPerHBuf := make([][]float64, len(TimeHorizonsMS))
			var gncBuf []byte

			for idx := range jobsChan {
				dayInt := tasks[idx]
				doQuantiles := dayInt < oosBoundaryYMD
				res := processStudyDay(
					sym, dayInt, variants, featRoot,
					&sigBuf, &fileBuf, &retBuf, &retsPerHBuf, &gncBuf,
					doQuantiles,
				)
				resultsChan <- res
				completed.Add(1)
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
		close(doneChan)
	}()

	isDays, oosDays := 0, 0
	for res := range resultsChan {
		if len(res.Metrics) == 0 {
			continue
		}
		isOOS := res.YMD >= oosBoundaryYMD
		if isOOS {
			oosDays++
		} else {
			isDays++
		}

		accMu.Lock()
		for vName, moms := range res.Metrics {
			if _, ok := isAcc[vName]; !ok {
				isAcc[vName] = make([]Moments, len(TimeHorizonsMS))
				oosAcc[vName] = make([]Moments, len(TimeHorizonsMS))
				isDailyIC[vName] = make(map[int][]float64)
				oosDailyIC[vName] = make(map[int][]float64)
				isBuckets[vName] = make(map[int][]BucketAgg)
				oosBuckets[vName] = make(map[int][]BucketAgg)
			}

			tMoments := isAcc[vName]
			tDailyIC := isDailyIC[vName]
			tBuckets := isBuckets[vName]
			if isOOS {
				tMoments = oosAcc[vName]
				tDailyIC = oosDailyIC[vName]
				tBuckets = oosBuckets[vName]
			}

			for hIdx := range TimeHorizonsMS {
				m := moms[hIdx]
				if m.Count <= 0 {
					continue
				}
				tMoments[hIdx].Add(m)

				num := m.Count*m.SumProd - m.SumSig*m.SumRet
				denX := m.Count*m.SumSqSig - m.SumSig*m.SumSig
				denY := m.Count*m.SumSqRet - m.SumRet*m.SumRet
				ic := 0.0
				if denX > 0 && denY > 0 {
					ic = num / math.Sqrt(denX*denY)
				}
				tDailyIC[hIdx] = append(tDailyIC[hIdx], ic)

				if qMap, ok := res.Quantiles[vName]; ok {
					if qList, ok2 := qMap[hIdx]; ok2 {
						if len(tBuckets[hIdx]) == 0 {
							tBuckets[hIdx] = make([]BucketAgg, NumBuckets)
						}
						for i, bucket := range qList {
							if i < NumBuckets {
								tBuckets[hIdx][i].Add(bucket)
							}
						}
					}
				}
			}
		}
		accMu.Unlock()
	}

	var finalKeys []string
	for k := range isAcc {
		finalKeys = append(finalKeys, k)
	}
	sort.Strings(finalKeys)

	for hIdx, ms := range TimeHorizonsMS {
		printHorizonTable(ms, finalKeys, isAcc, oosAcc, isDailyIC, oosDailyIC, hIdx, isDays, oosDays)
		printMonotonicityTable(ms, finalKeys, isBuckets, hIdx)
		fmt.Println()
	}
}

func processStudyDay(
	sym string, dayInt int, variants []string, featRoot string,
	sigBuf *[]float64, fileBuf *[]byte, retBuf *[]float64,
	retsPerH *[][]float64, gncBuf *[]byte,
	doQuantiles bool,
) DayResult {

	y := dayInt / 10000
	m := (dayInt % 10000) / 100
	d := dayInt % 100

	res := DayResult{
		YMD:       dayInt,
		Metrics:   make(map[string][]Moments),
		Quantiles: make(map[string]map[int][]BucketResult),
	}

	colsAny := DayColumnPool.Get()
	cols := colsAny.(*DayColumns)
	cols.Reset()
	defer DayColumnPool.Put(cols)

	rowCount, ok := loadDayColumns(sym, y, m, d, cols, gncBuf)
	if !ok || rowCount == 0 {
		return res
	}
	n := rowCount

	p := cols.Prices
	tm := cols.Times
	dStr := fmt.Sprintf("%04d%02d%02d", y, m, d)

	featureNames := []string{
		"f01_OFI", "f02_TCI", "f03_Whale", "f04_Lumpiness",
		"f05_Sweep", "f06_Fragility", "f07_Magnet",
		"f08_Velocity", "f09_Accel", "f10_Gap",
		"f11_DGT", "f12_Absorb", "f13_Fractal",
	}

	for hIdx, ms := range TimeHorizonsMS {
		computeReturns(p, tm, n, ms, retBuf)
		target := (*retsPerH)[hIdx]
		if cap(target) < n {
			target = make([]float64, n+n/4)
			(*retsPerH)[hIdx] = target
		}
		target = target[:n]
		copy(target, (*retBuf)[:n])
	}

	for _, v := range variants {
		sigPath := filepath.Join(featRoot, v, dStr+".bin")

		rawSigs, byteSize, ok := fastLoadBytes(sigPath, fileBuf)
		if !ok || byteSize == 0 {
			continue
		}

		dims := byteSize / (n * FeatBytes)
		if dims < 1 || dims > FeatDims {
			continue
		}

		if n > cap(*sigBuf) {
			*sigBuf = make([]float64, n+n/4)
		}

		for dim := 0; dim < dims; dim++ {
			target := (*sigBuf)[:n]

			decodeFeatureDim(rawSigs, n, dims, dim, target)

			key := v
			if dims > 1 {
				suffix := fmt.Sprintf("_d%d", dim+1)
				if dim < len(featureNames) {
					suffix = "_" + featureNames[dim]
				}
				key = v + suffix
			}

			moms := make([]Moments, len(TimeHorizonsMS))
			var qMap map[int][]BucketResult
			if doQuantiles {
				qMap = make(map[int][]BucketResult)
			}

			for hIdx := range TimeHorizonsMS {
				rets := (*retsPerH)[hIdx][:n]
				moms[hIdx] = CalcMomentsVectors(target, rets)
				if doQuantiles {
					qMap[hIdx] = ComputeQuantilesStrided(target, rets, NumBuckets, QuantileStride)
				}
			}

			res.Metrics[key] = moms
			if doQuantiles && len(qMap) > 0 {
				res.Quantiles[key] = qMap
			}
		}
	}
	return res
}

func discoverStudyDays(vDir string) iter.Seq[int] {
	return func(yield func(int) bool) {
		files, err := os.ReadDir(vDir)
		if err != nil {
			return
		}
		var days []int
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".bin") {
				if val := fastAtoi(strings.TrimSuffix(f.Name(), ".bin")); val > 0 {
					days = append(days, val)
				}
			}
		}
		sort.Ints(days)
		for _, d := range days {
			if !yield(d) {
				return
			}
		}
	}
}

func loadDayColumns(sym string, y, m, d int, cols *DayColumns, gncBuf *[]byte) (int, bool) {
	dir := filepath.Join(BaseDir, sym, fmt.Sprintf("%04d", y), fmt.Sprintf("%02d", m))
	idxPath := filepath.Join(dir, "index.quantdev")
	dataPath := filepath.Join(dir, "data.quantdev")

	offset, length := findBlobOffset(idxPath, d)
	if length == 0 {
		return 0, false
	}

	f, err := os.Open(dataPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return 0, false
	}

	off64 := int64(offset)
	len64 := int64(length)

	if off64 < 0 || len64 <= 0 || off64+len64 > stat.Size() {
		return 0, false
	}
	// Sanity cap: 512MB
	if len64 > 512*1024*1024 {
		return 0, false
	}

	need := int(len64)
	if cap(*gncBuf) < need {
		*gncBuf = make([]byte, need)
	}
	raw := (*gncBuf)[:need]

	if _, err := f.Seek(off64, io.SeekStart); err != nil {
		return 0, false
	}
	if _, err := io.ReadFull(f, raw); err != nil {
		return 0, false
	}

	return inflateGNCToColumns(raw, cols)
}

func findBlobOffset(idxPath string, day int) (uint64, uint64) {
	f, err := os.Open(idxPath)
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	var hdr [16]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return 0, 0
	}
	if string(hdr[0:4]) != IdxMagic {
		return 0, 0
	}
	count := binary.LittleEndian.Uint64(hdr[8:])
	var row [26]byte
	for i := uint64(0); i < count; i++ {
		if _, err := io.ReadFull(f, row[:]); err != nil {
			break
		}
		if int(binary.LittleEndian.Uint16(row[0:])) == day {
			return binary.LittleEndian.Uint64(row[2:]), binary.LittleEndian.Uint64(row[10:])
		}
	}
	return 0, 0
}

func fastLoadBytes(path string, fileBuf *[]byte) ([]byte, int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, false
	}
	size := int(fi.Size())
	if size == 0 {
		return nil, 0, false
	}
	if cap(*fileBuf) < size {
		*fileBuf = make([]byte, size)
	}
	buf := (*fileBuf)[:size]
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, 0, false
	}
	return buf, size, true
}

func decodeFeatureDim(raw []byte, n, dims, dim int, out []float64) {
	// FIX: Added explicit bounds check fallback
	minBytes := n * dims * FeatBytes
	if len(raw) < minBytes || dim < 0 || dim >= dims {
		for i := 0; i < n; i++ {
			offset := (i*dims + dim) * FeatBytes
			if offset+4 > len(raw) {
				out[i] = 0
				continue
			}
			bits := binary.LittleEndian.Uint32(raw[offset:])
			out[i] = float64(math.Float32frombits(bits))
		}
		return
	}

	f32s := unsafe.Slice((*float32)(unsafe.Pointer(&raw[0])), len(raw)/4)
	for i := 0; i < n; i++ {
		out[i] = float64(f32s[i*dims+dim])
	}
}

func computeReturns(p []float64, tm []int64, n int, horizonMS int, outBuf *[]float64) {
	if n > cap(*outBuf) {
		*outBuf = make([]float64, n+n/4)
	}
	outSlice := (*outBuf)[:n]
	hVal := int64(horizonMS)
	right := 0
	for left := 0; left < n; left++ {
		targetTime := tm[left] + hVal
		if right < left {
			right = left
		}
		for right < n && tm[right] < targetTime {
			right++
		}
		if right >= n {
			for k := left; k < n; k++ {
				outSlice[k] = 0
			}
			return
		}
		pStart := p[left]
		pEnd := p[right]
		if pStart > 0 {
			outSlice[left] = (pEnd - pStart) / pStart
		} else {
			outSlice[left] = 0
		}
	}
}

func parseOOSBoundary(d string) int {
	return fastAtoi(d[0:4])*10000 + fastAtoi(d[5:7])*100 + fastAtoi(d[8:10])
}

func fastAtoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

func printProgress(curr, total int, start time.Time) {
	if total == 0 {
		return
	}
	const barWidth = 40
	percent := float64(curr) / float64(total)
	if percent > 1.0 {
		percent = 1.0
	}
	filled := int(percent * float64(barWidth))
	empty := barWidth - filled
	bar := strings.Repeat("=", filled) + strings.Repeat("-", empty)
	if filled > 0 && filled < barWidth {
		bar = bar[:filled-1] + ">" + bar[filled:]
	}
	elapsed := time.Since(start).Seconds()
	rate := 0.0
	if elapsed > 0 {
		rate = float64(curr) / elapsed
	}
	fmt.Printf("\r[%s] %.1f%% (%d/%d) | %.1f days/s  ", bar, percent*100, curr, total, rate)
}

func printHorizonTable(hMS int, keys []string, isAcc, oosAcc map[string][]Moments, isDailyIC, oosDailyIC map[string]map[int][]float64, hIdx, isDays, oosDays int) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	sec := float64(hMS) / 1000.0
	fmt.Fprintf(w, "== Horizon %.3fs [IS: %d | OOS: %d] ==\n", sec, isDays, oosDays)
	fmt.Fprintln(w, "FEATURE\tIS_IC\tIS_T\tOOS_IC\tOOS_T\tAC1\t|AC1|\tAVG_SEG\tMAX_SEG\tIS_BPS/TR\tOOS_BPS/TR")
	for _, k := range keys {
		var isICSlice, oosICSlice []float64
		if m, ok := isDailyIC[k]; ok {
			isICSlice = m[hIdx]
		}
		if m, ok := oosDailyIC[k]; ok {
			oosICSlice = m[hIdx]
		}
		isStats := FinalizeMetrics(isAcc[k][hIdx], isICSlice)
		oosStats := FinalizeMetrics(oosAcc[k][hIdx], oosICSlice)
		fmt.Fprintf(w, "%s\t%.4f\t%.2f\t%.4f\t%.2f\t%.3f\t%.3f\t%.2f\t%.1f\t%.2f\t%.2f\n",
			k,
			isStats.ICPearson, isStats.IC_TStat,
			oosStats.ICPearson, oosStats.IC_TStat,
			isStats.AutoCorr, isStats.AutoCorrAbs,
			isStats.AvgSegLen, isStats.MaxSegLen,
			isStats.BreakevenBps, oosStats.BreakevenBps,
		)
	}
	w.Flush()
}

func printMonotonicityTable(hMS int, keys []string, isBuckets map[string]map[int][]BucketAgg, hIdx int) {
	sec := float64(hMS) / 1000.0
	fmt.Printf("\n-- Monotonicity Check (IS) Horizon %.3fs --\n", sec)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
	fmt.Fprintln(w, "FEATURE\tMONO\tB1(Sell)\tB2\tB3\tB4\tB5(Buy)")
	for _, k := range keys {
		aggs, ok := isBuckets[k][hIdx]
		if !ok || len(aggs) < NumBuckets {
			continue
		}
		brets := make([]float64, NumBuckets)
		for i := 0; i < NumBuckets; i++ {
			br := aggs[i].Finalize(i + 1)
			brets[i] = br.AvgRetBps
		}
		mono := bucketMonotonicity(brets)
		fmt.Fprintf(w, "%s\t%.3f", k, mono)
		for i := 0; i < NumBuckets; i++ {
			fmt.Fprintf(w, "\t%.1f", brets[i])
		}
		fmt.Fprintln(w, "")
	}
	w.Flush()
}

func bucketMonotonicity(rets []float64) float64 {
	n := len(rets)
	if n == 0 {
		return 0
	}
	var sumX, sumY, sumXY, sumX2, sumY2 float64
	nf := float64(n)
	for i := 0; i < n; i++ {
		x := float64(i + 1)
		y := rets[i]
		sumX += x
		sumY += y
		sumXY += x * y
		sumX2 += x * x
		sumY2 += y * y
	}
	num := nf*sumXY - sumX*sumY
	denX := nf*sumX2 - sumX*sumX
	denY := nf*sumY2 - sumY*sumY
	if denX <= 0 || denY <= 0 {
		return 0
	}
	return num / math.Sqrt(denX*denY)
}
