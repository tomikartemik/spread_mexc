package arbitrage

import (
	"strings"
	"sync"
	"time"

	"spread_mexc/internal/config"
	"spread_mexc/internal/mexc"
)

// SymbolState keeps mutable information for a trading pair.
type SymbolState struct {
	MexcSymbol string
	RestSymbol string
	DexPair    string
	DexKey     string
	ChainIndex string
	DexAddress string
	Contract   mexc.ContractMeta

	mu       sync.RWMutex
	price    PriceState
	position *Position
	tracker  trackingState
}

// PriceState stores the last known prices.
type PriceState struct {
	MexcPrice float64
	DexPrice  float64
	MexcTime  time.Time
	DexTime   time.Time
}

// Position describes an opened futures position.
type Position struct {
	Direction   Direction
	Amount      float64
	EntryPrice  float64
	EntrySpread float64
	OpenedAt    time.Time
}

// SymbolSnapshot exposes a thread-safe view of internal state.
type SymbolSnapshot struct {
	MexcSymbol string
	RestSymbol string
	DexPair    string
	MexcPrice  float64
	DexPrice   float64
	PriceTime  PriceState
	Position   *Position
}

type trackingState struct {
	Active    bool
	MaxSpread float64
	DropStart time.Time
}

// NewSymbolState constructs a SymbolState from configuration.
func NewSymbolState(sym config.ResolvedSymbol, meta mexc.ContractMeta) *SymbolState {
	return &SymbolState{
		MexcSymbol: sym.MexcSymbol,
		RestSymbol: sym.RestSymbol,
		DexPair:    sym.DexPair,
		DexKey:     strings.ToLower(sym.DexPair),
		ChainIndex: sym.ChainIndex,
		DexAddress: sym.DexAddress,
		Contract:   meta,
	}
}

// UpdateMexcPrice stores the latest CEX price.
func (s *SymbolState) UpdateMexcPrice(price float64) {
	s.mu.Lock()
	s.price.MexcPrice = price
	s.price.MexcTime = time.Now()
	s.mu.Unlock()
}

// UpdateDexPrice stores the latest DEX price.
func (s *SymbolState) UpdateDexPrice(price float64) {
	s.mu.Lock()
	s.price.DexPrice = price
	s.price.DexTime = time.Now()
	s.mu.Unlock()
}

// SetPosition saves the active position snapshot.
func (s *SymbolState) SetPosition(pos *Position) {
	s.mu.Lock()
	if pos != nil {
		copy := *pos
		s.position = &copy
	} else {
		s.position = nil
	}
	s.mu.Unlock()
}

// ClearPosition removes the active position reference.
func (s *SymbolState) ClearPosition() {
	s.SetPosition(nil)
}

// ResetTracking resets spread tracking logic.
func (s *SymbolState) ResetTracking() {
	s.mu.Lock()
	s.tracker = trackingState{}
	s.mu.Unlock()
}

// ShouldEnter evaluates whether spread meets tracking rules for entry.
func (s *SymbolState) ShouldEnter(spread float64, now time.Time, cfg config.Settings) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	tracker := &s.tracker
	if spread < cfg.EntryThreshold {
		*tracker = trackingState{}
		return false
	}

	if !tracker.Active {
		tracker.Active = true
		tracker.MaxSpread = spread
		tracker.DropStart = time.Time{}
		return false
	}

	if spread > tracker.MaxSpread {
		tracker.MaxSpread = spread
		tracker.DropStart = time.Time{}
		return false
	}

	drop := tracker.MaxSpread - spread
	if drop < cfg.ConfirmDrop {
		tracker.DropStart = time.Time{}
		return false
	}

	if tracker.DropStart.IsZero() {
		tracker.DropStart = now
		return false
	}

	duration := now.Sub(tracker.DropStart)
	if duration < cfg.ConfirmDuration {
		return false
	}

	if drop < cfg.EnterDelta {
		return false
	}

	if spread < cfg.MinEntrySpread {
		*tracker = trackingState{}
		return false
	}

	*tracker = trackingState{}
	return true
}

// Snapshot returns immutable copies for decision making.
func (s *SymbolState) Snapshot() SymbolSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot := SymbolSnapshot{
		MexcSymbol: s.MexcSymbol,
		RestSymbol: s.RestSymbol,
		DexPair:    s.DexPair,
		MexcPrice:  s.price.MexcPrice,
		DexPrice:   s.price.DexPrice,
		PriceTime:  s.price,
	}
	if s.position != nil {
		copy := *s.position
		snapshot.Position = &copy
	}
	return snapshot
}
