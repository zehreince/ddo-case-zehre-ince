// Komut worker arka plan işlerini çalıştırır:
//
//   - outbox yayımlayıcısı (saga adımları: ödeme iste, satıcıya bildir, kargo oluştur, iade)
//
//   - ödeme mutabakatı (callback'i gecikmiş ödemeleri sağlayıcıya sorar)
//
//   - süresi dolan rezervasyon ve siparişlerin süpürülmesi
//
//     worker              işçiyi başlatır; :WORKER_PORT'ta /health ve /metrics
//     worker healthcheck  konteyner sağlık kontrolü
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"carsihub/internal/inventory"
	"carsihub/internal/notification"
	"carsihub/internal/order"
	"carsihub/internal/outbox"
	"carsihub/internal/payment"
	"carsihub/internal/platform/config"
	"carsihub/internal/platform/db"
	"carsihub/internal/platform/httpx"
	"carsihub/internal/platform/logx"
	"carsihub/internal/platform/metrics"
	"carsihub/internal/shipping"
	"carsihub/internal/sweeper"
)

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(cfg.WorkerPort))
	}
	log := logx.New(os.Stdout, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL, 10)
	if err != nil {
		log.Error("veritabanına bağlanılamadı", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	policy, _ := inventory.ParsePolicy(cfg.PartialFulfillment)
	secret := []byte(cfg.WebhookSecret)
	orders := order.NewService(pool, policy, cfg.PaymentTTL)
	payments := payment.NewService(pool, orders, payment.NewHTTPProvider(cfg.PaymentProviderURL, secret, 5*time.Second),
		cfg.PublicBaseURL+"/api/v1/payments/callback", cfg.ReconcileAfter, log)
	ship := shipping.NewService(pool, shipping.NewHTTPCarrier(cfg.CarrierURL, secret, 5*time.Second),
		cfg.PublicBaseURL+"/api/v1/shipping/callback")

	// Saga: her olay türünün tüketicileri.
	pub := outbox.NewPublisher(pool, log, cfg.OutboxBatch, cfg.OutboxMaxAttempts)
	pub.Subscribe(order.EventPlaced, payments.RequestConsumer())
	pub.Subscribe(order.EventPaid, notification.NewConsumer(pool))
	pub.Subscribe(order.EventPaid, ship.Consumer())
	pub.Subscribe(payment.EventRefundRequested, payments.RefundConsumer())

	sw := sweeper.New(pool, orders, cfg.SweepInterval, log)
	m := metrics.New()
	m.MustRegister(sw.Collectors()...)
	m.MustRegister(pub.Collectors()...)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		if !sw.Healthy() {
			httpx.JSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded"})
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("GET /metrics", m.Handler())
	srv := &http.Server{Addr: fmt.Sprintf(":%d", cfg.WorkerPort), Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("worker http sunucusu durdu", "error", err)
			stop()
		}
	}()

	log.Info("worker başladı", "sweep_interval", cfg.SweepInterval.String(), "outbox_poll", cfg.OutboxPoll.String())
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); pub.Run(ctx, cfg.OutboxPoll) }()
	go func() { defer wg.Done(); reconcileLoop(ctx, payments, cfg.ReconcileAfter/2, log) }()
	go func() { defer wg.Done(); sw.Run(ctx) }()
	wg.Wait()

	log.Info("worker kapanıyor")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func reconcileLoop(ctx context.Context, payments *payment.Service, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(max(every, time.Second))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if n, err := payments.Reconcile(ctx, 100); err != nil && ctx.Err() == nil {
			log.Warn("ödeme mutabakatı", "checked", n, "error", err)
		} else if n > 0 {
			log.Info("ödeme mutabakatı", "checked", n)
		}
	}
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
