package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"text/tabwriter"
	"time"
)

// Global Report Accumulator
type SanityReport struct {
	Mu           sync.Mutex
	TotalMonths  int
	TotalDays    int
	TotalTrades  int64
	TotalBytes   int64
	CorruptFiles int
	MissingDays  []string // List of "YYYY-MM-DD" gaps
	Errors       []string
}

var report SanityReport

func runSanity() {
	start := time.Now()
	root := filepath.Join(BaseDir, Symbol())
	dirs, err := os.ReadDir(root)
	if err != nil {
		fmt.Printf("[sanity] ReadDir(%s): %v\n", root, err)
		return
	}

	var tasks []string
	// Discover all Month directories
	for _, y := range dirs {
		if !y.IsDir() {
			continue
		}
		yearPath := filepath.Join(root, y.Name())
		months, err := os.ReadDir(yearPath)
		if err != nil {
			fmt.Printf("[sanity] ReadDir(%s): %v\n", yearPath, err)
			continue
		}
		for _, m := range months {
			if m.IsDir() {
				tasks = append(tasks, filepath.Join(root, y.Name(), m.Name()))
			}
		}
	}

	fmt.Printf("--- SANITY CHECK: %s | %d Months Found ---\n", Symbol(), len(tasks))

	// Reset Report
	report = SanityReport{MissingDays: make([]string, 0), Errors: make([]string, 0)}
	report.TotalMonths = len(tasks)

	var wg sync.WaitGroup
	jobs := make(chan string, len(tasks))

	// Workers
	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				validateMonth(path)
			}
		}()
	}

	for _, t := range tasks {
		jobs <- t
	}
	close(jobs)
	wg.Wait()

	printSummary(time.Since(start))
}

func validateMonth(dirPath string) {
	// Parse Year/Month from path for gap detection
	// Path ends in .../YYYY/MM
	_, mStr := filepath.Split(dirPath)
	yStr := filepath.Base(filepath.Dir(dirPath))
	year := fastAtoi(yStr)
	month := fastAtoi(mStr)

	idxPath := filepath.Join(dirPath, "index.quantdev")
	dataPath := filepath.Join(dirPath, "data.quantdev")

	// Local accumulators to minimize locking
	var lTrades int64
	var lBytes int64
	var lDays int
	var lCorrupt int
	lErrors := make([]string, 0)
	presentDays := make(map[int]bool)

	// 1. Check Files Exist
	fIdx, err := os.Open(idxPath)
	if err != nil {
		report.Mu.Lock()
		report.Errors = append(report.Errors, fmt.Sprintf("MISSING IDX: %s", dirPath))
		report.Mu.Unlock()
		return
	}
	defer fIdx.Close()

	fData, err := os.Open(dataPath)
	if err != nil {
		report.Mu.Lock()
		report.Errors = append(report.Errors, fmt.Sprintf("MISSING DATA: %s", dirPath))
		report.Mu.Unlock()
		return
	}
	defer fData.Close()

	dstat, err := fData.Stat()
	if err != nil {
		report.Mu.Lock()
		report.Errors = append(report.Errors, fmt.Sprintf("STAT FAIL: %s (%v)", dataPath, err))
		report.Mu.Unlock()
		return
	}

	// 2. Validate Index Header
	var hdr [16]byte
	if _, err := io.ReadFull(fIdx, hdr[:]); err != nil {
		lErrors = append(lErrors, fmt.Sprintf("BAD IDX HDR: %s", dirPath))
		mergeReport(lTrades, lBytes, lDays, lCorrupt, lErrors, nil)
		return
	}
	if string(hdr[:4]) != IdxMagic {
		lErrors = append(lErrors, fmt.Sprintf("BAD MAGIC: %s", dirPath))
		mergeReport(lTrades, lBytes, lDays, lCorrupt, lErrors, nil)
		return
	}

	count := binary.LittleEndian.Uint64(hdr[8:])

	// 3. Iterate Days
	var row [26]byte
	for i := uint64(0); i < count; i++ {
		if _, err := io.ReadFull(fIdx, row[:]); err != nil {
			lErrors = append(lErrors, fmt.Sprintf("IDX TRUNCATED: %s", dirPath))
			break
		}

		day := int(binary.LittleEndian.Uint16(row[0:]))
		offset := int64(binary.LittleEndian.Uint64(row[2:]))
		length := int64(binary.LittleEndian.Uint64(row[10:]))
		expSum := binary.LittleEndian.Uint64(row[18:])

		presentDays[day] = true
		lDays++
		lBytes += length

		// Validate Data Blob
		if length < 32 {
			lCorrupt++
			lErrors = append(lErrors, fmt.Sprintf("Corrupt Blob (Len<32): %s Day %d", dirPath, day))
			continue
		}

		if offset < 0 || length < 0 || offset+length > dstat.Size() {
			lCorrupt++
			lErrors = append(lErrors, fmt.Sprintf("Blob exceeds file size: %s Day %d", dirPath, day))
			continue
		}

		if _, err := fData.Seek(offset, io.SeekStart); err != nil {
			lCorrupt++
			continue
		}

		// Read Header only first to check magic/count
		var blobHeader [32]byte
		if _, err := io.ReadFull(fData, blobHeader[:]); err != nil {
			lCorrupt++
			lErrors = append(lErrors, fmt.Sprintf("Read Fail: %s Day %d", dirPath, day))
			continue
		}

		if string(blobHeader[0:4]) != GNCMagic {
			lCorrupt++
			lErrors = append(lErrors, fmt.Sprintf("Bad GNC Magic: %s Day %d", dirPath, day))
			continue
		}

		tradeCount := binary.LittleEndian.Uint32(blobHeader[4:8])
		lTrades += int64(tradeCount)

		// Full Checksum (Expensive but necessary for 'Sanity')
		// Rewind to read full blob
		if _, err := fData.Seek(offset, io.SeekStart); err != nil {
			lCorrupt++
			continue
		}

		// Safety check on length alloc
		if length > 256*1024*1024 { // Cap at 256MB per day chunk for sanity
			lCorrupt++
			lErrors = append(lErrors, fmt.Sprintf("Huge Blob (%d MB): %s Day %d", length/1024/1024, dirPath, day))
			continue
		}

		blob := make([]byte, int(length))
		if _, err := io.ReadFull(fData, blob); err != nil {
			lCorrupt++
			continue
		}

		sum := sha256.Sum256(blob)
		if binary.LittleEndian.Uint64(sum[:8]) != expSum {
			lCorrupt++
			lErrors = append(lErrors, fmt.Sprintf("Checksum Mismatch: %s Day %d", dirPath, day))
		}
	}

	// 4. Gap Detection
	// Calculate valid days for this specific month/year
	expectedDays := daysInMonth(year, month)
	var missing []string

	// Don't check gaps for the current (incomplete) month if it matches today's month
	now := time.Now()
	isCurrentMonth := (now.Year() == year && int(now.Month()) == month)

	limit := expectedDays
	if isCurrentMonth {
		limit = now.Day() - 1 // Expect up to yesterday
	}

	for d := 1; d <= limit; d++ {
		if !presentDays[d] {
			missing = append(missing, fmt.Sprintf("%04d-%02d-%02d", year, month, d))
		}
	}

	mergeReport(lTrades, lBytes, lDays, lCorrupt, lErrors, missing)
}

