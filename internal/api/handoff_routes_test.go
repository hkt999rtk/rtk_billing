package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

const (
	handoffCloud     = "11111111-1111-1111-1111-111111111111"
	handoffOperation = "22222222-2222-2222-2222-222222222222"
	handoffSource    = "33333333-3333-3333-3333-333333333333"
	handoffTarget    = "44444444-4444-4444-4444-444444444444"
	handoffAuth      = "55555555-5555-5555-5555-555555555555"
)

type recordingHandoff struct {
	prepare   paymentstore.PrepareOwnershipHandoffInput
	settle    paymentstore.HandoffScope
	confirm   paymentstore.ConfirmHandoffSnapshotInput
	authorize paymentstore.AuthorizeHandoffCommitInput
	finalize  paymentstore.FinalizeHandoffInput
	abort     paymentstore.BeginHandoffAbortInput
	preflight paymentstore.CloudPreflightScope
	closure   paymentstore.PrepareCloudClosureInput
	close     paymentstore.CloseCloudInput
	cancel    paymentstore.CloudClosureScope
	cancelID  string
	cancelSHA string
	retire    paymentstore.CloseCloudInput
	err       error
}

func (f *recordingHandoff) GetCloudDeletionPreflight(_ context.Context, in paymentstore.CloudPreflightScope) (paymentstore.CloudDeletionPreflight, error) {
	f.preflight = in
	return paymentstore.CloudDeletionPreflight{CloudPreflightScope: in}, f.err
}
func (f *recordingHandoff) PrepareCloudClosure(_ context.Context, in paymentstore.PrepareCloudClosureInput) (paymentstore.CloudClosure, error) {
	f.closure = in
	return paymentstore.CloudClosure{ID: in.Scope.OperationID}, f.err
}
func (f *recordingHandoff) GetCloudClosureStatus(_ context.Context, in paymentstore.CloudClosureScope) (paymentstore.CloudClosureStatus, error) {
	f.cancel = in
	return paymentstore.CloudClosureStatus{}, f.err
}
func (f *recordingHandoff) CloseCloud(_ context.Context, in paymentstore.CloseCloudInput) (paymentstore.CloudClosureAck, error) {
	f.close = in
	return paymentstore.CloudClosureAck{}, f.err
}
func (f *recordingHandoff) CancelCloudClosure(_ context.Context, in paymentstore.CloudClosureScope, id, sha string) (paymentstore.CloudClosure, error) {
	f.cancel, f.cancelID, f.cancelSHA = in, id, sha
	return paymentstore.CloudClosure{ID: in.OperationID}, f.err
}
func (f *recordingHandoff) RetireCloudClose(_ context.Context, in paymentstore.CloseCloudInput) (paymentstore.CloudCloseResolution, error) {
	f.retire = in
	return paymentstore.CloudCloseResolution{}, f.err
}

func (f *recordingHandoff) PrepareOwnershipHandoff(_ context.Context, in paymentstore.PrepareOwnershipHandoffInput) (paymentstore.OwnershipHandoff, error) {
	f.prepare = in
	return paymentstore.OwnershipHandoff{ID: in.OperationID, SourceUserID: in.SourceUserID, TargetUserID: in.TargetUserID}, f.err
}
func (f *recordingHandoff) GetHandoffSettlementStatus(_ context.Context, in paymentstore.HandoffScope) (paymentstore.HandoffSettlementStatus, error) {
	f.settle = in
	return paymentstore.HandoffSettlementStatus{}, f.err
}
func (f *recordingHandoff) ConfirmHandoffSnapshot(_ context.Context, in paymentstore.ConfirmHandoffSnapshotInput) (paymentstore.HandoffSettlementStatus, error) {
	f.confirm = in
	return paymentstore.HandoffSettlementStatus{}, f.err
}
func (f *recordingHandoff) AuthorizeHandoffCommit(_ context.Context, in paymentstore.AuthorizeHandoffCommitInput) (paymentstore.HandoffCommitAuthorization, error) {
	f.authorize = in
	return paymentstore.HandoffCommitAuthorization{}, f.err
}
func (f *recordingHandoff) FinalizeOwnershipHandoff(_ context.Context, in paymentstore.FinalizeHandoffInput) (paymentstore.HandoffProtocolAck, error) {
	f.finalize = in
	return paymentstore.HandoffProtocolAck{}, f.err
}
func (f *recordingHandoff) BeginOwnershipHandoffAbort(_ context.Context, in paymentstore.BeginHandoffAbortInput) (paymentstore.HandoffProtocolAck, error) {
	f.abort = in
	return paymentstore.HandoffProtocolAck{}, f.err
}

