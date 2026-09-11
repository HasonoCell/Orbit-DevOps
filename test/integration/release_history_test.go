package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/google/uuid"
)

func TestReleaseHistoryCursorAndDetailRemainExplainable(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTarget(t, environment)
	first := createReleaseForTarget(t, environment, target.ID, "history-first")
	second := createReleaseForTarget(t, environment, target.ID, "history-second")
	third := createReleaseForTarget(t, environment, target.ID, "history-third")
	db := openTestDatabase(t, environment.databaseURL)
	operations := releaseoperation.New(db)

	firstLease := claimReleaseOperation(t, operations, "worker-history-first")
	if firstLease.ReleaseOperationID.String() != first.ReleaseOperation.ID {
		t.Fatalf("first history claim = %s, want %s", firstLease.ReleaseOperationID, first.ReleaseOperation.ID)
	}
	if err := operations.Succeed(context.Background(), firstLease); err != nil {
		t.Fatalf("complete first history operation: %v", err)
	}
	secondLease := claimReleaseOperation(t, operations, "worker-history-second")
	if secondLease.ReleaseOperationID.String() != second.ReleaseOperation.ID {
		t.Fatalf("second history claim = %s, want %s", secondLease.ReleaseOperationID, second.ReleaseOperation.ID)
	}
	if _, err := operations.Fail(context.Background(), secondLease, releaseoperation.Failure{
		Code: "image_invalid", Summary: "image cannot be deployed", Disposition: releaseoperation.NonRetryable,
	}); err != nil {
		t.Fatalf("fail second history operation: %v", err)
	}
	cancel := requestJSON(
		t,
		environment.server,
		http.MethodPost,
		"/api/v1/release-operations/"+third.ReleaseOperation.ID+"/cancel",
		"cancel-third-history-operation",
		"",
	)
	cancel.Body.Close()
	if cancel.StatusCode != http.StatusOK {
		t.Fatalf("cancel third history operation status = %d", cancel.StatusCode)
	}

	firstPageResponse := environment.get(
		t,
		"/api/v1/deployment-targets/"+target.ID+"/releases?limit=2",
	)
	defer firstPageResponse.Body.Close()
	if firstPageResponse.StatusCode != http.StatusOK {
		t.Fatalf("first history page status = %d", firstPageResponse.StatusCode)
	}
	firstPage := decodeReleaseHistoryPage(t, firstPageResponse)
	if len(firstPage.Items) != 2 || firstPage.NextCursor == nil {
		t.Fatalf("first history page = %#v", firstPage)
	}
	if firstPage.Items[0].Release.ID != third.Release.ID ||
		firstPage.Items[0].ReleaseOperation.Status != releaseoperation.StatusCanceled ||
		firstPage.Items[1].Release.ID != second.Release.ID ||
		firstPage.Items[1].ReleaseOperation.Status != releaseoperation.StatusFailed {
		t.Fatalf("first history page order/status = %#v", firstPage.Items)
	}

	// 在两页之间插入更新的 Release，旧游标仍只继续遍历原来更早的历史。
	createReleaseForTarget(t, environment, target.ID, "history-inserted-later")
	secondPageResponse := environment.get(
		t,
		"/api/v1/deployment-targets/"+target.ID+"/releases?limit=2&cursor="+
			url.QueryEscape(*firstPage.NextCursor),
	)
	defer secondPageResponse.Body.Close()
	if secondPageResponse.StatusCode != http.StatusOK {
		t.Fatalf("second history page status = %d", secondPageResponse.StatusCode)
	}
	secondPage := decodeReleaseHistoryPage(t, secondPageResponse)
	if len(secondPage.Items) != 1 || secondPage.Items[0].Release.ID != first.Release.ID ||
		secondPage.NextCursor != nil {
		t.Fatalf("second history page = %#v", secondPage)
	}

	invalidCursor := environment.get(
		t,
		"/api/v1/deployment-targets/"+target.ID+"/releases?cursor=not-a-cursor",
	)
	defer invalidCursor.Body.Close()
	assertError(t, invalidCursor, http.StatusBadRequest, "invalid_release_cursor")

	update := environment.putJSON(
		t,
		"/api/v1/deployment-targets/"+target.ID,
		"update-target-after-release",
		`{"stage":"development","replicas":3,"containerPort":9090}`,
	)
	update.Body.Close()
	if update.StatusCode != http.StatusOK {
		t.Fatalf("update target status = %d", update.StatusCode)
	}
	detailResponse := environment.get(t, "/api/v1/releases/"+first.Release.ID)
	defer detailResponse.Body.Close()
	if detailResponse.StatusCode != http.StatusOK {
		t.Fatalf("release detail status = %d", detailResponse.StatusCode)
	}
	detail := decodeReleaseDetail(t, detailResponse)
	if detail.ReleaseOperation.ID != first.ReleaseOperation.ID || detail.ReleaseOperation.Status != releaseoperation.StatusSucceeded ||
		len(detail.ReleaseOperation.Attempts) != 1 ||
		detail.ReleaseOperation.Attempts[0].Status != releaseoperation.AttemptSucceeded {
		t.Fatalf("release detail operation = %#v", detail.ReleaseOperation)
	}
	differences := map[string]snapshotDifferenceDocument{}
	for _, difference := range detail.SnapshotDifferences {
		differences[difference.Field] = difference
	}
	if differences["replicas"].ReleaseValue != strconv.Itoa(first.Release.TargetSnapshot.Replicas) ||
		differences["replicas"].CurrentValue != "3" ||
		differences["containerPort"].ReleaseValue != "8080" ||
		differences["containerPort"].CurrentValue != "9090" {
		t.Fatalf("release snapshot differences = %#v", detail.SnapshotDifferences)
	}
	actions := map[string]bool{}
	for _, record := range detail.AuditTimeline {
		actions[record.Action] = true
		if record.TargetID != first.Release.ID && record.TargetID != first.ReleaseOperation.ID {
			t.Errorf("unrelated audit target %s in release timeline", record.TargetID)
		}
	}
	if !actions["release.create"] || !actions["operation.claimed"] || !actions["operation.succeeded"] {
		t.Errorf("release audit actions = %#v", actions)
	}
}

