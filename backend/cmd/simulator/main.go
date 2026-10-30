// Komut simulator dış bağımlılıkları (ödeme sağlayıcı, kargo firması) taklit eder.
//
//	simulator              HTTP sunucusunu başlatır (SIM_PORT)
//	simulator healthcheck  konteyner sağlık kontrolü
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"carsihub/internal/platform/logx"
	"carsihub/internal/simulator"
)

func main() {
	port := envInt("SIM_PORT", 8090)
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(port))
	}
	log := logx.New(os.Stdout, env("LOG_LEVEL", "info"))

	secret := env("WEBHOOK_SECRET", "")
	if len(secret) < 32 {
		fmt.Fprintln(os.Stderr, "WEBHOOK_SECRET: en az 32 karakter olmalı")
		os.Exit(1)
	}
	cfg := simulator.Config{
		Seed:             int64(envInt("SIM_SEED", 42)),
		Secret:           []byte(secret),
		DeclineRate:      envFloat("SIM_PAYMENT_DECLINE_RATE", 0.05),
		LostCallbackRate: envFloat("SIM_PAYMENT_TIMEOUT_RATE", 0.05),
		DupCallbackRate:  envFloat("SIM_PAYMENT_DUP_CALLBACK_RATE", 0.05),
		PaymentLatency:   envDuration("SIM_PAYMENT_LATENCY", time.Second),
		ShipmentStep:     envDuration("SIM_SHIPMENT_STEP", 3*time.Second),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           simulator.New(ctx, cfg, log).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Info("simülatör dinlemede", "addr", srv.Addr, "seed", cfg.Seed,
		"decline_rate", cfg.DeclineRate, "lost_callback_rate", cfg.LostCallbackRate, "dup_callback_rate", cfg.DupCallbackRate)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("simülatör durdu", "error", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	n, err := strconv.Atoi(env(k, ""))
	if err != nil {
		return def
	}
	return n
}

func envFloat(k string, def float64) float64 {
	f, err := strconv.ParseFloat(env(k, ""), 64)
	if err != nil || f < 0 || f > 1 {
		return def
	}
	return f
}

func envDuration(k string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(env(k, ""))
	if err != nil || d < 0 {
		return def
	}
	return d
}

func healthcheck(port int) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
	if err != nil {
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
