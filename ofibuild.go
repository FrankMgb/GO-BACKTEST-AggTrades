package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// --- Build Configuration ---

// Hint for per-thread output buffer reservation; real days can exceed this and
// the buffer is grown as needed.
const BuildMaxRows = 10_000_000

// --- Main Builder (Single Champion Variant) ---

func runBuild() {
	start := time.Now()

	featRoot := filepath.Join(BaseDir, "features", Symbol)
	variantID := "B_Hawkes_Adaptive"

	fmt.Printf("--- FEATURE BUILDER | %s | 1 Variants ---\n", Symbol)
	fmt.Printf("Output: %s\n", featRoot)

	// Ensure variant directory exists.
	if err := os.MkdirAll(filepath.Join(featRoot, variantID), 0755); err != nil {
		fmt.Printf("[err] mkdir %s: %v\n", featRoot, err)
		return
	}

	// Discover all (Y,M,D) with data.
	tasks := discoverTasks()
	fmt.Printf("[build] Processing %d days.\n", len(tasks))

	workerCount := CPUThreads
	if workerCount < 1 {
		workerCount = 1
	}
	fmt.Printf("[build] Using %d threads.\n", workerCount)

	// Champion config (same as before).
	cfg := HawkesAdaptiveConfig{
		HawkesCfg: Hawkes2ScaleConfig{
			TauFast: 2, TauSlow: 300,
			MuBuy:  0.1,
			MuSell: 0.1,

			A_pp_fast: 1.2, A_pm_fast: 0.0,
			A_mp_fast: 0.0, A_mm_fast: 1.2,

			A_pp_slow: 0.3, A_pm_slow: 0.1,
			A_mp_slow: 0.1, A_mm_slow: 0.3,

			D0:           50_000,
			VolLambda:    0.999,
			ZScoreLambda: 0.9999,
			SquashScale:  2.0,
		},
		ActivityLambda: 0.99,
		ActMid:         15.0,
		ActSlope:       2.0,
	}

	jobs := make(chan ofiTask, len(tasks))
	var wg sync.WaitGroup

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Per-thread reusable output buffer.
			binBuf := make([]byte, 0, BuildMaxRows*8)

			for t := range jobs {
				processBuildDay(t, featRoot, variantID, cfg, &binBuf)
			}
		}()
	}

	for _, t := range tasks {
		jobs <- t
	}
	close(jobs)
	wg.Wait()

	fmt.Printf("[build] Complete in %s\n", time.Since(start))
}

// processBuildDay: load raw AGG3 once, run champion model over all rows.
func processBuildDay(
	t ofiTask,
	root string,
	variantID string,
	cfg HawkesAdaptiveConfig,
	binBuf *[]byte,
) {
	rawBytes, rowCount, ok := loadRawDay(t.Y, t.M, t.D)
	if !ok || rowCount == 0 {
		return
	}

	n := int(rowCount)
	dateStr := fmt.Sprintf("%04d%02d%02d", t.Y, t.M, t.D)
	outPath := filepath.Join(root, variantID, dateStr+".bin")

	// Skip if already built.
	if _, err := os.Stat(outPath); err == nil {
		return
	}

	// Ensure buffer capacity.
	reqSize := n * 8
	if cap(*binBuf) < reqSize {
		*binBuf = make([]byte, reqSize)
	}
	*binBuf = (*binBuf)[:reqSize]

	// Concrete model instance (no interface dispatch).
	st := NewHawkesAdaptiveState(cfg)

	// Hot loop: parse row, update model, write float64.
	for i := 0; i < n; i++ {
		off := i * RowSize
		row := ParseAggRow(rawBytes[off : off+RowSize])

		sig := st.Update(row)

		binary.LittleEndian.PutUint64((*binBuf)[i*8:], math.Float64bits(sig))
	}

	if err := os.WriteFile(outPath, *binBuf, 0644); err != nil {
		fmt.Printf("[err] write %s: %v\n", outPath, err)
	}
}

// --- Data Loader (Same Binary Layout as data.go) ---

