package main

import (
	"math"
	"slices"
)

type MetricStats struct {
	Count        int
	ICPearson    float64
	IC_TStat     float64
	Sharpe       float64
	HitRate      float64
	BreakevenBps float64
	AutoCorr     float64
	AutoCorrAbs  float64
	AvgSegLen    float64
	MaxSegLen    float64
}

type Moments struct {
	Count          float64
	SumSig         float64
	SumRet         float64
	SumProd        float64
	SumSqSig       float64
	SumSqRet       float64
	SumPnL         float64
	SumSqPnL       float64
	Hits           float64
	ValidHits      float64
	SumAbsDeltaSig float64
	SumProdLag     float64
	SumAbsSig      float64
	SumAbsProdLag  float64
	SegCount       float64
	SegLenTotal    float64
	SegLenMax      float64
}

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
	m.SumAbsDeltaSig += m2.SumAbsDeltaSig
	m.SumProdLag += m2.SumProdLag
	m.SumAbsSig += m2.SumAbsSig
	m.SumAbsProdLag += m2.SumAbsProdLag
	m.SegCount += m2.SegCount
	m.SegLenTotal += m2.SegLenTotal
	if m2.SegLenMax > m.SegLenMax {
		m.SegLenMax = m2.SegLenMax
	}
}

// CalcMomentsVectors is split into two loops to enable AVX-512 vectorization
// for the pure math portion, while isolating the branching logic.
func CalcMomentsVectors(sigs, rets []float64) Moments {
	var m Moments
	n := len(sigs)
	if n == 0 {
		return m
	}

	// --- LOOP 1: Pure Math (Vectorization Candidate) ---
	// Local accumulators to encourage register usage
	var sumSig, sumRet, sumProd, sumSqSig, sumSqRet, sumPnL, sumSqPnL, sumAbsSig float64

	// Bounds check elimination
	_ = rets[n-1]
	_ = sigs[n-1]

	for i := 0; i < n; i++ {
		s := sigs[i]
		r := rets[i]

		sumSig += s
		sumRet += r
		sumProd += s * r
		sumSqSig += s * s
		sumSqRet += r * r

		pnl := s * r
		sumPnL += pnl
		sumSqPnL += pnl * pnl

		// Branchless Abs for vectorization
		// Go 1.25 compiler recognizes this pattern for float abs
		absS := s
		if s < 0 {
			absS = -s
		}
		sumAbsSig += absS
	}

	m.Count = float64(n)
	m.SumSig = sumSig
	m.SumRet = sumRet
	m.SumProd = sumProd
	m.SumSqSig = sumSqSig
	m.SumSqRet = sumSqRet
	m.SumPnL = sumPnL
	m.SumSqPnL = sumSqPnL
	m.SumAbsSig = sumAbsSig

	// --- LOOP 2: Logic & Dependencies (Scalar/Branchy) ---
	var prevSig float64
	var prevSign float64
	var curSegLen float64

	var hits, validHits, sumAbsDelta, sumProdLag, sumAbsProdLag float64
	var segCount, segLenTotal, segLenMax float64

	for i := 0; i < n; i++ {
		s := sigs[i]
		r := rets[i]

		// Hit Logic
		if s != 0 && r != 0 {
			validHits++
			if (s > 0 && r > 0) || (s < 0 && r < 0) {
				hits++
			}
		}

		// Lag Logic
		if i > 0 {
			d := s - prevSig
			if d < 0 {
				d = -d
			}
			sumAbsDelta += d
			sumProdLag += s * prevSig

			absPrev := prevSig
			if absPrev < 0 {
				absPrev = -absPrev
			}
			absS := s
			if absS < 0 {
				absS = -absS
			}
			sumAbsProdLag += absS * absPrev
		}

		// Segmentation Logic
		sign := 0.0
		if s > 0 {
			sign = 1.0
		} else if s < 0 {
			sign = -1.0
		}

		if sign != 0 {
			if prevSign == sign {
				curSegLen++
			} else {
				if curSegLen > 0 {
					segCount++
					segLenTotal += curSegLen
					if curSegLen > segLenMax {
						segLenMax = curSegLen
					}
				}
				curSegLen = 1
			}
		} else {
			if curSegLen > 0 {
				segCount++
				segLenTotal += curSegLen
				if curSegLen > segLenMax {
					segLenMax = curSegLen
				}
				curSegLen = 0
			}
		}
		prevSig = s
		prevSign = sign
	}

	// Final segment close
	if curSegLen > 0 {
		segCount++
		segLenTotal += curSegLen
		if curSegLen > segLenMax {
			segLenMax = curSegLen
		}
	}

	m.Hits = hits
	m.ValidHits = validHits
	m.SumAbsDeltaSig = sumAbsDelta
	m.SumProdLag = sumProdLag
	m.SumAbsProdLag = sumAbsProdLag
	m.SegCount = segCount
	m.SegLenTotal = segLenTotal
	m.SegLenMax = segLenMax

	return m
}

