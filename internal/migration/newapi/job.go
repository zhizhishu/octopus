package newapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

const maxRetainedJobs = 20

type JobStatus string

const (
	JobStatusQueued    JobStatus = "queued"
	JobStatusRunning   JobStatus = "running"
	JobStatusSucceeded JobStatus = "succeeded"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCanceled  JobStatus = "canceled"
)

type JobStartRequest struct {
	SourceType        string  `json:"source_type"`
	SourceDSN         string  `json:"source_dsn"`
	SourceLogType     string  `json:"source_log_type"`
	SourceLogDSN      string  `json:"source_log_dsn"`
	Apply             bool    `json:"apply"`
	ConfirmApply      bool    `json:"confirm_apply"`
	DryRunJobID       string  `json:"dry_run_job_id"`
	IncludeLogs       *bool   `json:"include_logs,omitempty"`
	IncludeAPIKeys    *bool   `json:"include_api_keys,omitempty"`
	QuotaPerUnit      float64 `json:"quota_per_unit"`
	BatchSize         int     `json:"batch_size"`
	ConflictStrategy  string  `json:"conflict_strategy"`
	PasswordMode      string  `json:"password_mode"`
	APIKeyPrefix      string  `json:"api_key_prefix"`
	PreserveAdminRole bool    `json:"preserve_admin_role"`
	DebugSQL          bool    `json:"debug_sql"`
}

type JobRequestView struct {
	SourceType        string  `json:"source_type"`
	SourceDSN         string  `json:"source_dsn"`
	SourceLogType     string  `json:"source_log_type"`
	SourceLogDSN      string  `json:"source_log_dsn,omitempty"`
	Apply             bool    `json:"apply"`
	IncludeLogs       bool    `json:"include_logs"`
	IncludeAPIKeys    bool    `json:"include_api_keys"`
	QuotaPerUnit      float64 `json:"quota_per_unit"`
	BatchSize         int     `json:"batch_size"`
	ConflictStrategy  string  `json:"conflict_strategy"`
	PasswordMode      string  `json:"password_mode"`
	APIKeyPrefix      string  `json:"api_key_prefix"`
	PreserveAdminRole bool    `json:"preserve_admin_role"`
}

type JobSnapshot struct {
	ID          string         `json:"id"`
	Status      JobStatus      `json:"status"`
	Stage       string         `json:"stage"`
	Percent     int            `json:"percent"`
	Message     string         `json:"message"`
	Apply       bool           `json:"apply"`
	CanApply    bool           `json:"can_apply"`
	Fingerprint string         `json:"fingerprint"`
	Request     JobRequestView `json:"request"`
	Summary     *Summary       `json:"summary,omitempty"`
	Error       string         `json:"error,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	StartedAt   *time.Time     `json:"started_at,omitempty"`
	FinishedAt  *time.Time     `json:"finished_at,omitempty"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

type jobOptions struct {
	SourceType        string
	SourceDSN         string
	SourceLogType     string
	SourceLogDSN      string
	Apply             bool
	IncludeLogs       bool
	IncludeAPIKeys    bool
	QuotaPerUnit      float64
	BatchSize         int
	ConflictStrategy  string
	PasswordMode      string
	APIKeyPrefix      string
	PreserveAdminRole bool
	DebugSQL          bool
}

type migrationJob struct {
	id          string
	status      JobStatus
	stage       string
	percent     int
	message     string
	options     jobOptions
	fingerprint string
	summary     *Summary
	err         string
	createdAt   time.Time
	startedAt   *time.Time
	finishedAt  *time.Time
	updatedAt   time.Time
	cancel      context.CancelFunc
}

type JobManager struct {
	mu    sync.RWMutex
	jobs  map[string]*migrationJob
	order []string
}

func NewJobManager() *JobManager {
	return &JobManager{
		jobs: make(map[string]*migrationJob),
	}
}

func (m *JobManager) Start(req JobStartRequest, targetDB *gorm.DB) (JobSnapshot, error) {
	options, err := normalizeJobStartRequest(req)
	if err != nil {
		return JobSnapshot{}, err
	}
	if options.Apply && targetDB == nil {
		return JobSnapshot{}, fmt.Errorf("target database is not ready")
	}
	fingerprint := jobFingerprint(options)

	m.mu.Lock()
	defer m.mu.Unlock()

	if options.Apply {
		if !req.ConfirmApply {
			return JobSnapshot{}, fmt.Errorf("confirm_apply is required for apply jobs")
		}
		dryRun, ok := m.jobs[strings.TrimSpace(req.DryRunJobID)]
		if !ok || dryRun.status != JobStatusSucceeded || dryRun.options.Apply || dryRun.summary == nil {
			return JobSnapshot{}, fmt.Errorf("a successful dry-run job is required before apply")
		}
		if dryRun.fingerprint != fingerprint {
			return JobSnapshot{}, fmt.Errorf("apply options do not match the dry-run job")
		}
	}
	for _, job := range m.jobs {
		if job.fingerprint == fingerprint && job.options.Apply == options.Apply && isActiveJob(job.status) {
			return JobSnapshot{}, fmt.Errorf("a matching migration job is already running")
		}
	}

	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	job := &migrationJob{
		id:          newJobID(),
		status:      JobStatusQueued,
		stage:       "queued",
		percent:     0,
		message:     "waiting to start",
		options:     options,
		fingerprint: fingerprint,
		createdAt:   now,
		updatedAt:   now,
		cancel:      cancel,
	}
	m.jobs[job.id] = job
	m.order = append(m.order, job.id)
	m.pruneLocked()

	go m.run(ctx, job.id, targetDB)

	return job.snapshot(), nil
}

func (m *JobManager) Get(id string) (JobSnapshot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[strings.TrimSpace(id)]
	if !ok {
		return JobSnapshot{}, false
	}
	return job.snapshot(), true
}

func (m *JobManager) List() []JobSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]JobSnapshot, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		if job, ok := m.jobs[m.order[i]]; ok {
			result = append(result, job.snapshot())
		}
	}
	return result
}

