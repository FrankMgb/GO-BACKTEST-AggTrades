package main

import (
	"encoding/binary"
	"sync"
	"unique"
	"unsafe"
)

// --- Shared Configuration ---

const (
	// Ryzen 9 7900X: 12 Cores / 24 Threads.
	CPUThreads = 24
	BaseDir    = "data"

	// Binary Layout Constants (GNC-v2)
	PxScale = 100_000_000.0
	QtScale = 100_000_000.0

	// GNC Chunking
	GNCChunkSize = 65536

	// Magic Headers
	GNCMagic      = "GNC2"
	GNCHeaderSize = 32
	IdxMagic      = "QIDX"
	IdxVersion    = 1

	// Feature layout on disk (13 Canonical Atoms)
	FeatDims     = 13
	FeatBytes    = 4
	FeatRowBytes = FeatDims * FeatBytes
)

// Intern the symbol to keep it in L3 cache.
var SymbolHandle = unique.Make("ETHUSDT")

func Symbol() string { return SymbolHandle.Value() }

// --- OPTIMIZED DATA SCHEMA (SoA) ---

type DayColumns struct {
	Count   int
	Times   []int64   // epoch ms
	Prices  []float64 // scaled
	Qtys    []float64 // scaled
	Sides   []int8    // 1, -1
	Matches []uint16  // M_t

	// Internal scratch buffers to prevent allocations during inflate
	ScratchQtyDict      []float64
	ScratchChunkOffsets []uint32
}

func (c *DayColumns) Reset() {
	c.Count = 0
	// We do not release memory, just reset length to 0
	c.Times = c.Times[:0]
	c.Prices = c.Prices[:0]
	c.Qtys = c.Qtys[:0]
	c.Sides = c.Sides[:0]
	c.Matches = c.Matches[:0]

	// Reset scratch buffers
	c.ScratchQtyDict = c.ScratchQtyDict[:0]
	c.ScratchChunkOffsets = c.ScratchChunkOffsets[:0]
}

var DayColumnPool = sync.Pool{
	New: func() any {
		return &DayColumns{
			Times:               make([]int64, 0, 65536),
			Prices:              make([]float64, 0, 65536),
			Qtys:                make([]float64, 0, 65536),
			Sides:               make([]int8, 0, 65536),
			Matches:             make([]uint16, 0, 65536),
			ScratchQtyDict:      make([]float64, 0, 1024),
			ScratchChunkOffsets: make([]uint32, 0, 128),
		}
	},
}

// --- Shared GNC Decoder ---

