package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// --- Atomic Configuration ---

type AtomConfig struct {
	WhaleThreshold float64 // Volume threshold for iceberg detection
}

var DefaultAtoms = AtomConfig{
	WhaleThreshold: 5.0, // Lowered slightly to capture mid-sized absorption
}

const (
	EPS = 1e-9
)

type ofiTask struct {
	Y, M, D        int
	Offset, Length int64
}

// --- Execution Logic ---

func runBuild() {
	start := time.Now()

	// Pipeline: Symbols -> Build
	found := false
	for sym := range discoverSymbols() {
		found = true
		buildForSymbol(sym)
	}

	if !found {
		fmt.Printf("[build] no symbols discovered under %q\n", BaseDir)
	}
	fmt.Printf("[build] Complete in %s\n", time.Since(start))
}

func discoverSymbols() iter.Seq[string] {
	return func(yield func(string) bool) {
		entries, err := os.ReadDir(BaseDir)
		if err != nil {
			fmt.Printf("[build] ReadDir(%s): %v\n", BaseDir, err)
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if name == "features" || name == "common" || len(name) == 0 || name[0] == '.' {
				continue
			}
			if !yield(name) {
				return
			}
		}
	}
}

func buildForSymbol(sym string) {
	fmt.Printf(">>> Building %s (Enhanced Atoms v2)\n", sym)
	featRoot := filepath.Join(BaseDir, "features", sym)

	tasksCh := make(chan ofiTask, 1024)

	outDir := filepath.Join(featRoot, "Atoms_v1")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		fmt.Printf("[build] MkdirAll(%s): %v\n", outDir, err)
		return
	}

	var wg sync.WaitGroup

	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var binBuf []byte
			var gncBuf []byte
			for t := range tasksCh {
				processAtomDay(sym, t, outDir, DefaultAtoms, &binBuf, &gncBuf)
			}
		}()
	}

	count := 0
	for t := range discoverTasks(sym) {
		tasksCh <- t
		count++
	}
	close(tasksCh)

	if count == 0 {
		fmt.Printf("[build] no tasks for symbol %s\n", sym)
	}

	wg.Wait()
}

func discoverTasks(sym string) iter.Seq[ofiTask] {
	return func(yield func(ofiTask) bool) {
		root := filepath.Join(BaseDir, sym)
		years, err := os.ReadDir(root)
		if err != nil {
			return
		}

		for _, yDir := range years {
			if !yDir.IsDir() {
				continue
			}
			y, err := strconv.Atoi(yDir.Name())
			if err != nil || y <= 0 {
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
				if err != nil || m < 1 || m > 12 {
					continue
				}

				idxPath := filepath.Join(yearPath, mDir.Name(), "index.quantdev")
				f, err := os.Open(idxPath)
				if err != nil {
					continue
				}

				var hdr [16]byte
				if _, err := io.ReadFull(f, hdr[:]); err != nil {
					f.Close()
					continue
				}
				if string(hdr[0:4]) != IdxMagic {
					f.Close()
					continue
				}
				count := binary.LittleEndian.Uint64(hdr[8:])
				var row [26]byte
				for i := uint64(0); i < count; i++ {
					if _, err := io.ReadFull(f, row[:]); err != nil {
						break
					}
					d := int(binary.LittleEndian.Uint16(row[0:]))
					offset := int64(binary.LittleEndian.Uint64(row[2:]))
					length := int64(binary.LittleEndian.Uint64(row[10:]))
					if length > 0 {
						task := ofiTask{
							Y: y, M: m, D: d,
							Offset: offset, Length: length,
						}
						if !yield(task) {
							f.Close()
							return
						}
					}
				}
				f.Close()
			}
		}
	}
}