func (m *JobManager) Cancel(id string) (JobSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[strings.TrimSpace(id)]
	if !ok {
		return JobSnapshot{}, fmt.Errorf("migration job not found")
	}
	if !isActiveJob(job.status) {
		return job.snapshot(), nil
	}
	if job.cancel != nil {
		job.cancel()
	}
	now := time.Now()
	job.status = JobStatusCanceled
	job.stage = "complete"
	job.percent = 100
	job.message = "canceled"
	job.finishedAt = &now
	job.updatedAt = now
	return job.snapshot(), nil
}

func (m *JobManager) run(ctx context.Context, id string, targetDB *gorm.DB) {
	m.update(id, func(job *migrationJob) {
		now := time.Now()
		job.status = JobStatusRunning
		job.stage = "scan_users"
		job.percent = 4
		job.message = "opening New API source database"
		job.startedAt = &now
		job.updatedAt = now
	})

	options, ok := m.options(id)
	if !ok {
		return
	}

	sourceDB, closeSource, err := OpenDatabase(options.SourceType, options.SourceDSN, options.DebugSQL)
	if err != nil {
		m.fail(id, fmt.Errorf("open New API source database: %w", err))
		return
	}
	defer closeSource()

	sourceLogDB := sourceDB
	if strings.TrimSpace(options.SourceLogDSN) != "" && options.SourceLogDSN != options.SourceDSN {
		var closeLog func() error
		sourceLogDB, closeLog, err = OpenDatabase(options.SourceLogType, options.SourceLogDSN, options.DebugSQL)
		if err != nil {
			m.fail(id, fmt.Errorf("open New API log database: %w", err))
			return
		}
		defer closeLog()
	}

	summary, err := Run(ctx, Config{
		SourceDB:          sourceDB,
		SourceLogDB:       sourceLogDB,
		TargetDB:          targetDB,
		Apply:             options.Apply,
		IncludeLogs:       options.IncludeLogs,
		IncludeAPIKeys:    options.IncludeAPIKeys,
		QuotaPerUnit:      options.QuotaPerUnit,
		BatchSize:         options.BatchSize,
		ConflictStrategy:  options.ConflictStrategy,
		PasswordMode:      options.PasswordMode,
		APIKeyPrefix:      options.APIKeyPrefix,
		PreserveAdminRole: options.PreserveAdminRole,
		Progress: func(update ProgressUpdate) {
			m.progress(id, update)
		},
	})
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			m.cancelFinished(id)
			return
		}
		m.fail(id, err)
		return
	}
	m.succeed(id, summary)
}

func (m *JobManager) options(id string) (jobOptions, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[id]
	if !ok {
		return jobOptions{}, false
	}
	return job.options, true
}

func (m *JobManager) update(id string, fn func(*migrationJob)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job, ok := m.jobs[id]; ok {
		fn(job)
	}
}

func (m *JobManager) progress(id string, update ProgressUpdate) {
	m.update(id, func(job *migrationJob) {
		if !isActiveJob(job.status) {
			return
		}
		job.stage = strings.TrimSpace(update.Stage)
		job.percent = update.Percent
		job.message = strings.TrimSpace(update.Message)
		job.updatedAt = time.Now()
	})
}

func (m *JobManager) fail(id string, err error) {
	m.update(id, func(job *migrationJob) {
		if job.status == JobStatusCanceled {
			return
		}
		now := time.Now()
		job.status = JobStatusFailed
		job.stage = "complete"
		job.percent = 100
		job.message = "failed"
		job.err = err.Error()
		job.finishedAt = &now
		job.updatedAt = now
	})
}

func (m *JobManager) succeed(id string, summary *Summary) {
	m.update(id, func(job *migrationJob) {
		if job.status == JobStatusCanceled {
			return
		}
		now := time.Now()
		job.status = JobStatusSucceeded
		job.stage = "complete"
		job.percent = 100
		job.message = "completed"
		job.summary = summary
		job.finishedAt = &now
		job.updatedAt = now
	})
}

