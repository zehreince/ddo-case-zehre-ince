// Package simulator dış bağımlılıkları taklit eder: ödeme sağlayıcı ve kargo
// firması. Gerçek sistemlerdeki hataları üretir (ret, kaybolan callback, çift
// callback, sonuçlanmayan ödeme) ve aynı tohumla her zaman aynı sonucu verir.
//
// Belirli senaryoları deterministik denemek için tutarın kuruş hanesi kullanılır
// (ödeme sağlayıcılarının test kartları gibi):
//
//	xx,01 TL → ödeme reddedilir
//	xx,02 TL → ödeme başarılı ama callback kaybolur (yalnızca sorgulayınca görülür)
//	xx,03 TL → ödeme başarılı, callback iki kez gönderilir
//	xx,04 TL → ödeme hiç sonuçlanmaz (sağlayıcıda askıda kalır)
//	xx,05 TL → ödeme her zaman başarılı (oranlardan bağımsız; doğrulama ve kaos betiği için)
//
// Diğer tutarlarda sonuç tohum + ödeme kimliğinden türetilir ve
// SIM_PAYMENT_*_RATE oranlarına uyar.
//
// Durum bellektedir: servis yeniden başlarsa bilinen ödemeleri unutur. Bu da
// gerçekçi bir hatadır; API'nin mutabakatı bu durumda ödemeyi yeniden ister.
package simulator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"carsihub/internal/platform/webhook"
)

type Config struct {
	Seed             int64
	Secret           []byte
	DeclineRate      float64
	LostCallbackRate float64
	DupCallbackRate  float64
	PaymentLatency   time.Duration
	ShipmentStep     time.Duration
}

type Mode string

const (
	ModeSuccess      Mode = "success"
	ModeDecline      Mode = "decline"
	ModeLostCallback Mode = "lost_callback"
	ModeDupCallback  Mode = "duplicate_callback"
	ModeHang         Mode = "hang"
)

type payment struct {
	ID          string `json:"id"`
	AmountKurus int64  `json:"amount_kurus"`
	Status      string `json:"status"` // pending | succeeded | failed
	Reason      string `json:"reason,omitempty"`
	Refunded    bool   `json:"refunded"`
	Mode        Mode   `json:"mode"`
	callbackURL string
}

type shipment struct {
	ID             string `json:"id"`
	TrackingNumber string `json:"tracking_number"`
	Status         string `json:"status"`
}

type Server struct {
	cfg    Config
	log    *slog.Logger
	client *http.Client
	ctx    context.Context

	mu        sync.Mutex
	payments  map[string]*payment
	shipments map[string]*shipment
	callbacks int
}

// New sunucuyu kurar; ctx bitince zamanlanmış callback'ler gönderilmez.
func New(ctx context.Context, cfg Config, log *slog.Logger) *Server {
	return &Server{
		cfg: cfg, log: log, ctx: ctx, client: &http.Client{Timeout: 5 * time.Second},
		payments: map[string]*payment{}, shipments: map[string]*shipment{},
	}
}

// DecideMode ödemenin akıbetini belirler: önce kuruş hanesi, sonra tohumlu olasılık.
func (s *Server) DecideMode(id string, amountKurus int64) Mode {
	switch amountKurus % 100 {
	case 1:
		return ModeDecline
	case 2:
		return ModeLostCallback
	case 3:
		return ModeDupCallback
	case 4:
		return ModeHang
	case 5:
		return ModeSuccess
	}
	u := s.uniform(id)
	switch {
	case u < s.cfg.DeclineRate:
		return ModeDecline
	case u < s.cfg.DeclineRate+s.cfg.LostCallbackRate:
		return ModeLostCallback
	case u < s.cfg.DeclineRate+s.cfg.LostCallbackRate+s.cfg.DupCallbackRate:
		return ModeDupCallback
	}
	return ModeSuccess
}

// uniform tohum ve kimlikten [0,1) aralığında deterministik bir sayı üretir.
func (s *Server) uniform(id string) float64 {
	h := sha256.New()
	_ = binary.Write(h, binary.BigEndian, s.cfg.Seed)
	h.Write([]byte(id))
	return float64(binary.BigEndian.Uint64(h.Sum(nil))>>11) / (1 << 53)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /stats", s.stats)
	mux.HandleFunc("POST /payments", s.verified(s.createPayment))
	mux.HandleFunc("GET /payments/{id}", s.verified(s.getPayment))
	mux.HandleFunc("POST /payments/{id}/refund", s.verified(s.refund))
	mux.HandleFunc("POST /shipments", s.verified(s.createShipment))
	return mux
}

// verified yalnızca imzalı istekleri kabul eder (API ile aynı paylaşılan sır).
func (s *Server) verified(next func(http.ResponseWriter, *http.Request, []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "gövde okunamadı"})
			return
		}
		if err := webhook.Verify(s.cfg.Secret, r.Header, body, time.Now()); err != nil {
			writeJSON(w, 401, map[string]string{"error": err.Error()})
			return
		}
		next(w, r, body)
	}
}

