# postgres-testapp

Small live probe for the new Nais Postgres offering.

- migrations run once at startup using `PG*`
- the running application only connects using `READWRITE_PG*`
- one heartbeat is written per second and shown live over SSE
- named restore points and destructive wipes make PITR easy to verify

## PITR

1. Open the app and create a restore point.
2. Save the returned database timestamp.
3. Wipe the events.
4. Restore Postgres to the saved timestamp.
5. Verify that the marker and earlier events return.

The app expects certificate-based libpq environment variables, including
`PGSSLCERT`, `PGSSLKEY`, and `PGSSLROOTCERT`, for both prefixes.
