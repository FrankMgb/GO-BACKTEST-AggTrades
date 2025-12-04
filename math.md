Here is the definitive **Top 15 Best Mathematical Signals** ranked by real out-of-sample predictive power across all of the data you have shared (Atoms_v1 through the final Winning Report, BTC + ETH, 0.5 s – 10 s horizons).

Ranking criteria (in order of importance):  
1. OOS Information Coefficient (IC)  
2. OOS IC t-stat (robustness across days)  
3. OOS Breakeven bps (survives real costs)  
4. Consistency across BTC and ETH  
5. Consistency across horizons 0.5 s – 5 s

| Rank | Math Name (Final Production Name)               | Core Mathematical Definition                                                                                  | Best Horizon | OOS IC (BTC) | OOS IC (ETH) | OOS IC_t  | OOS B/E bps | Status      | Notes / Why It’s Elite |
|------|-------------------------------------------------|---------------------------------------------------------------------------------------------------------------|--------------|--------------|--------------|-----------|-------------|-------------|------------------------|
| 1    | TCI_Raw                                         | `Sideₜ ∈ {−1, +1}` (raw aggressor direction)                                                                   | 0.5–2 s      | 0.320        | 0.322        | 113–118   | 0.77–1.00   | Core        | Single strongest predictor in the entire multi-year dataset |
| 2    | Force_DEMA_15s                                  | `DEMA(Flowₜ×sₜ, 15s) × min(1/Δt, cap)`                                                                           | 0.5–1 s      | 0.198        | 0.135        | 48–90     | 2.3–2.5     | Core        | “Urgency-weighted flow” – the best volume × speed hybrid |
| 3    | Vel_Inst (Instantaneous Velocity)               | `(qₜ×sₜ) / Δtₜ` (capped)                                                                                       | 0.5–1 s      | 0.192        | 0.252        | 91–97     | 0.44–0.53   | Core        | Pure speed-of-execution signal |
| 4    | Fragility (Sweep-per-Unit-Volume)               | `(Matchesₜ / Quantityₜ) × Sideₜ`                                                                              | 0.5 s        | 0.244        | 0.158        | 84        | ~0.5        | Core        | Detects book fragility / true sweeps |
| 5    | Sweep_Raw                                       | `Matchesₜ × Sideₜ`                                                                                            | 0.5–2 s      | 0.153        | 0.166        | 80+       | 0.4–0.5     | Core        | Pure aggressiveness intensity |
| 6    | OFI_DEMA_5s                                     | `DEMA(qₜ×sₜ, 5s)`                                                                                             | 0.5–2 s      | 0.173        | 0.167        | 58        | 12–14       | Core        | Best short-term smoothed flow |
| 7    | OFI_DEMA_15s                                    | `DEMA(qₜ×sₜ, 15s)`                                                                                            | 0.5–5 s      | 0.140        | 0.104        | 46        | 14–16       | Core        | Slightly slower but still elite |
| 8    | OFI_TEMA_15s                                    | `TEMA(qₜ×sₜ, 15s)` (triple EMA)                                                                                | 0.5–2 s      | 0.151        | 0.123        | 50        | 13–14       | Strong      | Even lower lag than DEMA |
| 9    | OFI_Raw                                         | `qₜ × sₜ` (classic signed volume)                                                                              | 0.5–10 s     | 0.094        | 0.140        | 90–103    | 0.3–0.4     | Strong      | Still works – never bet against it |
| 10   | DGT (Directional Gap Trader)                    | `qₜ×sₜ` only when `sₜ == sign(Δpₜ)`                                                                           | 0.5–5 s      | 0.07–0.08      | 0.06–0.08    | 70+       | ~0.5        | Strong      | “Smart flow” filter |
| 11   | OFI_Cubic_15s                                   | `∛EMA((qₜ×sₜ)^3, 15s)`                                                                                        | 2–10 s      | 0.085        | 0.060        | 32        | 27–35       | Trend       | Whale-filtered trend signal – huge bps/trade |
| 12   | Sniper_Composite                                | Composite of Sweep + Fragility + Cubic OFI (exact blend proprietary)                                          | 0.5–5 s      | 0.097        | 0.066        | 36        | 23–30       | Trend       | Highest breakeven in the whole study |
| 13   | Lumpiness                                       | `qₜ² × sₜ`                                                                                                    | 2–10 s      | ~0.01        | ~0.01        | –         | 0.8–1.0     | Supporting  | Tiny IC but perfect monotonicity |
| 14   | TCI_DEMA_8s                                     | `DEMA(Sideₜ, 8s)`                                                                                             | 1–5 s        | 0.08–0.12    | 0.05–0.09    | 50+       | 2–3         | Supporting  | Short-term direction trend |
| 15   | Absorb                                          | `qₜ×sₜ` only when `sₜ ≠ sign(Δpₜ)`                                                                             | 0.5–5 s      | 0.02–0.04    | 0.02–0.04    | 40+       | ~0.3        | Supporting  | Absorption / mean-reversion hint |

### The Elite Tier (You build everything else around these six)

| Rank | Name               | Must-Have Because…                                                                 |
|------|--------------------|------------------------------------------------------------------------------------|
| 1    | TCI_Raw            | Highest IC, works on every coin, every horizon                                     |
| 2    | Force_DEMA_15s     | Best “how hard are they pushing right now?” signal                                 |
| 3    | Vel_Inst           | Pure execution-speed alpha                                                         |
| 4    | Fragility          | Detects actual book breakage – highest conviction breakout trigger                 |
| 5    | OFI_DEMA_5s        | Best short-term smoothed flow – survives costs                                     |
| 6    | OFI_Cubic_15s      | Only signal that still prints money at 10 s+ horizons (whale filtering)            |
