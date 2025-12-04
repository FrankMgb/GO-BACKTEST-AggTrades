package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	HostData   = "data.binance.vision"
	S3Prefix   = "data/futures/um"
	DataSet    = "aggTrades"
	FallbackDt = "2020-01-01"
)

var (
	httpClient *http.Client
	stopEvent  atomic.Bool
	dirLocks   sync.Map
)

var errNotFound = fmt.Errorf("404")

// --- Ingestion Memory Pool ---

type IngestBuffers struct {
	Ts       []int64
	Ps       []int64
	Qs       []uint64
	Ms       []uint16
	Buys     []bool
	TDeltas  []int32
	PDeltas  []int64
	QIDs     []uint16
	SideBits []byte
}

var ingestBufferPool = sync.Pool{
	New: func() any {
		const cap = 1_000_000
		return &IngestBuffers{
			Ts:       make([]int64, 0, cap),
			Ps:       make([]int64, 0, cap),
			Qs:       make([]uint64, 0, cap),
			Ms:       make([]uint16, 0, cap),
			Buys:     make([]bool, 0, cap),
			TDeltas:  make([]int32, GNCChunkSize),
			PDeltas:  make([]int64, GNCChunkSize),
			QIDs:     make([]uint16, GNCChunkSize),
			SideBits: make([]byte, (GNCChunkSize+7)/8),
		}
	},
}

func (b *IngestBuffers) Reset() {
	b.Ts = b.Ts[:0]
	b.Ps = b.Ps[:0]
	b.Qs = b.Qs[:0]
	b.Ms = b.Ms[:0]
	b.Buys = b.Buys[:0]
}

func init() {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}
	httpClient = &http.Client{
		Transport: tr,
		Timeout:   30 * time.Second,
	}
}

func runData() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	go func() {
		<-sigChan
		stopEvent.Store(true)
		fmt.Println("\n[warn] Stopping gracefully...")
	}()

	fmt.Printf("--- GNC-v2 Ingestion (Streaming) | Symbol: %s ---\n", Symbol())

	start, err := time.Parse("2006-01-02", FallbackDt)
	if err != nil {
		fmt.Printf("[fatal] invalid FallbackDt: %v\n", err)
		return
	}

	end := time.Now().UTC().AddDate(0, 0, -1)
	var days []time.Time
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		days = append(days, d)
	}

	fmt.Printf("[job] Processing %d days using %d threads.\n", len(days), CPUThreads)

	jobs := make(chan time.Time, len(days))
	results := make(chan string, len(days))
	var wg sync.WaitGroup

	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range jobs {
				if stopEvent.Load() {
					return
				}
				results <- processDay(d)
			}
		}()
	}

	for _, d := range days {
		jobs <- d
	}
	close(jobs)
	wg.Wait()
	close(results)

	stats := make(map[string]int)
	for r := range results {
		key := strings.SplitN(r, " ", 2)[0]
		stats[key]++
	}
	fmt.Printf("\n[done] %v\n", stats)
}

func processDay(d time.Time) string {
	y, m, day := d.Year(), int(d.Month()), d.Day()

	dirPath := filepath.Join(BaseDir, Symbol(), fmt.Sprintf("%04d", y), fmt.Sprintf("%02d", m))
	idxPath := filepath.Join(dirPath, "index.quantdev")
	dataPath := filepath.Join(dirPath, "data.quantdev")

	muAny, _ := dirLocks.LoadOrStore(dirPath, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)

	mu.Lock()
	indexed := isIndexed(idxPath, day)
	mu.Unlock()

	if indexed {
		return "skip"
	}

	sym := Symbol()
	url := fmt.Sprintf("https://%s/%s/daily/%s/%s/%s-%s-%04d-%02d-%02d.zip",
		HostData, S3Prefix, DataSet, sym, sym, DataSet, y, m, day)

	// Download ZIP
	zipBytes, err := download(url)
	if err != nil {
		if err == errNotFound {
			return "missing"
		}
		return "error_dl"
	}

	// Stream Parse
	bufs := ingestBufferPool.Get().(*IngestBuffers)
	bufs.Reset()
	defer ingestBufferPool.Put(bufs)

	gncBlob, count, err := streamZipToGNCBlob(zipBytes, bufs)
	if err != nil {
		return "error_parse"
	}
	if count == 0 {
		return "empty"
	}

	sum := sha256.Sum256(gncBlob)
	cSum := binary.LittleEndian.Uint64(sum[:8])

	mu.Lock()
	defer mu.Unlock()

	if isIndexed(idxPath, day) {
		return "skip_race"
	}

	if err := os.MkdirAll(dirPath, 0755); err != nil {
		return "error_mkdir"
	}

	fData, err := os.OpenFile(dataPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return "error_io"
	}

	stat, err := fData.Stat()
	if err != nil {
		fData.Close()
		return "error_stat"
	}
	offset := stat.Size()

	if _, err := fData.Write(gncBlob); err != nil {
		fData.Close()
		return "error_write"
	}
	fData.Close()

	if err := updateIndex(idxPath, day, offset, len(gncBlob), cSum); err != nil {
		return "error_idx"
	}

	return "ok"
}