func mergeReport(trades, bytes int64, days, corrupt int, errs []string, missing []string) {
	report.Mu.Lock()
	defer report.Mu.Unlock()

	report.TotalTrades += trades
	report.TotalBytes += bytes
	report.TotalDays += days
	report.CorruptFiles += corrupt
	if len(errs) > 0 {
		report.Errors = append(report.Errors, errs...)
	}
	if len(missing) > 0 {
		report.MissingDays = append(report.MissingDays, missing...)
	}
}

// --- Helpers ---

func daysInMonth(year, month int) int {
	// Days in month lookup
	if month == 2 {
		if isLeap(year) {
			return 29
		}
		return 28
	}
	if month == 4 || month == 6 || month == 9 || month == 11 {
		return 30
	}
	return 31
}

func isLeap(year int) bool {
	return year%4 == 0 && (year%100 != 0 || year%400 == 0)
}

func printSummary(duration time.Duration) {
	fmt.Println("\n=======================================================")
	fmt.Printf("   DATA INTEGRITY REPORT (%s)   \n", Symbol())
	fmt.Println("=======================================================")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Scan Duration:\t%s\n", duration)
	fmt.Fprintf(w, "Months Scanned:\t%d\n", report.TotalMonths)
	fmt.Fprintf(w, "Total Days:\t%d\n", report.TotalDays)
	fmt.Fprintf(w, "Total Trades:\t%s\n", fmtHumanInt(report.TotalTrades))
	fmt.Fprintf(w, "Total Size:\t%s\n", fmtHumanBytes(report.TotalBytes))
	fmt.Fprintf(w, "Corrupt files:\t%d\n", report.CorruptFiles)

	gapCount := len(report.MissingDays)
	fmt.Fprintf(w, "Missing Days:\t%d\n", gapCount)

	w.Flush()
	fmt.Println("-------------------------------------------------------")

	if gapCount > 0 {
		fmt.Println("GAPS FOUND (First 10):")
		sort.Strings(report.MissingDays)
		for i, gap := range report.MissingDays {
			if i >= 10 {
				fmt.Printf("... and %d more\n", gapCount-10)
				break
			}
			fmt.Printf(" - [MISSING] %s\n", gap)
		}
		fmt.Println("-------------------------------------------------------")
	}

	if len(report.Errors) > 0 {
		fmt.Println("CRITICAL ERRORS (First 10):")
		for i, err := range report.Errors {
			if i >= 10 {
				fmt.Printf("... and %d more\n", len(report.Errors)-10)
				break
			}
			fmt.Printf(" - [FAIL] %s\n", err)
		}
	} else if report.CorruptFiles == 0 && gapCount == 0 {
		fmt.Println(">> STATUS: GREEN (100% Integrity) <<")
	} else {
		fmt.Println(">> STATUS: AMBER (Gaps or Corruption Detected) <<")
	}
	fmt.Println("=======================================================")
}

func fmtHumanInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 1000 {
		return s
	}
	// Simple comma insertion
	var res []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			res = append(res, ',')
		}
		res = append(res, byte(c))
	}
	return string(res)
}

func fmtHumanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
