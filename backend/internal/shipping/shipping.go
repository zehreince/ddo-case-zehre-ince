// Package shipping saga'nın kargo adımıdır (F-6): ödemesi alınan her alt
// sipariş için kargo firmasından gönderi oluşturur ve firmadan gelen takip
// olaylarını işler.
//
// Kargo olayları sırasız ve tekrarlı gelebilir. Her olay event_id ile bir kez
// kaydedilir; gönderi durumu yalnızca ileri gider (created → picked_up →
// in_transit → delivered), geç gelen eski bir olay durumu geri almaz.
package shipping

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/outbox"
	"carsihub/internal/platform/audit"
	"carsihub/internal/platform/webhook"
)

const EventDelivered = "shipment.delivered"

var (
	ErrNotFound      = errors.New("gönderi bulunamadı")
	ErrInvalidStatus = errors.New("geçersiz kargo durumu")
)

var rank = map[string]int{"requested": 0, "created": 1, "picked_up": 2, "in_transit": 3, "delivered": 4}

// Carrier kargo firmasının sistemimizin kullandığı yüzüdür. Alt sipariş
// kimliğiyle idempotenttir: aynı gönderi iki kez istenirse aynı takip numarası döner.
type Carrier interface {
	Create(ctx context.Context, subOrderID, callbackURL string) (trackingNumber string, err error)
}

type HTTPCarrier struct {
	base   string
	secret []byte
	client *http.Client
}

func NewHTTPCarrier(base string, secret []byte, timeout time.Duration) *HTTPCarrier {
	return &HTTPCarrier{base: base, secret: secret, client: &http.Client{Timeout: timeout}}
}

func (c *HTTPCarrier) Create(ctx context.Context, subOrderID, callbackURL string) (string, error) {
	body, _ := json.Marshal(map[string]string{"id": subOrderID, "callback_url": callbackURL})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/shipments", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	webhook.SetHeaders(req.Header, c.secret, body, time.Now())
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("kargo firmasına ulaşılamadı: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("kargo firması %d döndü: %s", resp.StatusCode, b)
	}
	var r struct {
		TrackingNumber string `json:"tracking_number"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.TrackingNumber == "" {
		return "", fmt.Errorf("kargo yanıtı çözülemedi: %s", b)
	}
	return r.TrackingNumber, nil
}

type Service struct {
	pool        *pgxpool.Pool
	carrier     Carrier
	callbackURL string
}

func NewService(pool *pgxpool.Pool, carrier Carrier, callbackURL string) *Service {
	return &Service{pool: pool, carrier: carrier, callbackURL: callbackURL}
}

// ── order.paid → gönderi oluştur ─────────────────────────────────────────

type Consumer struct{ s *Service }

func (s *Service) Consumer() *Consumer { return &Consumer{s} }

func (c *Consumer) Name() string { return "shipping.create" }

// Handle her alt sipariş için gönderiyi durum bazlı idempotent olarak oluşturur:
// zaten oluşturulmuş gönderiye dokunmaz, yarım kalanı tamamlar. Kargo
// firmasına ulaşılamazsa hata döner ve outbox olayı yeniden dener.
func (c *Consumer) Handle(ctx context.Context, e outbox.Event) error {
	var p struct {
		SubOrders []struct {
			SubOrderID string `json:"sub_order_id"`
		} `json:"sub_orders"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return err
	}
	for _, so := range p.SubOrders {
		if err := c.s.ensureShipment(ctx, so.SubOrderID); err != nil {
			return err
		}
	}
	_, err := outbox.Once(ctx, c.s.pool, c.Name(), e.ID, func(pgx.Tx) error { return nil })
	return err
}

func (s *Service) ensureShipment(ctx context.Context, subOrderID string) error {
	var status string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO shipments (sub_order_id, status) VALUES ($1::uuid, 'requested')
		ON CONFLICT (sub_order_id) DO UPDATE SET sub_order_id = EXCLUDED.sub_order_id
		RETURNING status`, subOrderID).Scan(&status)
	if err != nil {
		return err
	}
	if status != "requested" {
		return nil
	}
	tracking, err := s.carrier.Create(ctx, subOrderID, s.callbackURL)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE shipments SET status = 'created', tracking_number = $2, updated_at = now()
		WHERE sub_order_id = $1::uuid AND status = 'requested'`, subOrderID, tracking)
	return err
}