func handoffRouteRequest(t *testing.T, server *Server, method, suffix, body string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v1/internal/billing/clouds/" + handoffCloud + "/ownership-handoffs/" + handoffOperation + suffix
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testHandoffToken)
	req.Header.Set("X-Billing-Ownership-Version", "7")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res := httptest.NewRecorder()
	server.Router().ServeHTTP(res, req)
	return res
}

func configuredRecordingHandoff(t *testing.T) (*Server, *recordingHandoff) {
	t.Helper()
	s := newHandoffTestServer(t)
	f := &recordingHandoff{}
	if err := s.ConfigureHandoff(HandoffAPIOptions{Token: testHandoffToken, Store: f}); err != nil {
		t.Fatal(err)
	}
	return s, f
}

func TestHandoffRoutesBindExactScopeAndPayloads(t *testing.T) {
	s, f := configuredRecordingHandoff(t)
	cutoff := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	cases := []struct {
		method, suffix, body string
		check                func()
	}{
		{http.MethodPost, "/prepare", `{"source_user_id":"` + handoffSource + `","target_user_id":"` + handoffTarget + `","cutoff":"` + cutoff.Format(time.RFC3339) + `"}`, func() {
			if f.prepare.OrganizationID != handoffCloud || f.prepare.OperationID != handoffOperation || f.prepare.OwnershipVersion != 7 || f.prepare.SourceUserID != handoffSource || f.prepare.TargetUserID != handoffTarget || !f.prepare.Cutoff.Equal(cutoff) {
				t.Fatalf("prepare input: %+v", f.prepare)
			}
		}},
		{http.MethodGet, "/settlement", "", func() {
			if f.settle.OrganizationID != handoffCloud || f.settle.OperationID != handoffOperation || f.settle.OwnershipVersion != 7 {
				t.Fatalf("settlement scope: %+v", f.settle)
			}
		}},
		{http.MethodPost, "/confirm", `{"user_id":"` + handoffSource + `","snapshot_version":2,"balance_minor":0,"currency":"TWD"}`, func() {
			if f.confirm.UserID != handoffSource || f.confirm.SnapshotVersion != 2 || f.confirm.BalanceMinor != 0 || f.confirm.Scope.OwnershipVersion != 7 {
				t.Fatalf("zero confirmation: %+v", f.confirm)
			}
		}},
		{http.MethodPost, "/confirm", `{"user_id":"` + handoffTarget + `","snapshot_version":3,"balance_minor":1,"currency":"TWD"}`, func() {
			if f.confirm.UserID != handoffTarget || f.confirm.BalanceMinor != 1 {
				t.Fatalf("positive confirmation: %+v", f.confirm)
			}
		}},
		{http.MethodPost, "/authorize-commit", `{"authorization_id":"` + handoffAuth + `","snapshot_version":3}`, func() {
			if f.authorize.AuthorizationID != handoffAuth || f.authorize.SnapshotVersion != 3 || f.authorize.Scope.OperationID != handoffOperation {
				t.Fatalf("authorization: %+v", f.authorize)
			}
		}},
		{http.MethodPost, "/finalize", `{"authorization_id":"` + handoffAuth + `","committed_owner_user_id":"` + handoffTarget + `","committed_ownership_version":8,"committed_at":"` + cutoff.Format(time.RFC3339) + `","am_commit_sha256":"` + strings.Repeat("a", 64) + `"}`, func() {
			if f.finalize.AuthorizationID != handoffAuth || f.finalize.CommittedOwnerUserID != handoffTarget || f.finalize.CommittedOwnershipVersion != 8 || f.finalize.AMCommitSHA256 != strings.Repeat("a", 64) {
				t.Fatalf("finalize: %+v", f.finalize)
			}
		}},
		{http.MethodPost, "/abort", `{"cancellation_id":"` + handoffAuth + `","am_cancellation_sha256":"` + strings.Repeat("b", 64) + `","authorization_id":"` + handoffOperation + `"}`, func() {
			if f.abort.CancellationID != handoffAuth || f.abort.AMCancellationSHA256 != strings.Repeat("b", 64) || f.abort.AuthorizationID != handoffOperation {
				t.Fatalf("abort: %+v", f.abort)
			}
		}},
	}
	for _, tc := range cases {
		res := handoffRouteRequest(t, s, tc.method, tc.suffix, tc.body)
		if res.Code != http.StatusOK || res.Header().Get("Cache-Control") != "no-store" || !strings.Contains(res.Body.String(), `"cloud_id":"`+handoffCloud+`"`) {
			t.Fatalf("%s: status=%d headers=%v body=%s", tc.suffix, res.Code, res.Header(), res.Body.String())
		}
		tc.check()
	}
}

