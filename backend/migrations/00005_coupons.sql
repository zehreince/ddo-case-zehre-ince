-- +goose Up

-- ── Kuponlar (T-3) ───────────────────────────────────────────────────────
CREATE TABLE coupons (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code               text NOT NULL CHECK (code ~ '^[A-Z0-9_-]{3,32}$'),
    scope              text NOT NULL CHECK (scope IN ('marketplace', 'seller')),
    seller_id          uuid REFERENCES sellers (id),
    kind               text NOT NULL CHECK (kind IN ('percent', 'fixed')),
    value              bigint NOT NULL CHECK (value > 0),           -- percent: baz puan, fixed: kuruş
    max_discount_kurus bigint NOT NULL DEFAULT 0 CHECK (max_discount_kurus >= 0),
    min_basket_kurus   bigint NOT NULL DEFAULT 0 CHECK (min_basket_kurus >= 0),
    category_ids       bigint[] NOT NULL DEFAULT '{}',
    stackable          boolean NOT NULL DEFAULT true,
    starts_at          timestamptz NOT NULL DEFAULT now(),
    ends_at            timestamptz,
    max_redemptions    integer CHECK (max_redemptions > 0),
    per_user_limit     integer CHECK (per_user_limit > 0),
    redeemed_count     integer NOT NULL DEFAULT 0 CHECK (redeemed_count >= 0),
    active             boolean NOT NULL DEFAULT true,
    created_by         uuid REFERENCES users (id),
    created_at         timestamptz NOT NULL DEFAULT now(),
    CHECK ((scope = 'seller') = (seller_id IS NOT NULL)),
    CHECK (kind = 'fixed' OR value <= 10000),
    CHECK (kind = 'percent' OR max_discount_kurus = 0),
    CHECK (ends_at IS NULL OR ends_at > starts_at),
    -- Kullanım limiti son savunma hattı: koşullu UPDATE'i atlayan bir hata bile limiti aşamaz.
    CONSTRAINT coupons_redemption_limit CHECK (max_redemptions IS NULL OR redeemed_count <= max_redemptions)
);
CREATE UNIQUE INDEX coupons_code_key ON coupons (code);
CREATE INDEX coupons_seller_idx ON coupons (seller_id, created_at DESC) WHERE seller_id IS NOT NULL;

-- Sipariş başına kupon kullanımı; iptal/süre dolumu/ödeme hatasında bırakılır.
CREATE TABLE coupon_redemptions (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    coupon_id      uuid NOT NULL REFERENCES coupons (id),
    user_id        uuid NOT NULL REFERENCES users (id),
    order_id       uuid NOT NULL REFERENCES orders (id),
    discount_kurus bigint NOT NULL CHECK (discount_kurus >= 0),
    status         text NOT NULL CHECK (status IN ('active', 'released')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (coupon_id, order_id)
);
CREATE INDEX coupon_redemptions_user_idx ON coupon_redemptions (coupon_id, user_id) WHERE status = 'active';
CREATE INDEX coupon_redemptions_order_idx ON coupon_redemptions (order_id);

CREATE TABLE cart_coupons (
    user_id   uuid NOT NULL REFERENCES users (id),
    coupon_id uuid NOT NULL REFERENCES coupons (id),
    added_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, coupon_id)
);

-- Siparişte uygulanan kuponun o anki tanımı. Kupon sonradan değişse de kısmi
-- iade sipariş anındaki kurallarla hesaplanır.
CREATE TABLE order_coupons (
    order_id       uuid NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    coupon_id      uuid NOT NULL REFERENCES coupons (id),
    code           text NOT NULL,
    snapshot       jsonb NOT NULL,
    discount_kurus bigint NOT NULL CHECK (discount_kurus >= 0),
    PRIMARY KEY (order_id, coupon_id)
);

-- ── Satır ve alt sipariş indirim kırılımı ────────────────────────────────
-- İndirim kimin cebinden çıkıyor (pazaryeri / satıcı) hakediş defteri için gerekir (T-4).
ALTER TABLE order_items
    ADD COLUMN category_id bigint REFERENCES categories (id),
    ADD COLUMN marketplace_discount_kurus bigint NOT NULL DEFAULT 0 CHECK (marketplace_discount_kurus >= 0),
    ADD COLUMN seller_discount_kurus bigint NOT NULL DEFAULT 0 CHECK (seller_discount_kurus >= 0),
    ADD COLUMN returned_quantity integer NOT NULL DEFAULT 0 CHECK (returned_quantity >= 0);
ALTER TABLE order_items
    ADD COLUMN paid_kurus bigint GENERATED ALWAYS AS (line_total_kurus - marketplace_discount_kurus - seller_discount_kurus) STORED,
    ADD CONSTRAINT order_items_paid_nonnegative CHECK (line_total_kurus >= marketplace_discount_kurus + seller_discount_kurus),
    ADD CONSTRAINT order_items_returned_le_quantity CHECK (returned_quantity <= quantity);

ALTER TABLE sub_orders
    ADD COLUMN marketplace_discount_kurus bigint NOT NULL DEFAULT 0 CHECK (marketplace_discount_kurus >= 0),
    ADD COLUMN seller_discount_kurus bigint NOT NULL DEFAULT 0 CHECK (seller_discount_kurus >= 0),
    ADD CONSTRAINT sub_orders_discount_split CHECK (discount_kurus = marketplace_discount_kurus + seller_discount_kurus);

-- +goose Down
ALTER TABLE sub_orders
    DROP CONSTRAINT sub_orders_discount_split,
    DROP COLUMN seller_discount_kurus,
    DROP COLUMN marketplace_discount_kurus;
ALTER TABLE order_items
    DROP CONSTRAINT order_items_returned_le_quantity,
    DROP CONSTRAINT order_items_paid_nonnegative,
    DROP COLUMN paid_kurus,
    DROP COLUMN returned_quantity,
    DROP COLUMN seller_discount_kurus,
    DROP COLUMN marketplace_discount_kurus,
    DROP COLUMN category_id;
DROP TABLE IF EXISTS order_coupons;
DROP TABLE IF EXISTS cart_coupons;
DROP TABLE IF EXISTS coupon_redemptions;
DROP TABLE IF EXISTS coupons;