func FinalizeMetrics(m Moments, dailyICs []float64) MetricStats {
	if m.Count <= 1 {
		return MetricStats{Count: int(m.Count)}
	}
	ms := MetricStats{Count: int(m.Count)}

	num := m.Count*m.SumProd - m.SumSig*m.SumRet
	denX := m.Count*m.SumSqSig - m.SumSig*m.SumSig
	denY := m.Count*m.SumSqRet - m.SumRet*m.SumRet
	if denX > 0 && denY > 0 {
		ms.ICPearson = num / math.Sqrt(denX*denY)
	}

	meanPnL := m.SumPnL / m.Count
	varPnL := (m.SumSqPnL / m.Count) - meanPnL*meanPnL
	if varPnL > 1e-18 {
		ms.Sharpe = meanPnL / math.Sqrt(varPnL)
	}

	if m.ValidHits > 0 {
		ms.HitRate = m.Hits / m.ValidHits
	}
	if m.SumAbsDeltaSig > 1e-18 {
		ms.BreakevenBps = (m.SumPnL / m.SumAbsDeltaSig) * 10000.0
	}

	meanSig := m.SumSig / m.Count
	covLag := (m.SumProdLag / m.Count) - meanSig*meanSig
	varSig := (m.SumSqSig / m.Count) - meanSig*meanSig
	if varSig > 1e-18 {
		ms.AutoCorr = covLag / varSig
	}

	if m.Count > 0 {
		meanAbs := m.SumAbsSig / m.Count
		covAbs := (m.SumAbsProdLag / m.Count) - meanAbs*meanAbs
		varAbs := (m.SumSqSig / m.Count) - meanAbs*meanAbs
		if varAbs > 1e-18 {
			ms.AutoCorrAbs = covAbs / varAbs
		}
	}

	if m.SegCount > 0 {
		ms.AvgSegLen = m.SegLenTotal / m.SegCount
	}
	ms.MaxSegLen = m.SegLenMax

	if len(dailyICs) > 1 {
		var sum, sumSq float64
		n := float64(len(dailyICs))
		for _, v := range dailyICs {
			sum += v
			sumSq += v * v
		}
		mean := sum / n
		variance := (sumSq / n) - mean*mean
		if variance > 1e-18 {
			stdDev := math.Sqrt(variance)
			ms.IC_TStat = mean / (stdDev / math.Sqrt(n))
		}
	}
	return ms
}

type BucketResult struct {
	ID        int
	AvgSig    float64
	AvgRetBps float64
	Count     int
}

func ComputeQuantilesStrided(sigs, rets []float64, numBuckets, stride int) []BucketResult {
	n := len(sigs)
	if n == 0 || numBuckets <= 0 {
		return nil
	}

	estSize := n / stride
	type pair struct{ s, r float64 }
	pairs := make([]pair, 0, estSize)

	for i := 0; i < n; i += stride {
		pairs = append(pairs, pair{s: sigs[i], r: rets[i]})
	}

	if len(pairs) == 0 {
		return nil
	}

	slices.SortFunc(pairs, func(a, b pair) int {
		if a.s < b.s {
			return -1
		}
		if a.s > b.s {
			return 1
		}
		return 0
	})

	subN := len(pairs)
	results := make([]BucketResult, numBuckets)
	bucketSize := subN / numBuckets
	if bucketSize == 0 {
		bucketSize = 1
	}

	for b := 0; b < numBuckets; b++ {
		start := b * bucketSize
		end := start + bucketSize
		if b == numBuckets-1 || end > subN {
			end = subN
		}

		var sumS, sumR float64
		count := 0
		for i := start; i < end; i++ {
			sumS += pairs[i].s
			sumR += pairs[i].r
			count++
		}
		if count > 0 {
			results[b] = BucketResult{
				ID:        b + 1,
				AvgSig:    sumS / float64(count),
				AvgRetBps: (sumR / float64(count)) * 10000.0,
				Count:     count * stride,
			}
		}
	}
	return results
}

type BucketAgg struct {
	Count     int
	SumSig    float64
	SumRetBps float64
}

func (ba *BucketAgg) Add(br BucketResult) {
	if br.Count <= 0 {
		return
	}
	ba.Count += br.Count
	ba.SumSig += br.AvgSig * float64(br.Count)
	ba.SumRetBps += br.AvgRetBps * float64(br.Count)
}

func (ba BucketAgg) Finalize(id int) BucketResult {
	if ba.Count == 0 {
		return BucketResult{ID: id}
	}
	den := float64(ba.Count)
	return BucketResult{
		ID:        id,
		AvgSig:    ba.SumSig / den,
		AvgRetBps: ba.SumRetBps / den,
		Count:     ba.Count,
	}
}