func TestHandoffRoutesRejectInvalidScopeAndStrictBodies(t *testing.T) {
	s, f := configuredRecordingHandoff(t)
	validConfirm := `{"user_id":"` + handoffSource + `","snapshot_version":2,"balance_minor":0,"currency":"TWD"}`
	for _, tc := range []struct {
		name, path, version, contentType, body string
	}{
		{"noncanonical version", "/v1/internal/billing/clouds/" + handoffCloud + "/ownership-handoffs/" + handoffOperation + "/confirm", "07", "application/json", validConfirm},
		{"invalid cloud", "/v1/internal/billing/clouds/not-a-uuid/ownership-handoffs/" + handoffOperation + "/confirm", "7", "application/json", validConfirm},
		{"invalid operation", "/v1/internal/billing/clouds/" + handoffCloud + "/ownership-handoffs/not-a-uuid/confirm", "7", "application/json", validConfirm},
		{"wrong media", "/v1/internal/billing/clouds/" + handoffCloud + "/ownership-handoffs/" + handoffOperation + "/confirm", "7", "text/plain", validConfirm},
		{"unknown field", "/v1/internal/billing/clouds/" + handoffCloud + "/ownership-handoffs/" + handoffOperation + "/confirm", "7", "application/json", strings.TrimSuffix(validConfirm, "}") + `,"extra":true}`},
		{"trailing value", "/v1/internal/billing/clouds/" + handoffCloud + "/ownership-handoffs/" + handoffOperation + "/confirm", "7", "application/json", validConfirm + `{}`},
		{"negative balance", "/v1/internal/billing/clouds/" + handoffCloud + "/ownership-handoffs/" + handoffOperation + "/confirm", "7", "application/json", `{"user_id":"` + handoffSource + `","snapshot_version":2,"balance_minor":-1,"currency":"TWD"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+testHandoffToken)
			req.Header.Set("X-Billing-Ownership-Version", tc.version)
			req.Header.Set("Content-Type", tc.contentType)
			res := httptest.NewRecorder()
			s.Router().ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
			}
		})
	}
	if f.confirm.UserID != "" {
		t.Fatalf("invalid request reached persistence: %+v", f.confirm)
	}
}

func TestHandoffReplyMapsPersistenceErrorsWithoutDiagnostics(t *testing.T) {
	scope := paymentstore.HandoffScope{OrganizationID: handoffCloud, OperationID: handoffOperation, OwnershipVersion: 1}
	for _, tc := range []struct {
		err        error
		status     int
		publicCode string
	}{
		{paymentstore.ErrNotFound, http.StatusNotFound, "BILLING_HANDOFF_NOT_FOUND"},
		{paymentstore.ErrHandoffParticipant, http.StatusForbidden, "BILLING_HANDOFF_PARTICIPANT_REQUIRED"},
		{paymentstore.ErrOwnershipVersionConflict, http.StatusConflict, "BILLING_HANDOFF_VERSION_CONFLICT"},
		{paymentstore.ErrSettlementEvidenceStale, http.StatusConflict, "BILLING_HANDOFF_SNAPSHOT_CONFLICT"},
		{paymentstore.ErrHandoffNotConfirmable, http.StatusConflict, "BILLING_HANDOFF_SNAPSHOT_CONFLICT"},
		{paymentstore.ErrOwnershipEvidenceMissing, http.StatusConflict, "BILLING_HANDOFF_CONFLICT"},
		{paymentstore.ErrHandoffFenced, http.StatusConflict, "BILLING_HANDOFF_CONFLICT"},
		{paymentstore.ErrConflict, http.StatusConflict, "BILLING_HANDOFF_CONFLICT"},
		{paymentstore.ErrIdempotencyConflict, http.StatusConflict, "BILLING_HANDOFF_CONFLICT"},
		{paymentstore.ErrAccountClosed, http.StatusConflict, "BILLING_HANDOFF_CONFLICT"},
		{errors.New("secret database host"), http.StatusServiceUnavailable, "BILLING_HANDOFF_UNAVAILABLE"},
	} {
		res := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(res)
		handoffReply(c, scope, "settlement", nil, tc.err)
		if res.Code != tc.status || !strings.Contains(res.Body.String(), tc.publicCode) || strings.Contains(res.Body.String(), "secret") {
			t.Fatalf("error=%v status=%d body=%s", tc.err, res.Code, res.Body.String())
		}
	}
	res := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(res)
	handoffReply(c, scope, "settlement", map[string]bool{"ready": true}, nil)
	var body map[string]any
	if err := json.NewDecoder(bytes.NewReader(res.Body.Bytes())).Decode(&body); err != nil || res.Code != http.StatusOK || body["cloud_id"] != handoffCloud {
		t.Fatalf("success reply: status=%d body=%s err=%v", res.Code, res.Body.String(), err)
	}
}

func closureRouteRequest(t *testing.T, server *Server, method, suffix, body string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v1/internal/billing/clouds/" + handoffCloud + "/closures/" + handoffOperation + suffix
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testHandoffToken)
	req.Header.Set("X-Billing-Ownership-Version", "7")
	req.Header.Set("X-Billing-Owner-User-ID", handoffSource)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res := httptest.NewRecorder()
	server.Router().ServeHTTP(res, req)
	return res
}

func TestCloudPreflightAndClosureRoutesBindCoordinatorEvidence(t *testing.T) {
	s, f := configuredRecordingHandoff(t)
	preflightReq := httptest.NewRequest(http.MethodGet, "/v1/internal/billing/clouds/"+handoffCloud+"/deletion-preflight", nil)
	preflightReq.Header.Set("Authorization", "Bearer "+testHandoffToken)
	preflightReq.Header.Set("X-Billing-Ownership-Version", "7")
	preflightReq.Header.Set("X-Billing-Owner-User-ID", handoffSource)
	preflightRes := httptest.NewRecorder()
	s.Router().ServeHTTP(preflightRes, preflightReq)
	if preflightRes.Code != http.StatusOK || f.preflight.OrganizationID != handoffCloud || f.preflight.OwnerUserID != handoffSource || f.preflight.OwnershipVersion != 7 {
		t.Fatalf("preflight status=%d input=%+v body=%s", preflightRes.Code, f.preflight, preflightRes.Body.String())
	}
	cutoff := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	digest := strings.Repeat("a", 64)
	settlement := "66666666-6666-6666-6666-666666666666"
	cases := []struct {
		method, suffix, body string
		check                func()
	}{
		{http.MethodPost, "/prepare", `{"cutoff":"` + cutoff.Format(time.RFC3339) + `","am_request_sha256":"` + digest + `"}`, func() {
			if f.closure.Scope.OperationID != handoffOperation || f.closure.Scope.OwnerUserID != handoffSource || f.closure.AMRequestSHA256 != digest || !f.closure.Cutoff.Equal(cutoff) {
				t.Fatalf("closure prepare: %+v", f.closure)
			}
		}},
		{http.MethodGet, "/status", "", func() {
			if f.cancel.OperationID != handoffOperation || f.cancel.OwnershipVersion != 7 {
				t.Fatalf("closure status: %+v", f.cancel)
			}
		}},
		{http.MethodPost, "/close", `{"settlement_id":"` + settlement + `","am_readiness_sha256":"` + digest + `"}`, func() {
			if f.close.SettlementID != settlement || f.close.AMReadinessSHA256 != digest {
				t.Fatalf("closure close: %+v", f.close)
			}
		}},
		{http.MethodPost, "/retire-command", `{"settlement_id":"` + settlement + `","am_readiness_sha256":"` + digest + `"}`, func() {
			if f.retire.SettlementID != settlement || f.retire.Scope.OperationID != handoffOperation {
				t.Fatalf("closure retire: %+v", f.retire)
			}
		}},
		{http.MethodPost, "/cancel", `{"cancellation_id":"` + settlement + `","am_cancellation_sha256":"` + digest + `"}`, func() {
			if f.cancelID != settlement || f.cancelSHA != digest || f.cancel.OwnerUserID != handoffSource {
				t.Fatalf("closure cancel: %+v %s %s", f.cancel, f.cancelID, f.cancelSHA)
			}
		}},
	}
	for _, tc := range cases {
		res := closureRouteRequest(t, s, tc.method, tc.suffix, tc.body)
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"cloud_id":"`+handoffCloud+`"`) {
			t.Fatalf("%s status=%d body=%s", tc.suffix, res.Code, res.Body.String())
		}
		tc.check()
	}
}

