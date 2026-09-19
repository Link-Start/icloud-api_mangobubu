package domain

import "time"

const (
	AliasDeletionWorkPending   = "pending"
	AliasDeletionWorkRunning   = "running"
	AliasDeletionWorkWaiting   = "waiting"
	AliasDeletionWorkPaused    = "paused"
	AliasDeletionWorkSucceeded = "succeeded"
	AliasDeletionWorkFailed    = "failed"
	AliasDeletionWorkCancelled = "cancelled"
)

// AliasDeletionWork is the sole execution record for a remote mailbox deletion.
// Several jobs may subscribe to one work item; credentials are always loaded
// afresh by the executor and are never persisted here.
type AliasDeletionWork struct {
	ID            string
	Sequence      int64
	AccountID     int64
	AliasID       int64
	Address       string
	AccountEmail  string
	AppleID       string
	AppleSubject  string
	Status        string
	ClaimToken    string
	Reconcile     bool
	NextRunAt     time.Time
	WaitReason    string
	Operation     string
	Used          int
	Limit         int
	Attempts      int
	HTTPStatus    int
	ServiceCode   string
	Deleted       bool
	Code          string
	Message       string
	LocalRetained bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
