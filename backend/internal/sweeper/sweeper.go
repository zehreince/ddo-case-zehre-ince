// Package sweeper süresi dolan stok tutmalarını periyodik olarak bırakır:
//   - süresi dolan sepet rezervasyonları (RESERVATION_TTL)
//   - ödeme süresi dolan siparişler (ORDER_PAYMENT_TTL) → stok iadesi
//
// Worker süreci içinde çalışır. Her tur küçük partilerle ilerler; böylece
// uzun bir transaction ne kilitleri uzun tutar ne de büyük bir geri alma riski taşır.
package sweeper

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"carsihub/internal/inventory"
	"carsihub/internal/order"
)

const batchSize = 500

type Sweeper struct {
	pool     *pgxpool.Pool
	orders   *order.Service
	interval time.Duration
	log      *slog.Logger
	now      func() time.Time

	lastSuccess atomic.Int64 // unix nano

	expiredReservations prometheus.Counter
	expiredOrders       prometheus.Counter
}

func New(pool *pgxpool.Pool, orders *order.Service, interval time.Duration, log *slog.Logger) *Sweeper {
	return &Sweeper{
		pool: pool, orders: orders, interval: interval, log: log, now: time.Now,
		expiredReservations: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sweeper_expired_cart_reservations_total", Help: "Süresi dolduğu için bırakılan sepet rezervasyonları.",
		}),
		expiredOrders: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sweeper_expired_orders_total", Help: "Ödeme süresi dolduğu için kapatılan siparişler.",
		}),
	}
}

func (s *Sweeper) Collectors() []prometheus.Collector {
	return []prometheus.Collector{s.expiredReservations, s.expiredOrders}
}

// Run bağlam iptal edilene kadar her aralıkta bir tur atar.
func (s *Sweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Sweeper) tick(ctx context.Context) {
	carts, orders, err := s.SweepOnce(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("süpürme başarısız", "error", err)
		}
		return
	}
	s.lastSuccess.Store(s.now().UnixNano())
	if carts+orders > 0 {
		s.log.Info("süpürme", "expired_cart_reservations", carts, "expired_orders", orders)
	}
}

// SweepOnce bekleyen her şeyi partiler halinde işler.
func (s *Sweeper) SweepOnce(ctx context.Context) (carts, orders int, err error) {
	for {
		n, err := s.expireCartBatch(ctx)
		carts += n
		if err != nil || n < batchSize {
			if err != nil {
				return carts, orders, err
			}
			break
		}
	}
	for {
		n, err := s.orders.ExpireUnpaid(ctx, batchSize)
		orders += n
		s.expiredOrders.Add(float64(n))
		if err != nil {
			return carts, orders, err
		}
		if n < batchSize {
			break
		}
	}
	return carts, orders, nil
}

func (s *Sweeper) expireCartBatch(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	n, err := inventory.ExpireCartReservations(ctx, tx, s.now(), batchSize)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	s.expiredReservations.Add(float64(n))
	return n, nil
}

// Healthy son başarılı tur üç aralıktan eskiyse false döner.
func (s *Sweeper) Healthy() bool {
	last := s.lastSuccess.Load()
	return last != 0 && s.now().Sub(time.Unix(0, last)) < 3*s.interval
}
