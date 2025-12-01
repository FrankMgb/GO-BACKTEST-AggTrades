package main

import "math"

// MetricStats holds the final calculated metrics for human consumption.
type MetricStats struct {
	Count        int
	ICPearson    float64 // Linear IC
	Sharpe       float64 // Per-trade Sharpe
	SharpeAnnual float64 // t-statistic (Sharpe * sqrt(N))
	HitRate      float64 // Directional accuracy
	BreakevenBps float64 // Cost per unit turnover to zero PnL
}

// Moments holds raw sums for global aggregation (IC, Sharpe, Turnover).
// Optimized for memory alignment on amd64.
type Moments struct {
	Count     float64
	SumSig    float64
	SumRet    float64
	SumProd   float64 // Sum(Sig * Ret)
	SumSqSig  float64
	SumSqRet  float64
	SumPnL    float64
	SumSqPnL  float64
	Hits      float64
	ValidHits float64
	Turnover  float64
}

// Add accumulates m2 into m (In-place).
func (m *Moments) Add(m2 Moments) {
	m.Count += m2.Count
	m.SumSig += m2.SumSig
	m.SumRet += m2.SumRet
	m.SumProd += m2.SumProd
	m.SumSqSig += m2.SumSqSig
	m.SumSqRet += m2.SumSqRet
	m.SumPnL += m2.SumPnL
	m.SumSqPnL += m2.SumSqPnL
	m.Hits += m2.Hits
	m.ValidHits += m2.ValidHits
	m.Turnover += m2.Turnover
}

// CalcMoments computes raw moments for a single chunk/day.
// Optimized for AVX2 pipeline (simple float operations, branch-light).
func CalcMoments(sig, ret []float64) Moments {
	n := len(sig)
	if n < 2 {
		return Moments{}
	}

	var m Moments
	m.Count = float64(n)

	// Local registers for hot loop
	var (
		sSum, rSum, sSq, rSq, prodSum float64
		pnlSum, pnlSq                 float64
		hits, valid                   float64
		turnover, prevSig             float64
	)

	prevSig = sig[0] // approximation

	for i := 0; i < n; i++ {
		s := sig[i]
		r := ret[i]

		// Pearson components
		sSum += s
		rSum += r
		sSq += s * s
		rSq += r * r
		prodSum += s * r

		// PnL components
		pnl := s * r
		pnlSum += pnl
		pnlSq += pnl * pnl

		// Hit Rate
		if s != 0 && r != 0 {
			valid++
			if (s > 0 && r > 0) || (s < 0 && r < 0) {
				hits++
			}
		}

		// Turnover
		if i > 0 {
			d := s - prevSig
			if d < 0 {
				d = -d
			}
			turnover += d
		}
		prevSig = s
	}

	m.SumSig = sSum
	m.SumRet = rSum
	m.SumSqSig = sSq
	m.SumSqRet = rSq
	m.SumProd = prodSum
	m.SumPnL = pnlSum
	m.SumSqPnL = pnlSq
	m.Hits = hits
	m.ValidHits = valid
	m.Turnover = turnover

	return m
}

// FinalizeMetrics computes the ratios from aggregated moments.
func FinalizeMetrics(m Moments) MetricStats {
	if m.Count <= 1 {
		return MetricStats{}
	}

	ms := MetricStats{Count: int(m.Count)}

	// 1. Global Pearson IC
	num := m.Count*m.SumProd - m.SumSig*m.SumRet
	denX := m.Count*m.SumSqSig - m.SumSig*m.SumSig
	denY := m.Count*m.SumSqRet - m.SumRet*m.SumRet
	if denX > 0 && denY > 0 {
		ms.ICPearson = num / math.Sqrt(denX*denY)
	}

	// 2. Per-Trade Sharpe
	meanPnL := m.SumPnL / m.Count
	varPnL := (m.SumSqPnL / m.Count) - (meanPnL * meanPnL)

	if varPnL > 0 {
		ms.Sharpe = meanPnL / math.Sqrt(varPnL)
		// Annualized via t-stat logic
		ms.SharpeAnnual = ms.Sharpe * math.Sqrt(m.Count)
	}

	// 3. Hit Rate
	if m.ValidHits > 0 {
		ms.HitRate = m.Hits / m.ValidHits
	}

	// 4. Breakeven
	if m.Turnover > 0 {
		ms.BreakevenBps = (m.SumPnL / m.Turnover) * 10000.0
	}

	return ms
}
