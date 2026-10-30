// Package payment ödeme adımını yürütür (F-4, T-2).
//
// Saga'daki yeri:
//
//	order.placed ──► RequestConsumer: sağlayıcıdan ödeme iste (idempotent, outbox yeniden dener)
//	sağlayıcı ───► callback (HMAC)  ─┐
//	Reconciler ──► sağlayıcıya sor  ─┴► ApplyResult
//	                                     ├─ başarılı + sipariş açık   → stoğu kesinleştir, order.paid
//	                                     ├─ başarılı + sipariş kapalı → telafi: iade (payment.refund_requested)
//	                                     └─ başarısız                 → telafi: stok iadesi, order.payment_failed
//
// Çift callback, gecikmiş callback ve callback ile mutabakatın yarışı aynı
// yoldan geçer; ödeme satırı kilit altında kontrol edildiği için etki bir kezdir.
package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/order"
	"carsihub/internal/outbox"
	"carsihub/internal/platform/audit"
)

const EventRefundRequested = "payment.refund_requested"

var ErrNotFound = errors.New("ödeme bulunamadı")

type Status string

const (
	StatusPending         Status = "pending"
	StatusRequested       Status = "requested"
	StatusSucceeded       Status = "succeeded"
	StatusFailed          Status = "failed"
	StatusRefundRequested Status = "refund_requested"
	StatusRefunded        Status = "refunded"
	StatusAbandoned       Status = "abandoned"
)

// final sonuç uygulanmış durumlardır; yeni bir sonuç bunları değiştirmez.
func (s Status) final() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusRefundRequested, StatusRefunded:
		return true
	}
	return false
}

type Service struct {
	pool           *pgxpool.Pool
	orders         *order.Service
	provider       Provider
	callbackURL    string
	reconcileAfter time.Duration
	log            *slog.Logger
}

func NewService(pool *pgxpool.Pool, orders *order.Service, provider Provider, callbackURL string, reconcileAfter time.Duration, log *slog.Logger) *Service {
	return &Service{pool: pool, orders: orders, provider: provider, callbackURL: callbackURL, reconcileAfter: reconcileAfter, log: log}
}

// Outcome ApplyResult'ın ne yaptığını söyler (log, test ve yanıt için).
type Outcome string

const (
	OutcomePaid      Outcome = "paid"
	OutcomeFailed    Outcome = "failed"
	OutcomeRefunding Outcome = "refunding" // ödeme geldi ama sipariş kapanmıştı
	OutcomeDuplicate Outcome = "duplicate" // sonuç zaten uygulanmıştı
)

