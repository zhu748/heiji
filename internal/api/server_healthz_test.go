package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestHealthzEndpoints(t *testing.T) {
	srv := NewServer(&config.Config{
		Host: "127.0.0.1",
		Port: 8317,
	}, nil, nil, t.TempDir()+"\\config.yaml")

	t.Run("GET", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

		srv.engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if got := rec.Body.String(); got != "{\"status\":\"ok\"}" {
			t.Fatalf("unexpected body: %s", got)
		}
	})

	t.Run("HEAD", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodHead, "/healthz", nil)

		srv.engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("expected empty body for HEAD, got %q", rec.Body.String())
		}
	})
}
