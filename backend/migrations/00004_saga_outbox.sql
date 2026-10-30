-- +goose Up

-- ── Outbox (T-2) ─────────────────────────────────────────────────────────
-- Olay, onu doğuran durum değişikliğiyle aynı transaction'da yazılır; ya ikisi
-- birden kalıcı olur ya hiçbiri. Worker olayları tüketicilere en az bir kez iletir.
CREATE TABLE outbox_events (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_type      text NOT NULL,
    aggregate_type  text NOT NULL,
    aggregate_id    text NOT NULL,
    payload         jsonb NOT NULL DEFAULT '{}',
    created_at      timestamptz NOT NULL DEFAULT now(),
    published_at    timestamptz,
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    -- Kiralama: işleyen worker bu süre boyunca olayı sahiplenir. Worker çökerse
    -- süre dolunca başka bir worker olayı devralır.
    lease_until     timestamptz,
    last_error      text,
    dead            boolean NOT NULL DEFAULT false
);
CREATE INDEX outbox_pending_idx ON outbox_events (next_attempt_at, id) WHERE published_at IS NULL AND NOT dead;
CREATE INDEX outbox_aggregate_idx ON outbox_events (aggregate_type, aggregate_id, id);

-- Tüketici idempotansı: (tüketici, olay) yalnızca bir kez işlenir. Kayıt,
-- tüketicinin kendi değişikliğiyle aynı transaction'da atılır.
CREATE TABLE processed_events (
    consumer     text NOT NULL,
    event_id     bigint NOT NULL REFERENCES outbox_events (id),
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

-- ── Ödeme (F-4) ──────────────────────────────────────────────────────────
-- Sipariş başına tek ödeme. id sağlayıcıya idempotency anahtarı olarak da gider.
CREATE TABLE payments (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id         uuid NOT NULL UNIQUE REFERENCES orders (id),
    amount_kurus     bigint NOT NULL CHECK (amount_kurus > 0),
    status           text NOT NULL CHECK (status IN (
                         'pending',          -- sağlayıcıya henüz iletilmedi
                         'requested',        -- sağlayıcı kabul etti, sonuç bekleniyor
                         'succeeded',
                         'failed',
                         'refund_requested', -- başarılı ama sipariş kapanmıştı → iade
                         'refunded',
                         'abandoned'         -- sipariş süresi doldu, sağlayıcı sonuç vermedi
                     )),
    failure_reason   text,
    requested_at     timestamptz,
    completed_at     timestamptz,
    callback_count   integer NOT NULL DEFAULT 0,
    last_checked_at  timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
-- Mutabakat: sonucu gecikmiş ödemeleri bulmak için.
CREATE INDEX payments_requested_idx ON payments (requested_at) WHERE status = 'requested';

-- ── Satıcı bildirimi ─────────────────────────────────────────────────────
CREATE TABLE seller_notifications (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seller_id    uuid NOT NULL REFERENCES sellers (id),
    sub_order_id uuid NOT NULL REFERENCES sub_orders (id),
    kind         text NOT NULL CHECK (kind IN ('new_order', 'order_cancelled')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    read_at      timestamptz,
    UNIQUE (sub_order_id, kind)
);
CREATE INDEX seller_notifications_seller_idx ON seller_notifications (seller_id, created_at DESC, id DESC);

-- ── Kargo (F-6) ──────────────────────────────────────────────────────────
CREATE TABLE shipments (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    sub_order_id    uuid NOT NULL UNIQUE REFERENCES sub_orders (id),
    tracking_number text UNIQUE,
    status          text NOT NULL CHECK (status IN ('requested', 'created', 'picked_up', 'in_transit', 'delivered', 'failed')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    delivered_at    timestamptz,
    CHECK ((status = 'delivered') = (delivered_at IS NOT NULL))
);

-- Kargo firmasından gelen olaylar; event_id ile tekilleştirilir (aynı olay iki kez gelebilir).
CREATE TABLE shipment_events (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    shipment_id uuid NOT NULL REFERENCES shipments (id),
    event_id    text NOT NULL UNIQUE,
    status      text NOT NULL CHECK (status IN ('picked_up', 'in_transit', 'delivered')),
    occurred_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX shipment_events_shipment_idx ON shipment_events (shipment_id, occurred_at);

-- Alt sipariş yaşam döngüsü kargo adımlarıyla genişler.
ALTER TABLE sub_orders DROP CONSTRAINT sub_orders_status_check;
ALTER TABLE sub_orders ADD CONSTRAINT sub_orders_status_check CHECK (status IN (
    'pending_payment', 'awaiting_shipment', 'shipped', 'delivered', 'shipment_failed', 'cancelled', 'expired'));

-- +goose Down
ALTER TABLE sub_orders DROP CONSTRAINT sub_orders_status_check;
ALTER TABLE sub_orders ADD CONSTRAINT sub_orders_status_check CHECK (status IN (
    'pending_payment', 'awaiting_shipment', 'cancelled', 'expired'));
DROP TABLE IF EXISTS shipment_events;
DROP TABLE IF EXISTS shipments;
DROP TABLE IF EXISTS seller_notifications;
DROP TABLE IF EXISTS payments;
DROP TABLE IF EXISTS processed_events;
DROP TABLE IF EXISTS outbox_events;