// ApplyResult sağlayıcıdan gelen sonucu (callback ya da mutabakat) uygular.
// Kilit sırası: sipariş → ödeme → teklif → rezervasyon.
func (s *Service) ApplyResult(ctx context.Context, paymentID, result, reason, source string) (Outcome, error) {
	if result != ProviderSucceeded && result != ProviderFailed {
		return "", fmt.Errorf("uygulanamayan sonuç: %q", result)
	}
	var orderID string
	err := s.pool.QueryRow(ctx, `SELECT order_id::text FROM payments WHERE id = $1::uuid`, paymentID).Scan(&orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, orderStatus, err := order.LockTx(ctx, tx, orderID)
	if err != nil {
		return "", err
	}
	var current Status
	if err := tx.QueryRow(ctx, `SELECT status FROM payments WHERE id = $1::uuid FOR UPDATE`, paymentID).Scan(&current); err != nil {
		return "", err
	}
	if source == "callback" {
		if _, err := tx.Exec(ctx, `UPDATE payments SET callback_count = callback_count + 1 WHERE id = $1::uuid`, paymentID); err != nil {
			return "", err
		}
	}

	var outcome Outcome
	switch {
	case current.final():
		outcome = OutcomeDuplicate
	case result == ProviderFailed:
		if err := s.orders.FailPaymentTx(ctx, tx, orderID, orderStatus, reason); err != nil {
			return "", err
		}
		err = s.setStatus(ctx, tx, paymentID, StatusFailed, reason)
		outcome = OutcomeFailed
	default:
		err = s.orders.MarkPaidTx(ctx, tx, orderID, orderStatus)
		switch {
		case errors.Is(err, order.ErrNotCancellable):
			// Telafi: sipariş iptal edildi ya da süresi doldu (stok iade edildi) ama
			// para çekildi. Para iade edilmeli.
			if err = s.setStatus(ctx, tx, paymentID, StatusRefundRequested, "sipariş kapanmıştı: "+string(orderStatus)); err == nil {
				err = outbox.Write(ctx, tx, EventRefundRequested, "payment", paymentID, map[string]string{"payment_id": paymentID, "order_id": orderID})
			}
			outcome = OutcomeRefunding
		case err == nil:
			err = s.setStatus(ctx, tx, paymentID, StatusSucceeded, "")
			outcome = OutcomePaid
		}
	}
	if err != nil {
		return "", err
	}
	if outcome != OutcomeDuplicate {
		if err := audit.Record(ctx, tx, audit.Entry{
			Action: "payment." + string(outcome), EntityType: "payment", EntityID: paymentID,
			Old: map[string]any{"status": current}, New: map[string]any{"result": result, "source": source, "reason": reason},
		}); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	s.log.Info("ödeme sonucu", "payment_id", paymentID, "order_id", orderID, "result", result, "source", source, "outcome", outcome)
	return outcome, nil
}

func (s *Service) setStatus(ctx context.Context, tx pgx.Tx, id string, st Status, reason string) error {
	_, err := tx.Exec(ctx, `
		UPDATE payments SET status = $2, failure_reason = NULLIF($3, ''),
		       completed_at = CASE WHEN $2 IN ('succeeded', 'failed') THEN now() ELSE completed_at END,
		       updated_at = now()
		WHERE id = $1::uuid`, id, st, reason)
	return err
}

// ── Tüketiciler ──────────────────────────────────────────────────────────

type placedPayload struct {
	OrderID   string `json:"order_id"`
	PaymentID string `json:"payment_id"`
}

// RequestConsumer order.placed olayında sağlayıcıdan ödeme ister. Sağlayıcıya
// ulaşılamazsa hata döner; outbox olayı üstel beklemeyle yeniden dener.
type RequestConsumer struct{ s *Service }

func (s *Service) RequestConsumer() *RequestConsumer { return &RequestConsumer{s} }

func (c *RequestConsumer) Name() string { return "payment.request" }

func (c *RequestConsumer) Handle(ctx context.Context, e outbox.Event) error {
	var p placedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return err
	}
	var (
		status      Status
		amount      int64
		orderStatus order.Status
	)
	err := c.s.pool.QueryRow(ctx, `
		SELECT p.status, p.amount_kurus, o.status FROM payments p JOIN orders o ON o.id = p.order_id
		WHERE p.id = $1::uuid`, p.PaymentID).Scan(&status, &amount, &orderStatus)
	if err != nil {
		return err
	}
	if status != StatusPending {
		return nil // daha önceki bir teslimde istendi
	}
	if orderStatus != order.StatusPendingPayment {
		// Ödeme istenmeden sipariş kapandı (müşteri iptali): sağlayıcıya hiç gitme.
		_, err := outbox.Once(ctx, c.s.pool, c.Name(), e.ID, func(tx pgx.Tx) error {
			return c.s.setStatus(ctx, tx, p.PaymentID, StatusAbandoned, "ödeme istenmeden sipariş kapandı")
		})
		return err
	}
	// Dış çağrı transaction dışında. Ödeme kimliği sağlayıcıda idempotency
	// anahtarıdır: bu adım tekrar çalışsa da müşteriden iki kez çekilmez.
	if err := c.s.provider.Create(ctx, p.PaymentID, amount, c.s.callbackURL); err != nil {
		return err
	}
	_, err = outbox.Once(ctx, c.s.pool, c.Name(), e.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE payments SET status = 'requested', requested_at = now(), updated_at = now()
			WHERE id = $1::uuid AND status = 'pending'`, p.PaymentID)
		return err
	})
	return err
}

// RefundConsumer payment.refund_requested olayında sağlayıcıdan iade ister.
type RefundConsumer struct{ s *Service }

func (s *Service) RefundConsumer() *RefundConsumer { return &RefundConsumer{s} }

func (c *RefundConsumer) Name() string { return "payment.refund" }

func (c *RefundConsumer) Handle(ctx context.Context, e outbox.Event) error {
	var p struct {
		PaymentID string `json:"payment_id"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return err
	}
	if err := c.s.provider.Refund(ctx, p.PaymentID); err != nil {
		return err
	}
	_, err := outbox.Once(ctx, c.s.pool, c.Name(), e.ID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE payments SET status = 'refunded', updated_at = now()
			WHERE id = $1::uuid AND status = 'refund_requested'`, p.PaymentID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "payment.refunded", EntityType: "payment", EntityID: p.PaymentID})
	})
	return err
}

// ── Mutabakat ────────────────────────────────────────────────────────────

// Reconcile sonucu gecikmiş ödemeleri sağlayıcıya sorar (ödeme zaman aşımı):
//   - sonuç varsa callback gelmiş gibi uygular (callback kaybolmuş olabilir)
//   - sağlayıcı ödemeyi tanımıyorsa yeniden ister (istek yolda kaybolmuş ya da sağlayıcı veriyi yitirmiş)
//   - hâlâ bekliyorsa ve sipariş kapandıysa ödemeyi terk edilmiş sayar
//
// İşlenen ödeme sayısını döner.
func (s *Service) Reconcile(ctx context.Context, limit int) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id::text, p.amount_kurus, o.status
		FROM payments p JOIN orders o ON o.id = p.order_id
		WHERE p.status = 'requested'
		  AND p.requested_at <= now() - $1::interval
		  AND (p.last_checked_at IS NULL OR p.last_checked_at <= now() - $1::interval)
		ORDER BY p.requested_at
		LIMIT $2`, s.reconcileAfter.String(), limit)
	if err != nil {
		return 0, err
	}
	type due struct {
		id          string
		amount      int64
		orderStatus order.Status
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
		var d due
		err := r.Scan(&d.id, &d.amount, &d.orderStatus)
		return d, err
	})
	if err != nil {
		return 0, err
	}

	var errs []error
	for _, d := range list {
		if err := s.reconcileOne(ctx, d.id, d.amount, d.orderStatus); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.id, err))
		}
	}
	return len(list), errors.Join(errs...)
}