func TestCloudClosureValidationAndErrorsAreExplicit(t *testing.T) {
	if !validClosureDigest(strings.Repeat("a", 64)) || validClosureDigest("short") || validClosureDigest(strings.Repeat("A", 64)) {
		t.Fatal("closure digest validation is not canonical")
	}
	s, _ := configuredRecordingHandoff(t)
	for _, tc := range []struct{ suffix, body string }{
		{"/prepare", `{"cutoff":"0001-01-01T00:00:00Z","am_request_sha256":"` + strings.Repeat("a", 64) + `"}`},
		{"/close", `{"settlement_id":"bad","am_readiness_sha256":"` + strings.Repeat("a", 64) + `"}`},
		{"/retire-command", `{"settlement_id":"` + handoffAuth + `","am_readiness_sha256":"bad"}`},
		{"/cancel", `{"cancellation_id":"bad","am_cancellation_sha256":"` + strings.Repeat("a", 64) + `"}`},
	} {
		if res := closureRouteRequest(t, s, http.MethodPost, tc.suffix, tc.body); res.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", tc.suffix, res.Code, res.Body.String())
		}
	}
	for _, err := range []error{paymentstore.ErrCloudClosureCommandRetired, paymentstore.ErrNotFound, paymentstore.ErrCloudClosureNotReady, paymentstore.ErrSettlementEvidenceStale, paymentstore.ErrConflict, paymentstore.ErrIdempotencyConflict, paymentstore.ErrOwnershipVersionConflict, paymentstore.ErrOwnershipEvidenceMissing, paymentstore.ErrHandoffFenced, errors.New("private")} {
		res := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(res)
		closureReply(c, paymentstore.CloudClosureScope{}, "operation", nil, err)
		if res.Code < 400 || strings.Contains(res.Body.String(), "private") {
			t.Fatalf("closure error %v: %d %s", err, res.Code, res.Body.String())
		}
	}
	for _, tc := range []struct {
		err    error
		status int
	}{{paymentstore.ErrNotFound, http.StatusNotFound}, {paymentstore.ErrOwnershipVersionConflict, http.StatusConflict}, {errors.New("private"), http.StatusServiceUnavailable}} {
		res := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(res)
		cloudPreflightError(c, tc.err)
		if res.Code != tc.status || strings.Contains(res.Body.String(), "private") {
			t.Fatalf("preflight error %v: %d %s", tc.err, res.Code, res.Body.String())
		}
	}
}
