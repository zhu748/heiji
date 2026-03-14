package management

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

const authImportJobRetention = 2 * time.Hour

type authImportJob struct {
	mu          sync.RWMutex
	ID          string
	FileName    string
	Status      string
	Total       int
	Processed   int
	Imported    int
	Failed      int
	Results     []authImportResult
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt time.Time
}

type authImportJobSnapshot struct {
	ID        string             `json:"job_id"`
	FileName  string             `json:"file_name,omitempty"`
	Status    string             `json:"status"`
	Total     int                `json:"total"`
	Processed int                `json:"processed"`
	Imported  int                `json:"imported"`
	Failed    int                `json:"failed"`
	Results   []authImportResult `json:"results,omitempty"`
	Error     string             `json:"error,omitempty"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
}

func newAuthImportJobID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405.000000000")))
	}
	return hex.EncodeToString(buf[:])
}

func (h *Handler) createAuthImportJob(fileName string, total int) *authImportJob {
	job := &authImportJob{
		ID:        newAuthImportJobID(),
		FileName:  fileName,
		Status:    "processing",
		Total:     total,
		Results:   make([]authImportResult, 0, total),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	h.importJobsMu.Lock()
	h.importJobs[job.ID] = job
	h.importJobsMu.Unlock()
	return job
}

func (h *Handler) getAuthImportJob(id string) (*authImportJob, bool) {
	h.importJobsMu.RLock()
	job, ok := h.importJobs[id]
	h.importJobsMu.RUnlock()
	return job, ok
}

func (h *Handler) purgeExpiredImportJobs() {
	now := time.Now()
	h.importJobsMu.Lock()
	defer h.importJobsMu.Unlock()
	for id, job := range h.importJobs {
		if job == nil {
			delete(h.importJobs, id)
			continue
		}
		snapshot := job.snapshot(false)
		updatedAt := snapshot.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = snapshot.CreatedAt
		}
		if snapshot.Status == "processing" {
			continue
		}
		if now.Sub(updatedAt) > authImportJobRetention {
			delete(h.importJobs, id)
		}
	}
}

func (job *authImportJob) addResult(result authImportResult) {
	job.mu.Lock()
	defer job.mu.Unlock()
	job.Results = append(job.Results, result)
	job.Processed++
	if result.Status == "imported" {
		job.Imported++
	} else {
		job.Failed++
	}
	job.UpdatedAt = time.Now()
}

func (job *authImportJob) complete(err error) {
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

func (job *authImportJob) snapshot(includeResults bool) authImportJobSnapshot {
	job.mu.RLock()
	defer job.mu.RUnlock()

	snapshot := authImportJobSnapshot{
		ID:        job.ID,
		FileName:  job.FileName,
		Status:    job.Status,
		Total:     job.Total,
		Processed: job.Processed,
		Imported:  job.Imported,
		Failed:    job.Failed,
		Error:     job.Error,
		CreatedAt: job.CreatedAt,
		UpdatedAt: job.UpdatedAt,
	}
	if includeResults && len(job.Results) > 0 {
		snapshot.Results = append([]authImportResult(nil), job.Results...)
	}
	return snapshot
}
