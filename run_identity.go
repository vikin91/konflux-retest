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

	"github.com/distribution/reference"
	"github.com/vikin91/konflux-retest/internal/refs"
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
		return runObservation{}, errors.New("invalid PipelineRun URL")
	}
	switch {
	case u.Scheme != "https":
		return runObservation{}, errors.New("unsupported PipelineRun URL scheme, expected HTTPS")
	case u.User != nil:
		return runObservation{}, errors.New("unsupported PipelineRun URL: embedded credentials are not allowed")
	case u.Port() != "":
		return runObservation{}, errors.New("unsupported PipelineRun URL with an explicit port")
	case !strings.HasPrefix(u.Host, "konflux-ui.apps.") || !strings.HasSuffix(u.Host, ".openshiftapps.com"):
		return runObservation{}, errors.New("unsupported PipelineRun URL host, expected konflux-ui.apps.<cluster>.openshiftapps.com")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "ns" || parts[2] != "pipelinerun" {
		return runObservation{}, errors.New("unsupported PipelineRun path; expected /ns/<namespace>/pipelinerun/<name>")
	}
	if !konfluxName.MatchString(parts[1]) {
		return runObservation{}, errors.New("unsupported PipelineRun namespace, expected a lowercase Kubernetes resource name")
	}
	if !konfluxName.MatchString(parts[3]) {
		return runObservation{}, errors.New("unsupported PipelineRun name, expected a lowercase Kubernetes resource name")
	}
	category = "authentication failures"
	cluster := strings.TrimPrefix(u.Host, "konflux-ui.apps.")
	token, err := c.tokenForRun(ctx, check.App.ID, "api."+cluster, parts[1])
	if err != nil {
		return runObservation{}, err
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
			return runObservation{}, fmt.Errorf("API: %w; could not decode PipelineRun archive response", apiErr)
		}
		if len(response.Records) != 1 || response.Next != "" {
			return runObservation{}, fmt.Errorf("ambiguous archived PipelineRun identity: records=%d, more_pages=%t; API: %w", len(response.Records), response.Next != "", apiErr)
		}
		data = response.Records[0].Data.Value
	}
	category = "identity validation failures"
	matches, err := matchRunIdentity(data, parts[1], parts[3], check.ID, repo, sha, tag, pr)
	if err != nil {
		return runObservation{}, err
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
		return false, errors.New("could not decode PipelineRun identity")
	}
	if run.Metadata.Name != name || run.Metadata.Namespace != namespace {
		return false, errors.New("PipelineRun identity mismatch for namespace or name")
	}
	a := run.Metadata.Annotations
	const prefix = "pipelinesascode.tekton.dev/"
	if a[prefix+"check-run-id"] != strconv.FormatInt(id, 10) {
		return false, errors.New("PipelineRun check-run-id annotation does not match the check")
	}
	if a[prefix+"sha"] != sha {
		return false, errors.New("PipelineRun SHA annotation does not match the commit")
	}
	if strings.TrimSuffix(a[prefix+"repo-url"], ".git") != "https://github.com/"+repo {
		return false, errors.New("PipelineRun repository annotation does not match the repository")
	}
	event, branch := a[prefix+"event-type"], a[prefix+"branch"]
	if event == "" || branch == "" {
		return false, errors.New("PipelineRun event identity unavailable: missing event type or branch")
	}
	if tag != "" {
		return (event == "push" || event == "retest-comment") && branch == "refs/tags/"+tag, nil
	}
	switch event {
	case "on-comment":
		sourceBranch := a[prefix+"source-branch"]
		if strings.HasPrefix(branch, "refs/tags/") || strings.HasPrefix(sourceBranch, "refs/tags/") {
			return false, nil
		}
		if sourceBranch == "" {
			return false, errors.New("PipelineRun PR comment identity unavailable: missing source branch")
		}
		// GitHub commit comments use the same source and target branch and repository,
		// even when Pipelines-as-Code attaches an associated PR number.
		if strings.TrimPrefix(sourceBranch, "refs/heads/") == strings.TrimPrefix(branch, "refs/heads/") {
			sourceRepo, isGitHub := strings.CutPrefix(strings.TrimSuffix(a[prefix+"source-repo-url"], ".git"), "https://github.com/")
			if !isGitHub || !refs.ValidRepository(sourceRepo) {
				return false, errors.New("PipelineRun PR comment identity unavailable: expected a GitHub source repository")
			}
			if strings.EqualFold(sourceRepo, repo) {
				return false, nil
			}
		}
	case "pull_request":
	default:
		return false, nil
	}
	number, err := strconv.Atoi(a[prefix+"pull-request"])
	if err != nil || number <= 0 || a[prefix+"pull-request"] != strconv.Itoa(number) {
		return false, errors.New("PipelineRun PR event identity unavailable: expected a positive pull-request number")
	}
	return number == pr, nil
}

// pipelineImageResults admits validated image outputs independently of retry eligibility.
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
	image, digest := values["IMAGE_URL"], values["IMAGE_DIGEST"]
	if _, err := reference.Parse(image); err != nil {
		image = ""
	}
	if !digestPattern.MatchString(digest) {
		digest = ""
	}
	return image, digest
}
