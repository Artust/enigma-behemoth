Project: "Behemoth" — World Boss Event Service

Scenario: You are building a backend service for a massive multiplayer RPG. A "World Boss" is a global entity that thousands of players attack simultaneously. Your goal is to build a high-performance microservice that tracks player contributions and manages reward eligibility.

Functional Requirements
POST /damage: Accept damage requests { player_id, boss_id, damage_amount } .
GET /boss/{id}: Return current boss HP and a Top 10 Leaderboard of contributors.
POST /rewards/claim: Allow a player to claim a reward after the boss is defeated. Reward tier depends on the % of damage contributed.

Technical Constraints
Performance: Handle a burst of 1,000+ QPS with p99 latency under 100ms for the damage endpoint.
Persistence safety: We value data durability. If the service restarts, the Boss HP and contribution history must not be lost.
Language: Build this from scratch using a statically typed language (Java, or C#).
Deliverables: A GitHub repository (or zip) containing the code, a docker-compose.yaml for local execution, and an ARCHITECT.md.
Timebox: 4–6 hours. Prioritize architecture and "happy path" stability over 100% feature completeness.

In your documentation, please address (include but not limited to):
1. System Overview: A high-level description of the chosen architecture.
2. Data Strategy:
Choice of Primary Database and why.
Caching strategy if any.
3. Concurrency & Safety:
How the system handles simultaneous writes to the Boss HP.
How the "Claim Reward" endpoint ensures exactly-once processing.
4. Assumptions & Trade-offs:
Any shortcuts taken to meet the 4–6 hour limit and what you would do differently with more time.
Any assumptions regarding edge cases you identified and your solutions.
