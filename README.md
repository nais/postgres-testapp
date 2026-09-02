# postgres-testapp

Test application for Nais Postgres

- migrations run once at startup using `PG*`
- the running application only connects using `READWRITE_PG*`
- one heartbeat is written per second and shown live over SSE
- named restore points and destructive wipes make PITR easy to verify
