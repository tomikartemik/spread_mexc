package main

import (
	"context"
	"errors"
	"log"
	"os/signal"
	"syscall"
	"time"

	"spread_mexc/internal/arbitrage"
	"spread_mexc/internal/config"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	bot, err := arbitrage.NewBot(cfg)
	if err != nil {
		log.Fatalf("bot init error: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("starting arbitrage bot for %d symbols", len(cfg.Symbols))
	if err := bot.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("bot stopped with error: %v", err)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelShutdown()
	if err := bot.CloseAllPositions(shutdownCtx); err != nil {
		log.Printf("failed to close positions on shutdown: %v", err)
	}

	log.Println("bot stopped")
}
