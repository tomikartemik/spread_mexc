package arbitrage

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type TradeSummary struct {
	Symbol    string
	Direction Direction
	Entry     float64
	Exit      float64
	Amount    float64
	Contract  float64
	Spread    float64
	PnL       float64
	ClosedAt  time.Time
}

type PnLTracker struct {
	mu             sync.Mutex
	initialBalance float64
	balance        float64
	dailyPnL       float64
	trades         []TradeSummary
	lastReset      time.Time
}

func NewPnLTracker(initial float64) *PnLTracker {
	now := time.Now()
	return &PnLTracker{
		initialBalance: initial,
		balance:        initial,
		lastReset:      now,
	}
}

func (p *PnLTracker) RecordTrade(symbol string, dir Direction, entry, exit, amount, contractSize, spread float64) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	if contractSize <= 0 {
		contractSize = 1
	}

	diff := exit - entry
	if dir == directionShort {
		diff = entry - exit
	}
	pnl := diff * amount * contractSize
	p.balance += pnl
	p.dailyPnL += pnl
	p.trades = append(p.trades, TradeSummary{
		Symbol:    symbol,
		Direction: dir,
		Entry:     entry,
		Exit:      exit,
		Amount:    amount,
		Contract:  contractSize,
		Spread:    spread,
		PnL:       pnl,
		ClosedAt:  time.Now(),
	})
	return pnl
}

func (p *PnLTracker) BuildReport(reportTime time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	var builder strings.Builder
	start := p.lastReset
	builder.WriteString(fmt.Sprintf("📊 Отчёт %s – %s\n", start.Format("02 Jan"), reportTime.Format("02 Jan 15:04")))
	totalChange := p.balance - p.initialBalance
	builder.WriteString(fmt.Sprintf("Баланс: %.2f USDT (изменение с запуска: %+.2f)\n", p.balance, totalChange))
	builder.WriteString(fmt.Sprintf("PnL за период: %+.2f USDT, сделок: %d\n", p.dailyPnL, len(p.trades)))

	if len(p.trades) == 0 {
		builder.WriteString("Сделок не было.\n")
	} else {
		builder.WriteString("Сделки:\n")
		for i, t := range p.trades {
			builder.WriteString(fmt.Sprintf(
				"%d) %s %s %.4f→%.4f vol %.4f pnl %+.2f\n",
				i+1,
				t.Symbol,
				string(t.Direction),
				t.Entry,
				t.Exit,
				t.Amount,
				t.PnL,
			))
			if i >= 19 {
				builder.WriteString(fmt.Sprintf("... и ещё %d сделок\n", len(p.trades)-i-1))
				break
			}
		}
	}
	return builder.String()
}

func (p *PnLTracker) ResetDaily(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dailyPnL = 0
	p.trades = nil
	p.lastReset = now
}

func nextReportTime(now time.Time) time.Time {
	loc := now.Location()
	next := time.Date(now.Year(), now.Month(), now.Day(), 1, 0, 0, 0, loc)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}