func (s *Service) reconcileOne(ctx context.Context, id string, amount int64, orderStatus order.Status) error {
	if _, err := s.pool.Exec(ctx, `UPDATE payments SET last_checked_at = now() WHERE id = $1::uuid`, id); err != nil {
		return err
	}
	status, reason, err := s.provider.Status(ctx, id)
	switch {
	case errors.Is(err, ErrProviderNotFound):
		if orderStatus != order.StatusPendingPayment {
			return s.abandon(ctx, id, "sağlayıcı ödemeyi tanımıyor, sipariş kapandı")
		}
		s.log.Warn("sağlayıcı ödemeyi tanımıyor, yeniden isteniyor", "payment_id", id)
		return s.provider.Create(ctx, id, amount, s.callbackURL)
	case err != nil:
		return err
	case status == ProviderSucceeded || status == ProviderFailed:
		_, err := s.ApplyResult(ctx, id, status, reason, "reconcile")
		return err
	case orderStatus != order.StatusPendingPayment:
		// Sağlayıcı hâlâ işlemde ama sipariş kapandı. Sonradan başarı gelirse
		// ApplyResult iade başlatır.
		return s.abandon(ctx, id, "ödeme süresinde sonuçlanmadı")
	}
	return nil
}

func (s *Service) abandon(ctx context.Context, id, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE payments SET status = 'abandoned', failure_reason = $2, updated_at = now()
		WHERE id = $1::uuid AND status = 'requested'`, id, reason)
	return err
}
