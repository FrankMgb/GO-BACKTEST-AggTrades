package main

import (
	"math"
	"sort"
)

type StreamResult struct {
	Times       []int64   // [sample]
	Prices      []float64 // [sample]
	Features    []float64 // [sample * numModels]
	Targets     []float64 // [sample * numHorizons]
	NumModels   int
	NumHorizons int
}

func RunStream(cols *DayColumns, models []ContinuousModel) StreamResult {
	n := cols.Count
	if n < 100 {
		return StreamResult{}
	}

	numModels := len(models)
	numHorizons := len(HorizonDelays)

	for _, m := range models {
		m.Reset()
	}

	// Rough capacity estimate: one sample per minute.
	estSamples := n / 60
	if estSamples < 1 {
		estSamples = 1
	}

	res := StreamResult{
		Times:       make([]int64, 0, estSamples),
		Prices:      make([]float64, 0, estSamples),
		Features:    make([]float64, 0, estSamples*numModels),
		Targets:     nil, // filled after labeling
		NumModels:   numModels,
		NumHorizons: numHorizons,
	}

	// Scratch slice reused per tick to hold model outputs.
	currFeats := make([]float64, numModels)

	lastT := cols.Times[0]
	nextSampleT := lastT + (SamplingRateSec * 1000)

	for i := 0; i < n; i++ {
		t := cols.Times[i]
		p := cols.Prices[i]
		v := cols.Qtys[i]

		dt := float64(t-lastT) / 1000.0
		if dt < 0 {
			dt = 0
		}
		lastT = t

		for j, m := range models {
			currFeats[j] = m.Update(dt, p, v)
		}

		if t >= nextSampleT {
			// Append one sample row.
			res.Times = append(res.Times, t)
			res.Prices = append(res.Prices, p)
			res.Features = append(res.Features, currFeats...)

			for t >= nextSampleT {
				nextSampleT += (SamplingRateSec * 1000)
			}
		}
	}

	sampleCount := len(res.Times)
	if sampleCount == 0 {
		return StreamResult{}
	}

	// Lookahead labeling on the flat arrays.
	maxTime := cols.Times[n-1]
	res.Targets = make([]float64, sampleCount*numHorizons)

	validCount := 0
	ticksTimes := cols.Times
	ticksPrices := cols.Prices

	for i := 0; i < sampleCount; i++ {
		basePrice := res.Prices[i]
		sampleT := res.Times[i]

		valid := true
		baseTarg := validCount * numHorizons

		for hIdx, delay := range HorizonDelays {
			targetT := sampleT + delay
			if targetT > maxTime {
				valid = false
				break
			}

			// Binary search for first tick with time >= targetT.
			idx := sort.Search(n, func(k int) bool {
				return ticksTimes[k] >= targetT
			})
			if idx == n {
				valid = false
				break
			}
			foundP := ticksPrices[idx]
			if foundP <= 0 {
				valid = false
				break
			}

			res.Targets[baseTarg+hIdx] = math.Log(foundP / basePrice)
		}

		if !valid {
			continue
		}

		// Pack valid rows to the front (Times, Prices, Features).
		if validCount != i {
			res.Times[validCount] = sampleT
			res.Prices[validCount] = basePrice

			srcFeat := i * numModels
			dstFeat := validCount * numModels
			copy(res.Features[dstFeat:dstFeat+numModels], res.Features[srcFeat:srcFeat+numModels])
		}

		validCount++
	}

	if validCount == 0 {
		return StreamResult{}
	}

	res.Times = res.Times[:validCount]
	res.Prices = res.Prices[:validCount]
	res.Features = res.Features[:validCount*numModels]
	res.Targets = res.Targets[:validCount*numHorizons]

	return res
}
