-- +goose Up

-- Zaman aralığı çakışmasını exclusion constraint ile engellemek için.
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- ── Komisyon oranları (T-4) ──────────────────────────────────────────────
-- category_id NULL: varsayılan oran. Aynı kategoride geçerlilik aralıkları
-- çakışamaz; yeni oran eklenince öncekinin bitişi veritabanı düzeyinde tutarlı kalır.
CREATE TABLE commission_rates (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    category_id bigint REFERENCES categories (id),
    rate_bps    integer NOT NULL CHECK (rate_bps BETWEEN 0 AND 10000),
    valid_from  timestamptz NOT NULL,
    valid_to    timestamptz,
    created_by  uuid REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    CHECK (valid_to IS NULL OR valid_to > valid_from),
    CONSTRAINT commission_rates_no_overlap EXCLUDE USING gist (
        (COALESCE(category_id, 0)) WITH =,
        tstzrange(valid_from, valid_to) WITH &&
    )
);

-- ── Çift taraflı defter ──────────────────────────────────────────────────
CREATE TABLE ledger_accounts (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code       text NOT NULL UNIQUE,
    name       text NOT NULL,
    type       text NOT NULL CHECK (type IN ('asset', 'liability', 'revenue', 'expense')),
    seller_id  uuid REFERENCES sellers (id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ledger_transactions (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind            text NOT NULL,
    reference_type  text NOT NULL,
    reference_id    text NOT NULL,
    -- Aynı iş olayı iki kez deftere yazılamaz (olay tekrar teslim edilse de).
    idempotency_key text NOT NULL UNIQUE,
    description     text NOT NULL DEFAULT '',
    occurred_at     timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ledger_transactions_ref_idx ON ledger_transactions (reference_type, reference_id);

CREATE TABLE ledger_entries (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id bigint NOT NULL REFERENCES ledger_transactions (id),
    account_id     bigint NOT NULL REFERENCES ledger_accounts (id),
    direction      text NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount_kurus   bigint NOT NULL CHECK (amount_kurus > 0)
);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id, transaction_id);
CREATE INDEX ledger_entries_tx_idx ON ledger_entries (transaction_id);

-- Borç = alacak kuralı commit anında denetlenir (ertelenmiş kısıt tetikleyicisi):
-- bir işlemin kayıtları aynı transaction'da birer birer eklenebilir, ama
-- dengesiz bir işlem asla kalıcı olamaz.
-- +goose StatementBegin
CREATE FUNCTION ledger_check_balance() RETURNS trigger AS $$
DECLARE
    diff bigint;
BEGIN
    SELECT COALESCE(sum(CASE direction WHEN 'debit' THEN amount_kurus ELSE -amount_kurus END), 0)
      INTO diff FROM ledger_entries WHERE transaction_id = NEW.transaction_id;
    IF diff <> 0 THEN
        RAISE EXCEPTION 'defter işlemi % dengesiz: borç - alacak = %', NEW.transaction_id, diff
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_balance();

-- Defter yalnızca eklenebilir; düzeltme ters kayıtla yapılır.
-- +goose StatementBegin
CREATE FUNCTION ledger_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'defter yalnızca eklenebilir (% reddedildi); düzeltme için ters kayıt atın', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER ledger_entries_append_only BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER ledger_transactions_append_only BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER ledger_entries_no_truncate BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_append_only();

-- ── Hakediş ──────────────────────────────────────────────────────────────
-- Dönem kapanışı: [starts_at, ends_at) aralığında vadesi gelen hakedişler ödenir.
CREATE TABLE settlement_periods (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    starts_at  timestamptz NOT NULL,
    ends_at    timestamptz NOT NULL,
    closed_at  timestamptz NOT NULL DEFAULT now(),
    closed_by  uuid REFERENCES users (id),
    CHECK (ends_at > starts_at),
    CONSTRAINT settlement_periods_no_overlap EXCLUDE USING gist (tstzrange(starts_at, ends_at) WITH &&)
);

-- Teslim edilen her alt sipariş bir hakediş kalemidir.
-- net = brüt - komisyon - kargo
CREATE TABLE settlement_items (
    id                   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sub_order_id         uuid NOT NULL UNIQUE REFERENCES sub_orders (id),
    seller_id            uuid NOT NULL REFERENCES sellers (id),
    gross_kurus          bigint NOT NULL CHECK (gross_kurus >= 0),
    commission_kurus     bigint NOT NULL CHECK (commission_kurus >= 0),
    shipping_fee_kurus   bigint NOT NULL CHECK (shipping_fee_kurus >= 0),
    net_kurus            bigint NOT NULL,
    delivered_at         timestamptz NOT NULL,
    due_at               timestamptz NOT NULL,
    period_id            bigint REFERENCES settlement_periods (id),
    created_at           timestamptz NOT NULL DEFAULT now(),
    CHECK (net_kurus = gross_kurus - commission_kurus - shipping_fee_kurus),
    CHECK (due_at >= delivered_at)
);
CREATE INDEX settlement_items_due_idx ON settlement_items (seller_id, due_at) WHERE period_id IS NULL;

-- İade/iptal sonrası düzeltmeler: satıcının hakedişinden düşülecek (ya da eklenecek) tutar.
-- Henüz ödenmemiş kalemin dönemi kapanınca, ödenmişse bir sonraki dönemde mahsup edilir.
CREATE TABLE settlement_adjustments (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    seller_id     uuid NOT NULL REFERENCES sellers (id),
    sub_order_id  uuid NOT NULL REFERENCES sub_orders (id),
    return_id     uuid NOT NULL,
    amount_kurus  bigint NOT NULL,          -- negatif: satıcıdan kesinti
    reason        text NOT NULL,
    period_id     bigint REFERENCES settlement_periods (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (return_id, sub_order_id)
);
CREATE INDEX settlement_adjustments_open_idx ON settlement_adjustments (seller_id) WHERE period_id IS NULL;

CREATE TABLE payouts (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    period_id    bigint NOT NULL REFERENCES settlement_periods (id),
    seller_id    uuid NOT NULL REFERENCES sellers (id),
    items_kurus  bigint NOT NULL,           -- vadesi gelen kalemlerin net toplamı
    adjust_kurus bigint NOT NULL,           -- düzeltmeler (iade kesintisi)
    carried_in   bigint NOT NULL DEFAULT 0, -- önceki dönemden devreden borç (negatif)
    amount_kurus bigint NOT NULL CHECK (amount_kurus >= 0),
    carry_out    bigint NOT NULL DEFAULT 0 CHECK (carry_out <= 0),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (period_id, seller_id),
    CHECK (amount_kurus + carry_out = items_kurus + adjust_kurus + carried_in)
);

-- ── İade (F-7) ───────────────────────────────────────────────────────────
CREATE TABLE returns (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id       uuid NOT NULL REFERENCES orders (id),
    customer_id    uuid NOT NULL REFERENCES users (id),
    refund_kurus   bigint NOT NULL CHECK (refund_kurus >= 0),
    clawback_kurus bigint NOT NULL CHECK (clawback_kurus >= 0),
    lost_coupons   text[] NOT NULL DEFAULT '{}',
    status         text NOT NULL CHECK (status IN ('refund_requested', 'refunded')),
    reason         text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    refunded_at    timestamptz
);
CREATE INDEX returns_order_idx ON returns (order_id);

CREATE TABLE return_items (
    return_id     uuid NOT NULL REFERENCES returns (id) ON DELETE CASCADE,
    order_item_id uuid NOT NULL REFERENCES order_items (id),
    quantity      integer NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (return_id, order_item_id)
);

-- +goose Down
DROP TABLE IF EXISTS return_items;
DROP TABLE IF EXISTS returns;
DROP TABLE IF EXISTS payouts;
DROP TABLE IF EXISTS settlement_adjustments;
DROP TABLE IF EXISTS settlement_items;
DROP TABLE IF EXISTS settlement_periods;
DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS ledger_transactions;
DROP TABLE IF EXISTS ledger_accounts;
DROP FUNCTION IF EXISTS ledger_append_only();
DROP FUNCTION IF EXISTS ledger_check_balance();
DROP TABLE IF EXISTS commission_rates;
DROP EXTENSION IF EXISTS btree_gist;
