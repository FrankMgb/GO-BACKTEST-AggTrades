Here’s a compact, “AI-readable” spec of the data schema your pipeline produces and consumes.

---

## 1. Directory & File Layout

Root:

* `data/`

  * `<SYMBOL>/` (e.g. `BTCUSDT/`)

    * `YYYY/`

      * `MM/`

        * `data.quantdev` – **binary GNC-v2 blobs**, concatenated by day
        * `index.quantdev` – **index** for the blobs in `data.quantdev`
  * `features/`

    * `<SYMBOL>/`

      * `TFI_v1/`

        * `YYYYMMDD.bin` – **per-day feature matrix** (ticks × 10 float32)

All downstream code assumes:

* Symbol is `Symbol()` from `common.go` (currently `"BTCUSDT"`).
* Times are in **epoch milliseconds (int64)**.
* Prices and quantities are **scaled integers** in storage, but **float64** in analysis.

---

## 2. Logical Tick Schema (DayColumns)

In-memory, all raw trade data for a day is represented as **columnar SoA**:

```go
type DayColumns struct {
    Count  int
    Times  []int64   // epoch ms
    Prices []float64 // unscaled price
    Qtys   []float64 // unscaled quantity
    Sides  []int8    // +1 = buy, -1 = sell
}
```

Semantics (per index `i`):

* `Times[i]`
  Milliseconds since Unix epoch of tick `i`.

* `Prices[i]`
  Trade price as float64, reconstructed as:
  `Prices[i] = storedPriceInt / PxScale`, where `PxScale = 1e8`.

* `Qtys[i]`
  Trade quantity as float64, reconstructed as:
  `Qtys[i] = storedQtyInt / QtScale`, where `QtScale = 1e8`.

* `Sides[i]`

  * `+1` → aggressor **buy** (taker is buying)
  * `-1` → aggressor **sell**

The **row index** across all arrays is aligned: all slices length = `Count`.

---

## 3. GNC-v2 Binary Format (`data.quantdev`)

### 3.1. Per-month index (`index.quantdev`)

Binary layout:

* Header (16 bytes):

  * `[0:4]`  – ASCII `"QIDX"` (IdxMagic)
  * `[4:8]`  – `uint32` version (IdxVersion = 1)
  * `[8:16]` – `uint64` `count` = number of days indexed

* Then `count` rows, each 26 bytes:

  * `[0:2]`   – `uint16` day of month (1–31)
  * `[2:10]`  – `uint64` byte offset into `data.quantdev`
  * `[10:18]` – `uint64` blob length in bytes
  * `[18:26]` – `uint64` checksum = first 8 bytes of `sha256(blob)`

This index is how you locate each day’s GNC-v2 blob inside `data.quantdev`.

---

### 3.2. Per-day blob (`GNC2` format, inside `data.quantdev`)

Each **blob** (for one day) is:

#### Header (32 bytes)

* `[0:4]`   – ASCII `"GNC2"` (GNCMagic)
* `[4:8]`   – `uint32` `totalRows` = total tick count in this blob
* `[8:16]`  – `int64` `baseTime` (epoch ms of the first tick of the first chunk)
* `[16:24]` – `int64` `basePrice` (scaled int: rawPrice * PxScale)
* `[24:32]` – `uint64` `footerOffset` = byte offset of the footer within this blob

#### Body

* Sequence of **chunks** at offsets listed in the footer (see below).
* Chunk layout (at `rawBlob[off:]`):

  * Header:

    * `[0:2]`   – `uint16` `n` = number of ticks in this chunk
    * `[2:10]`  – `int64` `chunkBaseT`
    * `[10:18]` – `int64` `chunkBaseP`
  * Arrays (all packed back-to-back):

    * `n` × `int32` time deltas   → `tDeltas`
    * `n` × `int32` price deltas  → `pDeltas`
    * `n` × `uint16` qty dict ids → `qIDs`
    * `ceil(n/8)` bytes side bits → `sideBits`

Reconstruction for tick `i` in this chunk:

