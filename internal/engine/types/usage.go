package types

import "encoding/json"

// ModelCost is per-token pricing, in **USD per MILLION tokens**. There is
// deliberately no separate one-hour cache-write price: those writes cost
// input * 2 (see CalculateCost).
type ModelCost struct {
	Unknown    bool    `json:"unknown,omitempty"`
	Input      float64 `json:"input"`      // $/million tokens
	Output     float64 `json:"output"`     // $/million tokens
	CacheRead  float64 `json:"cacheRead"`  // $/million tokens
	CacheWrite float64 `json:"cacheWrite"` // $/million tokens
}

// Cost is the computed per-component + total cost of a Usage (USD).
type Cost struct {
	Unknown    bool    `json:"unknown,omitempty"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// MarshalJSON keeps an unknown price from looking like a free request.
func (c Cost) MarshalJSON() ([]byte, error) {
	if c.Unknown {
		return []byte(`{"unknown":true}`), nil
	}
	type plain Cost
	return json.Marshal(plain(c))
}

// Usage is token accounting for a turn. Optional fields CacheWrite1h / Reasoning
// are pointers so "absent" is distinguishable from 0. CacheWrite1h is the
// subset of CacheWrite written with one-hour retention; Reasoning is a subset
// of Output.
type Usage struct {
	Input        int  `json:"input"`
	Output       int  `json:"output"`
	CacheRead    int  `json:"cacheRead"`
	CacheWrite   int  `json:"cacheWrite"`
	CacheWrite1h *int `json:"cacheWrite1h,omitempty"`
	Reasoning    *int `json:"reasoning,omitempty"`
	TotalTokens  int  `json:"totalTokens"`
	Cost         Cost `json:"cost"`
}

// Add folds one call's usage into a total without sharing optional counters.
func (u *Usage) Add(other Usage) {
	u.Input += other.Input
	u.Output += other.Output
	u.CacheRead += other.CacheRead
	u.CacheWrite += other.CacheWrite
	u.TotalTokens += other.TotalTokens
	if other.CacheWrite1h != nil {
		sum := *other.CacheWrite1h
		if u.CacheWrite1h != nil {
			sum += *u.CacheWrite1h
		}
		u.CacheWrite1h = &sum
	}
	if other.Reasoning != nil {
		sum := *other.Reasoning
		if u.Reasoning != nil {
			sum += *u.Reasoning
		}
		u.Reasoning = &sum
	}
	u.Cost.Unknown = u.Cost.Unknown || other.Cost.Unknown
	u.Cost.Input += other.Cost.Input
	u.Cost.Output += other.Cost.Output
	u.Cost.CacheRead += other.Cost.CacheRead
	u.Cost.CacheWrite += other.Cost.CacheWrite
	u.Cost.Total += other.Cost.Total
}

// CalculateCost fills u.Cost from the model's per-million pricing. One-hour
// cache writes are billed at 2× the base input rate (not a separate
// cache-write price); short writes use the cacheWrite rate. Mutates u in place
// and returns the computed cost.
func CalculateCost(mc ModelCost, u *Usage) Cost {
	if mc.Unknown {
		u.Cost = Cost{Unknown: true}
		return u.Cost
	}
	longWrite := 0
	if u.CacheWrite1h != nil {
		longWrite = *u.CacheWrite1h
	}
	shortWrite := u.CacheWrite - longWrite

	c := Cost{
		Input:      (mc.Input / 1_000_000) * float64(u.Input),
		Output:     (mc.Output / 1_000_000) * float64(u.Output),
		CacheRead:  (mc.CacheRead / 1_000_000) * float64(u.CacheRead),
		CacheWrite: (mc.CacheWrite*float64(shortWrite) + mc.Input*2*float64(longWrite)) / 1_000_000,
	}
	c.Total = c.Input + c.Output + c.CacheRead + c.CacheWrite
	u.Cost = c
	return c
}
