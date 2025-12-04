package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"
	"unsafe"
)

const (
	OOSDateStr = "2024-01-01"
)

var TimeHorizonsMS = []int{
	500,   // 0.5s - Sniper
	1000,  // 1s
	2000,  // 2s
	5000,  // 5s
	10000, // 10s
}

var oosBoundaryYMD int

func init() {
	oosBoundaryYMD = parseOOSBoundary(OOSDateStr)
}

type DayResult struct {
	YMD     int
	Metrics map[string][]Moments
}

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
	entries, _ := os.ReadDir(featRoot)
	var variants []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			variants = append(variants, e.Name())
		}
	}
	if len(variants) == 0 {
		return
	}

	// Discovery
	var tasks []int
	for d := range discoverStudyDays(filepath.Join(featRoot, variants[0])) {
		tasks = append(tasks, d)
	}
	totalTasks := len(tasks)
	fmt.Printf("Variants: %d | Days: %d\n", len(variants), totalTasks)

	isAcc := make(map[string][]Moments)
	oosAcc := make(map[string][]Moments)
	var accMu sync.Mutex

	resultsChan := make(chan DayResult, 64)
	jobsChan := make(chan int, len(tasks))
	var wg sync.WaitGroup
	var completed atomic.Int64
	doneChan := make(chan bool)

	go func() {
		ticker := time.NewTicker(1000 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-doneChan:
				return
			case <-ticker.C:
				printProgress(int(completed.Load()), totalTasks)
			}
		}
	}()

	for i := 0; i < 24; i++ {
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
				res := processStudyDay(sym, dayInt, variants, featRoot, &sigBuf, &fileBuf, &retBuf, &retsPerHBuf, &gncBuf)
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

	for res := range resultsChan {
		isOOS := res.YMD >= oosBoundaryYMD
		accMu.Lock()
		for vName, moms := range res.Metrics {
			if _, ok := isAcc[vName]; !ok {
				isAcc[vName] = make([]Moments, len(TimeHorizonsMS))
				oosAcc[vName] = make([]Moments, len(TimeHorizonsMS))
			}
			target := isAcc[vName]
			if isOOS {
				target = oosAcc[vName]
			}
			for hIdx := range TimeHorizonsMS {
				target[hIdx].Add(moms[hIdx])
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
		printHorizonTable(ms, finalKeys, isAcc, oosAcc, hIdx)
	}
}

func processStudyDay(
	sym string, dayInt int, variants []string, featRoot string,
	sigBuf *[]float64, fileBuf *[]byte, retBuf *[]float64,
	retsPerH *[][]float64, gncBuf *[]byte,
) DayResult {
	y, m, d := dayInt/10000, (dayInt%10000)/100, dayInt%100
	res := DayResult{
		YMD:     dayInt,
		Metrics: make(map[string][]Moments),
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

	p, tm := cols.Prices, cols.Times
	dStr := fmt.Sprintf("%04d%02d%02d", y, m, d)

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

		// --- HEADER PARSING ---
		if len(rawSigs) < 6 || string(rawSigs[0:4]) != AtomHeaderMagic {
			continue // wrong magic
		}

		ptr := 4
		numFeats := int(binary.LittleEndian.Uint16(rawSigs[ptr:]))
		ptr += 2

		featureNames := make([]string, numFeats)
		for i := 0; i < numFeats; i++ {
			if ptr >= len(rawSigs) {
				break
			}
			nameLen := int(rawSigs[ptr])
			ptr++
			if ptr+nameLen > len(rawSigs) {
				break
			}
			featureNames[i] = string(rawSigs[ptr : ptr+nameLen])
			ptr += nameLen
		}

		// Data starts at ptr
		dataBlob := rawSigs[ptr:]
		dims := numFeats

		if n > cap(*sigBuf) {
			*sigBuf = make([]float64, n+n/4)
		}

		for dim := 0; dim < dims; dim++ {
			target := (*sigBuf)[:n]
			decodeFeatureDim(dataBlob, n, dims, dim, target)

			key := v
			if dim < len(featureNames) {
				key = featureNames[dim]
			}
			moms := make([]Moments, len(TimeHorizonsMS))
			for hIdx := range TimeHorizonsMS {
				moms[hIdx] = CalcMomentsVectors(target, (*retsPerH)[hIdx][:n])
			}
			res.Metrics[key] = moms
		}
	}
	return res
}

func printHorizonTable(hMS int, keys []string, isAcc, oosAcc map[string][]Moments, hIdx int) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	sec := float64(hMS) / 1000.0
	fmt.Fprintf(w, "\n== Horizon %.3fs ==\n", sec)
	fmt.Fprintln(w, "FEATURE\tIS_IC\tOOS_IC\tIS_BPS\tOOS_BPS")
	for _, k := range keys {
		mIS := isAcc[k][hIdx]
		mOOS := oosAcc[k][hIdx]

		icIS, icOOS := 0.0, 0.0
		if mIS.Count > 0 {
			num := mIS.Count*mIS.SumProd - mIS.SumSig*mIS.SumRet
			den := math.Sqrt((mIS.Count*mIS.SumSqSig - mIS.SumSig*mIS.SumSig) * (mIS.Count*mIS.SumSqRet - mIS.SumRet*mIS.SumRet))
			if den > 0 {
				icIS = num / den
			}
		}
		if mOOS.Count > 0 {
			num := mOOS.Count*mOOS.SumProd - mOOS.SumSig*mOOS.SumRet
			den := math.Sqrt((mOOS.Count*mOOS.SumSqSig - mOOS.SumSig*mOOS.SumSig) * (mOOS.Count*mOOS.SumSqRet - mOOS.SumRet*mOOS.SumRet))
			if den > 0 {
				icOOS = num / den
			}
		}

		bpsIS := 0.0
		if mIS.SumAbsDeltaSig > 0 {
			bpsIS = (mIS.SumPnL / mIS.SumAbsDeltaSig) * 10000.0
		}
		bpsOOS := 0.0
		if mOOS.SumAbsDeltaSig > 0 {
			bpsOOS = (mOOS.SumPnL / mOOS.SumAbsDeltaSig) * 10000.0
		}

		fmt.Fprintf(w, "%s\t%.4f\t%.4f\t%.2f\t%.2f\n", k, icIS, icOOS, bpsIS, bpsOOS)
	}
	w.Flush()
}

// --- HELPERS ---

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
	// Re-using same logic, simplified for self-contained file
	minBytes := n * dims * 4
	if len(raw) < minBytes || dim < 0 || dim >= dims {
		for i := 0; i < n; i++ {
			out[i] = 0
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

func printProgress(curr, total int) {
	if total == 0 {
		return
	}
	percent := float64(curr) / float64(total)
	fmt.Printf("\rProgress: %.1f%% (%d/%d)", percent*100, curr, total)
}