func processAtomDay(sym string, t ofiTask, outDir string, cfg AtomConfig, binBuf, gncBuf *[]byte) {
	dateStr := fmt.Sprintf("%04d%02d%02d", t.Y, t.M, t.D)
	outPath := filepath.Join(outDir, dateStr+".bin")

	gncBlob, ok := loadRawGNC(sym, t, gncBuf)
	if !ok {
		return
	}

	colsAny := DayColumnPool.Get()
	cols := colsAny.(*DayColumns)
	cols.Reset()
	defer DayColumnPool.Put(cols)

	rowCount, ok := inflateGNCToColumns(gncBlob, cols)
	if !ok || rowCount < 2 {
		return
	}

	reqSize := rowCount * FeatRowBytes
	if cap(*binBuf) < reqSize {
		*binBuf = make([]byte, reqSize)
	}
	*binBuf = (*binBuf)[:reqSize]

	times := cols.Times
	qtys := cols.Qtys
	prices := cols.Prices
	sides := cols.Sides
	matches := cols.Matches

	writeVal := func(rowIdx, atomIdx int, val float64) {
		off := rowIdx*FeatRowBytes + atomIdx*4
		binary.LittleEndian.PutUint32((*binBuf)[off:], math.Float32bits(float32(val)))
	}

	prevP := prices[0]
	// State for stateful features
	prevFlow := 0.0

	for i := 0; i < rowCount; i++ {
		q := qtys[i]
		s := float64(sides[i])
		p := prices[i]

		// Net Flow for this step
		currFlow := q * s

		m := 1.0
		if len(matches) > i {
			m = float64(matches[i])
		}

		dt := 0.0
		if i > 0 {
			dt = float64(times[i] - times[i-1])
		}
		dp := 0.0
		if i > 0 {
			dp = p - prevP
		}

		// 1. OFI (Order Flow Imbalance) - Standard
		writeVal(i, 0, currFlow)

		// 2. TCI (Trade Continuation) - Standard
		writeVal(i, 1, s)

		// 3. Whale v2: Iceberg/Absorption Detector
		// Logic: If Volume is High but Price Change is approx Zero,
		// the PASSIVE side absorbed the aggressor.
		// If Aggressor = Buy (1) and dp=0, Seller absorbed it -> Bearish (-).
		val3 := 0.0
		if q > cfg.WhaleThreshold && math.Abs(dp) < EPS {
			// Invert sign of aggressor to show who "won" (the passive wall)
			val3 = -1.0 * s * q
		}
		writeVal(i, 2, val3)

		// 4. Lumpiness (Sign Flip)
		// Old: -(q^2)*s (Inverse correlation)
		// New: (q^2)*s (Positive correlation: Buy lumps = Bullish)
		writeVal(i, 3, (q*q)*s)

		// 5. Sweep - Standard
		writeVal(i, 4, m*s)

		// 6. Fragility - Standard
		val6 := 0.0
		if q > EPS {
			val6 = (m / q) * s
		}
		writeVal(i, 5, val6)

		// 7. Magnet v2: Round Number Proximity ($100)
		// Logic: Strongest (1.0) at X00.00, decays as we move away.
		// BTC typically respects 100/500/1000 levels.
		mod := math.Mod(p, 100.0)
		if mod > 50.0 {
			mod = 100.0 - mod
		}
		// Dist is between 0 and 50.
		// Feature = 1 / (1 + dist)
		writeVal(i, 6, 1.0/(1.0+mod))

		// 8. Velocity - Standard
		vel := 0.0
		if dt > EPS {
			vel = q / dt
		}
		writeVal(i, 7, vel*s)

		// 9. Accel v2: Flow Acceleration
		// Old: Derivative of Price Velocity (Noisy)
		// New: Change in Net Flow (Force)
		accel := currFlow - prevFlow
		writeVal(i, 8, accel)

		// 10. Gap - Standard
		writeVal(i, 9, dt*s)

		// 11. DGT - Standard
		signDp := 0.0
		if dp > 0 {
			signDp = 1.0
		} else if dp < 0 {
			signDp = -1.0
		}
		val11 := 0.0
		if s == signDp {
			val11 = q * s
		}
		writeVal(i, 10, val11)

		// 12. Absorb - Standard
		val12 := 0.0
		if s != signDp {
			val12 = q * s
		}
		writeVal(i, 11, val12)

		// 13. Fractal - Standard
		val13 := 0.0
		if q > EPS {
			val13 = math.Abs(dp) / q
		}
		writeVal(i, 12, val13)

		prevP = p
		prevFlow = currFlow
	}

	if err := os.WriteFile(outPath, *binBuf, 0644); err != nil {
		fmt.Printf("[build] WriteFile(%s): %v\n", outPath, err)
	}
}

func loadRawGNC(sym string, t ofiTask, buf *[]byte) ([]byte, bool) {
	path := filepath.Join(BaseDir,
		sym,
		fmt.Sprintf("%04d", t.Y),
		fmt.Sprintf("%02d", t.M),
		"data.quantdev",
	)
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	if _, err := f.Seek(t.Offset, io.SeekStart); err != nil {
		return nil, false
	}

	if t.Length <= 0 || t.Length > 1<<31-1 {
		return nil, false
	}
	need := int(t.Length)
	if cap(*buf) < need {
		*buf = make([]byte, need)
	}
	b := (*buf)[:need]

	if _, err := io.ReadFull(f, b); err != nil {
		return nil, false
	}
	if len(b) < 4 || string(b[0:4]) != GNCMagic {
		return nil, false
	}
	return b, true
}
