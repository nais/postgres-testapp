# postgres-testapp

Test application for Nais Postgres.

- migrations run once at startup using the admin connection in `PG*`
- the running application and SQL console only use the role in `READWRITE_PG*`
- heartbeat activity is stopped by default and can be started or stopped from the GUI or API
- the activity state is stored in Postgres and shared by all application replicas
- one heartbeat is written per second while activity is running and shown live over SSE
- named restore points and destructive wipes make PITR easy to verify
- the SQL console shows query results and PostgreSQL permission errors, for example when the readwrite role attempts `DROP TABLE events`

## API

```sh
curl -X POST https://<host>/api/start
curl -X POST https://<host>/api/stop
curl -X POST https://<host>/api/sql \
  -H 'Content-Type: application/json' \
  -d '{"sql":"SELECT current_user, count(*) FROM events GROUP BY current_user"}'
```

The SQL endpoint accepts one statement of at most 4000 characters, has a 10 second timeout, and returns at most 100 rows. It deliberately executes as the runtime readwrite role so the app can verify the database permission model. PostgreSQL errors are returned as JSON with HTTP 422.