func (s *Server) createPayment(w http.ResponseWriter, _ *http.Request, body []byte) {
	var in struct {
		ID          string `json:"id"`
		AmountKurus int64  `json:"amount_kurus"`
		CallbackURL string `json:"callback_url"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.ID == "" || in.AmountKurus <= 0 {
		writeJSON(w, 422, map[string]string{"error": "id ve amount_kurus gerekli"})
		return
	}
	s.mu.Lock()
	p, exists := s.payments[in.ID]
	if !exists {
		p = &payment{ID: in.ID, AmountKurus: in.AmountKurus, Status: "pending", Mode: s.DecideMode(in.ID, in.AmountKurus), callbackURL: in.CallbackURL}
		s.payments[in.ID] = p
	}
	view := *p
	s.mu.Unlock()

	// Aynı kimlikle tekrar istek: yeni tahsilat yok, aynı kayıt döner.
	if exists {
		writeJSON(w, 200, view)
		return
	}
	s.log.Info("ödeme alındı", "payment_id", in.ID, "amount_kurus", in.AmountKurus, "mode", view.Mode)
	time.AfterFunc(s.cfg.PaymentLatency, func() { s.settle(in.ID) })
	writeJSON(w, 202, view)
}

func (s *Server) settle(id string) {
	s.mu.Lock()
	p := s.payments[id]
	if p == nil || p.Status != "pending" {
		s.mu.Unlock()
		return
	}
	switch p.Mode {
	case ModeHang:
		s.mu.Unlock()
		return
	case ModeDecline:
		p.Status, p.Reason = "failed", "kart reddedildi"
	default:
		p.Status = "succeeded"
	}
	body, _ := json.Marshal(map[string]string{"payment_id": p.ID, "status": p.Status, "reason": p.Reason})
	mode, url := p.Mode, p.callbackURL
	s.mu.Unlock()

	switch mode {
	case ModeLostCallback:
		s.log.Info("callback bilerek gönderilmedi", "payment_id", id)
	case ModeDupCallback:
		s.deliver(url, body)
		s.deliver(url, body)
	default:
		s.deliver(url, body)
	}
}

func (s *Server) getPayment(w http.ResponseWriter, r *http.Request, _ []byte) {
	s.mu.Lock()
	p, ok := s.payments[r.PathValue("id")]
	var view payment
	if ok {
		view = *p
	}
	s.mu.Unlock()
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "ödeme bulunamadı"})
		return
	}
	writeJSON(w, 200, view)
}

func (s *Server) refund(w http.ResponseWriter, r *http.Request, _ []byte) {
	s.mu.Lock()
	p, ok := s.payments[r.PathValue("id")]
	if ok && p.Status == "succeeded" {
		p.Refunded = true
	}
	var view payment
	if ok {
		view = *p
	}
	s.mu.Unlock()
	switch {
	case !ok:
		writeJSON(w, 404, map[string]string{"error": "ödeme bulunamadı"})
	case view.Status != "succeeded":
		writeJSON(w, 409, map[string]string{"error": "yalnızca başarılı ödeme iade edilir"})
	default:
		s.log.Info("iade yapıldı", "payment_id", view.ID)
		writeJSON(w, 200, view)
	}
}

func (s *Server) createShipment(w http.ResponseWriter, _ *http.Request, body []byte) {
	var in struct {
		ID          string `json:"id"`
		CallbackURL string `json:"callback_url"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.ID == "" {
		writeJSON(w, 422, map[string]string{"error": "id gerekli"})
		return
	}
	s.mu.Lock()
	sh, exists := s.shipments[in.ID]
	if !exists {
		sum := sha256.Sum256(fmt.Appendf(nil, "%d:%s", s.cfg.Seed, in.ID))
		sh = &shipment{ID: in.ID, TrackingNumber: "TRK" + hex.EncodeToString(sum[:5]), Status: "created"}
		s.shipments[in.ID] = sh
	}
	view := *sh
	s.mu.Unlock()
	if !exists {
		go s.ship(sh.ID, in.CallbackURL)
	}
	writeJSON(w, 200, view)
}

// ship kargo olaylarını sırayla yayar. Teslim olayı her zaman iki kez gönderilir:
// gerçek kargo entegrasyonlarında sık görülen tekrarın API'de tekilleştirildiğini gösterir.
func (s *Server) ship(id, url string) {
	for _, st := range []string{"picked_up", "in_transit", "delivered"} {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(s.cfg.ShipmentStep):
		}
		s.mu.Lock()
		sh := s.shipments[id]
		sh.Status = st
		tracking := sh.TrackingNumber
		s.mu.Unlock()
		body, _ := json.Marshal(map[string]any{
			"event_id": tracking + ":" + st, "tracking_number": tracking, "status": st, "occurred_at": time.Now().UTC(),
		})
		s.deliver(url, body)
		if st == "delivered" {
			s.deliver(url, body)
		}
	}
}

// deliver imzalı callback'i gönderir; 5xx ya da ağ hatasında birkaç kez dener.
func (s *Server) deliver(url string, body []byte) {
	if url == "" {
		return
	}
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		webhook.SetHeaders(req.Header, s.cfg.Secret, body, time.Now())
		resp, err := s.client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			s.mu.Lock()
			s.callbacks++
			s.mu.Unlock()
			if resp.StatusCode < 500 {
				return
			}
		}
		s.log.Warn("callback teslim edilemedi", "url", url, "attempt", attempt+1, "error", err)
	}
}

func (s *Server) stats(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byMode := map[Mode]int{}
	for _, p := range s.payments {
		byMode[p.Mode]++
	}
	writeJSON(w, 200, map[string]any{"payments": len(s.payments), "by_mode": byMode, "shipments": len(s.shipments), "callbacks_sent": s.callbacks})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
