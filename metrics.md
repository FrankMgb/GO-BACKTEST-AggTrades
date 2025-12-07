Here is the definitive, comprehensive list of all metrics that will appear in your **Continuous-Time Algo Report**.

These are categorized by their function: **Discovery** (Is it predictive?), **Trading** (Is it profitable?), **Risk** (Is it safe?), and **Information Theory** (Is it novel?).

### 1. The "Algo Logic" Metrics (Profitability)
*These tell you the raw money-making potential of the math.*

* **SPREAD (bps)**
    * **Definition:** The difference in Average Return between the **Top 20%** of signal values (Long) and the **Bottom 20%** (Short).
    * **Why it matters:** This represents your "Raw Edge" before fees.
    * **Target:** `> 15.0 bps` (Crypto/Futures) or `> 5.0 bps` (FX/Equities).

* **WIN RATE (%)**
    * **Definition:** The percentage of time the "Top Bucket" signal resulted in a positive price return over the horizon.
    * **Why it matters:** Determines the psychological difficulty of trading the strategy.
    * **Target:** `> 52%` (High Frequency) or `> 55%` (Low Frequency).

* **AVG RETURN (bps)**
    * **Definition:** The average return per trade if you only took the Top Bucket signals.
    * **Why it matters:** Must be higher than your execution costs (Fees + Slippage).
    * **Target:** `> 10 bps` (Net of fees).

---

### 2. The "Physics" Metrics (Predictive Power)
*These tell you if the math actually relates to the price movement.*

* **IC (Information Coefficient)**
    * **Definition:** The Spearman Rank Correlation between your continuous feature and the future return.
    * **Why it matters:** Measures linear/monotonic directional accuracy.
    * **Target:** `> 0.03` (for 15m/1h horizons) or `> 0.05` (Excellent).

* **IR (Information Ratio)**
    * **Definition:** `Mean IC / StdDev IC`.
    * **Why it matters:** Measures consistency. A lower IC that works every day is better than a high IC that only works once a month.
    * **Target:** `> 0.15` (Good), `> 0.50` (World Class).

---

### 3. The "Risk & Reliability" Metrics (Safety)
*These tell you if the strategy will blow up your account.*

* **PROFIT FACTOR**
    * **Definition:** `(Sum of Gross Wins) / (Sum of Gross Losses)`.
    * **Why it matters:** Measures the "quality" of the edge. It punishes strategies that win often but take massive losses (picking up pennies in front of a steamroller).
    * **Target:** `> 1.3` (Tradeable), `> 1.5` (Solid), `> 2.0` (Excellent).

* **T-STAT (Stability)**
    * **Definition:** A statistical test calculating if the Average Return is significantly different from zero (Luck).
    * **Why it matters:** Filters out random noise.
    * **Target:** `> 2.0` (95% Confidence), `> 3.0` (Production Ready).

* **SAFE? (Boolean)**
    * **Definition:** A simple "YES/NO" flag based on `Spread > Fees` AND `T-Stat > 2.0`.
    * **Target:** `YES`.

---

### 4. The "Deep Insight" Metrics (Novelty)
*These tell you if the math is finding hidden relationships that standard tools miss.*

* **MI (Mutual Information in Bits)**
    * **Definition:** Measures how much uncertainty about the price is removed by knowing the feature. Detects non-linear relationships (e.g., Volatility expansions) that Correlation misses.
    * **Why it matters:** If IC is 0 but MI is high, you have a non-directional "Volatility Signal."
    * **Target:** `> 0.01 bits` (Signal exists), `> 0.05 bits` (Strong Signal).

* **PEAK LAG (Minutes)**
    * **Definition:** The time offset where the Correlation/MI is highest.
    * **Why it matters:** Tells you exactly how far in advance your math predicts the move.
    * **Target:** `5m - 20m` (Perfect for 1h trading). If `0m`, it is a lagging indicator.

* **HALF-LIFE (Minutes)**
    * **Definition:** How long it takes for the signal's autocorrelation to drop by 50%.
    * **Why it matters:** Tells you how fast you need to execute.
    * **Target:** Should match your horizon. If Half-Life is 1 min and you trade 1h candles, the signal will rot before you profit.