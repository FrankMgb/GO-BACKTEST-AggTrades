package main

import (
	"math"
	"sort"
)

// Consolidated OOS statistics for a single (model, horizon) pair.
type ReportStats struct {
	TrainCount int
	TestCount  int

	// Correlation / IC (OOS, test-only)
	PearsonIC  float64
	SpearmanIC float64

	// Directional accuracy (OOS)
	HitRate  float64 // fraction of non-zero returns where sign(signal) == sign(return)
	HitRateZ float64 // z-score vs 50% baseline (binomial approximation)

	// Conditional return curve (deciles, OOS)
	DecileMean         []float64 // length 10, in raw return units
	TopDecileRetBps    float64
	BottomDecileRetBps float64
	SpreadBps          float64 // TopDecile - BottomDecile (bps)

	// Information theoretic (OOS)
	MutualInfo   float64 // bits
	NormalizedMI float64 // MI / H(Y)

	// Probabilistic forecast quality (train on train, evaluate on test)
	BaselineLogLoss float64
	SignalLogLoss   float64
	DeltaLogLoss    float64 // Baseline - Signal; >0 is better

	// Economic / risk metrics for sign(signal) strategy (OOS)
	Sharpe       float64
	MaxDrawdown  float64
	AvgTrade     float64
	AvgWin       float64
	AvgLoss      float64
	WinLossRatio float64
}

// OOS rolling-window metrics on the test segment.
type WindowMetrics struct {
	StartTime float64
	EndTime   float64
	Count     int

	PearsonIC  float64
	SpearmanIC float64
	HitRate    float64
	Sharpe     float64
}

// OOS regime metrics (volatility or time-of-day on test segment).
type RegimeMetrics struct {
	Name  string
	Count int

	PearsonIC  float64
	SpearmanIC float64
	HitRate    float64
	Sharpe     float64
}

// internal helper for chronological train/test split
type trainTestSplit struct {
	TrainF []float64
	TrainR []float64

	TestT []float64
	TestF []float64
	TestR []float64
}

// AnalyzeFullSuiteOOS computes all core metrics OOS, with a single chronological
// train/test split for a given (model, horizon) signal.
func AnalyzeFullSuiteOOS(times, feats, returns []float64, trainFrac float64) ReportStats {
	s := splitTrainTest(times, feats, returns, trainFrac)
	trainN := len(s.TrainF)
	testN := len(s.TestF)

	stats := ReportStats{
		TrainCount: trainN,
		TestCount:  testN,
		DecileMean: make([]float64, 10),
	}
	if testN < 30 {
		// Too little test data to say anything meaningful.
		return stats
	}

	// 1. ICs (test-only)
	stats.PearsonIC = Pearson(s.TestF, s.TestR)
	stats.SpearmanIC = Spearman(s.TestF, s.TestR)

	// 2. Hit rate vs 50% baseline (test-only)
	stats.HitRate, stats.HitRateZ = HitRateStats(s.TestF, s.TestR)

	// 3. Conditional return curve (deciles, test-only)
	stats.DecileMean, stats.BottomDecileRetBps, stats.TopDecileRetBps, stats.SpreadBps =
		DecileCurve(s.TestF, s.TestR)

	// 4. Mutual information + NMI (test-only)
	stats.MutualInfo, stats.NormalizedMI = CalcMutualInfo(s.TestF, s.TestR, 10)

	// 5. Δ Log-loss vs baseline:
	//    - baseline LL uses test labels only
	//    - logistic parameters fit on train, LL evaluated on test
	stats.BaselineLogLoss, stats.SignalLogLoss, stats.DeltaLogLoss =
		LogLossImprovementTrainTest(s.TrainF, s.TrainR, s.TestF, s.TestR)

	// 6. Sharpe + basic risk profile (test-only)
	stats.Sharpe, stats.MaxDrawdown, stats.AvgTrade, stats.AvgWin, stats.AvgLoss, stats.WinLossRatio =
		StrategyRiskStats(s.TestF, s.TestR)

	return stats
}

