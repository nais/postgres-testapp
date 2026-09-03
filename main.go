package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

//go:embed web/index.html
var indexHTML []byte

type event struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Instance  string    `json:"instance"`
	Kind      string    `json:"kind"`
	Message   string    `json:"message"`
}

type server struct {
	db       *pgxpool.Pool
	instance string
	log      *slog.Logger
}

func main() {
	if err := run(); err != nil {
		slog.Error("application stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	instance, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("get hostname: %w", err)
	}

	if err := migrate(ctx); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	log.Info("migrations complete")

	runtimeURL, err := postgresURL("READWRITE_")
	if err != nil {
		return err
	}
	db, err := pgxpool.New(ctx, runtimeURL)
	if err != nil {
		return fmt.Errorf("configure runtime database: %w", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		return fmt.Errorf("connect as readwrite user: %w", err)
	}

	s := &server{db: db, instance: instance, log: log}
	go s.writeHeartbeats(ctx, time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("POST /api/markers", s.marker)
	mux.HandleFunc("POST /api/start", s.start)
	mux.HandleFunc("POST /api/stop", s.stop)
	mux.HandleFunc("POST /api/sql", s.sql)
	mux.HandleFunc("POST /api/wipe", s.wipe)

	httpServer := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "address", httpServer.Addr, "database_user", databaseUser(db))
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down HTTP server: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	}
}

func migrate(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, "")
	if err != nil {
		return fmt.Errorf("connect with migration user: %w", err)
	}
	defer conn.Close(context.Background())

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(context.Background())

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('postgres-testapp-migrations'))"); err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version integer PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
	)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}

	migrations := []struct {
		version int
		path    string
	}{
		{version: 1, path: "migrations/001_init.sql"},
		{version: 2, path: "migrations/002_activity_control.sql"},
	}
	for _, migration := range migrations {
		var applied bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", migration.version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %d: %w", migration.version, err)
		}
		if applied {
			continue
		}
		sql, err := migrationFiles.ReadFile(migration.path)
		if err != nil {
			return fmt.Errorf("read migration %d: %w", migration.version, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply migration %d: %w", migration.version, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", migration.version); err != nil {
			return fmt.Errorf("record migration %d: %w", migration.version, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func postgresURL(prefix string) (string, error) {
	required := []string{"PGHOST", "PGDATABASE", "PGUSER", "PGSSLMODE", "PGSSLCERT", "PGSSLKEY", "PGSSLROOTCERT"}
	values := make(map[string]string, len(required)+1)
	for _, name := range required {
		value := os.Getenv(prefix + name)
		if value == "" {
			return "", fmt.Errorf("required environment variable %s is empty", prefix+name)
		}
		values[name] = value
	}
	values["PGPORT"] = os.Getenv(prefix + "PGPORT")
	if values["PGPORT"] == "" {
		values["PGPORT"] = "5432"
	}

	query := url.Values{
		"sslmode":     {values["PGSSLMODE"]},
		"sslcert":     {values["PGSSLCERT"]},
		"sslkey":      {values["PGSSLKEY"]},
		"sslrootcert": {values["PGSSLROOTCERT"]},
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.User(values["PGUSER"]),
		Host:     net.JoinHostPort(values["PGHOST"], values["PGPORT"]),
		Path:     values["PGDATABASE"],
		RawQuery: query.Encode(),
	}
	return u.String(), nil
}

func databaseUser(db *pgxpool.Pool) string {
	return db.Config().ConnConfig.User
}

func (s *server) writeHeartbeats(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		var running bool
		if err := s.db.QueryRow(ctx, "SELECT running FROM app_control WHERE singleton").Scan(&running); err != nil {
			if !errors.Is(err, context.Canceled) {
				s.log.Error("read activity state", "err", err)
			}
			continue
		}
		if !running {
			continue
		}
		if _, err := s.db.Exec(ctx,
			"INSERT INTO events (instance, kind, message) VALUES ($1, 'heartbeat', 'alive')",
			s.instance,
		); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Error("write heartbeat", "err", err)
		}
	}
}

func (s *server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	var state struct {
		Database string    `json:"database"`
		User     string    `json:"user"`
		Now      time.Time `json:"database_time"`
		Count    int64     `json:"event_count"`
		FirstID  *int64    `json:"first_id"`
		LastID   *int64    `json:"last_id"`
		Running  bool      `json:"running"`
	}
	if err := s.db.QueryRow(r.Context(), `
		SELECT current_database(), current_user, clock_timestamp(), count(*), min(id), max(id),
		       (SELECT running FROM app_control WHERE singleton)
		FROM events
	`).Scan(&state.Database, &state.User, &state.Now, &state.Count, &state.FirstID, &state.LastID, &state.Running); err != nil {
		http.Error(w, "query database state", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	after := int64(0)
	if value := r.Header.Get("Last-Event-ID"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			http.Error(w, "invalid Last-Event-ID", http.StatusBadRequest)
			return
		}
		after = parsed
	}
	if after == 0 {
		if err := s.db.QueryRow(r.Context(), "SELECT greatest(coalesce(max(id), 0) - 100, 0) FROM events").Scan(&after); err != nil {
			http.Error(w, "find event cursor", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		events, err := s.eventsAfter(r.Context(), after)
		if err != nil {
			return
		}
		for _, event := range events {
			data, err := json.Marshal(event)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.ID, data); err != nil {
				return
			}
			after = event.ID
		}
		flusher.Flush()

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *server) eventsAfter(ctx context.Context, after int64) ([]event, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, created_at, instance, kind, message
		FROM events
		WHERE id > $1
		ORDER BY id
		LIMIT 1000
	`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]event, 0, 16)
	for rows.Next() {
		var event event
		if err := rows.Scan(&event.ID, &event.CreatedAt, &event.Instance, &event.Kind, &event.Message); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *server) marker(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Message string `json:"message"`
	}
	if err := decodeJSON(r.Body, &input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	input.Message = strings.TrimSpace(input.Message)
	if input.Message == "" || len(input.Message) > 200 {
		http.Error(w, "message must contain 1-200 characters", http.StatusBadRequest)
		return
	}

	var marker event
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO events (instance, kind, message)
		VALUES ($1, 'marker', $2)
		RETURNING id, created_at, instance, kind, message
	`, s.instance, input.Message).Scan(&marker.ID, &marker.CreatedAt, &marker.Instance, &marker.Kind, &marker.Message); err != nil {
		http.Error(w, "write marker", http.StatusInternalServerError)
		return
	}

	var restoreTarget time.Time
	if err := s.db.QueryRow(r.Context(), "SELECT clock_timestamp()").Scan(&restoreTarget); err != nil {
		http.Error(w, "read restore target", http.StatusInternalServerError)
		return
	}
	s.log.Info("PITR marker created", "event_id", marker.ID, "restore_target", restoreTarget, "message", marker.Message)
	writeJSON(w, http.StatusCreated, struct {
		Marker        event     `json:"marker"`
		RestoreTarget time.Time `json:"restore_target"`
	}{Marker: marker, RestoreTarget: restoreTarget})
}

func (s *server) start(w http.ResponseWriter, r *http.Request) {
	s.setRunning(w, r, true)
}

func (s *server) stop(w http.ResponseWriter, r *http.Request) {
	s.setRunning(w, r, false)
}

func (s *server) setRunning(w http.ResponseWriter, r *http.Request, running bool) {
	command, err := s.db.Exec(r.Context(), "UPDATE app_control SET running = $1 WHERE singleton", running)
	if err != nil {
		http.Error(w, "update activity state", http.StatusInternalServerError)
		return
	}
	if command.RowsAffected() != 1 {
		http.Error(w, "activity state is missing", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Running bool `json:"running"`
	}{Running: running})
}

func (s *server) sql(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SQL string `json:"sql"`
	}
	if err := decodeJSON(r.Body, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, struct {
			Error string `json:"error"`
		}{Error: err.Error()})
		return
	}
	input.SQL = strings.TrimSpace(input.SQL)
	if input.SQL == "" || len(input.SQL) > 4000 {
		writeJSON(w, http.StatusBadRequest, struct {
			Error string `json:"error"`
		}{Error: "sql must contain 1-4000 characters"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, err := s.db.Query(ctx, input.SQL)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, struct {
			Error string `json:"error"`
		}{Error: err.Error()})
		return
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	columns := make([]string, len(fields))
	for i, field := range fields {
		columns[i] = field.Name
	}
	resultRows := make([][]any, 0, 16)
	truncated := false
	for rows.Next() {
		if len(resultRows) == 100 {
			truncated = true
			break
		}
		values, err := rows.Values()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, struct {
				Error string `json:"error"`
			}{Error: "read query result"})
			return
		}
		result := make([]any, len(values))
		for i, value := range values {
			if value != nil {
				result[i] = fmt.Sprint(value)
			}
		}
		resultRows = append(resultRows, result)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, struct {
			Error string `json:"error"`
		}{Error: err.Error()})
		return
	}
	rows.Close()
	writeJSON(w, http.StatusOK, struct {
		Columns   []string `json:"columns"`
		Rows      [][]any  `json:"rows"`
		Command   string   `json:"command"`
		Truncated bool     `json:"truncated"`
	}{
		Columns:   columns,
		Rows:      resultRows,
		Command:   rows.CommandTag().String(),
		Truncated: truncated,
	})
}

func (s *server) wipe(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Confirm string `json:"confirm"`
	}
	if err := decodeJSON(r.Body, &input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if input.Confirm != "wipe" {
		http.Error(w, `confirm must be "wipe"`, http.StatusBadRequest)
		return
	}

	command, err := s.db.Exec(r.Context(), "DELETE FROM events")
	if err != nil {
		http.Error(w, "wipe events", http.StatusInternalServerError)
		return
	}
	var wipedAt time.Time
	if err := s.db.QueryRow(r.Context(), "SELECT clock_timestamp()").Scan(&wipedAt); err != nil {
		http.Error(w, "read database time", http.StatusInternalServerError)
		return
	}
	s.log.Warn("events wiped", "rows", command.RowsAffected(), "database_time", wipedAt)
	writeJSON(w, http.StatusOK, struct {
		Rows    int64     `json:"rows"`
		WipedAt time.Time `json:"wiped_at"`
	}{Rows: command.RowsAffected(), WipedAt: wipedAt})
}

func decodeJSON(body io.ReadCloser, target any) error {
	defer body.Close()
	decoder := json.NewDecoder(io.LimitReader(body, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("write JSON response", "err", err)
	}
}
