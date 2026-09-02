package main

import (
	"net/url"
	"testing"
)

func TestPostgresURL(t *testing.T) {
	env := map[string]string{
		"READWRITE_PGHOST":        "database.example",
		"READWRITE_PGPORT":        "6432",
		"READWRITE_PGDATABASE":    "app",
		"READWRITE_PGUSER":        "test app",
		"READWRITE_PGSSLMODE":     "verify-full",
		"READWRITE_PGSSLCERT":     "/certs/client.crt",
		"READWRITE_PGSSLKEY":      "/certs/client.key",
		"READWRITE_PGSSLROOTCERT": "/certs/ca.crt",
	}
	for name, value := range env {
		t.Setenv(name, value)
	}

	got, err := postgresURL("READWRITE_")
	if err != nil {
		t.Fatalf("postgresURL: %v", err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	if parsed.User.Username() != "test app" {
		t.Errorf("username = %q, want %q", parsed.User.Username(), "test app")
	}
	if parsed.Host != "database.example:6432" {
		t.Errorf("host = %q, want %q", parsed.Host, "database.example:6432")
	}
	if parsed.Query().Get("sslmode") != "verify-full" {
		t.Errorf("sslmode = %q, want verify-full", parsed.Query().Get("sslmode"))
	}
}

func TestPostgresURLRequiresConnectionEnvironment(t *testing.T) {
	for _, name := range []string{"PGHOST", "PGDATABASE", "PGUSER", "PGSSLMODE", "PGSSLCERT", "PGSSLKEY", "PGSSLROOTCERT"} {
		t.Setenv("READWRITE_"+name, "")
	}
	if _, err := postgresURL("READWRITE_"); err == nil {
		t.Fatal("postgresURL returned nil error with empty environment")
	}
}
