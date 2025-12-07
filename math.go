package main

import (
	"math"
)

// ContinuousModel defines a physics object that updates on dt/price/volume.
type ContinuousModel interface {
	Name() string
	Reset()
	Update(dt float64, p, v float64) float64
}

// ============================================================================
// 1. Baseline Hawkes_Intensity (keep as-is; this is your proven baseline)
// ============================================================================

type ModelHawkesIntensity struct {
	intensity float64
	alpha     float64
	beta      float64
}

func NewHawkesIntensity() *ModelHawkesIntensity {
	// Original parameters that produced your baseline table.
	return &ModelHawkesIntensity{alpha: 5.0, beta: 2.0}
}

func (m *ModelHawkesIntensity) Name() string { return "Hawkes_Intensity" }

func (m *ModelHawkesIntensity) Reset() { m.intensity = 0 }

func (m *ModelHawkesIntensity) Update(dt float64, p, v float64) float64 {
	if dt > 0 {
		m.intensity *= math.Exp(-m.beta * dt)
	}
	m.intensity += m.alpha * math.Log1p(v)
	return m.intensity
}

// ============================================================================
// 2. Hawkes_OFI: your new signed order-flow imbalance variant
// ============================================================================

type ModelHawkesOFI struct {
	buyInt  float64
	sellInt float64
	beta    float64 // decay rate
	lastP   float64
	init    bool
}

func NewHawkesOFI() *ModelHawkesOFI {
	// beta=0.002 -> half-life ~ 350s; longer memory than the baseline Hawkes.
	return &ModelHawkesOFI{beta: 0.002}
}

func (m *ModelHawkesOFI) Name() string { return "Hawkes_OFI" }

func (m *ModelHawkesOFI) Reset() {
	m.buyInt, m.sellInt, m.lastP, m.init = 0, 0, 0, false
}

func (m *ModelHawkesOFI) Update(dt float64, p, v float64) float64 {
	if !m.init {
		m.lastP = p
		m.init = true
		return 0
	}

	if dt > 0 {
		decay := math.Exp(-m.beta * dt)
		m.buyInt *= decay
		m.sellInt *= decay
	}

	impact := math.Log1p(v)

	// Tick rule on trades (no quotes available).
	if p > m.lastP {
		m.buyInt += impact
	} else if p < m.lastP {
		m.sellInt += impact
		// p == lastP is treated as neutral; only decay applies.
	}

	m.lastP = p
	return m.buyInt - m.sellInt
}

// ============================================================================
// 3. Streaming signature: Sig_LevyArea (with sign fix)
// ============================================================================

type ModelSignature struct {
	area      float64
	lastP     float64
	lastV     float64
	cumVol    float64
	decayRate float64
	init      bool
}

func NewSignature() *ModelSignature {
	// decay=0.001 -> tau ~ 1000s (~16 minutes), as you suggested.
	return &ModelSignature{decayRate: 0.001}
}

func (m *ModelSignature) Name() string { return "Sig_LevyArea" }

func (m *ModelSignature) Reset() {
	m.area, m.lastP, m.lastV, m.cumVol, m.init = 0, 0, 0, 0, false
}

func (m *ModelSignature) Update(dt float64, p, v float64) float64 {
	if !m.init {
		m.lastP, m.cumVol, m.lastV, m.init = p, v, v, true
		return 0
	}
	m.cumVol += v

	// Lead-lag area increment (same structure as before).
	inc := (p-m.lastP)*m.cumVol - (m.cumVol-m.lastV)*m.lastP

	if dt > 0 {
		m.area *= math.Exp(-m.decayRate * dt)
	}
	m.area += inc

	m.lastP = p
	m.lastV = m.cumVol

	// IMPORTANT: flip sign to correct the observed systematic anti-signal.
	return -m.area
}

// ============================================================================
// 4. Hilbert_Phase: robust, symplectic oscillator implementation
// ============================================================================

type ModelHilbert struct {
	x1, x2 float64 // x1: smoothed price, x2: velocity
	r, h   float64 // r: natural frequency, h: damping ratio
	init   bool
}

func NewHilbert() *ModelHilbert {
	// r=0.005 -> T ≈ 2π/r ≈ 21 minutes; good coarse-grained cycle.
	// h=1.0   -> critical damping (stable, non-oscillatory).
	return &ModelHilbert{r: 0.005, h: 1.0}
}

// Keep the original name so reports remain on the same row label.
func (m *ModelHilbert) Name() string { return "Hilbert_Phase" }

func (m *ModelHilbert) Reset() { m.x1, m.x2, m.init = 0, 0, false }

func (m *ModelHilbert) Update(dt float64, p, v float64) float64 {
	if !m.init {
		m.x1, m.init = p, true
		return 0
	}
	if dt <= 0 {
		// Return current phase estimate if no time passed.
		// Use the same real/imag decomposition as below.
		re := p - m.x1
		im := m.x2 / m.r
		return math.Atan2(im, re)
	}

	// x'' + 2*h*r*x' + r^2*(x - p) = 0
	err := m.x1 - p
	acc := -m.r*m.r*err - 2.0*m.r*m.h*m.x2

	// Symplectic Euler (semi-implicit) for stability.
	m.x2 += acc * dt
	m.x1 += m.x2 * dt

	re := p - m.x1
	im := m.x2 / m.r

	phase := math.Atan2(im, re)
	if math.IsNaN(phase) || math.IsInf(phase, 0) {
		// Safety clamp: if something goes wrong numerically, don’t poison the series.
		return 0
	}
	return phase
}

// ============================================================================
// 5. Model registry
// ============================================================================

func GetContinuousModels() []ContinuousModel {
	return []ContinuousModel{
		NewHawkesIntensity(), // baseline, proven positive
		NewHawkesOFI(),       // your new OFI-based variant
		NewSignature(),       // sign-corrected signature
		NewHilbert(),         // robust Hilbert_Phase
	}
}
