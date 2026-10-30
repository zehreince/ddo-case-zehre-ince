// Package ops pazaryeri operasyon ekibinin saga'yı izleme ve müdahale uçlarıdır:
// outbox'ta bekleyen, yeniden denenen ve bırakılmış (dead) olaylar.
package ops

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/auth"
	"carsihub/internal/platform/audit"
	"carsihub/internal/platform/httpx"
)

type Handler struct {
	pool   *pgxpool.Pool
	tokens *auth.Tokens
	log    *slog.Logger
}

func NewHandler(pool *pgxpool.Pool, tokens *auth.Tokens, log *slog.Logger) *Handler {
	return &Handler{pool: pool, tokens: tokens, log: log}
}

func (h *Handler) Mount(r chi.Router) {
	r.Route("/ops", func(r chi.Router) {
		r.Use(auth.Authenticate(h.tokens), auth.RequireRole(auth.RoleOperations, auth.RoleAdmin))
		r.Get("/outbox", h.outbox)
		r.Post("/outbox/{id}/retry", h.retry)
	})
}

type TypeStats struct {
	EventType string `json:"event_type"`
	Pending   int    `json:"pending"`
	Retrying  int    `json:"retrying"`
	Dead      int    `json:"dead"`
}

type Problem struct {
	ID        int64     `json:"id"`
	EventType string    `json:"event_type"`
	Aggregate string    `json:"aggregate"`
	Attempts  int       `json:"attempts"`
	Dead      bool      `json:"dead"`
	LastError *string   `json:"last_error"`
	NextTry   time.Time `json:"next_attempt_at"`
}

type Summary struct {
	Pending          int         `json:"pending"`
	Retrying         int         `json:"retrying"`
	Dead             int         `json:"dead"`
	OldestPendingSec *float64    `json:"oldest_pending_seconds"`
	ByType           []TypeStats `json:"by_type"`
	Problems         []Problem   `json:"problems"`
}

// outbox yayımlanmamış olayların özetini ve sorunlu olanların son 20'sini döner.
// outbox_pending_idx yalnızca yayımlanmamış satırları kapsar; tablo büyüse de sorgu küçük kalır.
func (h *Handler) outbox(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := Summary{ByType: []TypeStats{}, Problems: []Problem{}}
	rows, err := h.pool.Query(ctx, `
		SELECT event_type,
		       count(*) FILTER (WHERE NOT dead),
		       count(*) FILTER (WHERE NOT dead AND attempts > 0),
		       count(*) FILTER (WHERE dead)
		FROM outbox_events WHERE published_at IS NULL
		GROUP BY event_type ORDER BY event_type`)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	if s.ByType, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (TypeStats, error) {
		var t TypeStats
		err := row.Scan(&t.EventType, &t.Pending, &t.Retrying, &t.Dead)
		return t, err
	}); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	for _, t := range s.ByType {
		s.Pending += t.Pending
		s.Retrying += t.Retrying
		s.Dead += t.Dead
	}
	if err := h.pool.QueryRow(ctx, `
		SELECT extract(epoch FROM now() - min(created_at))::float8
		FROM outbox_events WHERE published_at IS NULL AND NOT dead`).Scan(&s.OldestPendingSec); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	rows, err = h.pool.Query(ctx, `
		SELECT id, event_type, aggregate_type || ':' || aggregate_id, attempts, dead, last_error, next_attempt_at
		FROM outbox_events WHERE published_at IS NULL AND attempts > 0
		ORDER BY id DESC LIMIT 20`)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	if s.Problems, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Problem, error) {
		var p Problem
		err := row.Scan(&p.ID, &p.EventType, &p.Aggregate, &p.Attempts, &p.Dead, &p.LastError, &p.NextTry)
		return p, err
	}); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, s)
}

// retry bırakılmış (dead) bir olayı yeniden kuyruğa alır: bağımlılık düzeltildikten
// sonra operasyonun elle müdahalesi.
func (h *Handler) retry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, "olay bulunamadı")
		return
	}
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	var attempts int
	err = tx.QueryRow(r.Context(), `
		SELECT attempts FROM outbox_events WHERE id = $1 AND dead AND published_at IS NULL FOR UPDATE`, id).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusConflict, "invalid_state", "yalnızca bırakılmış (dead) olaylar yeniden kuyruğa alınabilir")
		return
	}
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		UPDATE outbox_events SET dead = false, attempts = 0, next_attempt_at = now(), lease_until = NULL WHERE id = $1`, id); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if err := audit.Record(r.Context(), tx, audit.Entry{
		ActorID: p.UserID, ActorRole: string(p.Role), Action: "outbox.retry", EntityType: "outbox_event",
		EntityID: strconv.FormatInt(id, 10), Old: map[string]any{"dead": true, "attempts": attempts},
		RequestID: httpx.RequestIDFrom(r.Context()),
	}); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