// --- Streaming ZIP -> GNC Pipeline ---

func streamZipToGNCBlob(zipData []byte, bufs *IngestBuffers) ([]byte, uint64, error) {
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, 0, err
	}

	for _, f := range r.File {
		if !strings.HasSuffix(f.Name, ".csv") {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			continue
		}

		// FIX: No defer inside loop. Explicit close after usage.
		count, err := scanCSVToBuffers(rc, bufs)
		rc.Close()

		if err != nil {
			return nil, 0, err
		}

		return encodeGNC(bufs, count)
	}
	return nil, 0, fmt.Errorf("no csv found")
}

func scanCSVToBuffers(r io.Reader, bufs *IngestBuffers) (int, error) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	var price int64
	var ts int64
	var qty uint64
	var firstId, lastId int64
	var isBuyerMaker bool

	count := 0
	firstLine := true

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		if firstLine {
			firstLine = false
			if line[0] < '0' || line[0] > '9' {
				continue
			}
		}

		col := 0
		start := 0

		price = 0
		qty = 0
		ts = 0
		firstId = 0
		lastId = 0
		isBuyerMaker = false

		for i := 0; i <= len(line); i++ {
			var c byte
			if i < len(line) {
				c = line[i]
			} else {
				c = ','
			}

			if c == ',' {
				field := line[start:i]
				switch col {
				case 1:
					price = fastParseFixed(field)
				case 2:
					qty = uint64(fastParseFixed(field))
				case 3:
					firstId = fastParseInt(field)
				case 4:
					lastId = fastParseInt(field)
				case 5:
					ts = fastParseInt(field)
				case 6:
					if len(field) > 0 && (field[0] == 't' || field[0] == 'T') {
						isBuyerMaker = true
					}
				}
				col++
				start = i + 1
			}
		}

		bufs.Ps = append(bufs.Ps, price)
		bufs.Qs = append(bufs.Qs, qty)
		bufs.Ts = append(bufs.Ts, ts)
		bufs.Buys = append(bufs.Buys, !isBuyerMaker)

		matches := int64(1)
		if lastId >= firstId {
			matches = lastId - firstId + 1
		}
		if matches > 65535 {
			matches = 65535
		}
		bufs.Ms = append(bufs.Ms, uint16(matches))

		count++
	}

	return count, scanner.Err()
}

// encodeGNC and encodeChunk ... (Standard GNC logic)
func encodeGNC(bufs *IngestBuffers, count int) ([]byte, uint64, error) {
	if count == 0 {
		return nil, 0, nil
	}

	var buf bytes.Buffer
	buf.Grow(count * 20)

	baseTime := bufs.Ts[0]
	basePrice := bufs.Ps[0]

	buf.WriteString(GNCMagic)

	var scratch [8]byte
	binary.LittleEndian.PutUint32(scratch[:4], uint32(count))
	buf.Write(scratch[:4])

	binary.LittleEndian.PutUint64(scratch[:], uint64(baseTime))
	buf.Write(scratch[:])

	binary.LittleEndian.PutUint64(scratch[:], uint64(basePrice))
	buf.Write(scratch[:])

	footerOffsetPos := buf.Len()
	binary.LittleEndian.PutUint64(scratch[:], 0)
	buf.Write(scratch[:])

	qtyDict := make(map[uint64]uint16, 1000)
	var dictLog []uint64
	chunkOffsets := make([]uint32, 0)

	for i := 0; i < count; i += GNCChunkSize {
		end := i + GNCChunkSize
		if end > count {
			end = count
		}
		chunkOffsets = append(chunkOffsets, uint32(buf.Len()))
		if err := encodeChunk(&buf, bufs, i, end, qtyDict, &dictLog); err != nil {
			return nil, 0, err
		}
	}

	footerStart := buf.Len()

	binary.LittleEndian.PutUint32(scratch[:4], uint32(len(dictLog)))
	buf.Write(scratch[:4])
	for _, q := range dictLog {
		binary.LittleEndian.PutUint64(scratch[:], q)
		buf.Write(scratch[:])
	}

	binary.LittleEndian.PutUint32(scratch[:4], uint32(len(chunkOffsets)))
	buf.Write(scratch[:4])
	for _, off := range chunkOffsets {
		binary.LittleEndian.PutUint32(scratch[:4], off)
		buf.Write(scratch[:4])
	}

	finalBytes := buf.Bytes()
	binary.LittleEndian.PutUint64(finalBytes[footerOffsetPos:], uint64(footerStart))

	return finalBytes, uint64(count), nil
}

