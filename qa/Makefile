# QA test suite for Behemoth — Performance, Persistence safety, and the core
# correctness that underpins them. Everything runs against an ISOLATED
# behemoth-qa stack (compose.qa.yaml) on ports 18080/16379/15432, so it never
# collides with a stack running on the default 8080/6379/5432.
#
# Run from the qa/ directory:  make -C qa <target>
COMPOSE      := docker compose -f compose.qa.yaml
REDIS_ADDR   ?= localhost:16379
POSTGRES_DSN ?= postgres://behemoth:behemoth@localhost:15432/behemoth?sslmode=disable
BASE_URL     ?= http://localhost:18080
export BASE_URL

.PHONY: help up down wait seed integration durability perf-damage perf-mixed perf-overload perf all

help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-16s %s\n",$$1,$$2}'

up: ## build + start the isolated QA stack, wait for readiness
	$(COMPOSE) up --build -d
	@$(MAKE) --no-print-directory wait

down: ## stop the QA stack and wipe its volumes
	$(COMPOSE) down -v

wait: ## block until /readyz is OK
	@for i in $$(seq 1 60); do \
	  curl -sf $(BASE_URL)/readyz >/dev/null 2>&1 && { echo "ready at $(BASE_URL)"; exit 0; }; \
	  sleep 1; done; \
	echo "not ready at $(BASE_URL)/readyz" >&2; exit 1

seed: ## seed QA bosses (boss-load, boss-durab)
	$(COMPOSE) exec -T postgres psql -U behemoth -d behemoth < seed_qa.sql

integration: ## LAYER 1 — Go correctness/integration tests (race-enabled)
	cd .. && REDIS_ADDR="$(REDIS_ADDR)" POSTGRES_DSN="$(POSTGRES_DSN)" \
	  go test -tags=integration -race -count=1 -v ./qa/integration/...

durability: ## LAYER 2 — persistence-safety black-box tests
	./durability/restart_graceful.sh
	./durability/crash_hard.sh
	./durability/no_false_ack.sh
	./durability/redis_wipe.sh

perf-damage: ## LAYER 3 — steady-state p99<100ms proof (needs k6)
	k6 run perf/load_damage.js

perf-mixed: ## LAYER 3 — mixed read+write load (needs k6)
	k6 run perf/load_mixed.js

perf-overload: ## LAYER 3 — overload -> fast 503 backpressure (starves writer, then restores app)
	QA_WRITER_CONCURRENCY=1 QA_WRITER_QUEUE_SIZE=50 QA_BATCH_MAX_SIZE=50 QA_BATCH_MAX_WAIT=5ms $(COMPOSE) up -d app
	@$(MAKE) --no-print-directory wait
	-k6 run perf/load_overload.js
	$(COMPOSE) up -d app
	@$(MAKE) --no-print-directory wait

perf: perf-damage perf-mixed ## run the core perf suite

all: up seed integration durability perf ## full run: stack -> correctness -> durability -> perf
