package main

import "math"

// --- Interface ---

type Atom interface {
	Name() string
	Reset() // Clear state for new day
	// Update calculates the feature value for the current trade.
	Update(q, s, p, flow, dt float64) float64
}

// --- Constants ---

const (
	InputFlow = iota // Use q * s
	InputSign        // Use s (Trade Continuation / Direction)
)

// --- Registry ---

func GetActiveAtoms() []Atom {
	return []Atom{
		// --- Baseline ---
		&RawOFI{},
		&RawTCI{},

		// --- Standard EMA (Baseline) ---
		&EMAAtom{NameStr: "OFI_EMA_15s", Tau: 15.0, Input: InputFlow},
		&EMAAtom{NameStr: "TCI_EMA_15s", Tau: 15.0, Input: InputSign}, // Added TCI variant

		// --- Sniper Math (Lag Reduced) ---
		&DEMAAtom{NameStr: "OFI_DEMA_15s", Tau: 15.0, Input: InputFlow},
		&TEMAAtom{NameStr: "OFI_TEMA_15s", Tau: 15.0, Input: InputFlow},
		&DEMAAtom{NameStr: "OFI_DEMA_5s", Tau: 5.0, Input: InputFlow},

		// --- Sign-Based Sniper (New Capability!) ---
		&DEMAAtom{NameStr: "TCI_DEMA_15s", Tau: 15.0, Input: InputSign},

		// --- Advanced / Composite ---
		&RSIAtom{NameStr: "OFI_RSI_15s", Tau: 15.0, Input: InputFlow},
		&CubicAtom{NameStr: "OFI_Cubic_15s", Tau: 15.0, Input: InputFlow},

		// --- Volatility / Force ---
		&ImpulseAtom{NameStr: "Impulse_15s", Tau: 15.0, Input: InputFlow},  // Flow * Speed
		&ForceAtom{NameStr: "Force_DEMA_15s", Tau: 15.0, Input: InputFlow}, // DEMA * Speed
		&InstVelAtom{},

		// --- Experimental ---
		// Composite mixing TEMA and Cubic (both using InputFlow by default)
		&CompositeAtom{
			NameStr: "Sniper_Composite",
			Tau:     15.0,
			Input:   InputFlow,
			WeightA: 0.5,
			WeightB: 0.5,
		},
	}
}

// --- Helper ---

// resolveInput centralizes the Flow vs Sign logic.
// Inlined by compiler for zero overhead.
func resolveInput(mode int, flow, s float64) float64 {
	if mode == InputSign {
		return s
	}
	return flow
}

// --- Implementations ---

// 1. Raw OFI
type RawOFI struct{}

func (r *RawOFI) Name() string                             { return "OFI_Raw" }
func (r *RawOFI) Reset()                                   {}
func (r *RawOFI) Update(q, s, p, flow, dt float64) float64 { return flow }

// 2. Raw TCI
type RawTCI struct{}

func (r *RawTCI) Name() string                             { return "TCI_Raw" }
func (r *RawTCI) Reset()                                   {}
func (r *RawTCI) Update(q, s, p, flow, dt float64) float64 { return s }

// 3. Standard EMA
type EMAAtom struct {
	NameStr string
	Tau     float64
	Input   int
	val     float64
}

func (a *EMAAtom) Name() string { return a.NameStr }
func (a *EMAAtom) Reset()       { a.val = 0 }
func (a *EMAAtom) Update(q, s, p, flow, dt float64) float64 {
	x := resolveInput(a.Input, flow, s)
	alpha := 1.0 - math.Exp(-dt/a.Tau)
	a.val += alpha * (x - a.val)
	return a.val
}

// 4. DEMA (Double EMA)
type DEMAAtom struct {
	NameStr string
	Tau     float64
	Input   int
	e1, e2  float64
}

func (a *DEMAAtom) Name() string { return a.NameStr }
func (a *DEMAAtom) Reset()       { a.e1 = 0; a.e2 = 0 }
func (a *DEMAAtom) Update(q, s, p, flow, dt float64) float64 {
	x := resolveInput(a.Input, flow, s)
	alpha := 1.0 - math.Exp(-dt/a.Tau)
	a.e1 += alpha * (x - a.e1)
	a.e2 += alpha * (a.e1 - a.e2)
	return 2*a.e1 - a.e2
}

// 5. TEMA (Triple EMA)
type TEMAAtom struct {
	NameStr    string
	Tau        float64
	Input      int
	e1, e2, e3 float64
}

