The winner is **B_Hawkes_Adaptive** – i.e., a **2-scale Hawkes process on signed dollar flow with an activity-adaptive kernel mix and EWMA-normalized imbalance, squashed through a tanh.**

Let me spell out exactly what that “math” is.

---

## 1. Core object: signed, size-scaled dollar flow

For each trade (your `AggRow`):

* Price: `px = TradePrice(row)`
* Dollar volume: `d = TradeDollar(row)` (price × qty)
* Trade sign: `s = TradeSign(row)`

  * `+1` for taker buy, `-1` for taker sell

You convert raw aggressor flow into a **log-saturated mark**:

```go
mark := 0.0
if d > 0 && D0 > 0 {
    mark = math.Log(1.0 + d/D0)
}
```

So very large trades move intensity more than small ones, but sublinearly (log).

---

## 2. Two Hawkes excitation kernels (fast & slow)

You keep four excitation states:

* Fast: `eBuyFast`, `eSellFast`
* Slow: `eBuySlow`, `eSellSlow`

Each decays exponentially in calendar time:

```go
df := math.Exp((-1.0 / TauFast) * dtSec)
ds := math.Exp((-1.0 / TauSlow) * dtSec)

eBuyFast  *= df
eSellFast *= df
eBuySlow  *= ds
eSellSlow *= ds
```

Then you update with the signed mark:

```go
if s > 0 {
    eBuyFast  += mark
    eBuySlow  += mark
} else {
    eSellFast += mark
    eSellSlow += mark
}
```

So buys pump buy excitations, sells pump sell excitations, with different memory for fast vs slow.

---

## 3. Hawkes intensities: buy vs sell pressure

You map excitations into **directional intensities** using two 2×2 kernels (fast & slow):

Fast part:

```go
bf := MuBuy  + A_pp_fast*eBuyFast + A_pm_fast*eSellFast
sf := MuSell + A_mp_fast*eBuyFast + A_mm_fast*eSellFast
```

Slow part:

```go
bs := MuBuy  + A_pp_slow*eBuySlow + A_pm_slow*eSellSlow
ss := MuSell + A_mp_slow*eBuySlow + A_mm_slow*eSellSlow
```

Interpretation:

* `bf, bs` = intensities of **buyside** arrival (fast/slow)
* `sf, ss` = intensities of **sellside** arrival (fast/slow)

This is exactly a **bivariate Hawkes** on buy/sell order flow.

---

## 4. Activity-adaptive mixing: slow vs fast regime

You estimate current trade activity via an EWMA of `1/dt`:

```go
actEWMA = actLambda*actEWMA + (1-actLambda)*(1.0/dtSec)
```

Then turn that into a slow-weight `wSlow` using a logistic transform:

```go
x := (log(actEWMA + eps) - log(ActMid + eps)) * ActSlope
wSlow = 1.0 / (1.0 + exp(x))   // high activity → x > 0 → wSlow ↓
wSlow ∈ [0, 1]
wFast := 1.0 - wSlow
```

Finally mix fast/slow intensities:

```go
buy  := wFast*bf + wSlow*bs
sell := wFast*sf + wSlow*ss
if buy < 0  { buy = 0 }
if sell < 0 { sell = 0 }
```

So in **calm** markets you lean more on slow kernel; in **hyperactive** markets you lean more on fast kernel.

---

## 5. Imbalance → volatility-normalized → z-score → tanh

Construct **directional intensity imbalance**:

```go
imb := 0.0
if den := buy + sell; den > 1e-12 {
    imb = (buy - sell) / den  // ∈ [-1, 1]
}
```

Estimate volatility via log-return EWMA (`VolEWMA`):

```go
vol.Update(px)
sigma := vol.Sigma()
if sigma <= 0 { sigma = 1 }
```

Normalize imbalance by volatility (risk-scaling):

```go
x := imb / sigma
zVal := z.Update(x) // EWMA-based z-score
```

Finally squash to bounded trading signal:

```go
signal := math.Tanh(SquashScale * zVal)
```

This `signal` is what you write to the `.bin` and what `study` uses against future returns.

---

## 6. Why this “won” empirically

Against all the other variants you tried (plain EMA, multi-EMA, non-adaptive Hawkes), this one:

* Keeps **directional Hawkes structure** (buy vs sell excitations).
* Adds **trade-size awareness** via log-mark on dollar volume.
* Adapts **kernel timescales to activity** (fast regime on busy tape).
* Normalizes by **realized volatility** and then **z-scores** the resulting series.
* Uses a **smooth tanh squasher** to avoid insane leverage in extreme regimes.

And your results show:

* Strong, stable **OOS IC and Sharpe**, particularly at 10–30s horizons.
* OOS stat > IS stat → model is not obviously overfit.
* Breakeven cost ~16–17 bps per unit turnover across horizons.

So, the math that “won” is:

> A two-scale, activity-adaptive Hawkes model of signed, log-saturated dollar flow, volatility-normalized and z-scored, passed through a tanh squasher to produce a bounded OFI-style trading signal.

If you want, the next step could be to derive a **very small tweak** on this same structure (e.g. adjust only `TauFast`, `ActSlope`, or `SquashScale`) and reason about its likely effect on the signal *before* we touch any code.
