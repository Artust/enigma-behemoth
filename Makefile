.PHONY: up down build test vet load seed recovery logs smoke

up:            ## build + start the full stack
	docker compose up --build -d

down:          ## stop and remove volumes
	docker compose down -v

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

seed:
	./scripts/seed.sh

load:          ## load test with k6 (if installed)
	k6 run scripts/load_test.js

loadgo:        ## load test with the built-in Go probe (no external deps)
	go run ./scripts/loadprobe

recovery:
	./scripts/recovery_test.sh

logs:
	docker compose logs -f app

smoke:         ## quick end-to-end curl against a running stack
	curl -s localhost:8080/healthz; echo
	curl -s -X POST localhost:8080/damage -H 'Content-Type: application/json' \
	  -d '{"player_id":"p1","boss_id":"boss-1","damage_amount":500}'; echo
	curl -s localhost:8080/boss/boss-1; echo
