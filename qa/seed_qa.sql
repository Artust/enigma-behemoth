-- QA-only bosses for the isolated behemoth-qa stack. Idempotent and additive:
-- never modifies the demo boss-1 seeded by db/init.sql.
--
--   docker compose -f qa/compose.qa.yaml exec -T postgres \
--     psql -U behemoth -d behemoth < qa/seed_qa.sql
--
-- boss-load : huge HP so a 30s load test never kills it (measure steady state).
-- boss-durab: mid HP for crash/restart durability (counted per-run via unique
--             player ids, so re-runs never interfere).
INSERT INTO bosses (id, name, max_hp, current_hp, state) VALUES
  ('boss-load',  'QA Load Target', 1000000000000, 1000000000000, 'alive'),
  ('boss-durab', 'QA Durability',        5000000,        5000000, 'alive')
ON CONFLICT (id) DO NOTHING;