func inflateGNCToColumns(rawBlob []byte, cols *DayColumns) (int, bool) {
	if len(rawBlob) < GNCHeaderSize {
		return 0, false
	}
	if string(rawBlob[0:4]) != GNCMagic {
		return 0, false
	}

	totalRows := int(binary.LittleEndian.Uint32(rawBlob[4:8]))
	if totalRows <= 0 {
		cols.Reset()
		return 0, true
	}

	// 1. Ensure Capacity (One-time allocation check)
	if cap(cols.Times) < totalRows {
		cols.Times = make([]int64, 0, totalRows)
	}
	if cap(cols.Prices) < totalRows {
		cols.Prices = make([]float64, 0, totalRows)
	}
	if cap(cols.Qtys) < totalRows {
		cols.Qtys = make([]float64, 0, totalRows)
	}
	if cap(cols.Sides) < totalRows {
		cols.Sides = make([]int8, 0, totalRows)
	}
	if cap(cols.Matches) < totalRows {
		cols.Matches = make([]uint16, 0, totalRows)
	}

	// 2. Parse Footer for Dictionary and Chunk Offsets
	footerOffset := binary.LittleEndian.Uint64(rawBlob[24:32])
	if footerOffset >= uint64(len(rawBlob)) {
		return 0, false
	}

	dictBlob := rawBlob[footerOffset:]
	if len(dictBlob) < 4 {
		return 0, false
	}

	dictCount := binary.LittleEndian.Uint32(dictBlob[0:4])
	ptr := 4

	if uint64(ptr)+uint64(dictCount)*8+4 > uint64(len(dictBlob)) {
		return 0, false
	}

	// Use Scratch Buffer for Dictionary (No Alloc)
	if cap(cols.ScratchQtyDict) < int(dictCount) {
		cols.ScratchQtyDict = make([]float64, 0, int(dictCount))
	}
	qtyDict := cols.ScratchQtyDict[:dictCount]

	for i := 0; i < int(dictCount); i++ {
		qRaw := binary.LittleEndian.Uint64(dictBlob[ptr : ptr+8])
		qtyDict[i] = float64(qRaw) / QtScale
		ptr += 8
	}

	if len(dictBlob) < ptr+4 {
		return 0, false
	}
	chunkCount := binary.LittleEndian.Uint32(dictBlob[ptr : ptr+4])
	ptr += 4

	if uint64(ptr)+uint64(chunkCount)*4 > uint64(len(dictBlob)) {
		return 0, false
	}

	// Use Scratch Buffer for Offsets (No Alloc)
	if cap(cols.ScratchChunkOffsets) < int(chunkCount) {
		cols.ScratchChunkOffsets = make([]uint32, 0, int(chunkCount))
	}
	chunkOffsets := cols.ScratchChunkOffsets[:chunkCount]

	for i := 0; i < int(chunkCount); i++ {
		chunkOffsets[i] = binary.LittleEndian.Uint32(dictBlob[ptr : ptr+4])
		ptr += 4
	}

	// 3. Process Chunks
	for _, off := range chunkOffsets {
		if uint64(off)+18 > uint64(len(rawBlob)) {
			return 0, false
		}
		chunk := rawBlob[off:]
		n := int(binary.LittleEndian.Uint16(chunk[0:2]))
		baseT := int64(binary.LittleEndian.Uint64(chunk[2:10]))
		baseP := int64(binary.LittleEndian.Uint64(chunk[10:18]))

		// Offsets in chunk
		pTime := 18
		pPrice := pTime + n*4
		pQty := pPrice + n*8 // int64 price deltas
		pMatches := pQty + n*2
		pSide := pMatches + n*2

		// Backward compatibility
		hasMatches := true
		if pSide > len(chunk) {
			pSideLegacy := pQty + n*2
			if pSideLegacy <= len(chunk) {
				hasMatches = false
				pSide = pSideLegacy
			} else {
				return 0, false
			}
		}

		// Unsafe slicing to avoid copying data from the blob
		tDeltas := unsafe.Slice((*int32)(unsafe.Pointer(&chunk[pTime])), n)
		pDeltas := unsafe.Slice((*int64)(unsafe.Pointer(&chunk[pPrice])), n)
		qIDs := unsafe.Slice((*uint16)(unsafe.Pointer(&chunk[pQty])), n)

		var ms []uint16
		if hasMatches {
			ms = unsafe.Slice((*uint16)(unsafe.Pointer(&chunk[pMatches])), n)
		}

		sideBits := chunk[pSide:]
		if len(sideBits) < (n+7)/8 {
			return 0, false
		}

		lastT := baseT
		lastP := baseP

		// Hot loop: Unroll or keep simple for bounds-check elimination
		for i := 0; i < n; i++ {
			lastT += int64(tDeltas[i])
			lastP += pDeltas[i]

			cols.Times = append(cols.Times, lastT)
			cols.Prices = append(cols.Prices, float64(lastP)/PxScale)

			qID := int(qIDs[i])
			// Bounds check elimination hint: dict is fixed
			if qID < len(qtyDict) {
				cols.Qtys = append(cols.Qtys, qtyDict[qID])
			} else {
				cols.Qtys = append(cols.Qtys, 0)
			}

			if hasMatches {
				cols.Matches = append(cols.Matches, ms[i])
			} else {
				cols.Matches = append(cols.Matches, 1)
			}

			bitByte := sideBits[i/8]
			isBuy := (bitByte & (1 << (i % 8))) != 0
			side := int8(-1)
			if isBuy {
				side = 1
			}
			cols.Sides = append(cols.Sides, side)
		}
	}

	cols.Count = len(cols.Prices)
	return cols.Count, true
}
