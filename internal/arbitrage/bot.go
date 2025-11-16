package arbitrage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"spread_mexc/internal/config"
	"spread_mexc/internal/mexc"
	"spread_mexc/internal/notify"
	"spread_mexc/internal/okx"
)

// Direction represents the trade direction relative to the spread.
type Direction string

const (
	directionLong  Direction = "long"
	directionShort Direction = "short"
)

// Bot wires together pricing feeds and the trade executor.
type Bot struct {
	cfg       config.Settings
	client    *mexc.Client
	states    []*SymbolState
	notifier  notify.Sender
	okxClient *okx.Client
	tokens    []okx.Token
	tracker   *PnLTracker
}

// NewBot initializes state and fetches contract metadata.
func NewBot(cfg config.Settings) (*Bot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	contracts, err := mexc.FetchContractMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch contract metadata: %w", err)
	}

	client := mexc.NewClient(mexc.Config{
		AuthToken: cfg.MexcAuthToken,
		BaseURL:   cfg.MexcBaseURL,
		Timeout:   cfg.MexcTimeout,
	})

	states := make([]*SymbolState, 0, len(cfg.Symbols))
	for _, sym := range cfg.Symbols {
		meta, ok := contracts[sym.RestSymbol]
		if !ok {
			return nil, fmt.Errorf("contract %s not found on MEXC", sym.RestSymbol)
		}
		state := NewSymbolState(sym, meta)
		states = append(states, state)
	}

	var notifier notify.Sender
	if cfg.Telegram != nil {
		notifier = notify.NewTelegram(cfg.Telegram.BotToken, cfg.Telegram.ChatID)
	}

	okxClient := okx.New(cfg.OKX.BaseURL, cfg.OKX.AccessKey, cfg.OKX.SecretKey, cfg.OKX.Passphrase, 10*time.Second, cfg.OKX.RequestDelay, cfg.OKX.MaxRetries)
	tokens := make([]okx.Token, 0, len(states))
	for _, st := range states {
		tokens = append(tokens, okx.Token{
			Name:       st.MexcSymbol,
			ChainIndex: st.ChainIndex,
			Address:    st.DexAddress,
		})
	}

	return &Bot{
		cfg:       cfg,
		client:    client,
		states:    states,
		notifier:  notifier,
		okxClient: okxClient,
		tokens:    tokens,
		tracker:   NewPnLTracker(cfg.InitialBalance),
	}, nil
}

// Run starts the pricing workers and decision loop until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workerCount := 3
	if b.notifier != nil && b.tracker != nil {
		workerCount++
	}

	errCh := make(chan error, workerCount)
	var wg sync.WaitGroup
	wg.Add(workerCount)

	go func() {
		defer wg.Done()
		if err := b.mexcPriceLoop(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("mexc prices: %w", err)
			cancel()
		}
	}()

	go func() {
		defer wg.Done()
		if err := b.okxLoop(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("okx prices: %w", err)
			cancel()
		}
	}()

	go func() {
		defer wg.Done()
		if err := b.decisionLoop(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("decision loop: %w", err)
			cancel()
		}
	}()

	if b.notifier != nil && b.tracker != nil {
		go func() {
			defer wg.Done()
			if err := b.dailyReportLoop(ctx); err != nil && !errors.Is(err, context.Canceled) {
				errCh <- fmt.Errorf("report loop: %w", err)
				cancel()
			}
		}()
	}

	var runErr error
	select {
	case runErr = <-errCh:
	case <-ctx.Done():
		runErr = ctx.Err()
	}

	cancel()
	wg.Wait()
	return runErr
}

func (b *Bot) mexcPriceLoop(ctx context.Context) error {
	if err := b.refreshMexcPrices(ctx); err != nil {
		log.Printf("initial price fetch failed: %v", err)
	}
	ticker := time.NewTicker(b.cfg.MexcPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := b.refreshMexcPrices(ctx); err != nil {
				log.Printf("mexc price refresh failed: %v", err)
			}
		}
	}
}

