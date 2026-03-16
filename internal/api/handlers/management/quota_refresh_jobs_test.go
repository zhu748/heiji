package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestQuotaRefreshJobDeletes401Credential(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()

	originalURLs := antigravityQuotaProbeURLs
	antigravityQuotaProbeURLs = []string{server.URL}
	defer func() { antigravityQuotaProbeURLs = originalURLs }()

	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "ag.json")
	if err := os.WriteFile(authPath, []byte(`{"type":"antigravity"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "ag.json",
		Provider: "antigravity",
		FileName: "ag.json",
		Metadata: map[string]any{
			"access_token": "bad-token",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
		Attributes: map[string]string{
			"path": authPath,
		},
		Status: coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = store

	body := bytes.NewBufferString(`{"names":["ag.json"]}`)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota-refresh-jobs", body)
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.StartQuotaRefreshJob(ctx)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d with body %s", rec.Code, rec.Body.String())
	}

	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode accepted response: %v", err)
	}
	if accepted.JobID == "" {
		t.Fatalf("expected job id")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		statusRec := httptest.NewRecorder()
		statusCtx, _ := gin.CreateTestContext(statusRec)
		statusReq := httptest.NewRequest(http.MethodGet, "/v0/management/quota-refresh-jobs/"+accepted.JobID, nil)
		statusCtx.Request = statusReq
		statusCtx.Params = gin.Params{{Key: "id", Value: accepted.JobID}}

		h.GetQuotaRefreshJob(statusCtx)

		if statusRec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d with body %s", statusRec.Code, statusRec.Body.String())
		}

		var payload struct {
			Status    string `json:"status"`
			Processed int    `json:"processed"`
			Deleted   int    `json:"deleted"`
			Results   []struct {
				Name    string `json:"name"`
				Status  string `json:"status"`
				Deleted bool   `json:"deleted"`
			} `json:"results"`
		}
		if err := json.Unmarshal(statusRec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode status response: %v", err)
		}
		if payload.Status == "completed" {
			if payload.Processed != 1 || payload.Deleted != 1 {
				t.Fatalf("unexpected payload: %+v", payload)
			}
			if len(payload.Results) != 1 || payload.Results[0].Status != "deleted" || !payload.Results[0].Deleted {
				t.Fatalf("unexpected result payload: %+v", payload.Results)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for quota refresh completion: %s", statusRec.Body.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

	if _, err := os.Stat(authPath); !os.IsNotExist(err) {
		t.Fatalf("expected auth file removed, stat err=%v", err)
	}
}
