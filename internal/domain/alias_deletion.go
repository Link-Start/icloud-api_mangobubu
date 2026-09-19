package domain

import "time"

const (
	AliasDeletionJobQueued      = "queued"
	AliasDeletionJobRunning     = "running"
	AliasDeletionJobCompleted   = "completed"
	AliasDeletionJobInterrupted = "interrupted"
)

// AliasDeletionJob is an owner's durable subscription to deletion work. Legacy
// snapshots have QueueVersion zero and are never resumed after a restart.
type AliasDeletionJob struct {
	ID                         string                 `json:"id"`
	AdminID                    int64                  `json:"admin_id"`
	RequestID                  string                 `json:"request_id"`
	Status                     string                 `json:"status"`
	Items                      []AliasDeletionJobItem `json:"items"`
	CreatedAt                  time.Time              `json:"created_at"`
	UpdatedAt                  time.Time              `json:"updated_at"`
	QueueVersion               int                    `json:"queue_version,omitempty"`
	AuthorizingPasswordVersion int64                  `json:"-"`
	Username                   string                 `json:"-"`
	IP                         string                 `json:"-"`
	CancelRequested            bool                   `json:"cancel_requested,omitempty"`
}

// AliasDeletionJobItem records the last confirmed result for one alias. Done
// stays false if a restart interrupts work before its outcome is recorded.
type AliasDeletionJobItem struct {
	ID            int64     `json:"id"`
	Address       string    `json:"address"`
	Done          bool      `json:"done"`
	Deleted       bool      `json:"deleted"`
	Code          string    `json:"code"`
	Message       string    `json:"message"`
	LocalRetained bool      `json:"local_retained"`
	AccountID     int64     `json:"account_id,omitempty"`
	AccountEmail  string    `json:"account_email,omitempty"`
	AppleSubject  string    `json:"apple_subject,omitempty"`
	WorkID        string    `json:"work_id,omitempty"`
	State         string    `json:"state,omitempty"`
	RetryAt       time.Time `json:"retry_at,omitzero"`
	WaitReason    string    `json:"wait_reason,omitempty"`
	Used          int       `json:"used,omitempty"`
	Limit         int       `json:"limit,omitempty"`
	Operation     string    `json:"operation,omitempty"`
	HTTPStatus    int       `json:"http_status,omitempty"`
	ServiceCode   string    `json:"service_code,omitempty"`
}
