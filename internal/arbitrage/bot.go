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
	total := len(b.states)
	if total == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	batchSize := b.cfg.MexcPriceBatchSize
	if batchSize <= 0 || batchSize > total {
		batchSize = total
	}
	start := 0

	targets, next := b.batchTargets(start, batchSize)
	if err := b.refreshMexcPrices(ctx, targets); err != nil {
		log.Printf("initial price fetch failed: %v", err)
	}
	start = next
	ticker := time.NewTicker(b.cfg.MexcPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			targets, next := b.batchTargets(start, batchSize)
			if err := b.refreshMexcPrices(ctx, targets); err != nil {
				log.Printf("mexc price refresh failed: %v", err)
			}
			start = next
		}
	}
}

func (b *Bot) batchTargets(start, size int) ([]*SymbolState, int) {
	total := len(b.states)
	if total == 0 {
		return nil, 0
	}
	if size <= 0 || size >= total {
		return b.states, 0
	}
	if start >= total {
		start = 0
	}
	end := start + size
	if end > total {
		end = total
	}
	targets := b.states[start:end]
	next := end
	if next >= total {
		next = 0
	}
	return targets, next
}

func (b *Bot) refreshMexcPrices(ctx context.Context, targets []*SymbolState) error {
	if len(targets) == 0 {
		return nil
	}
	workerCount := b.cfg.MexcPriceWorkers
	if workerCount > len(targets) {
		workerCount = len(targets)
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
	for _, state := range targets {
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

	limitPrice := b.adjustEntryPrice(price, dir)

	side := mexc.SideOpenShort
	if dir == directionLong {
		side = mexc.SideOpenLong
	}

	order := mexc.MarketOrder{
		Symbol:     state.RestSymbol,
		Side:       side,
		Volume:     amount,
		Price:      limitPrice,
		OpenType:   2,
		Leverage:   b.cfg.Leverage,
		ReduceOnly: false,
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if _, err := b.submitOrderWithScaling(ctx, state, order, b.client.SubmitLimitOrder); err != nil {
		return err
	}

	state.SetPosition(&Position{
		Direction:   dir,
		Amount:      amount,
		EntryPrice:  limitPrice,
		EntrySpread: spread,
		OpenedAt:    time.Now(),
	})
	balance := b.refreshBalance()
	log.Printf("%s opened %s at %.6f amount %.6f spread %.2f%%", state.MexcSymbol, dir, limitPrice, amount, spread)
	b.notifyf(b.formatOpenMessage(state, dir, limitPrice, spread, balance))
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

	exitPrice := b.adjustExitPrice(snap.MexcPrice, pos.Direction)

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
		Price:      exitPrice,
		OpenType:   2,
		Leverage:   b.cfg.Leverage,
		ReduceOnly: true,
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if _, err := b.submitOrderWithScaling(ctx, state, order, b.client.SubmitLimitOrder); err != nil {
		return err
	}

	state.ClearPosition()
	pnl := 0.0
	if b.tracker != nil {
		pnl = b.tracker.RecordTrade(state.MexcSymbol, pos.Direction, pos.EntryPrice, exitPrice, pos.Amount, state.Contract.ContractSize, spread)
	}
	balance := b.refreshBalance()
	log.Printf("%s closed %s (%s) pnl %+.4f", state.MexcSymbol, pos.Direction, reason, pnl)
	b.notifyf(b.formatCloseMessage(state, pos, exitPrice, spread, pnl, balance))
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
	quote := b.cfg.QuoteSize
	raw := quote / (price * contract.ContractSize)
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
			b.refreshBalance()
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

func (b *Bot) formatOpenMessage(state *SymbolState, dir Direction, price, spread, balance float64) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("🔔 Открыт %s\n", strings.ToUpper(string(dir))))
	builder.WriteString(fmt.Sprintf("• Инструмент: %s\n", state.RestSymbol))
	builder.WriteString(fmt.Sprintf("• Вход: %.6f\n", price))
	builder.WriteString(fmt.Sprintf("• Спред входа: %.2f%%\n", spread))
	builder.WriteString(fmt.Sprintf("Баланс: %.2f USDT\n", balance))
	return builder.String()
}

func (b *Bot) formatCloseMessage(state *SymbolState, pos *Position, exitPrice, exitSpread float64, pnl float64, balance float64) string {
	var builder strings.Builder
	emoji := "❌"
	if pnl >= 0 {
		emoji = "✅"
	}
	builder.WriteString(fmt.Sprintf("%s Закрыт %s\n", emoji, strings.ToUpper(string(pos.Direction))))
	builder.WriteString(fmt.Sprintf("• Инструмент: %s\n", state.RestSymbol))
	builder.WriteString(fmt.Sprintf("• Вход: %.6f\n", pos.EntryPrice))
	builder.WriteString(fmt.Sprintf("• Выход: %.6f\n", exitPrice))
	builder.WriteString(fmt.Sprintf("• Спред входа: %.2f%%\n", pos.EntrySpread))
	builder.WriteString(fmt.Sprintf("• Спред выхода: %.2f%%\n", exitSpread))
	change := (exitPrice - pos.EntryPrice) / pos.EntryPrice * 100
	if pos.Direction == directionShort {
		change = -change
	}
	builder.WriteString(fmt.Sprintf("• PnL: %+.4f USDT (%+.3f%%)\n", pnl, change))
	builder.WriteString(fmt.Sprintf("Баланс: %.2f USDT\n", balance))
	if delta, percent := b.totalPnL(); b.tracker != nil {
		builder.WriteString(fmt.Sprintf("Общий PNL: %+.2f USDT (%+.1f%%)\n", delta, percent))
	}
	return builder.String()
}

func (b *Bot) totalPnL() (float64, float64) {
	if b.tracker == nil {
		return 0, 0
	}
	return b.tracker.TotalPnL()
}

func (b *Bot) submitOrderWithScaling(ctx context.Context, state *SymbolState, order mexc.MarketOrder, submit func(context.Context, mexc.MarketOrder) (*mexc.SubmitOrderResponse, error)) (*mexc.SubmitOrderResponse, error) {
	amount := order.Volume
	step := state.Contract.VolumeStep
	if step <= 0 {
		step = 1
	}
	minVol := state.Contract.MinVolume
	const maxAttempts = 5

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := submit(ctx, order)
		if err == nil && (resp == nil || resp.Success) {
			return resp, nil
		}
		b.logSubmitError(state, order, err)
		if !b.shouldScaleOrder(err) || attempt == maxAttempts {
			if err != nil {
				return resp, err
			}
			return resp, fmt.Errorf("mexc rejected order")
		}

		amount = math.Floor((amount*0.8)/step) * step
		if amount < minVol {
			return resp, fmt.Errorf("volume %.6f below minimum %.6f after scaling", amount, minVol)
		}
		order.Volume = amount
		log.Printf("%s: scaling order to %.6f due to limit (attempt %d)", state.MexcSymbol, amount, attempt)
		time.Sleep(150 * time.Millisecond)
	}
	return nil, fmt.Errorf("failed to submit order after scaling attempts")
}

func (b *Bot) shouldScaleOrder(err error) bool {
	var statusErr *mexc.StatusError
	if errors.As(err, &statusErr) {
		if statusErr.Code == http.StatusTooManyRequests || statusErr.Code == http.StatusForbidden || statusErr.Code == http.StatusBadRequest {
			return true
		}
	}
	var orderErr *mexc.OrderError
	if errors.As(err, &orderErr) {
		msg := strings.ToLower(orderErr.Msg)
		keywords := []string{"limit", "max", "insufficient", "risk", "margin", "size"}
		for _, k := range keywords {
			if strings.Contains(msg, k) {
				return true
			}
		}
	}
	return false
}

func (b *Bot) logSubmitError(state *SymbolState, order mexc.MarketOrder, err error) {
	if err == nil {
		log.Printf("%s order rejected: unknown reason", state.MexcSymbol)
		return
	}
	switch e := err.(type) {
	case *mexc.OrderError:
		log.Printf("%s order rejected: %s", state.MexcSymbol, e.Msg)
	case *mexc.StatusError:
		log.Printf("%s order rejected: http %d %s", state.MexcSymbol, e.Code, e.Body)
	default:
		log.Printf("%s order rejected (%d %.6f): %v", state.MexcSymbol, order.Side, order.Price, err)
	}
}

func (b *Bot) adjustEntryPrice(price float64, dir Direction) float64 {
	offset := b.cfg.EntryOffsetPercent / 100
	if offset <= 0 {
		return price
	}
	if dir == directionLong {
		return price * (1 - offset)
	}
	return price * (1 + offset)
}

func (b *Bot) adjustExitPrice(price float64, dir Direction) float64 {
	offset := b.cfg.EntryOffsetPercent / 100
	if offset <= 0 {
		return price
	}
	if dir == directionLong {
		return price * (1 + offset)
	}
	return price * (1 - offset)
}

func (b *Bot) refreshBalance() float64 {
	if b.client == nil {
		if b.tracker != nil {
			return b.tracker.CurrentBalance()
		}
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	balance, err := b.client.GetAccountBalance(ctx, b.cfg.BalanceCurrency)
	if err != nil {
		log.Printf("failed to fetch account balance: %v", err)
		if b.tracker != nil {
			return b.tracker.CurrentBalance()
		}
		return 0
	}
	if b.tracker != nil {
		b.tracker.UpdateBalance(balance)
	}
	return balance
}
