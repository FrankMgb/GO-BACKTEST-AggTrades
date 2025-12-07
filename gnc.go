package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"unsafe"
)

// These constants MUST match the downloader project.
const (
	IdxMagic = "QIDX" // index.quantdev magic

	TBMagic   = "TBV1" // trade-block magic
	TBVersion = 1
	TBHdrSize = 64

	// Zen4 cache line size (and alignment unit for columns).
	CacheLine = 64
)

// --- TBV1 header + zero-copy TradeBlock view ---

type tbHeader struct {
	Rows     uint64
	OffAgg   uint32
	OffPrice uint32
	OffQty   uint32
	OffFirst uint32
	OffLast  uint32
	OffTime  uint32
	OffBits  uint32
	BitWords uint64
}

// parseTBHeader validates header + bounds and returns layout info.
func parseTBHeader(hdr []byte, blobLen uint64) (tbHeader, error) {
	var h tbHeader
	if len(hdr) < TBHdrSize {
		return h, fmt.Errorf("header too short")
	}
	if string(hdr[0:4]) != TBMagic {
		return h, fmt.Errorf("magic mismatch")
	}
	v := binary.LittleEndian.Uint32(hdr[4:8])
	if v != TBVersion {
		return h, fmt.Errorf("version mismatch: %d", v)
	}

	rows := binary.LittleEndian.Uint64(hdr[8:16])
	if rows == 0 {
		return h, fmt.Errorf("zero rows")
	}
	h.Rows = rows
	h.OffAgg = binary.LittleEndian.Uint32(hdr[16:20])
	h.OffPrice = binary.LittleEndian.Uint32(hdr[20:24])
	h.OffQty = binary.LittleEndian.Uint32(hdr[24:28])
	h.OffFirst = binary.LittleEndian.Uint32(hdr[28:32])
	h.OffLast = binary.LittleEndian.Uint32(hdr[32:36])
	h.OffTime = binary.LittleEndian.Uint32(hdr[36:40])
	h.OffBits = binary.LittleEndian.Uint32(hdr[40:44])

	if blobLen < uint64(TBHdrSize) {
		return h, fmt.Errorf("blob too small")
	}

	offs := []uint32{
		h.OffAgg, h.OffPrice, h.OffQty,
		h.OffFirst, h.OffLast, h.OffTime, h.OffBits,
	}
	for _, off := range offs {
		if off < TBHdrSize {
			return h, fmt.Errorf("offset %d < header size", off)
		}
		// Enforce the intended 64-byte alignment for all columns.
		if off%CacheLine != 0 {
			return h, fmt.Errorf("offset %d not %d-byte aligned", off, CacheLine)
		}
	}

	validateCol := func(off uint32, elemSize uint64) error {
		end := uint64(off) + rows*elemSize
		if end > blobLen {
			return fmt.Errorf("column out of range (off=%d)", off)
		}
		return nil
	}

	if err := validateCol(h.OffAgg, 8); err != nil {
		return h, err
	}
	if err := validateCol(h.OffPrice, 8); err != nil {
		return h, err
	}
	if err := validateCol(h.OffQty, 8); err != nil {
		return h, err
	}
	if err := validateCol(h.OffFirst, 8); err != nil {
		return h, err
	}
	if err := validateCol(h.OffLast, 8); err != nil {
		return h, err
	}
	if err := validateCol(h.OffTime, 8); err != nil {
		return h, err
	}

	bitWords := (rows + 63) / 64
	if bitWords == 0 {
		return h, fmt.Errorf("invalid bitWords")
	}
	bitsEnd := uint64(h.OffBits) + bitWords*8
	if bitsEnd > blobLen {
		return h, fmt.Errorf("bitset out of range")
	}
	h.BitWords = bitWords
	return h, nil
}

// TradeBlock is a zero-copy view over a TBV1 blob.
type TradeBlock struct {
	Count int

	AggTradeIDs   []uint64
	Prices        []float64
	Quantities    []float64
	FirstTradeIDs []uint64
	LastTradeIDs  []uint64
	Times         []int64

	BuyerBits []uint64
}