// loadRawDay decompresses one day's AGG3 blob and returns the raw rows buffer
// (header stripped) and rowCount.
func loadRawDay(y, m, d int) ([]byte, uint64, bool) {
	dir := filepath.Join(BaseDir, Symbol, fmt.Sprintf("%04d", y), fmt.Sprintf("%02d", m))
	idxPath := filepath.Join(dir, "index.quantdev")
	dataPath := filepath.Join(dir, "data.quantdev")

	offset, length := findBlobOffset(idxPath, d)
	if length == 0 {
		return nil, 0, false
	}

	f, err := os.Open(dataPath)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()

	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
		return nil, 0, false
	}

	compData := make([]byte, length)
	if _, err := io.ReadFull(f, compData); err != nil {
		return nil, 0, false
	}

	r, err := zlib.NewReader(bytes.NewReader(compData))
	if err != nil {
		return nil, 0, false
	}
	raw, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		return nil, 0, false
	}

	if len(raw) < HeaderSize {
		return nil, 0, false
	}

	rowCount := binary.LittleEndian.Uint64(raw[8:])
	return raw[HeaderSize:], rowCount, true
}

// findBlobOffset reads the index file and returns (offset,length) for day d.
func findBlobOffset(idxPath string, day int) (uint64, uint64) {
	f, err := os.Open(idxPath)
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	hdr := make([]byte, 16)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return 0, 0
	}
	if string(hdr[:4]) != IdxMagic {
		return 0, 0
	}

	count := binary.LittleEndian.Uint64(hdr[8:])
	row := make([]byte, 26)

	for i := uint64(0); i < count; i++ {
		if _, err := io.ReadFull(f, row); err != nil {
			break
		}
		if int(binary.LittleEndian.Uint16(row[0:])) == day {
			return binary.LittleEndian.Uint64(row[2:]), binary.LittleEndian.Uint64(row[10:])
		}
	}
	return 0, 0
}

// discoverTasks enumerates all (Y,M,D) from existing index files.
func discoverTasks() []ofiTask {
	root := filepath.Join(BaseDir, Symbol)
	var tasks []ofiTask

	years, err := os.ReadDir(root)
	if err != nil {
		return tasks
	}

	for _, yDir := range years {
		if !yDir.IsDir() {
			continue
		}
		y, err := strconv.Atoi(yDir.Name())
		if err != nil || y <= 0 {
			continue
		}

		months, err := os.ReadDir(filepath.Join(root, yDir.Name()))
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

			idxPath := filepath.Join(root, yDir.Name(), mDir.Name(), "index.quantdev")
			f, err := os.Open(idxPath)
			if err != nil {
				continue
			}

			hdr := make([]byte, 16)
			if _, err := io.ReadFull(f, hdr); err != nil {
				f.Close()
				continue
			}
			if string(hdr[:4]) != IdxMagic {
				f.Close()
				continue
			}

			count := binary.LittleEndian.Uint64(hdr[8:])
			row := make([]byte, 26)
			for i := uint64(0); i < count; i++ {
				if _, err := io.ReadFull(f, row); err != nil {
					break
				}
				d := int(binary.LittleEndian.Uint16(row[0:]))
				if d >= 1 && d <= 31 {
					tasks = append(tasks, ofiTask{Y: y, M: m, D: d})
				}
			}
			f.Close()
		}
	}

	return tasks
}

type ofiTask struct{ Y, M, D int }

// --- Champion Model: Hawkes Adaptive ---------------------------------------

// Hawkes2ScaleConfig: shared kernel parameters.
type Hawkes2ScaleConfig struct {
	TauFast, TauSlow, MuBuy, MuSell            float64
	A_pp_fast, A_pm_fast, A_mp_fast, A_mm_fast float64
	A_pp_slow, A_pm_slow, A_mp_slow, A_mm_slow float64
	D0, VolLambda, ZScoreLambda, SquashScale   float64
}

type HawkesAdaptiveConfig struct {
	HawkesCfg                        Hawkes2ScaleConfig
	ActivityLambda, ActMid, ActSlope float64
}

type HawkesAdaptiveState struct {
	base                                         Hawkes2ScaleConfig
	lastTsMs                                     int64
	eBuyFast, eSellFast                          float64
	eBuySlow, eSellSlow                          float64
	actLambda, actEWMA, actMid, actSlope, squash float64
	vol                                          VolEWMA
	z                                            ZScoreEWMA
}

func NewHawkesAdaptiveState(cfg HawkesAdaptiveConfig) *HawkesAdaptiveState {
	return &HawkesAdaptiveState{
		base:      cfg.HawkesCfg,
		actLambda: cfg.ActivityLambda,
		actMid:    cfg.ActMid,
		actSlope:  cfg.ActSlope,
		squash:    cfg.HawkesCfg.SquashScale,
		vol:       VolEWMA{Lambda: cfg.HawkesCfg.VolLambda},
		z:         ZScoreEWMA{Lambda: cfg.HawkesCfg.ZScoreLambda},
	}
}