// RollingWindowMetricsOOS computes OOS metrics over multiple contiguous time
// windows on the test segment (after the same train/test split).
func RollingWindowMetricsOOS(times, feats, returns []float64, trainFrac float64, windows int) []WindowMetrics {
	s := splitTrainTest(times, feats, returns, trainFrac)
	n := len(s.TestF)
	if n < 60 || windows <= 0 {
		return nil
	}

	// Require at least ~20 points per window.
	if windows > n/20 {
		windows = n / 20
	}
	if windows < 1 {
		windows = 1
	}

	wSize := n / windows
	var out []WindowMetrics

	for w := 0; w < windows; w++ {
		start := w * wSize
		end := (w + 1) * wSize
		if w == windows-1 {
			end = n
		}
		if end-start < 20 {
			continue
		}

		sig := s.TestF[start:end]
		ret := s.TestR[start:end]
		tStart := s.TestT[start]
		tEnd := s.TestT[end-1]

		hit, _ := HitRateStats(sig, ret)
		sh, _, _, _, _, _ := StrategyRiskStats(sig, ret)

		out = append(out, WindowMetrics{
			StartTime:  tStart,
			EndTime:    tEnd,
			Count:      len(sig),
			PearsonIC:  Pearson(sig, ret),
			SpearmanIC: Spearman(sig, ret),
			HitRate:    hit,
			Sharpe:     sh,
		})
	}
	return out
}

// VolRegimeMetricsOOS computes OOS metrics across volatility regimes
// (low/medium/high), based on |return| within the test segment.
func VolRegimeMetricsOOS(times, feats, returns []float64, trainFrac float64) []RegimeMetrics {
	s := splitTrainTest(times, feats, returns, trainFrac)
	n := len(s.TestR)
	if n < 60 {
		return nil
	}

	vols := make([]float64, n)
	for i := 0; i < n; i++ {
		vols[i] = math.Abs(s.TestR[i])
	}

	sorted := make([]float64, n)
	copy(sorted, vols)
	sort.Float64s(sorted)

	q1 := sorted[n/3]
	q2 := sorted[2*n/3]

	var idxLow, idxMed, idxHigh []int
	for i := 0; i < n; i++ {
		v := vols[i]
		switch {
		case v <= q1:
			idxLow = append(idxLow, i)
		case v <= q2:
			idxMed = append(idxMed, i)
		default:
			idxHigh = append(idxHigh, i)
		}
	}

	makeRegime := func(name string, idxs []int) RegimeMetrics {
		if len(idxs) < 20 {
			return RegimeMetrics{Name: name, Count: len(idxs)}
		}
		sig := make([]float64, len(idxs))
		ret := make([]float64, len(idxs))
		for j, i := range idxs {
			sig[j] = s.TestF[i]
			ret[j] = s.TestR[i]
		}
		hit, _ := HitRateStats(sig, ret)
		sh, _, _, _, _, _ := StrategyRiskStats(sig, ret)
		return RegimeMetrics{
			Name:       name,
			Count:      len(idxs),
			PearsonIC:  Pearson(sig, ret),
			SpearmanIC: Spearman(sig, ret),
			HitRate:    hit,
			Sharpe:     sh,
		}
	}

	return []RegimeMetrics{
		makeRegime("VolLow", idxLow),
		makeRegime("VolMed", idxMed),
		makeRegime("VolHigh", idxHigh),
	}
}

