-- +goose Up

-- ── Katalog ──────────────────────────────────────────────────────────────
-- Ürün ve varyant pazaryerine aittir; aynı varyantı birden fazla satıcı
-- kendi fiyatı ve stoğuyla satabilir (offers). Stok satıcı × varyant bazındadır.

CREATE TABLE sellers (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid UNIQUE REFERENCES users (id),
    name       text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE categories (
    id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    parent_id bigint REFERENCES categories (id),
    name      text NOT NULL,
    slug      text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9-]+$')
);

CREATE TABLE products (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    category_id bigint NOT NULL REFERENCES categories (id),
    name        text NOT NULL CHECK (length(name) BETWEEN 1 AND 300),
    description text NOT NULL DEFAULT '',
    attributes  jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(attributes) = 'object'),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX products_keyset_idx ON products (created_at DESC, id DESC);

CREATE TABLE product_variants (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id uuid NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    sku        text NOT NULL UNIQUE,
    attributes jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(attributes) = 'object'),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX product_variants_product_idx ON product_variants (product_id);

-- Para kuruş (bigint). Satılabilir stok = stock_on_hand - stock_reserved.
-- CHECK kısıtı son savunma hattıdır: uygulama hatası olsa bile eksi stok
-- ya da eldekinden fazla rezervasyon veritabanına yazılamaz.
CREATE TABLE offers (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seller_id      uuid NOT NULL REFERENCES sellers (id),
    variant_id     uuid NOT NULL REFERENCES product_variants (id),
    price_kurus    bigint NOT NULL CHECK (price_kurus > 0),
    stock_on_hand  integer NOT NULL CHECK (stock_on_hand >= 0),
    stock_reserved integer NOT NULL DEFAULT 0 CHECK (stock_reserved >= 0),
    active         boolean NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (seller_id, variant_id),
    CONSTRAINT offers_reserved_le_on_hand CHECK (stock_reserved <= stock_on_hand)
);
CREATE INDEX offers_variant_idx ON offers (variant_id) WHERE active;
CREATE INDEX offers_seller_keyset_idx ON offers (seller_id, created_at DESC, id DESC);

-- ── Siparişler (F-3) ─────────────────────────────────────────────────────
-- Müşteri tek sipariş verir; satıcı başına bir alt sipariş oluşur.

CREATE TABLE orders (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id      uuid NOT NULL REFERENCES users (id),
    status           text NOT NULL CHECK (status IN ('pending_payment', 'paid', 'cancelled', 'expired')),
    subtotal_kurus   bigint NOT NULL CHECK (subtotal_kurus > 0),
    discount_kurus   bigint NOT NULL DEFAULT 0 CHECK (discount_kurus >= 0),
    total_kurus      bigint NOT NULL,
    payment_deadline timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CHECK (total_kurus = subtotal_kurus - discount_kurus AND total_kurus >= 0)
);
CREATE INDEX orders_customer_keyset_idx ON orders (customer_id, created_at DESC, id DESC);
-- Ödeme süresi dolan siparişleri bulan süpürücü için.
CREATE INDEX orders_pending_deadline_idx ON orders (payment_deadline) WHERE status = 'pending_payment';

CREATE TABLE sub_orders (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id       uuid NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    seller_id      uuid NOT NULL REFERENCES sellers (id),
    status         text NOT NULL CHECK (status IN ('pending_payment', 'awaiting_shipment', 'cancelled', 'expired')),
    subtotal_kurus bigint NOT NULL CHECK (subtotal_kurus > 0),
    discount_kurus bigint NOT NULL DEFAULT 0 CHECK (discount_kurus >= 0),
    total_kurus    bigint NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (order_id, seller_id),
    CHECK (total_kurus = subtotal_kurus - discount_kurus AND total_kurus >= 0)
);
CREATE INDEX sub_orders_seller_idx ON sub_orders (seller_id, created_at DESC);

-- ── Stok rezervasyonları (T-1) ───────────────────────────────────────────
-- active: stok tutuluyor (sepette ya da ödeme bekleyen siparişte)
-- committed: ödeme alındı, stok eldeki miktardan düşüldü
-- released: elle bırakıldı (sepetten çıkarma, iptal, ödeme hatası)
-- expired: süresi doldu, süpürücü bıraktı
CREATE TABLE stock_reservations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    offer_id   uuid NOT NULL REFERENCES offers (id),
    user_id    uuid NOT NULL REFERENCES users (id),
    order_id   uuid REFERENCES orders (id),
    quantity   integer NOT NULL CHECK (quantity > 0),
    status     text NOT NULL CHECK (status IN ('active', 'committed', 'released', 'expired')),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
-- Süpürücü yalnızca aktif ve sepette (siparişe bağlanmamış) olanları tarar.
CREATE INDEX stock_reservations_expiry_idx ON stock_reservations (expires_at) WHERE status = 'active' AND order_id IS NULL;
CREATE INDEX stock_reservations_order_idx ON stock_reservations (order_id) WHERE order_id IS NOT NULL;

CREATE TABLE order_items (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id          uuid NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    sub_order_id      uuid NOT NULL REFERENCES sub_orders (id) ON DELETE CASCADE,
    offer_id          uuid NOT NULL REFERENCES offers (id),
    reservation_id    uuid NOT NULL UNIQUE REFERENCES stock_reservations (id),
    quantity          integer NOT NULL CHECK (quantity > 0),
    unit_price_kurus  bigint NOT NULL CHECK (unit_price_kurus > 0),
    line_total_kurus  bigint NOT NULL,
    CHECK (line_total_kurus = quantity * unit_price_kurus)
);
CREATE INDEX order_items_sub_order_idx ON order_items (sub_order_id);

-- ── Sepet (F-2) ──────────────────────────────────────────────────────────
-- Sepet kalemi bir rezervasyonla stok tutar. Rezervasyon süresi dolunca kalem
-- sepette kalır; ödeme adımında yeniden rezerve edilmeye çalışılır.
CREATE TABLE cart_items (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid NOT NULL REFERENCES users (id),
    offer_id       uuid NOT NULL REFERENCES offers (id),
    quantity       integer NOT NULL CHECK (quantity BETWEEN 1 AND 100),
    reservation_id uuid UNIQUE REFERENCES stock_reservations (id),
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, offer_id)
);

-- ── Idempotency-Key ──────────────────────────────────────────────────────
-- Durum değiştiren kritik POST'larda aynı anahtarla gelen tekrar istek
-- işlemi yeniden yapmaz, ilk yanıtı aynen döner.
CREATE TABLE idempotency_keys (
    user_id       uuid NOT NULL REFERENCES users (id),
    key           text NOT NULL CHECK (length(key) BETWEEN 1 AND 200),
    method        text NOT NULL,
    path          text NOT NULL,
    request_hash  bytea NOT NULL,
    status        text NOT NULL CHECK (status IN ('processing', 'completed')),
    response_code integer,
    response_body bytea,
    created_at    timestamptz NOT NULL DEFAULT now(),
    completed_at  timestamptz,
    PRIMARY KEY (user_id, key)
);

-- +goose Down
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS cart_items;
DROP TABLE IF EXISTS order_items;
DROP TABLE IF EXISTS stock_reservations;
DROP TABLE IF EXISTS sub_orders;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS offers;
DROP TABLE IF EXISTS product_variants;
DROP TABLE IF EXISTS products;
DROP TABLE IF EXISTS categories;
DROP TABLE IF EXISTS sellers;