func (st *HawkesAdaptiveState) Update(row AggRow) float64 {
	ts := row.TsMs
	d := TradeDollar(row)
	s := TradeSign(row)
	px := TradePrice(row)

	// dt in seconds
	var dtSec float64
	if st.lastTsMs == 0 {
		st.lastTsMs = ts
		dtSec = 0
	} else {
		dtSec = float64(ts-st.lastTsMs) / 1000.0
		if dtSec < 0 {
			dtSec = 0
		}
		st.lastTsMs = ts
	}

	// Activity EWMA + Hawkes decay
	if dtSec > 0 {
		st.actEWMA = st.actLambda*st.actEWMA + (1-st.actLambda)*(1.0/dtSec)

		df := math.Exp((-1.0 / st.base.TauFast) * dtSec)
		ds := math.Exp((-1.0 / st.base.TauSlow) * dtSec)

		st.eBuyFast *= df
		st.eSellFast *= df
		st.eBuySlow *= ds
		st.eSellSlow *= ds
	}

	// Log-saturated mark on dollar flow (uses Log1p for precision).
	mark := 0.0
	if d > 0 && st.base.D0 > 0 {
		mark = math.Log1p(d / st.base.D0)
	}

	// Signed excitation update.
	if s > 0 {
		st.eBuyFast += mark
		st.eBuySlow += mark
	} else {
		st.eSellFast += mark
		st.eSellSlow += mark
	}

	// Fast kernel intensities.
	bf := st.base.MuBuy + st.base.A_pp_fast*st.eBuyFast + st.base.A_pm_fast*st.eSellFast
	sf := st.base.MuSell + st.base.A_mp_fast*st.eBuyFast + st.base.A_mm_fast*st.eSellFast

	// Slow kernel intensities.
	bs := st.base.MuBuy + st.base.A_pp_slow*st.eBuySlow + st.base.A_pm_slow*st.eSellSlow
	ss := st.base.MuSell + st.base.A_mp_slow*st.eBuySlow + st.base.A_mm_slow*st.eSellSlow

	// Activity-based slow weight (higher activity -> lower slow weight).
	wSlow := 0.5
	if st.actEWMA > 0 {
		x := (math.Log(st.actEWMA+1e-9) - math.Log(st.actMid+1e-9)) * st.actSlope
		wSlow = 1.0 / (1.0 + math.Exp(x))
	}
	if wSlow < 0 {
		wSlow = 0
	} else if wSlow > 1 {
		wSlow = 1
	}
	wFast := 1.0 - wSlow

	buy := wFast*bf + wSlow*bs
	sell := wFast*sf + wSlow*ss
	if buy < 0 {
		buy = 0
	}
	if sell < 0 {
		sell = 0
	}

	// Directional imbalance.
	imb := 0.0
	if den := buy + sell; den > 1e-12 {
		imb = (buy - sell) / den
	}

	// Vol normalization and z-scoring.
	st.vol.Update(px)
	sigma := st.vol.Sigma()
	if sigma <= 0 {
		sigma = 1
	}

	zVal := st.z.Update(imb / sigma)
	return Squash(zVal, st.squash)
}

// --- Shared EWMA Helpers ----------------------------------------------------

type VolEWMA struct {
	Lambda  float64
	VarEwma float64
	LastPx  float64
	HasLast bool
}

func (v *VolEWMA) Update(price float64) {
	if !v.HasLast {
		v.LastPx = price
		v.HasLast = true
		return
	}
	if price <= 0 {
		return
	}
	r := math.Log(price / v.LastPx)
	v.LastPx = price
	v.VarEwma = v.Lambda*v.VarEwma + (1-v.Lambda)*r*r
}

func (v *VolEWMA) Sigma() float64 {
	if v.VarEwma <= 0 {
		return 0
	}
	return math.Sqrt(v.VarEwma)
}

type ZScoreEWMA struct {
	Lambda float64
	Mean   float64
	Var    float64
	Warmed bool
}

func (z *ZScoreEWMA) Update(x float64) float64 {
	if !z.Warmed {
		z.Mean = x
		z.Var = 0
		z.Warmed = true
		return 0
	}
	mPrev := z.Mean
	z.Mean = z.Lambda*z.Mean + (1-z.Lambda)*x
	dx := x - mPrev
	z.Var = z.Lambda*z.Var + (1-z.Lambda)*dx*dx
	if z.Var <= 0 {
		return 0
	}
	return (x - z.Mean) / math.Sqrt(z.Var)
}

func Squash(x, scale float64) float64 {
	return math.Tanh(scale * x)
}