func (a *TEMAAtom) Name() string { return a.NameStr }
func (a *TEMAAtom) Reset()       { a.e1 = 0; a.e2 = 0; a.e3 = 0 }
func (a *TEMAAtom) Update(q, s, p, flow, dt float64) float64 {
	x := resolveInput(a.Input, flow, s)
	alpha := 1.0 - math.Exp(-dt/a.Tau)
	a.e1 += alpha * (x - a.e1)
	a.e2 += alpha * (a.e1 - a.e2)
	a.e3 += alpha * (a.e2 - a.e3)
	return 3*a.e1 - 3*a.e2 + a.e3
}

// 6. RSI (Relative Strength)
type RSIAtom struct {
	NameStr  string
	Tau      float64
	Input    int
	up, down float64
}

func (a *RSIAtom) Name() string { return a.NameStr }
func (a *RSIAtom) Reset()       { a.up = 0; a.down = 0 }
func (a *RSIAtom) Update(q, s, p, flow, dt float64) float64 {
	x := resolveInput(a.Input, flow, s)
	alpha := 1.0 - math.Exp(-dt/a.Tau)

	u, d := 0.0, 0.0
	if x > 0 {
		u = x
	} else {
		d = -x
	}

	a.up += alpha * (u - a.up)
	a.down += alpha * (d - a.down)
	sum := a.up + a.down

	if sum < 1e-9 {
		return 0.0
	}
	// Scaled -1 to 1
	return 2.0*(a.up/sum) - 1.0
}

// 7. Cubic (Non-Linear)
type CubicAtom struct {
	NameStr string
	Tau     float64
	Input   int
	val     float64
}

func (a *CubicAtom) Name() string { return a.NameStr }
func (a *CubicAtom) Reset()       { a.val = 0 }
func (a *CubicAtom) Update(q, s, p, flow, dt float64) float64 {
	x := resolveInput(a.Input, flow, s)
	alpha := 1.0 - math.Exp(-dt/a.Tau)

	// Track EMA of Cube
	x3 := x * x * x
	a.val += alpha * (x3 - a.val)

	// Return Cube Root to linearize scale for downstream tasks
	if a.val > 0 {
		return math.Cbrt(a.val)
	}
	return -math.Cbrt(-a.val)
}

// 8. Impulse (EMA * Speed)
type ImpulseAtom struct {
	NameStr string
	Tau     float64
	Input   int
	val     float64
}

func (a *ImpulseAtom) Name() string { return a.NameStr }
func (a *ImpulseAtom) Reset()       { a.val = 0 }
func (a *ImpulseAtom) Update(q, s, p, flow, dt float64) float64 {
	x := resolveInput(a.Input, flow, s)
	alpha := 1.0 - math.Exp(-dt/a.Tau)
	a.val += alpha * (x - a.val)

	// Speed Cap
	speed := 1.0 / dt // dt is guaranteed >= 1e-4 by build.go
	if speed > 100 {
		speed = 100
	}

	return a.val * speed
}

// 9. Force (DEMA * Speed)
type ForceAtom struct {
	NameStr string
	Tau     float64
	Input   int
	e1, e2  float64
}

func (a *ForceAtom) Name() string { return a.NameStr }
func (a *ForceAtom) Reset()       { a.e1 = 0; a.e2 = 0 }
func (a *ForceAtom) Update(q, s, p, flow, dt float64) float64 {
	x := resolveInput(a.Input, flow, s)
	alpha := 1.0 - math.Exp(-dt/a.Tau)

	a.e1 += alpha * (x - a.e1)
	a.e2 += alpha * (a.e1 - a.e2)
	dema := 2*a.e1 - a.e2

	speed := 1.0 / dt
	if speed > 100 {
		speed = 100
	}

	return dema * speed
}

// 10. Instant Velocity
type InstVelAtom struct{}

func (a *InstVelAtom) Name() string { return "Vel_Inst" }
func (a *InstVelAtom) Reset()       {}
func (a *InstVelAtom) Update(q, s, p, flow, dt float64) float64 {
	// Redundant check removed as requested
	speed := 1.0 / dt
	if speed > 100 {
		speed = 100
	}
	return flow * speed
}

// 11. Composite (TEMA + Cubic)
type CompositeAtom struct {
	NameStr          string
	Tau              float64
	Input            int
	WeightA, WeightB float64
	tema             TEMAAtom
	cubic            CubicAtom
}

func (a *CompositeAtom) Name() string { return a.NameStr }
func (a *CompositeAtom) Reset() {
	// Propagate configuration to children
	a.tema.Tau = a.Tau
	a.tema.Input = a.Input
	a.tema.Reset()

	a.cubic.Tau = a.Tau
	a.cubic.Input = a.Input
	a.cubic.Reset()
}
func (a *CompositeAtom) Update(q, s, p, flow, dt float64) float64 {
	v1 := a.tema.Update(q, s, p, flow, dt)
	v2 := a.cubic.Update(q, s, p, flow, dt)
	return a.WeightA*v1 + a.WeightB*v2
}