func (b *Bot) refreshMexcPrices(ctx context.Context) error {
	workerCount := b.cfg.MexcPriceWorkers
	if workerCount > len(b.states) {
		workerCount = len(b.states)
	}
	if workerCount < 1 {
		workerCount = 1
	}
	jobs := make(chan *SymbolState)
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for state := range jobs {
				price, err := b.fetchMexcPriceWithRetry(ctx, state.RestSymbol)
				if err != nil {
					log.Printf("failed to fetch price for %s: %v", state.RestSymbol, err)
					continue
				}
				state.UpdateMexcPrice(price)
				if b.cfg.MexcPriceDelay > 0 {
					time.Sleep(b.cfg.MexcPriceDelay)
				}
			}
		}()
	}

sendLoop:
	for _, state := range b.states {
		select {
		case <-ctx.Done():
			break sendLoop
		case jobs <- state:
		}
	}
	close(jobs)
	wg.Wait()
	return nil
}

func (b *Bot) fetchMexcPriceWithRetry(ctx context.Context, symbol string) (float64, error) {
	delay := b.cfg.MexcPriceRetryDelay
	var lastErr error
	for attempt := 1; attempt <= b.cfg.MexcPriceMaxRetries; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		price, err := b.client.TickerPrice(reqCtx, symbol)
		cancel()
		if err == nil {
			return price, nil
		}
		lastErr = err
		var se *mexc.StatusError
		if errors.As(err, &se) {
			if (se.Code == http.StatusTooManyRequests || se.Code == http.StatusForbidden) && attempt < b.cfg.MexcPriceMaxRetries {
				time.Sleep(delay)
				delay *= 2
				continue
			}
		}
		break
	}
	return 0, lastErr
}

func (b *Bot) okxLoop(ctx context.Context) error {
	if err := b.refreshDexPrices(ctx); err != nil {
		log.Printf("initial OKX fetch failed: %v", err)
	}
	ticker := time.NewTicker(b.cfg.OKX.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := b.refreshDexPrices(ctx); err != nil {
				log.Printf("okx price refresh failed: %v", err)
			}
		}
	}
}

func (b *Bot) refreshDexPrices(ctx context.Context) error {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	prices, err := b.okxClient.FetchPrices(reqCtx, b.tokens)
	if err != nil {
		return err
	}
	for _, state := range b.states {
		price, ok := prices[okx.TokenKey(state.ChainIndex, state.DexAddress)]
		if !ok {
			continue
		}
		state.UpdateDexPrice(price)
	}
	return nil
}

func (b *Bot) decisionLoop(ctx context.Context) error {
	ticker := time.NewTicker(b.cfg.DecisionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			for _, state := range b.states {
				b.evaluateSymbol(ctx, state)
			}
		}
	}
}

func (b *Bot) evaluateSymbol(ctx context.Context, state *SymbolState) {
	snap := state.Snapshot()
	if snap.MexcPrice <= 0 || snap.DexPrice <= 0 {
		return
	}

	spread := calculateSpread(snap.MexcPrice, snap.DexPrice)
	if b.cfg.DebugPrices {
		log.Printf("[DEBUG] %s Dex=%.6f Mexc=%.6f Spread=%.2f%%", state.MexcSymbol, snap.DexPrice, snap.MexcPrice, spread)
	}

	absolute := math.Abs(spread)

	if snap.Position == nil {
		if absolute >= b.cfg.EntryThreshold {
			dir := directionShort
			if spread < 0 {
				dir = directionLong
			}
			if state.ShouldEnter(absolute, time.Now(), b.cfg) {
				if err := b.openPosition(ctx, state, dir, spread, snap.MexcPrice); err != nil {
					log.Printf("open %s failed: %v", state.MexcSymbol, err)
				}
			}
		} else {
			state.ResetTracking()
		}
		return
	}

	if absolute <= b.cfg.ExitThreshold {
		if err := b.closePosition(ctx, state, "target reached", spread); err != nil {
			log.Printf("close %s failed: %v", state.MexcSymbol, err)
		}
		return
	}

	adverse := calculateAdverseMove(snap.Position, snap.MexcPrice)
	if adverse >= b.cfg.StopLoss {
		if err := b.closePosition(ctx, state, "stop loss", spread); err != nil {
			log.Printf("close %s stop failed: %v", state.MexcSymbol, err)
		}
	}
}

