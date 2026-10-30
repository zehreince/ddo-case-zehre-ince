package shipping

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"carsihub/internal/auth"
	"carsihub/internal/platform/httpx"
	"carsihub/internal/platform/webhook"
)

type Handler struct {
	svc    *Service
	tokens *auth.Tokens
	secret []byte
	log    *slog.Logger
}

func NewHandler(svc *Service, tokens *auth.Tokens, secret []byte, log *slog.Logger) *Handler {
	return &Handler{svc: svc, tokens: tokens, secret: secret, log: log}
}

// Mount rotaları bağlar:
//   - POST /shipping/callback: kargo firmasının imzalı (HMAC) olay bildirimi
//   - POST /carrier/shipments/{tracking}/events: kargo entegrasyonu rolünün elle olay girişi
//   - GET  /shipments/{tracking}: takip (müşteri yalnızca kendi gönderisini)
func (h *Handler) Mount(r chi.Router) {
	r.Post("/shipping/callback", h.callback)
	r.Group(func(r chi.Router) {
		r.Use(auth.Authenticate(h.tokens))
		r.With(auth.RequireRole(auth.RoleCarrier)).Post("/carrier/shipments/{tracking}/events", h.carrierEvent)
		r.With(auth.RequireRole(auth.RoleCustomer, auth.RoleOperations, auth.RoleCarrier, auth.RoleAdmin)).
			Get("/shipments/{tracking}", h.track)
	})
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "gövde okunamadı")
		return
	}
	if err := webhook.Verify(h.secret, r.Header, body, time.Now()); err != nil {
		httpx.Error(w, http.StatusUnauthorized, "invalid_signature", err.Error())
		return
	}
	var e Event
	if err := json.Unmarshal(body, &e); err != nil || e.EventID == "" || e.TrackingNumber == "" {
		httpx.ValidationError(w, map[string]string{"event": "event_id, tracking_number ve status gerekli"})
		return
	}
	h.apply(w, r, e, "carrier_webhook")
}

type manualEvent struct {
	Status     string    `json:"status"`
	OccurredAt time.Time `json:"occurred_at"`
}

// carrierEvent kargo entegrasyonu kullanıcısının olay girmesidir. Olay kimliği
// Idempotency-Key başlığıdır; aynı anahtarla tekrar gönderim yeni olay sayılmaz.
func (h *Handler) carrierEvent(w http.ResponseWriter, r *http.Request) {
	var in manualEvent
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "gövde geçerli JSON değil")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		httpx.ValidationError(w, map[string]string{"Idempotency-Key": "kargo olayları için zorunlu (olay kimliği)"})
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	h.apply(w, r, Event{
		EventID: "manual:" + p.UserID + ":" + key, TrackingNumber: chi.URLParam(r, "tracking"),
		Status: in.Status, OccurredAt: in.OccurredAt,
	}, string(p.Role))
}

func (h *Handler) apply(w http.ResponseWriter, r *http.Request, e Event, actor string) {
	duplicate, err := h.svc.ApplyEvent(r.Context(), e, actor)
	switch {
	case errors.Is(err, ErrInvalidStatus):
		httpx.ValidationError(w, map[string]string{"status": "picked_up, in_transit ya da delivered olmalı"})
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, err.Error())
	case err != nil:
		httpx.Internal(w, r, h.log, err)
	default:
		httpx.JSON(w, http.StatusOK, map[string]bool{"duplicate": duplicate})
	}
}

func (h *Handler) track(w http.ResponseWriter, r *http.Request) {
	sh, err := h.svc.Track(r.Context(), chi.URLParam(r, "tracking"))
	p, _ := auth.PrincipalFrom(r.Context())
	if err == nil && p.Role == auth.RoleCustomer && sh.CustomerID != p.UserID {
		err = ErrNotFound
	}
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, err.Error())
	case err != nil:
		httpx.Internal(w, r, h.log, err)
	default:
		httpx.JSON(w, http.StatusOK, sh)
	}
}