// TimeOfDayRegimeMetricsOOS computes OOS metrics across time-of-day regimes
// (early / mid / late) on the test segment, using ms-of-day from timestamps.
func TimeOfDayRegimeMetricsOOS(times, feats, returns []float64, trainFrac float64) []RegimeMetrics {
	s := splitTrainTest(times, feats, returns, trainFrac)
	n := len(s.TestT)
	if n < 60 {
		return nil
	}

	const dayMillis = 24 * 60 * 60 * 1000.0
	third := dayMillis / 3.0

	var earlyIdx, midIdx, lateIdx []int
	for i := 0; i < n; i++ {
		tod := math.Mod(s.TestT[i], dayMillis)
		switch {
		case tod < third:
			earlyIdx = append(earlyIdx, i)
		case tod < 2*third:
			midIdx = append(midIdx, i)
		default:
			lateIdx = append(lateIdx, i)
		}
	}

	makeRegime := func(name string, idxs []int) RegimeMetrics {
		if len(idxs) < 20 {
			return RegimeMetrics{Name: name, Count: len(idxs)}
		}
		sig := make([]float64, len(idxs))
		ret := make([]float64, len(idxs))
		for j, i := range idxs {
			sig[j] = s.TestF[i]
			ret[j] = s.TestR[i]
		}
		hit, _ := HitRateStats(sig, ret)
		sh, _, _, _, _, _ := StrategyRiskStats(sig, ret)
		return RegimeMetrics{
			Name:       name,
			Count:      len(idxs),
			PearsonIC:  Pearson(sig, ret),
			SpearmanIC: Spearman(sig, ret),
			HitRate:    hit,
			Sharpe:     sh,
		}
	}

	return []RegimeMetrics{
		makeRegime("TOD_Early", earlyIdx),
		makeRegime("TOD_Mid", midIdx),
		makeRegime("TOD_Late", lateIdx),
	}
}

// ---------------------- shared train/test split ----------------------

type parallelSorter struct {
	times, feats, rets []float64
}

func (p parallelSorter) Len() int { return len(p.times) }

func (p parallelSorter) Swap(i, j int) {
	p.times[i], p.times[j] = p.times[j], p.times[i]
	p.feats[i], p.feats[j] = p.feats[j], p.feats[i]
	p.rets[i], p.rets[j] = p.rets[j], p.rets[i]
}

func (p parallelSorter) Less(i, j int) bool {
	return p.times[i] < p.times[j]
}

func splitTrainTest(times, feats, returns []float64, trainFrac float64) trainTestSplit {
	n := len(feats)
	if n == 0 || n != len(returns) || n != len(times) {
		return trainTestSplit{}
	}
	if trainFrac <= 0 || trainFrac >= 1 {
		trainFrac = 0.7
	}

	// Sort all three slices chronologically by time in place.
	sort.Sort(parallelSorter{times: times, feats: feats, rets: returns})

	trainN := int(trainFrac * float64(n))
	if trainN < 20 {
		trainN = 20
	}
	if trainN > n-30 {
		trainN = n - 30
	}
	if trainN <= 0 || trainN >= n {
		trainN = n / 2
	}
	testN := n - trainN
	if testN <= 0 {
		return trainTestSplit{}
	}

	return trainTestSplit{
		TrainF: feats[:trainN],
		TrainR: returns[:trainN],

		TestT: times[trainN:],
		TestF: feats[trainN:],
		TestR: returns[trainN:],
	}
}

// ---------------------- Correlation / IC ----------------------

// Pearson returns the Pearson correlation coefficient between x and y.
func Pearson(x, y []float64) float64 {
	n := len(x)
	if n == 0 || n != len(y) {
		return 0
	}
	var sx, sy, sxx, syy, sxy float64
	for i := 0; i < n; i++ {
		xi := x[i]
		yi := y[i]
		sx += xi
		sy += yi
		sxx += xi * xi
		syy += yi * yi
		sxy += xi * yi
	}
	nf := float64(n)
	num := sxy - sx*sy/nf
	denx := sxx - sx*sx/nf
	deny := syy - sy*sy/nf
	if denx <= 0 || deny <= 0 {
		return 0
	}
	return num / math.Sqrt(denx*deny)
}

// Spearman rank correlation: Pearson over rank-transformed inputs.
func Spearman(x, y []float64) float64 {
	n := len(x)
	if n == 0 || n != len(y) {
		return 0
	}

	rx := rankify(x)
	ry := rankify(y)
	return Pearson(rx, ry)
}

// rankify converts values to average ranks (1..n). Ties get averaged ranks.
func rankify(vals []float64) []float64 {
	n := len(vals)
	type kv struct {
		v float64
		i int
	}
	tmp := make([]kv, n)
	for i, v := range vals {
		tmp[i] = kv{v: v, i: i}
	}
	sort.Slice(tmp, func(i, j int) bool { return tmp[i].v < tmp[j].v })

	ranks := make([]float64, n)
	var i int
	for i < n {
		j := i + 1
		for j < n && tmp[j].v == tmp[i].v {
			j++
		}
		// average rank of [i, j)
		rank := 0.5*float64(i+j-1) + 1.0
		for k := i; k < j; k++ {
			ranks[tmp[k].i] = rank
		}
		i = j
	}
	return ranks
}

