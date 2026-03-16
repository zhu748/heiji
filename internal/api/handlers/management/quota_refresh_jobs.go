package management

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

var (
	antigravityQuotaProbeURLs = []string{
		"https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
		"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:fetchAvailableModels",
		"https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
	}
	claudeQuotaProbeURL    = "https://api.anthropic.com/api/oauth/usage"
	codexQuotaProbeURL     = "https://chatgpt.com/backend-api/wham/usage"
	geminiCLIQuotaProbeURL = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"
	kimiQuotaProbeURL      = "https://api.kimi.com/coding/v1/usages"
)

const quotaRefreshJobRetention = 2 * time.Hour

type quotaRefreshJob struct {
	mu          sync.RWMutex
	ID          string
	Status      string
	Total       int
	Processed   int
	Deleted     int
	Results     []quotaRefreshResult
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt time.Time
}

type quotaRefreshResult struct {
	Name       string `json:"name"`
	Provider   string `json:"provider,omitempty"`
	Status     string `json:"status"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Deleted    bool   `json:"deleted,omitempty"`
	Message    string `json:"message,omitempty"`
}

type quotaRefreshJobSnapshot struct {
	ID        string               `json:"job_id"`
	Status    string               `json:"status"`
	Total     int                  `json:"total"`
	Processed int                  `json:"processed"`
	Deleted   int                  `json:"deleted"`
	Results   []quotaRefreshResult `json:"results,omitempty"`
	Error     string               `json:"error,omitempty"`
	CreatedAt time.Time            `json:"created_at"`
	UpdatedAt time.Time            `json:"updated_at"`
}

type quotaRefreshRequest struct {
	Names []string `json:"names"`
}

func (h *Handler) StartQuotaRefreshJob(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req quotaRefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	auths := h.resolveQuotaRefreshTargets(req.Names)
	if len(auths) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no valid auth files selected"})
		return
	}

	job := h.createQuotaRefreshJob(len(auths))
	go h.runQuotaRefreshJob(job, auths)

	c.JSON(http.StatusAccepted, gin.H{
		"status": "accepted",
		"job_id": job.ID,
		"total":  job.Total,
	})
}

func (h *Handler) GetQuotaRefreshJob(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("id"))
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "job id is required"})
		return
	}
	job, ok := h.getQuotaRefreshJob(jobID)
	if !ok || job == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota refresh job not found"})
		return
	}
	c.JSON(http.StatusOK, job.snapshot(true))
}

func (h *Handler) createQuotaRefreshJob(total int) *quotaRefreshJob {
	job := &quotaRefreshJob{
		ID:        newAuthImportJobID(),
		Status:    "processing",
		Total:     total,
		Results:   make([]quotaRefreshResult, 0, total),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	h.quotaJobsMu.Lock()
	h.quotaJobs[job.ID] = job
	h.quotaJobsMu.Unlock()
	return job
}

func (h *Handler) getQuotaRefreshJob(id string) (*quotaRefreshJob, bool) {
	h.quotaJobsMu.RLock()
	job, ok := h.quotaJobs[id]
	h.quotaJobsMu.RUnlock()
	return job, ok
}

func (h *Handler) purgeExpiredQuotaJobs() {
	now := time.Now()
	h.quotaJobsMu.Lock()
	defer h.quotaJobsMu.Unlock()
	for id, job := range h.quotaJobs {
		if job == nil {
			delete(h.quotaJobs, id)
			continue
		}
		snapshot := job.snapshot(false)
		if snapshot.Status == "processing" {
			continue
		}
		updatedAt := snapshot.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = snapshot.CreatedAt
		}
		if now.Sub(updatedAt) > quotaRefreshJobRetention {
			delete(h.quotaJobs, id)
		}
	}
}

func (job *quotaRefreshJob) addResult(result quotaRefreshResult) {
	job.mu.Lock()
	defer job.mu.Unlock()
	job.Results = append(job.Results, result)
	job.Processed++
	if result.Deleted {
		job.Deleted++
	}
	job.UpdatedAt = time.Now()
}

func (job *quotaRefreshJob) complete(err error) {
	job.mu.Lock()
	defer job.mu.Unlock()
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
	} else {
		job.Status = "completed"
		job.Error = ""
	}
	now := time.Now()
	job.CompletedAt = now
	job.UpdatedAt = now
}

func (job *quotaRefreshJob) snapshot(includeResults bool) quotaRefreshJobSnapshot {
	job.mu.RLock()
	defer job.mu.RUnlock()

	snapshot := quotaRefreshJobSnapshot{
		ID:        job.ID,
		Status:    job.Status,
		Total:     job.Total,
		Processed: job.Processed,
		Deleted:   job.Deleted,
		Error:     job.Error,
		CreatedAt: job.CreatedAt,
		UpdatedAt: job.UpdatedAt,
	}
	if includeResults && len(job.Results) > 0 {
		snapshot.Results = append([]quotaRefreshResult(nil), job.Results...)
	}
	return snapshot
}

func (h *Handler) resolveQuotaRefreshTargets(names []string) []*coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}

	trimmedNames := make([]string, 0, len(names))
	seenNames := make(map[string]struct{}, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if _, exists := seenNames[trimmed]; exists {
			continue
		}
		seenNames[trimmed] = struct{}{}
		trimmedNames = append(trimmedNames, trimmed)
	}

	auths := h.authManager.List()
	resolved := make([]*coreauth.Auth, 0, len(trimmedNames))
	seenIDs := make(map[string]struct{}, len(trimmedNames))
	for _, name := range trimmedNames {
		for _, auth := range auths {
			if auth == nil {
				continue
			}
			if auth.Disabled {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(auth.FileName), name) || strings.EqualFold(strings.TrimSpace(auth.ID), name) {
				if _, exists := seenIDs[auth.ID]; exists {
					break
				}
				seenIDs[auth.ID] = struct{}{}
				resolved = append(resolved, auth.Clone())
				break
			}
		}
	}
	return resolved
}

func (h *Handler) runQuotaRefreshJob(job *quotaRefreshJob, auths []*coreauth.Auth) {
	if job == nil {
		return
	}

	ctx := context.Background()
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		result := h.refreshQuotaForAuth(ctx, auth)
		job.addResult(result)
	}
	job.complete(nil)
}

func (h *Handler) refreshQuotaForAuth(ctx context.Context, auth *coreauth.Auth) quotaRefreshResult {
	result := quotaRefreshResult{
		Name:     strings.TrimSpace(auth.FileName),
		Provider: strings.TrimSpace(auth.Provider),
		Status:   "skipped",
	}
	if result.Name == "" {
		result.Name = strings.TrimSpace(auth.ID)
	}
	if auth == nil {
		result.Message = "auth not found"
		return result
	}

	statusCode, message, err := h.executeQuotaProbe(ctx, auth)
	if statusCode > 0 {
		result.HTTPStatus = statusCode
	}
	if message != "" {
		result.Message = message
	}
	if err != nil {
		result.Status = "failed"
		if result.Message == "" {
			result.Message = err.Error()
		}
		return result
	}
	if statusCode == http.StatusUnauthorized {
		if errDelete := h.deleteAuthForQuotaRefresh(ctx, auth); errDelete != nil {
			result.Status = "failed"
			result.Message = fmt.Sprintf("401 detected but delete failed: %v", errDelete)
			return result
		}
		result.Status = "deleted"
		result.Deleted = true
		if result.Message == "" {
			result.Message = "credential returned 401 and was deleted"
		}
		return result
	}
	if statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices {
		result.Status = "success"
		if result.Message == "" {
			result.Message = "quota refreshed"
		}
		return result
	}
	result.Status = "failed"
	if result.Message == "" {
		result.Message = fmt.Sprintf("quota probe returned status %d", statusCode)
	}
	return result
}

func (h *Handler) executeQuotaProbe(ctx context.Context, auth *coreauth.Auth) (int, string, error) {
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "antigravity":
		projectID := strings.TrimSpace(stringValue(auth.Metadata, "project_id"))
		if projectID == "" {
			projectID = "bamboo-precept-lgxtn"
		}
		payload := fmt.Sprintf(`{"project":%q}`, projectID)
		var lastStatus int
		var lastMessage string
		for _, probeURL := range antigravityQuotaProbeURLs {
			status, body, err := h.performQuotaProbeRequest(ctx, auth, http.MethodPost, probeURL, map[string]string{
				"Authorization": "Bearer $TOKEN$",
				"Content-Type":  "application/json",
				"User-Agent":    "antigravity/1.11.5 windows/amd64",
			}, payload)
			if status == http.StatusUnauthorized || (status >= http.StatusOK && status < http.StatusMultipleChoices) || err != nil {
				return status, body, err
			}
			lastStatus = status
			lastMessage = body
		}
		return lastStatus, lastMessage, nil
	case "claude":
		return h.performQuotaProbeRequest(ctx, auth, http.MethodGet, claudeQuotaProbeURL, map[string]string{
			"Authorization":  "Bearer $TOKEN$",
			"Content-Type":   "application/json",
			"anthropic-beta": "oauth-2025-04-20",
		}, "")
	case "codex":
		accountID := resolveCodexChatgptAccountID(auth)
		if accountID == "" {
			return 0, "missing chatgpt account id", fmt.Errorf("missing chatgpt account id")
		}
		return h.performQuotaProbeRequest(ctx, auth, http.MethodGet, codexQuotaProbeURL, map[string]string{
			"Authorization":      "Bearer $TOKEN$",
			"Content-Type":       "application/json",
			"User-Agent":         "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal",
			"Chatgpt-Account-Id": accountID,
		}, "")
	case "gemini-cli":
		projectID := strings.TrimSpace(stringValue(auth.Metadata, "project_id"))
		if projectID == "" {
			_, accountValue := auth.AccountInfo()
			projectID = extractGeminiCLIProjectID(strings.TrimSpace(accountValue))
		}
		if projectID == "" {
			return 0, "missing project id", fmt.Errorf("missing project id")
		}
		payload := fmt.Sprintf(`{"project":%q}`, projectID)
		return h.performQuotaProbeRequest(ctx, auth, http.MethodPost, geminiCLIQuotaProbeURL, map[string]string{
			"Authorization": "Bearer $TOKEN$",
			"Content-Type":  "application/json",
		}, payload)
	case "kimi":
		return h.performQuotaProbeRequest(ctx, auth, http.MethodGet, kimiQuotaProbeURL, map[string]string{
			"Authorization": "Bearer $TOKEN$",
		}, "")
	default:
		return 0, "provider not supported for quota refresh", fmt.Errorf("provider %s not supported", auth.Provider)
	}
}

func (h *Handler) performQuotaProbeRequest(ctx context.Context, auth *coreauth.Auth, method, urlStr string, headers map[string]string, body string) (int, string, error) {
	token, err := h.resolveTokenForAuth(ctx, auth)
	if err != nil {
		return 0, "", err
	}

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, reader)
	if err != nil {
		return 0, "", err
	}

	hostOverride := ""
	for key, value := range headers {
		if strings.EqualFold(key, "host") {
			hostOverride = strings.TrimSpace(value)
			continue
		}
		req.Header.Set(key, strings.ReplaceAll(value, "$TOKEN$", token))
	}
	if hostOverride != "" {
		req.Host = hostOverride
	}

	client := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth),
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	message := strings.TrimSpace(string(bodyBytes))
	return resp.StatusCode, message, nil
}

func (h *Handler) deleteAuthForQuotaRefresh(ctx context.Context, auth *coreauth.Auth) error {
	if h == nil || auth == nil {
		return fmt.Errorf("auth not found")
	}
	targetPath := strings.TrimSpace(authAttribute(auth, "path"))
	if targetPath == "" && h.cfg != nil {
		if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
			targetPath = filepath.Join(h.cfg.AuthDir, filepath.Base(fileName))
		}
	}
	if targetPath != "" {
		if !filepath.IsAbs(targetPath) {
			if abs, err := filepath.Abs(targetPath); err == nil {
				targetPath = abs
			}
		}
		if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := h.deleteTokenRecord(ctx, targetPath); err != nil {
			return err
		}
	}
	if auth.ID != "" {
		h.disableAuth(ctx, auth.ID)
	} else if targetPath != "" {
		h.disableAuth(ctx, targetPath)
	}
	return nil
}

func resolveCodexChatgptAccountID(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	rawIDToken, _ := auth.Metadata["id_token"].(string)
	rawIDToken = strings.TrimSpace(rawIDToken)
	if rawIDToken == "" {
		return ""
	}
	claims, err := codex.ParseJWTToken(rawIDToken)
	if err != nil || claims == nil {
		return ""
	}
	return strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID)
}

func extractGeminiCLIProjectID(account string) string {
	account = strings.TrimSpace(account)
	if account == "" {
		return ""
	}
	start := strings.LastIndex(account, "(")
	end := strings.LastIndex(account, ")")
	if start < 0 || end <= start {
		return ""
	}
	return strings.TrimSpace(account[start+1 : end])
}