func encodeChunk(w *bytes.Buffer, bufs *IngestBuffers, start, end int, dict map[uint64]uint16, log *[]uint64) error {
	count := end - start
	ts := bufs.Ts[start:end]
	ps := bufs.Ps[start:end]
	qs := bufs.Qs[start:end]
	ms := bufs.Ms[start:end]
	buys := bufs.Buys[start:end]

	tDeltas := bufs.TDeltas[:count]
	pDeltas := bufs.PDeltas[:count]
	qIDs := bufs.QIDs[:count]
	sideBits := bufs.SideBits[:(count+7)/8]

	for k := range sideBits {
		sideBits[k] = 0
	}

	chunkBaseT := ts[0]
	chunkBaseP := ps[0]
	var lastT, lastP int64 = chunkBaseT, chunkBaseP

	tDeltas[0] = 0
	pDeltas[0] = 0

	id, ok := dict[qs[0]]
	if !ok {
		if len(*log) >= 65535 {
			return fmt.Errorf("qty dict overflow")
		}
		id = uint16(len(*log))
		dict[qs[0]] = id
		*log = append(*log, qs[0])
	}
	qIDs[0] = id
	if buys[0] {
		sideBits[0] |= 1
	}

	for i := 1; i < count; i++ {
		dt := ts[i] - lastT
		dp := ps[i] - lastP

		if dt > 2147483647 || dt < -2147483648 {
			return fmt.Errorf("time delta overflow")
		}

		tDeltas[i] = int32(dt)
		lastT = ts[i]
		pDeltas[i] = dp
		lastP = ps[i]

		id, ok := dict[qs[i]]
		if !ok {
			if len(*log) >= 65535 {
				return fmt.Errorf("qty dict overflow")
			}
			id = uint16(len(*log))
			dict[qs[i]] = id
			*log = append(*log, qs[i])
		}
		qIDs[i] = id

		if buys[i] {
			sideBits[i/8] |= (1 << (i % 8))
		}
	}

	var head [18]byte
	binary.LittleEndian.PutUint16(head[0:], uint16(count))
	binary.LittleEndian.PutUint64(head[2:], uint64(chunkBaseT))
	binary.LittleEndian.PutUint64(head[10:], uint64(chunkBaseP))
	w.Write(head[:])

	w.Write(unsafeBytes(tDeltas))
	w.Write(unsafeBytes(pDeltas))
	w.Write(unsafeBytes(qIDs))
	w.Write(unsafeBytes(ms))
	w.Write(sideBits)

	return nil
}

func fastParseFixed(b []byte) int64 {
	var num int64
	var seenDot bool
	var dec int
	for _, c := range b {
		if c == '.' {
			seenDot = true
			continue
		}
		if c < '0' || c > '9' {
			continue
		}
		num = num*10 + int64(c-'0')
		if seenDot {
			dec++
		}
	}
	for dec < 8 {
		num *= 10
		dec++
	}
	return num
}

func fastParseInt(b []byte) int64 {
	var n int64
	for _, c := range b {
		if c < '0' || c > '9' {
			continue
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

func unsafeBytes[T any](s []T) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(s))), len(s)*int(unsafe.Sizeof(s[0])))
}

func download(url string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := httpClient.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if resp.StatusCode == 404 {
			resp.Body.Close()
			return nil, errNotFound
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return data, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("unknown dl error")
	}
	return nil, lastErr
}

func isIndexed(idxPath string, day int) bool {
	f, err := os.Open(idxPath)
	if err != nil {
		return false
	}
	defer f.Close()
	return checkIndex(f, day)
}

func checkIndex(f *os.File, day int) bool {
	var hdr [16]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return false
	}
	if string(hdr[0:4]) != IdxMagic {
		return false
	}
	count := binary.LittleEndian.Uint64(hdr[8:])
	var row [26]byte
	for i := uint64(0); i < count; i++ {
		if _, err := io.ReadFull(f, row[:]); err != nil {
			return false
		}
		if int(binary.LittleEndian.Uint16(row[0:])) == day {
			return true
		}
	}
	return false
}

func updateIndex(idxPath string, day int, offset int64, length int, csum uint64) error {
	f, err := os.OpenFile(idxPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if stat.Size() == 0 {
		var hdr [16]byte
		copy(hdr[0:], IdxMagic)
		binary.LittleEndian.PutUint32(hdr[4:], uint32(IdxVersion))
		if _, err := f.Write(hdr[:]); err != nil {
			return err
		}
	}

	if _, err := f.Seek(8, io.SeekStart); err != nil {
		return err
	}
	var count uint64
	if err := binary.Read(f, binary.LittleEndian, &count); err != nil {
		return err
	}

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	var row [26]byte
	binary.LittleEndian.PutUint16(row[0:], uint16(day))
	binary.LittleEndian.PutUint64(row[2:], uint64(offset))
	binary.LittleEndian.PutUint64(row[10:], uint64(length))
	binary.LittleEndian.PutUint64(row[18:], csum)
	if _, err := f.Write(row[:]); err != nil {
		return err
	}

	if _, err := f.Seek(8, io.SeekStart); err != nil {
		return err
	}
	return binary.Write(f, binary.LittleEndian, count+1)
}