func (m *JobManager) cancelFinished(id string) {
	m.update(id, func(job *migrationJob) {
		now := time.Now()
		job.status = JobStatusCanceled
		job.stage = "complete"
		job.percent = 100
		job.message = "canceled"
		job.finishedAt = &now
		job.updatedAt = now
	})
}

func (m *JobManager) pruneLocked() {
	for len(m.order) > maxRetainedJobs {
		oldest := m.order[0]
		job := m.jobs[oldest]
		if job != nil && isActiveJob(job.status) {
			return
		}
		delete(m.jobs, oldest)
		m.order = m.order[1:]
	}
}

func (job *migrationJob) snapshot() JobSnapshot {
	return JobSnapshot{
		ID:          job.id,
		Status:      job.status,
		Stage:       job.stage,
		Percent:     job.percent,
		Message:     job.message,
		Apply:       job.options.Apply,
		CanApply:    !job.options.Apply && job.status == JobStatusSucceeded && job.summary != nil,
		Fingerprint: job.fingerprint,
		Request:     job.requestView(),
		Summary:     job.summary,
		Error:       job.err,
		CreatedAt:   job.createdAt,
		StartedAt:   job.startedAt,
		FinishedAt:  job.finishedAt,
		UpdatedAt:   job.updatedAt,
	}
}

func (job *migrationJob) requestView() JobRequestView {
	return JobRequestView{
		SourceType:        job.options.SourceType,
		SourceDSN:         redactDSN(job.options.SourceDSN),
		SourceLogType:     job.options.SourceLogType,
		SourceLogDSN:      redactDSN(job.options.SourceLogDSN),
		Apply:             job.options.Apply,
		IncludeLogs:       job.options.IncludeLogs,
		IncludeAPIKeys:    job.options.IncludeAPIKeys,
		QuotaPerUnit:      job.options.QuotaPerUnit,
		BatchSize:         job.options.BatchSize,
		ConflictStrategy:  job.options.ConflictStrategy,
		PasswordMode:      job.options.PasswordMode,
		APIKeyPrefix:      job.options.APIKeyPrefix,
		PreserveAdminRole: job.options.PreserveAdminRole,
	}
}

func normalizeJobStartRequest(req JobStartRequest) (jobOptions, error) {
	sourceType := normalizeDBType(req.SourceType)
	sourceDSN := strings.TrimSpace(req.SourceDSN)
	if sourceDSN == "" {
		return jobOptions{}, fmt.Errorf("source_dsn is required")
	}
	sourceLogType := normalizeDBType(req.SourceLogType)
	if strings.TrimSpace(req.SourceLogType) == "" {
		sourceLogType = sourceType
	}
	batchSize := req.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	if batchSize > 5000 {
		batchSize = 5000
	}
	cfg := normalizeConfig(Config{
		QuotaPerUnit:      req.QuotaPerUnit,
		BatchSize:         batchSize,
		ConflictStrategy:  req.ConflictStrategy,
		PasswordMode:      req.PasswordMode,
		APIKeyPrefix:      req.APIKeyPrefix,
		PreserveAdminRole: req.PreserveAdminRole,
	})
	return jobOptions{
		SourceType:        sourceType,
		SourceDSN:         sourceDSN,
		SourceLogType:     sourceLogType,
		SourceLogDSN:      strings.TrimSpace(req.SourceLogDSN),
		Apply:             req.Apply,
		IncludeLogs:       false,
		IncludeAPIKeys:    false,
		QuotaPerUnit:      cfg.QuotaPerUnit,
		BatchSize:         cfg.BatchSize,
		ConflictStrategy:  cfg.ConflictStrategy,
		PasswordMode:      cfg.PasswordMode,
		APIKeyPrefix:      cfg.APIKeyPrefix,
		PreserveAdminRole: cfg.PreserveAdminRole,
		DebugSQL:          req.DebugSQL,
	}, nil
}

func jobFingerprint(options jobOptions) string {
	copy := options
	copy.Apply = false
	copy.DebugSQL = false
	encoded, _ := json.Marshal(copy)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func newJobID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return "job-" + hex.EncodeToString(b[:])
}

func isActiveJob(status JobStatus) bool {
	return status == JobStatusQueued || status == JobStatusRunning
}

func redactDSN(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.Index(strings.ToLower(value), "password="); idx >= 0 {
		start := idx + len("password=")
		end := strings.Index(value[start:], " ")
		if end < 0 {
			return value[:start] + "****"
		}
		return value[:start] + "****" + value[start+end:]
	}
	at := strings.LastIndex(value, "@")
	colon := strings.Index(value, ":")
	if at > 0 && colon > -1 && colon < at {
		return value[:colon+1] + "****" + value[at:]
	}
	return value
}