```text
T[0] = chunkBaseT + int64(tDeltas[0])  (tDeltas[0] = 0 by convention)
P[0] = chunkBaseP + int64(pDeltas[0])  (pDeltas[0] = 0)

For i > 0:
    T[i] = T[i-1] + int64(tDeltas[i])
    P[i] = P[i-1] + int64(pDeltas[i])
```

Then:

```text
time_ms   = T[i]
price_f64 = float64(P[i]) / PxScale
qty_f64   = float64(qtyDict[qIDs[i]]) / QtScale
sideBits  = sideBits[i/8] & (1 << (i % 8)) != 0

side = +1 if bit is set (buy), else -1 (sell)
```

These get appended into `DayColumns.{Times, Prices, Qtys, Sides}`.

#### Footer (dictionary + chunk offsets)

At `footerOffset`:

* `[0:4]` – `uint32` `dictCount`
* Next `dictCount` × 8 bytes – `uint64` quantity values (scaled; `qtyDict`)
* Then:

  * `uint32` `chunkCount`
  * `chunkCount` × 4 bytes – `uint32` chunk offsets (from start of blob)

Safety checks in code verify that `footerOffset`, `dictCount`, and `chunkCount` don’t run off the end of `rawBlob`.

---

## 4. Feature Files Schema (`features/<sym>/TFI_v1/YYYYMMDD.bin`)

These are the **model features per tick**, written as a dense float32 matrix.

### 4.1. Shape & layout

For a given day:

* Let `N = rowCount` (ticks in that day, same as `DayColumns.Count`).
* Let `D = dims` = `byteSize / (N * FeatBytes)`
  (usually `D = FeatDims = 10`, but code is generic).

File content:

* `N * D` float32 values, **row-major**: all features for tick 0, then tick 1, etc.
* Stored as little-endian `float32`, no header.

Indexing:

```go
// rawSigs is []byte of size N*D*4
// dim in [0, D)
for i := 0; i < N; i++ {
    offset := (i*D + dim) * 4
    bits := binary.LittleEndian.Uint32(rawSigs[offset:])
    val := math.Float32frombits(bits)
    // val is feature[dim] at tick i
}
```

Tick alignment:

* **Row `i` in features** corresponds to **tick `i` in `DayColumns`** for that day.

### 4.2. Feature dimensions (semantic labels)

When `D <= 10`, features are labeled in this order:

1. `f1_RWVI`   – flow imbalance with exponential decay
2. `f2_CWTCI`  – convolutional signed count / “whale tape” indicator
3. `f3_BOBFI`  – short vs long window flow interaction
4. `f4_VAI`    – volume-adjusted imbalance with volatility normalization
5. `f5_TAFI`   – trend-aligned flow imbalance
6. `f6_CFRI`   – conflict-flow return indicator (trend vs flow)
7. `f7_MSCS`   – multi-scale consensus of signed flow
8. `f8_STSSI`  – skew / tail-shape statistic of signed volume
9. `f9_SAFP`   – fuzzy 1-lag auto-correlation of sign stream (approximate)
10. `f10_RCTFI` – regime/conviction composite (volatility & bar intensity)

The exact formulas are encoded in `build.go`, but for schema purposes: each dimension is a **single float32 per tick**, aligned one-to-one with raw trades.

---

## 5. High-level contract for “another AI”

If a future AI / model wants to use this data, it can:

1. **From raw ticks (GNC-v2)**:

   * Use `index.quantdev` to locate a day’s blob in `data.quantdev`.
   * Decode blob into `DayColumns` with the rules above.
   * You now have per-tick:
     `(time_ms, price, qty, side)` as `int64, float64, float64, int8`.

2. **From features**:

   * Open `features/<sym>/TFI_v1/YYYYMMDD.bin`.
   * Let `N` match the day’s tick count; `D` inferred from file length.
   * Interpret file as `N × D` float32 matrix, row-major, aligned to tick order.
   * Optional: map `dim` 0–9 → named features above.

That’s the full schema: no hidden fields, no variable-length records beyond the chunking/dictionary described here.
