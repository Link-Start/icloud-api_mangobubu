package domain

import "time"

// AliasDeletionQuota describes the shared Apple deletion allowance for one
// verified Apple principal. RetryAt is zero when the caller may proceed.
type AliasDeletionQuota struct {
	Used    int       `json:"used"`
	Limit   int       `json:"limit"`
	RetryAt time.Time `json:"retry_at,omitempty"`
}
