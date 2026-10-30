#!/usr/bin/env bash
# Kaos senaryosu (T-2): bir bağımlılığı durdurup sipariş akışının toparlandığını gösterir.
#
#   1) Ödeme sağlayıcı çöker: siparişler alınır, stok tutulur; "ödeme iste" olayları
#      outbox'ta birikir ve üstel beklemeyle yeniden denenir. Sağlayıcı geri gelince
#      hepsi kendiliğinden ödenir. Hiçbir olay kaybolmaz, hiçbir ödeme iki kez çekilmez.
#   2) Worker çöker: olaylar veritabanında (outbox) durduğu için kaybolmaz; worker
#      yeniden başlayınca kaldığı yerden devam eder.
#
# Kullanım: make chaos   (sistem ayakta olmalı: make up)
set -euo pipefail
cd "$(dirname "$0")/.."

ORDERS="${CHAOS_ORDERS:-5}"
OUTAGE="${CHAOS_OUTAGE_SECONDS:-20}"
RECOVERY_TIMEOUT="${CHAOS_RECOVERY_TIMEOUT:-90}"

dc() { docker compose "$@"; }
step() { docker compose --profile verify run --rm -T verify chaos "$@"; }
say() { printf '\n\033[1m▶ %s\033[0m\n' "$*"; }

say "Hazırlık: sistem ayakta mı?"
dc up -d --wait api worker simulator >/dev/null
dc --profile verify build verify >/dev/null

# ── Senaryo 1: ödeme sağlayıcı yok ────────────────────────────────────────
say "Senaryo 1 — ödeme sağlayıcı (simulator) durduruluyor"
dc stop simulator >/dev/null

say "${ORDERS} sipariş veriliyor (sağlayıcı yokken)"
IDS=$(step place "$ORDERS")
echo "  siparişler: $IDS"

say "${OUTAGE} sn kesinti; worker ödeme isteğini tekrar tekrar deniyor"
sleep "$OUTAGE"
step status $IDS

say "Ödeme sağlayıcı geri getiriliyor"
dc start simulator >/dev/null
dc up -d --wait simulator >/dev/null

say "Toparlanma bekleniyor (en fazla ${RECOVERY_TIMEOUT} sn)"
step await "$RECOVERY_TIMEOUT" $IDS

# ── Senaryo 2: worker çöküyor ─────────────────────────────────────────────
say "Senaryo 2 — worker öldürülüyor (SIGKILL, düzgün kapanış yok)"
dc kill worker >/dev/null

say "${ORDERS} sipariş veriliyor (worker yokken)"
IDS=$(step place "$ORDERS")
echo "  siparişler: $IDS"
step status $IDS

say "Worker yeniden başlatılıyor"
dc up -d --wait worker >/dev/null

say "Toparlanma bekleniyor (en fazla ${RECOVERY_TIMEOUT} sn)"
step await "$RECOVERY_TIMEOUT" $IDS

say "Kaos senaryosu başarılı: iki kesintiden sonra da tüm siparişler ödendi, bırakılmış olay yok."
