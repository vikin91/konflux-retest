package konfluxretest

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

type runObservation struct {
	matches               bool
	imageURL, imageDigest string
}

// runIdentity verifies original PipelineRun annotations, never lossy Kubernetes labels.
// Annotation keys follow Pipelines-as-Code pkg/kubeinteraction/labels.go.
func (c *konfluxClient) runIdentity(ctx context.Context, check checkRun, repo, sha, tag string, pr int) (runObservation, error) {
	u, err := url.Parse(check.DetailsURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || !strings.HasPrefix(u.Host, "konflux-ui.apps.") || !strings.HasSuffix(u.Host, ".openshiftapps.com") {
		return runObservation{}, errors.New("unsupported PipelineRun URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "ns" || parts[2] != "pipelinerun" || !konfluxName.MatchString(parts[1]) || !konfluxName.MatchString(parts[3]) {
		return runObservation{}, errors.New("unsupported PipelineRun path")
	}
	cluster := strings.TrimPrefix(u.Host, "konflux-ui.apps.")
	token, err := c.token(ctx, "api."+cluster)
	if err != nil || token == "" {
		return runObservation{}, errors.New("PipelineRun identity authentication unavailable")
	}
	data, err := c.get(ctx, "https://api."+cluster+"/apis/tekton.dev/v1/namespaces/"+parts[1]+"/pipelineruns/"+parts[3], token)
	if err != nil {
		filter := `data_type in ["tekton.dev/v1.PipelineRun", "tekton.dev/v1beta1.PipelineRun"] && data.metadata.name == ` + strconv.Quote(parts[3])
		address := "https://tekton-results-tekton-results.apps." + cluster + "/apis/results.tekton.dev/v1alpha2/parents/" + parts[1] + "/results/-/records?" + url.Values{"filter": {filter}, "page_size": {"2"}}.Encode()
		data, err = c.get(ctx, address, token)
		if err != nil {
			return runObservation{}, errors.New("PipelineRun identity unavailable in API and archive")
		}
		var response struct {
			Records []struct {
				Data struct {
					Value []byte `json:"value"`
				} `json:"data"`
			} `json:"records"`
			Next string `json:"next_page_token"`
		}
		if json.Unmarshal(data, &response) != nil || len(response.Records) != 1 || response.Next != "" {
			return runObservation{}, errors.New("ambiguous archived PipelineRun identity")
		}
		data = response.Records[0].Data.Value
	}
	matches, err := matchRunIdentity(data, parts[1], parts[3], check.ID, repo, sha, tag, pr)
	if err != nil || !matches {
		return runObservation{}, err
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
	if json.Unmarshal(data, &run) != nil || run.Metadata.Name != name || run.Metadata.Namespace != namespace {
		return false, errors.New("PipelineRun identity mismatch")
	}
	a := run.Metadata.Annotations
	const prefix = "pipelinesascode.tekton.dev/"
	if a[prefix+"check-run-id"] != strconv.FormatInt(id, 10) || a[prefix+"sha"] != sha || strings.TrimSuffix(a[prefix+"repo-url"], ".git") != "https://github.com/"+repo {
		return false, errors.New("PipelineRun lacks matching check, repository or SHA evidence")
	}
	if a[prefix+"event-type"] == "" || a[prefix+"branch"] == "" {
		return false, errors.New("PipelineRun event identity unavailable")
	}
	if tag != "" {
		return a[prefix+"event-type"] == "push" && a[prefix+"branch"] == "refs/tags/"+tag, nil
	}
	return a[prefix+"event-type"] == "pull_request" && a[prefix+"pull-request"] == strconv.Itoa(pr), nil
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