func (b *Bot) openPosition(ctx context.Context, state *SymbolState, dir Direction, spread float64, price float64) error {
	if price <= 0 {
		return fmt.Errorf("invalid price for entry")
	}
	amount, err := b.computeAmount(state, price)
	if err != nil {
		return err
	}
	if amount <= 0 {
		return fmt.Errorf("computed zero amount")
	}

	side := mexc.SideOpenShort
	if dir == directionLong {
		side = mexc.SideOpenLong
	}

	order := mexc.MarketOrder{
		Symbol:     state.RestSymbol,
		Side:       side,
		Volume:     amount,
		Price:      price,
		OpenType:   2,
		Leverage:   b.cfg.Leverage,
		ReduceOnly: false,
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := b.client.SubmitMarketOrder(ctx, order)
	if err != nil {
		return err
	}
	if resp != nil && !resp.Success {
		return fmt.Errorf("mexc rejected order: %s", resp.Msg)
	}

	state.SetPosition(&Position{
		Direction:   dir,
		Amount:      amount,
		EntryPrice:  price,
		EntrySpread: spread,
		OpenedAt:    time.Now(),
	})
	log.Printf("%s opened %s at %.6f amount %.6f spread %.2f%%", state.MexcSymbol, dir, price, amount, spread)
	b.notifyf(b.formatOpenMessage(state, dir, price, amount, spread))
	return nil
}

func (b *Bot) closePosition(ctx context.Context, state *SymbolState, reason string, spread float64) error {
	snap := state.Snapshot()
	if snap.Position == nil {
		return nil
	}
	if snap.MexcPrice <= 0 {
		return fmt.Errorf("missing mexc price for close")
	}
	pos := snap.Position

	var side int
	if pos.Direction == directionLong {
		side = mexc.SideCloseLong
	} else {
		side = mexc.SideCloseShort
	}

	order := mexc.MarketOrder{
		Symbol:     state.RestSymbol,
		Side:       side,
		Volume:     pos.Amount,
		Price:      snap.MexcPrice,
		OpenType:   2,
		Leverage:   b.cfg.Leverage,
		ReduceOnly: true,
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := b.client.SubmitMarketOrder(ctx, order)
	if err != nil {
		return err
	}
	if resp != nil && !resp.Success {
		return fmt.Errorf("mexc rejected order: %s", resp.Msg)
	}

	state.ClearPosition()
	pnl := 0.0
	if b.tracker != nil {
		pnl = b.tracker.RecordTrade(state.MexcSymbol, pos.Direction, pos.EntryPrice, snap.MexcPrice, pos.Amount, state.Contract.ContractSize, spread)
	}
	log.Printf("%s closed %s (%s) pnl %+.4f", state.MexcSymbol, pos.Direction, reason, pnl)
	b.notifyf(b.formatCloseMessage(state, pos, snap.MexcPrice, spread, reason, pnl))
	return nil
}

// CloseAllPositions пытается закрыть все активные позиции (например, при остановке бота).
func (b *Bot) CloseAllPositions(ctx context.Context) error {
	for _, state := range b.states {
		snap := state.Snapshot()
		if snap.Position == nil {
			continue
		}
		spread := 0.0
		if snap.MexcPrice > 0 && snap.DexPrice > 0 {
			spread = calculateSpread(snap.MexcPrice, snap.DexPrice)
		}
		if err := b.closePosition(ctx, state, "shutdown", spread); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) computeAmount(state *SymbolState, price float64) (float64, error) {
	if price <= 0 {
		return 0, fmt.Errorf("invalid price")
	}
	contract := state.Contract
	if contract.ContractSize <= 0 {
		return 0, fmt.Errorf("contract size missing")
	}
	raw := b.cfg.QuoteSize / (price * contract.ContractSize)
	step := contract.VolumeStep
	if step <= 0 {
		step = 1
	}
	quantized := math.Floor(raw/step) * step
	if quantized < contract.MinVolume {
		return 0, fmt.Errorf("amount %.6f below min %.6f", quantized, contract.MinVolume)
	}
	return quantized, nil
}

func calculateSpread(mexcPrice, dexPrice float64) float64 {
	if dexPrice == 0 {
		return 0
	}
	return (mexcPrice - dexPrice) / dexPrice * 100
}

func calculateAdverseMove(pos *Position, current float64) float64 {
	if pos == nil || pos.EntryPrice == 0 {
		return 0
	}
	change := (current - pos.EntryPrice) / pos.EntryPrice * 100
	if pos.Direction == directionLong {
		if change < 0 {
			return -change
		}
		return 0
	}
	if change > 0 {
		return change
	}
	return 0
}

func (b *Bot) notifyf(format string, args ...interface{}) {
	if b.notifier == nil {
		return
	}
	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}
	go func(msg string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := b.notifier.Notify(ctx, msg); err != nil {
			log.Printf("notify send failed: %v", err)
		}
	}(message)
}

func (b *Bot) dailyReportLoop(ctx context.Context) error {
	if b.notifier == nil || b.tracker == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	next := nextReportTime(time.Now())
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			report := b.tracker.BuildReport(next)
			ctxNotify, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := b.notifier.Notify(ctxNotify, report); err != nil {
				log.Printf("failed to send daily report: %v", err)
			}
			cancel()
			b.tracker.ResetDaily(next)
			next = next.Add(24 * time.Hour)
			timer.Reset(time.Until(next))
		}
	}
}

