package payment

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"carsihub/internal/platform/httpx"
	"carsihub/internal/platform/webhook"
)

// Handler sağlayıcının callback ucudur. Kullanıcı oturumu yerine HMAC imzasıyla
// yetkilendirilir (bkz. platform/webhook).
type Handler struct {
	svc    *Service
	secret []byte
	log    *slog.Logger
}

func NewHandler(svc *Service, secret []byte, log *slog.Logger) *Handler {
	return &Handler{svc: svc, secret: secret, log: log}
}

func (h *Handler) Mount(r chi.Router) {
	r.Post("/payments/callback", h.callback)
}

type callbackBody struct {
	PaymentID string `json:"payment_id"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "gövde okunamadı")
		return
	}
	if err := webhook.Verify(h.secret, r.Header, body, time.Now()); err != nil {
		h.log.Warn("imzasız ya da geçersiz ödeme callback'i", "error", err)
		httpx.Error(w, http.StatusUnauthorized, "invalid_signature", err.Error())
		return
	}
	var in callbackBody
	if err := json.Unmarshal(body, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "gövde geçerli JSON değil")
		return
	}
	if !httpx.IsUUID(in.PaymentID) || (in.Status != ProviderSucceeded && in.Status != ProviderFailed) {
		httpx.ValidationError(w, map[string]string{"payment_id/status": "geçerli bir ödeme ve sonuç gerekli"})
		return
	}
	outcome, err := h.svc.ApplyResult(r.Context(), in.PaymentID, in.Status, in.Reason, "callback")
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, err.Error())
	case err != nil:
		// 5xx: sağlayıcı callback'i yeniden dener; ApplyResult idempotent.
		httpx.Internal(w, r, h.log, err)
	default:
		httpx.JSON(w, http.StatusOK, map[string]string{"outcome": string(outcome)})
	}
}