// ---------------------- Hit rate / sign accuracy ----------------------

// HitRateStats computes:
//   - hit rate on non-zero returns where sign(signal) == sign(return)
//   - z-score vs 50% null hypothesis (binomial approximation).
func HitRateStats(signal, ret []float64) (hitRate, z float64) {
	n := len(signal)
	if n == 0 || n != len(ret) {
		return 0, 0
	}

	var hits, trials int
	for i := 0; i < n; i++ {
		r := ret[i]
		s := signal[i]
		if r == 0 || s == 0 {
			continue
		}
		trials++
		if (r > 0 && s > 0) || (r < 0 && s < 0) {
			hits++
		}
	}
	if trials == 0 {
		return 0, 0
	}
	hitRate = float64(hits) / float64(trials)

	// Binomial z vs p0 = 0.5
	p0 := 0.5
	variance := p0 * (1 - p0) / float64(trials)
	if variance <= 0 {
		return hitRate, 0
	}
	z = (hitRate - p0) / math.Sqrt(variance)
	return hitRate, z
}

// ---------------------- Decile curve ----------------------

// DecileCurve builds a conditional return curve by signal decile.
// Returns:
//
//	decMeans[10]       - average raw return per decile
//	bottomBps, topBps  - decile 0 and 9 in basis points
//	spreadBps          - top - bottom in basis points
func DecileCurve(signal, ret []float64) (decMeans []float64, bottomBps, topBps, spreadBps float64) {
	n := len(signal)
	if n == 0 || n != len(ret) {
		return make([]float64, 10), 0, 0, 0
	}

	type pair struct {
		s float64
		r float64
	}
	data := make([]pair, 0, n)
	for i := 0; i < n; i++ {
		data = append(data, pair{s: signal[i], r: ret[i]})
	}
	sort.Slice(data, func(i, j int) bool { return data[i].s < data[j].s })

	decMeans = make([]float64, 10)
	counts := make([]int, 10)
	if n < 10 {
		// not enough to split meaningfully
		for i := range data {
			decMeans[0] += data[i].r
			counts[0]++
		}
		if counts[0] > 0 {
			decMeans[0] /= float64(counts[0])
		}
		return decMeans, decMeans[0] * 1e4, decMeans[0] * 1e4, 0
	}

	for i := 0; i < n; i++ {
		dec := int(float64(i) / float64(n) * 10.0)
		if dec == 10 {
			dec = 9
		}
		decMeans[dec] += data[i].r
		counts[dec]++
	}
	for d := 0; d < 10; d++ {
		if counts[d] > 0 {
			decMeans[d] /= float64(counts[d])
		}
	}
	bottom := decMeans[0]
	top := decMeans[9]
	bottomBps = bottom * 1e4
	topBps = top * 1e4
	spreadBps = (top - bottom) * 1e4
	return decMeans, bottomBps, topBps, spreadBps
}

// ---------------------- Mutual information ----------------------

// CalcMutualInfo estimates MI(signal, return) in bits using a simple
// equal-frequency binning scheme. Bins must be >= 2.
func CalcMutualInfo(signal, ret []float64, bins int) (miBits, nmi float64) {
	n := len(signal)
	if n == 0 || n != len(ret) || bins < 2 {
		return 0, 0
	}

	// Quantile-based bins for signal and returns separately.
	sBin := quantileBins(signal, bins)
	rBin := quantileBins(ret, bins)

	// Joint and marginals.
	joint := make([][]float64, bins)
	for i := range joint {
		joint[i] = make([]float64, bins)
	}
	margS := make([]float64, bins)
	margR := make([]float64, bins)

	for i := 0; i < n; i++ {
		sb := sBin[i]
		rb := rBin[i]
		if sb < 0 || sb >= bins || rb < 0 || rb >= bins {
			continue
		}
		joint[sb][rb]++
		margS[sb]++
		margR[rb]++
	}

	nf := float64(n)
	for i := 0; i < bins; i++ {
		margS[i] /= nf
		margR[i] /= nf
		for j := 0; j < bins; j++ {
			joint[i][j] /= nf
		}
	}

	// Mutual information in bits.
	var mi float64
	for i := 0; i < bins; i++ {
		for j := 0; j < bins; j++ {
			p := joint[i][j]
			if p <= 0 {
				continue
			}
			px := margS[i]
			py := margR[j]
			if px <= 0 || py <= 0 {
				continue
			}
			mi += p * math.Log2(p/(px*py))
		}
	}

	// Entropy of Y (returns).
	var hy float64
	for j := 0; j < bins; j++ {
		p := margR[j]
		if p > 0 {
			hy -= p * math.Log2(p)
		}
	}
	if hy > 0 {
		nmi = mi / hy
	}
	return mi, nmi
}

