This is the **Canonical 2025 List of Microstructure Atoms**.

It incorporates your specific corrections to ensure **Atomic Purity**:
1.  **No Windows:** HHI is replaced by Instantaneous Lumpiness.
2.  **No Lookbacks:** DGT/Absorb use immediate tick-to-tick price response.
3.  **Refined Gravity:** Magnet uses multi-tier distance.

This is the exact mathematical definition set used for non-book-dependent HFT feature generation.

---

### **Definitions**
* $q_t$: Quantity (Volume) of trade at time $t$.
* $s_t$: Aggressor Side ($+1$ for Buy, $-1$ for Sell).
* $p_t$: Execution Price.
* $\Delta t$: Time since last trade ($t - t_{-1}$).
* $M_t$: Match Count (`last_trade_id` - `first_trade_id` + 1).
* $\mathbb{1}_{\{condition\}}$: Indicator function (1 if true, 0 if false).

---

### **Group A: Force Atoms (Mass & Aggression)**

| # | Atom Name | Pure Math Formula ($A_t$) | Physics & Logic |
| :--- | :--- | :--- | :--- |
| **1** | **OFI** | $$q_t \cdot s_t$$ | **Net Force.** The fundamental vector of market movement. |
| **2** | **TCI** | $$1 \cdot s_t$$ | **Participation.** One vote per trade event. Measures consensus vs. noise. |
| **3** | **Whale** | $$q_t \cdot s_t \cdot \mathbb{1}_{\{q_t > K_{95}\}}$$ | **Shock.** Filters for tail-risk events that clear levels. ($K_{95}$ is a scalar threshold). |
| **4** | **Lumpiness** | $$-q_t^2 \cdot s_t$$ | **Mean Reversion.** Penalizes "fat" flow. A large $q^2$ implies an idiosyncrasy or fat-finger that often reverts. |

---

### **Group B: Structure Atoms (Book Geometry)**

| # | Atom Name | Pure Math Formula ($A_t$) | Physics & Logic |
| :--- | :--- | :--- | :--- |
| **5** | **Sweep** | $$M_t \cdot s_t$$ | **Urgency.** The willingness to cross the spread and eat multiple limit orders. |
| **6** | **Fragility** | $$(M_t / q_t) \cdot s_t$$ | **Thinness.** Matches per unit of volume. High value = Paper-thin book (Retail). Low value = Icebergs. |
| **7** | **Magnet** | $$\frac{1}{1 + \min(|p_t \pmod{1}|, |p_t \pmod{0.5}|)}$$ | **Gravity.** Measures proximity to psychological barriers ($1.0, 0.5$). Velocity increases as this approaches 1. |

---

### **Group C: Kinetic Atoms (Time & Speed)**

| # | Atom Name | Pure Math Formula ($A_t$) | Physics & Logic |
| :--- | :--- | :--- | :--- |
| **8** | **Velocity** | $$\frac{q_t}{\Delta t} \cdot s_t$$ | **Heat.** Volume per millisecond. High velocity signals regime change or breakout. |
| **9** | **Acceleration** | $$(\text{Vel}_t - \text{Vel}_{t-1})$$ | **Jerk.** The derivative of velocity. Detects the *start* of the impulse before velocity peaks. |
| **10** | **Gap** | $$\Delta t \cdot s_t$$ | **Vacuum.** Asymmetry in arrival times. Large gap + Buy = Sellers are absent/hesitant. |

---

### **Group D: Response Atoms (Impact Efficiency)**

| # | Atom Name | Pure Math Formula ($A_t$) | Physics & Logic |
| :--- | :--- | :--- | :--- |
| **11** | **DGT** | $$q_t \cdot s_t \cdot \mathbb{1}_{\{s_t = \text{sign}(p_t - p_{t-1})\}}$$ | **Continuation.** Flow that successfully moved the price in the immediate tick. |
| **12** | **Absorb** | $$q_t \cdot s_t \cdot \mathbb{1}_{\{s_t \neq \text{sign}(p_t - p_{t-1})\}}$$ | **Blockade.** Flow that struck a wall. Includes trades where price did not move ($p_t = p_{t-1}$). |
| **13** | **Fractal** | $$\frac{|p_t - p_{t-1}|}{q_t}$$ | **Cost.** The "Amihud" proxy. Price impact per unit of volume. High = Illiquid/Gapping. |