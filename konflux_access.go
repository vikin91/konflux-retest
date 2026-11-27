package konfluxretest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type (
	// KonfluxAccessFailure identifies why a Konflux preflight failed.
	KonfluxAccessFailure string

	// KonfluxAccessError reports the target and cause of a failed Konflux preflight.
	KonfluxAccessError struct {
		Host, Namespace string
		Failure         KonfluxAccessFailure
		cause           error
	}
)

const (
	// KonfluxAccessConfiguration indicates a missing token provider or invalid target.
	KonfluxAccessConfiguration KonfluxAccessFailure = "configuration"
	// KonfluxAccessAuthentication indicates an unavailable or rejected credential.
	KonfluxAccessAuthentication KonfluxAccessFailure = "authentication"
	// KonfluxAccessForbidden indicates that caller policy or the cluster denied access.
	KonfluxAccessForbidden KonfluxAccessFailure = "forbidden"
	// KonfluxAccessTransport indicates a failure sending the review or reading its response.
	KonfluxAccessTransport KonfluxAccessFailure = "transport"
	// KonfluxAccessReview indicates an unexpected response or inconclusive review.
	KonfluxAccessReview KonfluxAccessFailure = "review"
)

// Error describes the failed access check without including credentials or server response text.
func (e *KonfluxAccessError) Error() string {
	return fmt.Sprintf("Konflux access check for host %q, namespace %q: %v", e.Host, e.Namespace, e.cause)
}

// Unwrap exposes the cause, including context cancellation and deadline errors.
func (e *KonfluxAccessError) Unwrap() error { return e.cause }

// CheckKonfluxAccess verifies that KonfluxToken grants get access to PipelineRuns
// in namespace without reading any runs. Use the GitHub App ID for lookup policy,
// or zero when no App applies. Run the check before starting a batch of assessments.
func CheckKonfluxAccess(ctx context.Context, config Config, appID int64, apiHost, namespace string) error {
	target := KonfluxAccessError{}
	fail := func(failure KonfluxAccessFailure, cause error) error {
		target.Failure, target.cause = failure, cause
		return &target
	}
	for label := range strings.SplitSeq(strings.ToLower(apiHost), ".") {
		if len(label) > 63 || !konfluxName.MatchString(label) {
			return fail(KonfluxAccessConfiguration, errors.New("expected a cluster API hostname without a scheme or port"))
		}
	}
	target.Host = apiHost
	if len(namespace) > 63 || !konfluxName.MatchString(namespace) {
		return fail(KonfluxAccessConfiguration, errors.New("expected a Kubernetes namespace name"))
	}
	target.Namespace = namespace
	if config.KonfluxToken == nil {
		return fail(KonfluxAccessConfiguration, errors.New("KonfluxToken is required"))
	}

	httpClient := config.serviceHTTPClient()
	client := konfluxClient{http: &httpClient, token: config.KonfluxToken, allowLookup: config.AllowKonfluxLookup}
	token, err := client.tokenForRun(ctx, appID, apiHost, namespace)
	if errors.Is(err, errKonfluxLookupDenied) {
		return fail(KonfluxAccessForbidden, err)
	}
	if err != nil {
		return fail(KonfluxAccessAuthentication, err)
	}

	body := fmt.Sprintf(`{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","spec":{"resourceAttributes":{"namespace":%q,"verb":"get","group":"tekton.dev","version":"v1","resource":"pipelineruns"}}}`, namespace)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+apiHost+"/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", strings.NewReader(body))
	if err != nil {
		return fail(KonfluxAccessConfiguration, errors.New("could not construct the access review request"))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.http.Do(req)
	if err != nil {
		return fail(KonfluxAccessTransport, diagnosticError("could not reach the cluster API", err))
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fail(KonfluxAccessAuthentication, errors.New("cluster rejected the token (HTTP 401)"))
	case http.StatusForbidden:
		return fail(KonfluxAccessForbidden, errors.New("cluster refused the access review (HTTP 403)"))
	case http.StatusOK, http.StatusCreated:
	default:
		return fail(KonfluxAccessReview, fmt.Errorf("access review returned HTTP %d", resp.StatusCode))
	}
	var review struct {
		Status struct {
			Allowed         *bool  `json:"allowed"`
			Denied          bool   `json:"denied"`
			EvaluationError string `json:"evaluationError"`
		} `json:"status"`
	}
	const reviewLimit = 16 << 10
	data, err := io.ReadAll(io.LimitReader(resp.Body, reviewLimit+1))
	if err != nil {
		return fail(KonfluxAccessTransport, diagnosticError("could not read the access review response", err))
	}
	if len(data) > reviewLimit || json.Unmarshal(data, &review) != nil || review.Status.Allowed == nil || (*review.Status.Allowed && review.Status.Denied) {
		return fail(KonfluxAccessReview, errors.New("invalid access review response"))
	}
	if *review.Status.Allowed {
		return nil
	}
	if review.Status.EvaluationError != "" && !review.Status.Denied {
		return fail(KonfluxAccessReview, errors.New("cluster could not determine PipelineRun access"))
	}
	return fail(KonfluxAccessForbidden, errors.New("cluster denied get access to PipelineRuns"))
}
