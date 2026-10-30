-- +goose Up

-- Arama (T-5) için: trigram benzerliği ve aksan/Türkçe karakter normalizasyonu.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS unaccent;

-- Denetim kaydı: kim, ne zaman, neyi, eski/yeni değer. Yalnızca eklenebilir.
CREATE TABLE audit_log (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at  timestamptz NOT NULL DEFAULT now(),
    actor_id     uuid,
    actor_role   text,
    action       text NOT NULL CHECK (action <> ''),
    entity_type  text NOT NULL CHECK (entity_type <> ''),
    entity_id    text NOT NULL,
    old_value    jsonb,
    new_value    jsonb,
    request_id   text
);

CREATE INDEX audit_log_entity_idx ON audit_log (entity_type, entity_id, occurred_at DESC);
CREATE INDEX audit_log_actor_idx  ON audit_log (actor_id, occurred_at DESC);

-- Uygulama hatası ya da elle yapılan bir sorgu geçmişi değiştiremesin diye
-- kural veritabanı seviyesinde zorlanır.
-- +goose StatementBegin
CREATE FUNCTION audit_log_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_log yalnızca eklenebilir (% reddedildi)', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER audit_log_no_update_delete
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();

CREATE TRIGGER audit_log_no_truncate
    BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();

-- +goose Down
DROP TABLE IF EXISTS audit_log;
DROP FUNCTION IF EXISTS audit_log_append_only();
DROP EXTENSION IF EXISTS unaccent;
DROP EXTENSION IF EXISTS pg_trgm;