func (b *Bot) formatOpenMessage(state *SymbolState, dir Direction, price, amount, spread float64) string {
	var builder strings.Builder
	emoji := "🔔"
	builder.WriteString(fmt.Sprintf("%s Открыт %s\n", emoji, strings.ToUpper(string(dir))))
	builder.WriteString(fmt.Sprintf("• Инструмент: %s\n", state.RestSymbol))
	builder.WriteString(fmt.Sprintf("• Размер: %.4f\n", amount))
	builder.WriteString(fmt.Sprintf("• Вход: %.6f\n", price))
	builder.WriteString(fmt.Sprintf("• Спред входа: %.2f%%\n", spread))
	notional := price * amount * state.Contract.ContractSize
	builder.WriteString(fmt.Sprintf("• Сумма входа: %.2f USDT\n", notional))
	builder.WriteString(fmt.Sprintf("• Плечо: x%d\n", b.cfg.Leverage))
	if line := b.balanceLine(); line != "" {
		builder.WriteString(line)
	}
	return builder.String()
}

func (b *Bot) formatCloseMessage(state *SymbolState, pos *Position, exitPrice, exitSpread float64, reason string, pnl float64) string {
	var builder strings.Builder
	emoji := "❌"
	if pnl >= 0 {
		emoji = "✅"
	}
	builder.WriteString(fmt.Sprintf("%s Закрыт %s\n", emoji, strings.ToUpper(string(pos.Direction))))
	builder.WriteString(fmt.Sprintf("• Инструмент: %s\n", state.RestSymbol))
	builder.WriteString(fmt.Sprintf("• Размер: %.4f\n", pos.Amount))
	builder.WriteString(fmt.Sprintf("• Вход: %.6f\n", pos.EntryPrice))
	builder.WriteString(fmt.Sprintf("• Выход: %.6f\n", exitPrice))
	builder.WriteString(fmt.Sprintf("• Спред входа: %.2f%%\n", pos.EntrySpread))
	builder.WriteString(fmt.Sprintf("• Спред выхода: %.2f%%\n", exitSpread))
	entryNotional := pos.EntryPrice * pos.Amount * state.Contract.ContractSize
	exitNotional := exitPrice * pos.Amount * state.Contract.ContractSize
	builder.WriteString(fmt.Sprintf("• Сумма входа: %.2f USDT\n", entryNotional))
	builder.WriteString(fmt.Sprintf("• Сумма выхода: %.2f USDT\n", exitNotional))
	change := (exitPrice - pos.EntryPrice) / pos.EntryPrice * 100
	if pos.Direction == directionShort {
		change = -change
	}
	builder.WriteString(fmt.Sprintf("• PnL: %+.4f USDT (%+.3f%%)\n", pnl, change))
	builder.WriteString(fmt.Sprintf("• Причина: %s\n", reason))
	if line := b.balanceLine(); line != "" {
		builder.WriteString(line)
	}
	return builder.String()
}

func (b *Bot) balanceLine() string {
	if b.tracker == nil {
		return ""
	}
	return fmt.Sprintf("Баланс: %.2f USDT\n", b.tracker.CurrentBalance())
}