// ── Kargo olayları ───────────────────────────────────────────────────────

type Event struct {
	EventID        string    `json:"event_id"`
	TrackingNumber string    `json:"tracking_number"`
	Status         string    `json:"status"`
	OccurredAt     time.Time `json:"occurred_at"`
}

// ApplyEvent bir kargo olayını işler. Aynı event_id ikinci kez gelirse
// (duplicate=true) hiçbir şey değişmez.
func (s *Service) ApplyEvent(ctx context.Context, e Event, actor string) (duplicate bool, err error) {
	if r, ok := rank[e.Status]; !ok || r < rank["picked_up"] {
		return false, ErrInvalidStatus
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var shipmentID, subOrderID, current string
	err = tx.QueryRow(ctx, `
		SELECT id::text, sub_order_id::text, status FROM shipments WHERE tracking_number = $1 FOR UPDATE`, e.TrackingNumber).
		Scan(&shipmentID, &subOrderID, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO shipment_events (shipment_id, event_id, status, occurred_at) VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (event_id) DO NOTHING`, shipmentID, e.EventID, e.Status, e.OccurredAt)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return true, nil
	}
	if rank[e.Status] > rank[current] {
		if err := s.advance(ctx, tx, shipmentID, subOrderID, current, e); err != nil {
			return false, err
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			ActorRole: actor, Action: "shipment." + e.Status, EntityType: "shipment", EntityID: shipmentID,
			Old: map[string]string{"status": current}, New: map[string]string{"status": e.Status, "event_id": e.EventID},
		}); err != nil {
			return false, err
		}
	}
	return false, tx.Commit(ctx)
}

func (s *Service) advance(ctx context.Context, tx pgx.Tx, shipmentID, subOrderID, from string, e Event) error {
	var deliveredAt *time.Time
	if e.Status == "delivered" {
		deliveredAt = &e.OccurredAt
	}
	if _, err := tx.Exec(ctx, `
		UPDATE shipments SET status = $2, delivered_at = $3, updated_at = now() WHERE id = $1::uuid`,
		shipmentID, e.Status, deliveredAt); err != nil {
		return err
	}
	subStatus := "shipped"
	if e.Status == "delivered" {
		subStatus = "delivered"
	}
	if _, err := tx.Exec(ctx, `UPDATE sub_orders SET status = $2 WHERE id = $1::uuid`, subOrderID, subStatus); err != nil {
		return err
	}
	if e.Status == "delivered" {
		// Hakediş vadesi teslim tarihinden başlar (T-4).
		return outbox.Write(ctx, tx, EventDelivered, "sub_order", subOrderID, map[string]any{
			"sub_order_id": subOrderID, "delivered_at": e.OccurredAt,
		})
	}
	return nil
}

type Shipment struct {
	TrackingNumber string       `json:"tracking_number"`
	Status         string       `json:"status"`
	SubOrderID     string       `json:"sub_order_id"`
	OrderID        string       `json:"order_id"`
	CustomerID     string       `json:"-"`
	DeliveredAt    *time.Time   `json:"delivered_at"`
	Events         []EventEntry `json:"events"`
}

type EventEntry struct {
	Status     string    `json:"status"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Track gönderiyi olay geçmişiyle döner.
func (s *Service) Track(ctx context.Context, tracking string) (*Shipment, error) {
	sh := Shipment{TrackingNumber: tracking, Events: []EventEntry{}}
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT sh.id::text, sh.status, sh.sub_order_id::text, so.order_id::text, o.customer_id::text, sh.delivered_at
		FROM shipments sh JOIN sub_orders so ON so.id = sh.sub_order_id JOIN orders o ON o.id = so.order_id
		WHERE sh.tracking_number = $1`, tracking).
		Scan(&id, &sh.Status, &sh.SubOrderID, &sh.OrderID, &sh.CustomerID, &sh.DeliveredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT status, occurred_at FROM shipment_events WHERE shipment_id = $1::uuid ORDER BY occurred_at, id`, id)
	if err != nil {
		return nil, err
	}
	sh.Events, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (EventEntry, error) {
		var e EventEntry
		err := r.Scan(&e.Status, &e.OccurredAt)
		return e, err
	})
	return &sh, err
}
