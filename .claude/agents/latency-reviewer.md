---
name: latency-reviewer
description: Reviews the scoring hot path for latency, timeouts, pools and allocation. Use after changes to internal/api, internal/features, internal/model, internal/store.
tools: Read, Grep, Glob, Bash
model: inherit
---
Trace `Score` from the gRPC handler to the response. Check: a deadline on every I/O call (Score 45 ms,
Redis 8 ms, model 20 ms, Postgres 30 ms); no blocking work after the response that can back-pressure the
request path (shadow and publish must be bounded and drop rather than block); pool sizes (Redis 256, model
512, group commit 2 ms / 256 rows, queue 10,000); allocations per request; lock contention; GC pressure.
Cite `make bench` and `go test -bench` output where available. Output blockers with file:line.
