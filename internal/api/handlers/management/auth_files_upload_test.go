package management

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
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

func TestUploadAuthFile_ZipArchiveImportsMultipleJSONFiles(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	var archive bytes.Buffer
	zipWriter := zip.NewWriter(&archive)
	files := map[string]string{
		"one.json":             `{"type":"codex","email":"one@example.com"}`,
		"nested/two.json":      `{"type":"claude","email":"two@example.com"}`,
		"nested/ignore-me.txt": `not json`,
	}
	for name, content := range files {
		w, errCreate := zipWriter.Create(name)
		if errCreate != nil {
			t.Fatalf("create zip entry %s: %v", name, errCreate)
		}
		if _, errWrite := w.Write([]byte(content)); errWrite != nil {
			t.Fatalf("write zip entry %s: %v", name, errWrite)
		}
	}
	if errClose := zipWriter.Close(); errClose != nil {
		t.Fatalf("close zip writer: %v", errClose)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, errCreate := writer.CreateFormFile("file", "auths.zip")
	if errCreate != nil {
		t.Fatalf("create multipart file: %v", errCreate)
	}
	if _, errWrite := part.Write(archive.Bytes()); errWrite != nil {
		t.Fatalf("write multipart file: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}
	if got, ok := payload["imported"].(float64); !ok || int(got) != 2 {
		t.Fatalf("expected imported=2, got %#v", payload["imported"])
	}
	results, ok := payload["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("expected 2 result entries, got %#v", payload["results"])
	}

	for _, name := range []string{"one.json", "two.json"} {
		if _, errStat := os.Stat(filepath.Join(authDir, name)); errStat != nil {
			t.Fatalf("expected imported file %s to exist: %v", name, errStat)
		}
	}

	auths := manager.List()
	if len(auths) != 2 {
		t.Fatalf("expected 2 auths registered, got %d", len(auths))
	}
}

func TestUploadAuthFile_ZipArchiveReturnsPerFileFailures(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	var archive bytes.Buffer
	zipWriter := zip.NewWriter(&archive)
	files := map[string]string{
		"ok.json":        `{"type":"codex","email":"ok@example.com"}`,
		"bad.json":       `{not-json`,
		"nested/ok.json": `{"type":"claude","email":"dup@example.com"}`,
	}
	for name, content := range files {
		w, errCreate := zipWriter.Create(name)
		if errCreate != nil {
			t.Fatalf("create zip entry %s: %v", name, errCreate)
		}
		if _, errWrite := w.Write([]byte(content)); errWrite != nil {
			t.Fatalf("write zip entry %s: %v", name, errWrite)
		}
	}
	if errClose := zipWriter.Close(); errClose != nil {
		t.Fatalf("close zip writer: %v", errClose)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, errCreate := writer.CreateFormFile("file", "auths.zip")
	if errCreate != nil {
		t.Fatalf("create multipart file: %v", errCreate)
	}
	if _, errWrite := part.Write(archive.Bytes()); errWrite != nil {
		t.Fatalf("write multipart file: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}
	if got, ok := payload["imported"].(float64); !ok || int(got) != 1 {
		t.Fatalf("expected imported=1, got %#v", payload["imported"])
	}
	results, ok := payload["results"].([]any)
	if !ok || len(results) != 3 {
		t.Fatalf("expected 3 result entries, got %#v", payload["results"])
	}
	auths := manager.List()
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth registered, got %d", len(auths))
	}
}

func TestUploadAuthFile_ZipArchiveAsyncImportReportsProgress(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	var archive bytes.Buffer
	zipWriter := zip.NewWriter(&archive)
	files := map[string]string{
		"one.json":        `{"type":"codex","email":"one@example.com"}`,
		"nested/two.json": `{"type":"claude","email":"two@example.com"}`,
	}
	for name, content := range files {
		w, errCreate := zipWriter.Create(name)
		if errCreate != nil {
			t.Fatalf("create zip entry %s: %v", name, errCreate)
		}
		if _, errWrite := w.Write([]byte(content)); errWrite != nil {
			t.Fatalf("write zip entry %s: %v", name, errWrite)
		}
	}
	if errClose := zipWriter.Close(); errClose != nil {
		t.Fatalf("close zip writer: %v", errClose)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, errCreate := writer.CreateFormFile("file", "auths.zip")
	if errCreate != nil {
		t.Fatalf("create multipart file: %v", errCreate)
	}
	if _, errWrite := part.Write(archive.Bytes()); errWrite != nil {
		t.Fatalf("write multipart file: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files?async=1", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusAccepted, rec.Code, rec.Body.String())
	}

	var accepted struct {
		JobID string `json:"job_id"`
		Total int    `json:"total"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &accepted); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}
	if accepted.JobID == "" {
		t.Fatalf("expected job id in response")
	}
	if accepted.Total != 2 {
		t.Fatalf("expected total=2, got %d", accepted.Total)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		statusRec := httptest.NewRecorder()
		statusCtx, _ := gin.CreateTestContext(statusRec)
		statusReq := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/import-jobs/"+accepted.JobID, nil)
		statusCtx.Request = statusReq
		statusCtx.Params = gin.Params{{Key: "id", Value: accepted.JobID}}

		h.GetAuthFileImportJob(statusCtx)

		if statusRec.Code != http.StatusOK {
			t.Fatalf("expected status endpoint to return 200, got %d with body %s", statusRec.Code, statusRec.Body.String())
		}

		var payload struct {
			Status    string `json:"status"`
			Total     int    `json:"total"`
			Processed int    `json:"processed"`
			Imported  int    `json:"imported"`
			Failed    int    `json:"failed"`
			Results   []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"results"`
		}
		if errUnmarshal := json.Unmarshal(statusRec.Body.Bytes(), &payload); errUnmarshal != nil {
			t.Fatalf("decode status response: %v", errUnmarshal)
		}
		if payload.Total != 2 {
			t.Fatalf("expected job total=2, got %d", payload.Total)
		}
		if payload.Status == "completed" {
			if payload.Processed != 2 || payload.Imported != 2 || payload.Failed != 0 {
				t.Fatalf("unexpected completion payload: %+v", payload)
			}
			if len(payload.Results) != 2 {
				t.Fatalf("expected 2 results, got %d", len(payload.Results))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for async import completion; last payload=%s", statusRec.Body.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
}