// mapTradeBlock creates a view over raw blob without extra allocations.
func mapTradeBlock(raw []byte) (*TradeBlock, error) {
	h, err := parseTBHeader(raw, uint64(len(raw)))
	if err != nil {
		return nil, err
	}
	if len(raw) < TBHdrSize {
		return nil, fmt.Errorf("short blob")
	}

	count := int(h.Rows)
	if count < 0 {
		return nil, fmt.Errorf("negative count")
	}

	tb := &TradeBlock{Count: count}
	base := unsafe.Pointer(&raw[0])

	tb.AggTradeIDs = unsafe.Slice((*uint64)(unsafe.Add(base, uintptr(h.OffAgg))), count)
	tb.Prices = unsafe.Slice((*float64)(unsafe.Add(base, uintptr(h.OffPrice))), count)
	tb.Quantities = unsafe.Slice((*float64)(unsafe.Add(base, uintptr(h.OffQty))), count)
	tb.FirstTradeIDs = unsafe.Slice((*uint64)(unsafe.Add(base, uintptr(h.OffFirst))), count)
	tb.LastTradeIDs = unsafe.Slice((*uint64)(unsafe.Add(base, uintptr(h.OffLast))), count)
	tb.Times = unsafe.Slice((*int64)(unsafe.Add(base, uintptr(h.OffTime))), count)
	tb.BuyerBits = unsafe.Slice((*uint64)(unsafe.Add(base, uintptr(h.OffBits))), int(h.BitWords))

	return tb, nil
}

// IsBuyerMaker checks the boolean bitset efficiently.
func (tb *TradeBlock) IsBuyerMaker(i int) bool {
	if i < 0 || i >= tb.Count {
		return false
	}
	wordIdx := i / 64
	bitIdx := i % 64
	return (tb.BuyerBits[wordIdx] & (1 << bitIdx)) != 0
}

// --- DayColumns (simple SoA view used by RunStream) ---

// DayColumns is the SoA representation of a single day's trades,
// used by the streaming feature engine. Right now we only need
// time/price/quantity for the continuous models.
type DayColumns struct {
	Count  int
	Times  []int64
	Prices []float64
	Qtys   []float64
}

// DayColumnPool reduces allocation pressure (critical for GOGC=200).
var DayColumnPool = sync.Pool{
	New: func() any {
		// Pre-allocate for ~1.5M rows (typical busy day).
		const initCap = 1_500_000
		return &DayColumns{
			Times:  make([]int64, 0, initCap),
			Prices: make([]float64, 0, initCap),
			Qtys:   make([]float64, 0, initCap),
		}
	},
}

// Reset clears the struct for reuse without freeing memory.
func (c *DayColumns) Reset() {
	c.Count = 0
	c.Times = c.Times[:0]
	c.Prices = c.Prices[:0]
	c.Qtys = c.Qtys[:0]
}

// FillFromTradeBlock copies the TBV1 SoA into the DayColumns view.
func (c *DayColumns) FillFromTradeBlock(tb *TradeBlock) {
	c.Reset()
	n := tb.Count
	if n == 0 {
		return
	}

	if cap(c.Times) < n {
		c.Times = make([]int64, n)
		c.Prices = make([]float64, n)
		c.Qtys = make([]float64, n)
	} else {
		c.Times = c.Times[:n]
		c.Prices = c.Prices[:n]
		c.Qtys = c.Qtys[:n]
	}

	copy(c.Times, tb.Times)
	copy(c.Prices, tb.Prices)
	copy(c.Qtys, tb.Quantities)

	c.Count = n
}

// ofiTask identifies a single day (year, month, day) for one symbol.
type ofiTask struct {
	Year, Month, Day int
}

// LoadGNCFile locates and reads a single TBV1 blob for (sym, day) into buf.
// Returns false on any error or if the day is not present in the index.
//
// NOTE: Name kept as LoadGNCFile for API compatibility with existing code;
// it now actually loads a TBV1 trade-block blob.
func LoadGNCFile(baseDir, sym string, t ofiTask, buf *[]byte) bool {
	dir := filepath.Join(baseDir, sym, sprintfYear(t.Year), sprintfMonth(t.Month))
	idxPath := filepath.Join(dir, "index.quantdev")
	dataPath := filepath.Join(dir, "data.quantdev")

	offset, length := findBlobOffset(idxPath, t.Day)
	if length == 0 {
		return false
	}

	// Safety check: prevent panic if index is corrupted and length is massive.
	// 512MB is a reasonable upper bound for a single day's blob.
	if length > 512*1024*1024 {
		return false
	}

	f, err := os.Open(dataPath)
	if err != nil {
		return false
	}
	defer f.Close()

	if cap(*buf) < int(length) {
		*buf = make([]byte, length)
	}
	*buf = (*buf)[:length]

	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
		return false
	}
	if _, err := io.ReadFull(f, *buf); err != nil {
		return false
	}
	return true
}

