-- +goose Up

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text NOT NULL CHECK (email = lower(email) AND email LIKE '%_@_%'),
    password_hash text NOT NULL,
    full_name     text NOT NULL CHECK (length(full_name) BETWEEN 1 AND 200),
    role          text NOT NULL CHECK (role IN ('customer', 'seller', 'operations', 'finance', 'carrier', 'admin')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    disabled_at   timestamptz
);

CREATE UNIQUE INDEX users_email_key ON users (email);
-- Yönetici kullanıcı listesi keyset sayfalaması: ORDER BY created_at DESC, id DESC
CREATE INDEX users_keyset_idx ON users (created_at DESC, id DESC);

-- Refresh token'lar düz metin saklanmaz; yalnızca SHA-256 özeti tutulur.
-- Aynı oturumdan türeyen token'lar bir "aile" oluşturur. İptal edilmiş bir
-- token tekrar kullanılırsa (çalınma belirtisi) tüm aile iptal edilir.
CREATE TABLE refresh_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id   uuid NOT NULL,
    token_hash  bytea NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz,
    replaced_by uuid REFERENCES refresh_tokens (id),
    CHECK (expires_at > created_at)
);

CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id) WHERE revoked_at IS NULL;
CREATE INDEX refresh_tokens_user_idx   ON refresh_tokens (user_id);

-- +goose Down
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS users;
