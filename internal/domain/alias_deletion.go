package domain

import "time"

const (
	AliasDeletionJobQueued      = "queued"
	AliasDeletionJobRunning     = "running"
	AliasDeletionJobCompleted   = "completed"
	AliasDeletionJobInterrupted = "interrupted"
)

// AliasDeletionJob is a durable progress snapshot, not an executable queue.
// It contains no Apple session or other credentials; interrupted jobs are
// retained for inspection and must never be resumed automatically.
type AliasDeletionJob struct {
	ID        string                 `json:"id"`
	AdminID   int64                  `json:"admin_id"`
	RequestID string                 `json:"request_id"`
	Status    string                 `json:"status"`
	Items     []AliasDeletionJobItem `json:"items"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

// AliasDeletionJobItem records the last confirmed result for one alias. Done
// stays false if a restart interrupts work before its outcome is recorded.
type AliasDeletionJobItem struct {
	ID            int64  `json:"id"`
	Address       string `json:"address"`
	Done          bool   `json:"done"`
	Deleted       bool   `json:"deleted"`
	Code          string `json:"code"`
	Message       string `json:"message"`
	LocalRetained bool   `json:"local_retained"`
}