// quantileBins assigns each value to a [0,bins) bin with equal counts as much
// as possible.
func quantileBins(vals []float64, bins int) []int {
	n := len(vals)
	type kv struct {
		v float64
		i int
	}
	tmp := make([]kv, n)
	for i, v := range vals {
		tmp[i] = kv{v: v, i: i}
	}
	sort.Slice(tmp, func(i, j int) bool { return tmp[i].v < tmp[j].v })

	out := make([]int, n)
	if n == 0 {
		return out
	}
	for i := 0; i < n; i++ {
		b := int(float64(i) / float64(n) * float64(bins))
		if b == bins {
			b = bins - 1
		}
		out[tmp[i].i] = b
	}
	return out
}

// ---------------------- Log-loss / logistic improvement ----------------------

// LogLossImprovementTrainTest fits a simple 1D logistic model on train
//
//	p(y>0 | f) = sigmoid(a + b * f)
//
// and compares its log-loss on test vs a constant-probability baseline.
func LogLossImprovementTrainTest(trainF, trainR, testF, testR []float64) (baseLL, signalLL, delta float64) {
	// Convert returns to binary labels: y = 1 if r > 0 else 0.
	toLabels := func(r []float64) []float64 {
		y := make([]float64, len(r))
		for i, v := range r {
			if v > 0 {
				y[i] = 1
			} else {
				y[i] = 0
			}
		}
		return y
	}
	yTrain := toLabels(trainR)
	yTest := toLabels(testR)

	if len(yTrain) == 0 || len(yTest) == 0 {
		return 0, 0, 0
	}

	// Baseline: constant probability = mean of test labels.
	var sumY float64
	for _, v := range yTest {
		sumY += v
	}
	p0 := sumY / float64(len(yTest))
	if p0 <= 0 {
		p0 = 1e-6
	}
	if p0 >= 1 {
		p0 = 1 - 1e-6
	}
	baseLL = avgLogLossConst(yTest, p0)

	// Fit 1D logistic regression on train.
	a, b := fitLogistic1D(trainF, yTrain)
	signalLL = avgLogLossLogistic(testF, yTest, a, b)

	delta = baseLL - signalLL
	return baseLL, signalLL, delta
}

func avgLogLossConst(y []float64, p float64) float64 {
	ll := 0.0
	for _, t := range y {
		if p <= 0 {
			p = 1e-6
		}
		if p >= 1 {
			p = 1 - 1e-6
		}
		if t > 0.5 {
			ll -= math.Log(p)
		} else {
			ll -= math.Log(1 - p)
		}
	}
	return ll / float64(len(y))
}

func avgLogLossLogistic(f, y []float64, a, b float64) float64 {
	n := len(f)
	if n == 0 || n != len(y) {
		return 0
	}
	ll := 0.0
	for i := 0; i < n; i++ {
		z := a + b*f[i]
		p := 1.0 / (1.0 + math.Exp(-z))
		if p <= 0 {
			p = 1e-6
		}
		if p >= 1 {
			p = 1 - 1e-6
		}
		if y[i] > 0.5 {
			ll -= math.Log(p)
		} else {
			ll -= math.Log(1 - p)
		}
	}
	return ll / float64(n)
}

