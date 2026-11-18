package konfluxretest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type runObservation struct {
	matches               bool
	imageURL, imageDigest string
}

// runIdentity verifies original PipelineRun annotations, never lossy Kubernetes labels.
// Annotation keys follow Pipelines-as-Code pkg/kubeinteraction/labels.go.
func (c *konfluxClient) runIdentity(ctx context.Context, check checkRun, repo, sha, tag string, pr int) (observation runObservation, err error) {
	category := "unsupported URLs"
	defer func() {
		if err != nil {
			err = &identityFailure{category: category, cause: err}
		}
	}()
	u, err := url.Parse(check.DetailsURL)
	if err != nil {
		return runObservation{}, fmt.Errorf("parse PipelineRun URL: %w", preserveCause(err, ""))
	}
	switch {
	case u.Scheme != "https":
		return runObservation{}, fmt.Errorf("unsupported PipelineRun URL scheme %q; expected https", u.Scheme)
	case u.User != nil:
		return runObservation{}, errors.New("unsupported PipelineRun URL: embedded credentials are not allowed")
	case u.Port() != "":
		return runObservation{}, fmt.Errorf("unsupported PipelineRun URL: explicit port %q is not allowed", u.Port())
	case !strings.HasPrefix(u.Host, "konflux-ui.apps.") || !strings.HasSuffix(u.Host, ".openshiftapps.com"):
		return runObservation{}, fmt.Errorf("unsupported PipelineRun URL host %q; expected konflux-ui.apps.<cluster>.openshiftapps.com", u.Host)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "ns" || parts[2] != "pipelinerun" {
		return runObservation{}, errors.New("unsupported PipelineRun path; expected /ns/<namespace>/pipelinerun/<name>")
	}
	if !konfluxName.MatchString(parts[1]) {
		return runObservation{}, fmt.Errorf("unsupported PipelineRun namespace %q; expected a lowercase Kubernetes resource name", parts[1])
	}
	if !konfluxName.MatchString(parts[3]) {
		return runObservation{}, fmt.Errorf("unsupported PipelineRun name %q; expected a lowercase Kubernetes resource name", parts[3])
	}
	category = "authentication failures"
	cluster := strings.TrimPrefix(u.Host, "konflux-ui.apps.")
	token, err := c.token(ctx, "api."+cluster)
	if err != nil {
		return runObservation{}, fmt.Errorf("PipelineRun identity authentication for api.%s: %w", cluster, preserveCause(err, token))
	}
	if token == "" {
		return runObservation{}, fmt.Errorf("PipelineRun identity authentication for api.%s returned an empty token", cluster)
	}
	category = "API/archive failures"
	liveTimeout := 5 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		// Reserve half of the remaining caller budget for the archive when the deadline is short.
		liveTimeout = min(liveTimeout, time.Until(deadline)/2)
	}
	liveCtx, cancel := context.WithTimeout(ctx, liveTimeout)
	data, err := c.get(liveCtx, "https://api."+cluster+"/apis/tekton.dev/v1/namespaces/"+parts[1]+"/pipelineruns/"+parts[3], token)
	cancel()
	if err != nil {
		apiErr := err
		if ctxErr := ctx.Err(); ctxErr != nil {
			return runObservation{}, fmt.Errorf("PipelineRun identity lookup stopped before archive fallback: API: %w; caller: %w", apiErr, ctxErr)
		}
		filter := `data_type in ["tekton.dev/v1.PipelineRun", "tekton.dev/v1beta1.PipelineRun"] && data.metadata.name == ` + strconv.Quote(parts[3])
		address := "https://tekton-results-tekton-results.apps." + cluster + "/apis/results.tekton.dev/v1alpha2/parents/" + parts[1] + "/results/-/records?" + url.Values{"filter": {filter}, "page_size": {archivePageSize}}.Encode()
		data, err = c.get(ctx, address, token)
		if err != nil {
			return runObservation{}, fmt.Errorf("PipelineRun identity unavailable in API and archive: API: %w; archive: %w", apiErr, err)
		}
		var response struct {
			Records []struct {
				Data struct {
					Value []byte `json:"value"`
				} `json:"data"`
			} `json:"records"`
			Next string `json:"next_page_token"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return runObservation{}, fmt.Errorf("API: %w; decode PipelineRun archive response: %w", apiErr, preserveCause(err, token))
		}
		if len(response.Records) != 1 || response.Next != "" {
			return runObservation{}, fmt.Errorf("ambiguous archived PipelineRun identity: records=%d, more_pages=%t; API: %w", len(response.Records), response.Next != "", apiErr)
		}
		data = response.Records[0].Data.Value
	}
	category = "identity validation failures"
	matches, err := matchRunIdentity(data, parts[1], parts[3], check.ID, repo, sha, tag, pr)
	if err != nil {
		return runObservation{}, preserveCause(err, token)
	}
	if !matches {
		return runObservation{}, nil
	}
	imageURL, imageDigest := pipelineImageResults(data)
	return runObservation{matches: true, imageURL: imageURL, imageDigest: imageDigest}, nil
}

func matchRunIdentity(data []byte, namespace, name string, id int64, repo, sha, tag string, pr int) (bool, error) {
	var run struct {
		Metadata struct {
			Name, Namespace string
			Annotations     map[string]string
		}
	}
	if err := json.Unmarshal(data, &run); err != nil {
		return false, fmt.Errorf("decode PipelineRun identity: %w", err)
	}
	if run.Metadata.Name != name || run.Metadata.Namespace != namespace {
		return false, fmt.Errorf("PipelineRun identity mismatch: got %q/%q; expected %q/%q", run.Metadata.Namespace, run.Metadata.Name, namespace, name)
	}
	a := run.Metadata.Annotations
	const prefix = "pipelinesascode.tekton.dev/"
	if a[prefix+"check-run-id"] != strconv.FormatInt(id, 10) {
		return false, fmt.Errorf("PipelineRun check-run-id annotation %q does not match %d", a[prefix+"check-run-id"], id)
	}
	if a[prefix+"sha"] != sha {
		return false, fmt.Errorf("PipelineRun SHA annotation %q does not match %q", a[prefix+"sha"], sha)
	}
	if strings.TrimSuffix(a[prefix+"repo-url"], ".git") != "https://github.com/"+repo {
		return false, fmt.Errorf("PipelineRun repository annotation %q does not match %q", diagnosticURL(a[prefix+"repo-url"]), "https://github.com/"+repo)
	}
	if a[prefix+"event-type"] == "" || a[prefix+"branch"] == "" {
		return false, fmt.Errorf("PipelineRun event identity unavailable: event-type=%q, branch=%q", a[prefix+"event-type"], a[prefix+"branch"])
	}
	if tag != "" {
		return a[prefix+"event-type"] == "push" && a[prefix+"branch"] == "refs/tags/"+tag, nil
	}
	if a[prefix+"event-type"] != "pull_request" {
		return false, nil
	}
	number, err := strconv.Atoi(a[prefix+"pull-request"])
	if err != nil || number <= 0 || a[prefix+"pull-request"] != strconv.Itoa(number) {
		return false, errors.New("PipelineRun PR event identity unavailable: expected a positive pull-request number")
	}
	return number == pr, nil
}

// pipelineImageResults preserves literal image outputs without changing retry eligibility.
func pipelineImageResults(data []byte) (string, string) {
	type result struct {
		Name  string          `json:"name"`
		Value json.RawMessage `json:"value"`
	}
	var run struct {
		Status struct {
			Results         []result `json:"results"`
			PipelineResults []result `json:"pipelineResults"`
		} `json:"status"`
	}
	if json.Unmarshal(data, &run) != nil {
		return "", ""
	}
	values := map[string]string{}
	for _, output := range append(run.Status.Results, run.Status.PipelineResults...) {
		if output.Name != "IMAGE_URL" && output.Name != "IMAGE_DIGEST" {
			continue
		}
		var value string
		if _, duplicate := values[output.Name]; duplicate || json.Unmarshal(output.Value, &value) != nil {
			return "", ""
		}
		values[output.Name] = value
	}
	return values["IMAGE_URL"], values["IMAGE_DIGEST"]
}
