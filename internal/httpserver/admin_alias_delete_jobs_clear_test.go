package httpserver

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"icloud-api/internal/domain"
)

func TestAliasDeletionJobClearCompletedIsOwnerScopedAndCSRFProtected(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, owner := env.createSession(t, "clear-owner", "unused-password")
	otherCookie, otherCSRF, other := env.createSession(t, "clear-other", "unused-password")
	const path = "/admin/api/v1/aliases/batch/jobs/clear-completed"
	const jobID = "completed-job-00001"
	for _, adminID := range []int64{owner.ID, other.ID} {
		if err := env.store.CreateAliasDeletionJob(t.Context(), domain.AliasDeletionJob{
			ID: jobID, AdminID: adminID, Status: domain.AliasDeletionJobCompleted,
			CreatedAt: time.Now(), Items: []domain.AliasDeletionJobItem{{ID: 101, Done: true, Deleted: true}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name    string
		cookies []*http.Cookie
		csrf    string
		want    int
	}{
		{name: "no session", csrf: csrf, want: http.StatusUnauthorized},
		{name: "no csrf", cookies: []*http.Cookie{cookie}, want: http.StatusForbidden},
		{name: "other csrf", cookies: []*http.Cookie{cookie}, csrf: otherCSRF, want: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := env.request(t, http.MethodPost, path, nil, "", test.cookies, test.csrf)
			if response.Code != test.want {
				t.Fatalf("clear status = %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
			jobs, err := env.store.ListAliasDeletionJobs(t.Context(), owner.ID, 20)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("rejected request changed history: %#v, %v", jobs, err)
			}
		})
	}
	for index := range 2 {
		response := env.request(t, http.MethodPost, path, nil, "", []*http.Cookie{cookie}, csrf)
		var body struct {
			Data struct {
				Cleared       int      `json:"cleared"`
				ClearedJobIDs []string `json:"cleared_job_ids"`
			}
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || body.Data.Cleared != 1-index || !reflect.DeepEqual(body.Data.ClearedJobIDs, []string{jobID}) {
			t.Fatalf("clear attempt %d = %d %s", index, response.Code, response.Body.String())
		}
	}
	list := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs", nil, "", []*http.Cookie{cookie}, "")
	var listBody struct {
		Data struct{ Jobs []adminAPIAliasDeletionJobDTO }
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listBody); err != nil {
		t.Fatal(err)
	}
	if list.Code != http.StatusOK || listBody.Data.Jobs == nil || len(listBody.Data.Jobs) != 0 {
		t.Fatalf("cleared list = %d %s", list.Code, list.Body.String())
	}
	latest := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs/latest", nil, "", []*http.Cookie{cookie}, "")
	var latestBody struct{ Data *adminAPIAliasDeletionJobDTO }
	if err := json.Unmarshal(latest.Body.Bytes(), &latestBody); err != nil {
		t.Fatal(err)
	}
	if latest.Code != http.StatusOK || latestBody.Data != nil {
		t.Fatalf("cleared latest = %d %s", latest.Code, latest.Body.String())
	}
	get := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs/"+jobID, nil, "", []*http.Cookie{cookie}, "")
	if get.Code != http.StatusOK || decodeTestAliasDeletionJob(t, get).Deleted != 1 {
		t.Fatalf("cleared job lookup = %d %s", get.Code, get.Body.String())
	}
	// The alias no longer needs to exist for the original operation ID to replay.
	env.server.SetHMESyncService(&fakeHMESyncService{})
	retry := submitTestDeletionJob(t, env, cookie, csrf, jobID, 101)
	if retry.JobID != jobID || retry.Status != domain.AliasDeletionJobCompleted || retry.Deleted != 1 {
		t.Fatalf("cleared operation replay = %#v", retry)
	}
	conflict := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", adminAPITestJSON(t, map[string]any{"alias_ids": []int64{102}, "operation_id": jobID}), "application/json", []*http.Cookie{cookie}, csrf)
	if conflict.Code != http.StatusConflict || adminAPITestErrorCode(t, conflict) != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("cleared operation conflict = %d %s", conflict.Code, conflict.Body.String())
	}
	otherList := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs", nil, "", []*http.Cookie{otherCookie}, "")
	if err := json.Unmarshal(otherList.Body.Bytes(), &listBody); err != nil {
		t.Fatal(err)
	}
	if otherList.Code != http.StatusOK || len(listBody.Data.Jobs) != 1 || listBody.Data.Jobs[0].JobID != jobID {
		t.Fatalf("other administrator's history = %d %s", otherList.Code, otherList.Body.String())
	}
}

func TestAliasDeletionJobClearCompletedEmptyResponse(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, _ := env.createSession(t, "clear-empty", "unused-password")
	response := env.request(t, http.MethodPost, "/admin/api/v1/aliases/batch/jobs/clear-completed", nil, "", []*http.Cookie{cookie}, csrf)
	var body struct {
		Data struct {
			Cleared       int      `json:"cleared"`
			ClearedJobIDs []string `json:"cleared_job_ids"`
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || body.Data.Cleared != 0 || body.Data.ClearedJobIDs == nil || len(body.Data.ClearedJobIDs) != 0 {
		t.Fatalf("empty clear = %d %s", response.Code, response.Body.String())
	}
}