// fitLogistic1D does a crude Newton-Raphson fit for a 1D logistic model.
// It is intentionally simple; we're not trying to be perfect here.
func fitLogistic1D(f, y []float64) (a, b float64) {
	n := len(f)
	if n == 0 || n != len(y) {
		return 0, 0
	}

	// Standardize features to improve conditioning.
	var meanF, varF float64
	for _, v := range f {
		meanF += v
	}
	meanF /= float64(n)
	for _, v := range f {
		d := v - meanF
		varF += d * d
	}
	if varF <= 0 {
		varF = 1
	}
	stdF := math.Sqrt(varF / float64(n))
	if stdF == 0 {
		stdF = 1
	}

	fn := make([]float64, n)
	for i := 0; i < n; i++ {
		fn[i] = (f[i] - meanF) / stdF
	}

	// Initialize params.
	a, b = 0.0, 0.0
	const iters = 25

	for iter := 0; iter < iters; iter++ {
		var g0, g1, h00, h01, h11 float64
		for i := 0; i < n; i++ {
			z := a + b*fn[i]
			p := 1.0 / (1.0 + math.Exp(-z))
			wi := p * (1 - p) // variance
			yi := y[i]

			g0 += (p - yi)
			g1 += (p - yi) * fn[i]

			h00 += wi
			h01 += wi * fn[i]
			h11 += wi * fn[i] * fn[i]
		}
		// Solve 2x2 system H * delta = -g
		det := h00*h11 - h01*h01
		if det == 0 {
			break
		}
		da := (-g0*h11 + g1*h01) / det
		db := (-g1*h00 + g0*h01) / det

		// Dampen updates.
		a += 0.5 * da
		b += 0.5 * db

		if math.Abs(da) < 1e-6 && math.Abs(db) < 1e-6 {
			break
		}
	}

	// Map back to original feature scale:
	//   z = a + b * ((f - meanF)/stdF) = (a - b*meanF/stdF) + (b/stdF)*f
	// so newA = a - b*meanF/stdF, newB = b/stdF.
	newA := a - b*meanF/stdF
	newB := b / stdF
	return newA, newB
}

// ---------------------- Strategy risk / Sharpe ----------------------

// StrategyRiskStats computes returns of a naive sign(signal) strategy:
//
//	r_strat = sign(signal) * return
//
// and then Sharpe, max drawdown, and simple trade stats.
func StrategyRiskStats(signal, ret []float64) (sharpe, maxDD, avgTrade, avgWin, avgLoss, winLoss float64) {
	n := len(signal)
	if n == 0 || n != len(ret) {
		return 0, 0, 0, 0, 0, 0
	}

	var trades []float64
	trades = trades[:0]

	for i := 0; i < n; i++ {
		s := signal[i]
		r := ret[i]
		if s == 0 || r == 0 {
			continue
		}
		var sign float64
		if s > 0 {
			sign = 1
		} else {
			sign = -1
		}
		trades = append(trades, sign*r)
	}

	m := len(trades)
	if m == 0 {
		return 0, 0, 0, 0, 0, 0
	}

	// Basic stats.
	var mean, m2 float64
	for _, x := range trades {
		mean += x
		m2 += x * x
	}
	mean /= float64(m)
	variance := m2/float64(m) - mean*mean
	if variance < 0 {
		variance = 0
	}
	std := math.Sqrt(variance)
	if std > 0 {
		sharpe = mean / std
	}

	avgTrade = mean

	// Win/loss stats + max drawdown
	var winSum, lossSum float64
	var winCount, lossCount int

	equity := 0.0
	peak := 0.0
	maxDrawdown := 0.0

	for _, x := range trades {
		if x > 0 {
			winSum += x
			winCount++
		} else if x < 0 {
			lossSum += x
			lossCount++
		}

		equity += x
		if equity > peak {
			peak = equity
		}
		dd := equity - peak
		if dd < maxDrawdown {
			maxDrawdown = dd
		}
	}

	if winCount > 0 {
		avgWin = winSum / float64(winCount)
	}
	if lossCount > 0 {
		avgLoss = lossSum / float64(lossCount)
	}
	if avgLoss != 0 {
		winLoss = math.Abs(avgWin / avgLoss)
	}

	// maxDrawdown is negative; return positive magnitude.
	return sharpe, -maxDrawdown, avgTrade, avgWin, avgLoss, winLoss
}
