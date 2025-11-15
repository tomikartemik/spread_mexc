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