// InflateGNC decodes a TBV1 blob into DayColumns by mapping the TradeBlock
// and copying just the SoA slices we care about (time, price, qty).
//
// Signature is kept as (int, error) for compatibility with the previous code.
func InflateGNC(rawBlob []byte, cols *DayColumns) (int, error) {
	cols.Reset()

	tb, err := mapTradeBlock(rawBlob)
	if err != nil {
		return 0, err
	}
	if tb.Count == 0 {
		return 0, nil
	}

	cols.FillFromTradeBlock(tb)
	return cols.Count, nil
}

// --- Discovery helpers over the TBV1 index tree ---

// discoverSymbols yields all symbols (top-level dirs) under BaseDir.
func discoverSymbols() iter.Seq[string] {
	return func(yield func(string) bool) {
		entries, _ := os.ReadDir(BaseDir)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if len(name) == 0 || name[0] == '.' || name == "features" {
				continue
			}
			if !yield(name) {
				return
			}
		}
	}
}

// discoverTasks yields all (year, month, day) tasks for a symbol.
// Reads 26-byte index rows: Day[2] + Offset[8] + Length[8] + Checksum[8].
func discoverTasks(sym string) iter.Seq[ofiTask] {
	return func(yield func(ofiTask) bool) {
		root := filepath.Join(BaseDir, sym)
		years, err := os.ReadDir(root)
		if err != nil {
			return
		}
		for _, y := range years {
			if !y.IsDir() || len(y.Name()) != 4 {
				continue
			}
			year, err := strconv.Atoi(y.Name())
			if err != nil {
				continue
			}

			months, err := os.ReadDir(filepath.Join(root, y.Name()))
			if err != nil {
				continue
			}
			for _, m := range months {
				if !m.IsDir() || len(m.Name()) != 2 {
					continue
				}
				month, err := strconv.Atoi(m.Name())
				if err != nil {
					continue
				}

				idxPath := filepath.Join(root, y.Name(), m.Name(), "index.quantdev")
				f, err := os.Open(idxPath)
				if err != nil {
					continue
				}

				var hdr [16]byte
				if _, err := io.ReadFull(f, hdr[:]); err == nil && string(hdr[0:4]) == IdxMagic {
					count := binary.LittleEndian.Uint64(hdr[8:16])
					var row [26]byte
					for i := uint64(0); i < count; i++ {
						if _, err := io.ReadFull(f, row[:]); err != nil {
							break
						}
						day := int(binary.LittleEndian.Uint16(row[0:2]))
						if !yield(ofiTask{year, month, day}) {
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

// findBlobOffset scans a single index.quantdev for a given day.
func findBlobOffset(idxPath string, day int) (uint64, uint64) {
	f, err := os.Open(idxPath)
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	var hdr [16]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil || string(hdr[0:4]) != IdxMagic {
		return 0, 0
	}
	count := binary.LittleEndian.Uint64(hdr[8:16])

	var row [26]byte
	for i := uint64(0); i < count; i++ {
		if _, err := io.ReadFull(f, row[:]); err != nil {
			return 0, 0
		}
		if int(binary.LittleEndian.Uint16(row[0:2])) == day {
			offset := binary.LittleEndian.Uint64(row[2:10])
			length := binary.LittleEndian.Uint64(row[10:18])
			return offset, length
		}
	}
	return 0, 0
}

func sprintfYear(y int) string  { return strconv.Itoa(y) }
func sprintfMonth(m int) string { return sprintf2(m) }

func sprintf2(x int) string {
	if x < 10 && x >= 0 {
		return "0" + strconv.Itoa(x)
	}
	return strconv.Itoa(x)
}
