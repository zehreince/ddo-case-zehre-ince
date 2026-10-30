// Komut api, ÇarşıHub HTTP API sunucusudur.
//
//	api              sunucuyu başlatır
//	api healthcheck  konteyner sağlık kontrolü (distroless imajda curl yok)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"carsihub/internal/auth"
	"carsihub/internal/cart"
	"carsihub/internal/catalog"
	"carsihub/internal/coupon"
	"carsihub/internal/inventory"
	"carsihub/internal/notification"
	"carsihub/internal/ops"
	"carsihub/internal/order"
	"carsihub/internal/payment"
	"carsihub/internal/platform/config"
	"carsihub/internal/platform/db"
	"carsihub/internal/platform/idempotency"
	"carsihub/internal/platform/logx"
	"carsihub/internal/platform/metrics"
	"carsihub/internal/server"
	"carsihub/internal/settlement"
	"carsihub/internal/shipping"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log := logx.New(os.Stdout, cfg.LogLevel)

	if err := run(cfg, log); err != nil {
		log.Error("sunucu durdu", "error", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	policy, err := inventory.ParsePolicy(cfg.PartialFulfillment)
	if err != nil {
		return err
	}
	tokens := auth.NewTokens(cfg.JWTSecret, cfg.AccessTTL)
	idem := idempotency.Middleware(pool, func(ctx context.Context) (string, bool) {
		p, ok := auth.PrincipalFrom(ctx)
		return p.UserID, ok
	}, log)
	orders := order.NewService(pool, policy, cfg.PaymentTTL)
	secret := []byte(cfg.WebhookSecret)
	payments := payment.NewService(pool, orders, payment.NewHTTPProvider(cfg.PaymentProviderURL, secret, 5*time.Second),
		cfg.PublicBaseURL+"/api/v1/payments/callback", cfg.ReconcileAfter, log)
	ship := shipping.NewService(pool, shipping.NewHTTPCarrier(cfg.CarrierURL, secret, 5*time.Second),
		cfg.PublicBaseURL+"/api/v1/shipping/callback")

	router := server.NewRouter(server.Deps{
		Config: cfg, Log: log, DB: pool, Metrics: metrics.New(),
		Modules: []server.Module{
			auth.NewHandler(auth.NewService(pool, tokens, cfg.RefreshTTL), tokens, log, cfg.CookieSecure),
			catalog.NewHandler(catalog.NewService(pool), tokens, log),
			cart.NewHandler(cart.NewService(pool, orders, cfg.ReservationTTL), tokens, idem, log),
			order.NewHandler(orders, tokens, idem, log),
			payment.NewHandler(payments, secret, log),
			shipping.NewHandler(ship, tokens, secret, log),
			notification.NewHandler(pool, tokens, log),
			ops.NewHandler(pool, tokens, log),
			coupon.NewHandler(coupon.NewService(pool), tokens, log),
			settlement.NewHandler(settlement.NewService(pool, cfg.PayoutDelay, cfg.ShippingFeeKurus), tokens, log),
		},
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.APIPort),
		Handler:           router,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("api dinlemede", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Düzgün kapanış: yeni bağlantı alma, süren istekleri bitir.
	log.Info("kapanıyor", "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func healthcheck() int {
	port := os.Getenv("API_PORT")
	if port == "" {
		port = "8080"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/health", nil)
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
