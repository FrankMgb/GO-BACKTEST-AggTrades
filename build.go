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

type ofiTask struct {
	Y, M, D        int
	Offset, Length int64
}

func runBuild() {
	start := time.Now()
	found := false
	for sym := range discoverSymbols() {
		found = true
		buildForSymbol(sym)
	}
	if !found {
		fmt.Printf("[build] no symbols discovered under %q\n", filepath.Join(BaseDir))
	}
	fmt.Printf("[build] Complete in %s\n", time.Since(start))
}

func discoverSymbols() iter.Seq[string] {
	return func(yield func(string) bool) {
		entries, err := os.ReadDir(BaseDir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() && e.Name() != "features" && e.Name() != "common" && e.Name()[0] != '.' {
				if !yield(e.Name()) {
					return
				}
			}
		}
	}
}

func buildForSymbol(sym string) {
	fmt.Printf(">>> Building %s (Atoms v4: Modular)\n", sym)
	featRoot := filepath.Join(BaseDir, "features", sym)
	outDir := filepath.Join(featRoot, "Atoms_v4")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Printf("[build] MkdirAll: %v\n", err)
		return
	}

	tasksCh := make(chan ofiTask, 1024)
	var wg sync.WaitGroup

	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var binBuf []byte
			var gncBuf []byte
			for t := range tasksCh {
				processDynamicDay(sym, t, outDir, &binBuf, &gncBuf)
			}
		}()
	}

	for t := range discoverTasks(sym) {
		tasksCh <- t
	}
	close(tasksCh)
	wg.Wait()
}

func discoverTasks(sym string) iter.Seq[ofiTask] {
	return func(yield func(ofiTask) bool) {
		root := filepath.Join(BaseDir, sym)
		years, _ := os.ReadDir(root)
		for _, yDir := range years {
			if !yDir.IsDir() {
				continue
			}
			y, err := strconv.Atoi(yDir.Name())
			if err != nil {
				continue
			}
			yearPath := filepath.Join(root, yDir.Name())
			months, _ := os.ReadDir(yearPath)
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
						if !yield(ofiTask{Y: y, M: m, D: d, Offset: offset, Length: length}) {
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

func processDynamicDay(sym string, t ofiTask, outDir string, binBuf, gncBuf *[]byte) {
	// 1. Get Atoms
	atoms := GetActiveAtoms()
	numAtoms := len(atoms)
	if numAtoms == 0 {
		return
	}

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

	times := cols.Times
	qtys := cols.Qtys
	sides := cols.Sides
	prices := cols.Prices

	// 2. Prepare Header
	// Magic(4) + Count(2) + [Len(1) + Str]...
	var headerBuf []byte
	headerBuf = append(headerBuf, AtomHeaderMagic...)
	var scratch [2]byte
	binary.LittleEndian.PutUint16(scratch[:], uint16(numAtoms))
	headerBuf = append(headerBuf, scratch[:]...)

	for _, a := range atoms {
		name := a.Name()
		if len(name) > 255 {
			name = name[:255]
		}
		headerBuf = append(headerBuf, uint8(len(name)))
		headerBuf = append(headerBuf, []byte(name)...)
		a.Reset()
	}

	// 3. Alloc Buffer
	rowBytes := numAtoms * 4
	reqSize := len(headerBuf) + (rowCount * rowBytes)
	if cap(*binBuf) < reqSize {
		*binBuf = make([]byte, reqSize)
	}
	*binBuf = (*binBuf)[:reqSize]

	// 4. Write Header
	copy((*binBuf)[0:], headerBuf)
	writePtr := len(headerBuf)

	prevTime := times[0]

	for i := 0; i < rowCount; i++ {
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

		// Dynamic Update Loop
		for _, atom := range atoms {
			val := atom.Update(q, s, p, flow, dtSec)

			// Clamp
			if val > 50 {
				val = 50
			}
			if val < -50 {
				val = -50
			}

			// Inline Write
			bits := math.Float32bits(float32(val))
			(*binBuf)[writePtr] = byte(bits)
			(*binBuf)[writePtr+1] = byte(bits >> 8)
			(*binBuf)[writePtr+2] = byte(bits >> 16)
			(*binBuf)[writePtr+3] = byte(bits >> 24)
			writePtr += 4
		}
		prevTime = ts
	}

	dateStr := fmt.Sprintf("%04d%02d%02d", t.Y, t.M, t.D)
	outPath := filepath.Join(outDir, dateStr+".bin")
	if err := os.WriteFile(outPath, *binBuf, 0o644); err != nil {
		fmt.Printf("[build] Write error: %v\n", err)
	}
}

func loadRawGNC(sym string, t ofiTask, buf *[]byte) ([]byte, bool) {
	path := filepath.Join(
		BaseDir, sym,
		fmt.Sprintf("%04d", t.Y), fmt.Sprintf("%02d", t.M),
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
	need := int(t.Length)
	if cap(*buf) < need {
		*buf = make([]byte, need)
	}
	b := (*buf)[:need]
	if _, err := io.ReadFull(f, b); err != nil {
		return nil, false
	}
	if string(b[0:4]) != GNCMagic {
		return nil, false
	}
	return b, true
}