func TestRollbackCopiesSourceSnapshotAndIsIdempotent(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTarget(t, environment)
	source := createReleaseForTarget(t, environment, target.ID, "rollback-source")
	update := environment.putJSON(
		t,
		"/api/v1/deployment-targets/"+target.ID,
		"update-before-rollback",
		`{"stage":"development","replicas":4,"containerPort":7070}`,
	)
	update.Body.Close()
	if update.StatusCode != http.StatusOK {
		t.Fatalf("update before rollback status = %d", update.StatusCode)
	}

	rollback := func(releaseID string, key string) *http.Response {
		return requestJSON(
			t,
			environment.server,
			http.MethodPost,
			"/api/v1/releases/"+releaseID+"/rollback",
			key,
			"",
		)
	}
	firstResponse := rollback(source.Release.ID, "rollback-source-release")
	defer firstResponse.Body.Close()
	if firstResponse.StatusCode != http.StatusCreated {
		t.Fatalf("rollback status = %d, want %d", firstResponse.StatusCode, http.StatusCreated)
	}
	created := decodeReleaseAcceptance(t, firstResponse)
	if created.Release.ID == source.Release.ID || created.Release.RollbackOfReleaseID == nil ||
		*created.Release.RollbackOfReleaseID != source.Release.ID {
		t.Fatalf("rollback lineage = %#v", created.Release)
	}
	if created.Release.DeploymentTargetID != source.Release.DeploymentTargetID ||
		created.Release.ImageReference != source.Release.ImageReference ||
		created.Release.TargetSnapshot != source.Release.TargetSnapshot {
		t.Fatalf("rollback did not copy source snapshot: %#v", created.Release)
	}
	if created.Release.TargetSnapshot.Replicas == 4 || created.Release.TargetSnapshot.ContainerPort == 7070 {
		t.Fatal("rollback mixed current target configuration into source snapshot")
	}
	if created.ReleaseOperation.ReleaseID != created.Release.ID ||
		created.ReleaseOperation.Status != releaseoperation.StatusPending {
		t.Fatalf("rollback operation = %#v", created.ReleaseOperation)
	}

	replayResponse := rollback(source.Release.ID, "rollback-source-release")
	defer replayResponse.Body.Close()
	if replayResponse.StatusCode != http.StatusCreated {
		t.Fatalf("rollback replay status = %d", replayResponse.StatusCode)
	}
	replayed := decodeReleaseAcceptance(t, replayResponse)
	if replayed.Release.ID != created.Release.ID || replayed.ReleaseOperation.ID != created.ReleaseOperation.ID {
		t.Fatalf("rollback replay = %#v, want %#v", replayed, created)
	}
	db := openTestDatabase(t, environment.databaseURL)
	var rollbackAuditCount int
	if err := db.QueryRowContext(
		context.Background(),
		`SELECT count(*) FROM audit_records
		 WHERE target_type = 'release' AND target_id = $1 AND action = 'release.rollback'`,
		created.Release.ID,
	).Scan(&rollbackAuditCount); err != nil {
		t.Fatalf("count rollback audit records: %v", err)
	}
	if rollbackAuditCount != 1 {
		t.Errorf("release.rollback audit count = %d, want 1", rollbackAuditCount)
	}

	otherSource := createReleaseForTarget(t, environment, target.ID, "rollback-other-source")
	conflict := rollback(otherSource.Release.ID, "rollback-source-release")
	defer conflict.Body.Close()
	assertError(t, conflict, http.StatusConflict, "idempotency_conflict")

	otherTarget := createDeploymentTargetWithSuffix(t, environment, "rollback-other-target")
	snapshotPayload, err := json.Marshal(source.Release.TargetSnapshot)
	if err != nil {
		t.Fatalf("encode cross-target snapshot: %v", err)
	}
	_, err = db.ExecContext(
		context.Background(),
		`INSERT INTO releases
		 (id, deployment_target_id, image_reference, target_snapshot,
		  rollback_of_release_id, created_by, created_at)
		 VALUES ($1, $2, $3, $4, $5, 'constraint-test', $6)`,
		uuid.New(),
		otherTarget.ID,
		source.Release.ImageReference,
		snapshotPayload,
		source.Release.ID,
		time.Now().UTC(),
	)
	if err == nil {
		t.Fatal("database accepted a rollback source from another deployment target")
	}
}

func decodeReleaseHistoryPage(t *testing.T, response *http.Response) releaseHistoryPageDocument {
	t.Helper()
	var page releaseHistoryPageDocument
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatalf("decode release history page: %v", err)
	}
	return page
}
