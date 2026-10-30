.PHONY: setup up down logs test test-backend test-frontend lint verify reset-demo load

COMPOSE := docker compose

setup:  ## Git hook'larını açar (commit'te gofmt/vet, push'ta testler)
	git config core.hooksPath .githooks
	cd frontend && npm ci

up:  ## Sistemi derleyip ayağa kaldırır, sağlıklı olana kadar bekler
	$(COMPOSE) up -d --build --wait

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f api

test: test-backend test-frontend

test-backend:
	cd backend && go test -race -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

test-frontend:
	cd frontend && npm test

GOLANGCI := golangci/golangci-lint:v2.5.0

lint:  ## CI ile aynı golangci-lint sürümünü Docker içinde koşturur
	docker run --rm -v "$(CURDIR)/backend:/app" -w /app $(GOLANGCI) golangci-lint run ./...
	cd frontend && npm run typecheck

verify: up  ## Tüm özellik ve görevleri uçtan uca dener, PASS/FAIL tablosu basar
	$(COMPOSE) --profile verify run --rm --build verify

reset-demo:  ## Veritabanını silip ilk seed haline döndürür
	$(COMPOSE) down -v
	$(COMPOSE) up -d --build --wait

load:
	@echo "yük testleri henüz eklenmedi"
