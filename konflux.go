package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type (
	konfluxClient struct {
		http  *http.Client
		token func(context.Context, string) (string, error)
	}
	konfluxTask struct {
		Metadata struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Params []tektonParam `json:"params"`
		} `json:"spec"`
	}
	logEvidence struct {
		reason   string
		accepted bool
	}
)

var konfluxName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// evidence reads only the latest check's archived wait task, bound to its image,
// check-run ID and head SHA. A generic timeout is not missing-image evidence.
func (c *konfluxClient) evidence(ctx context.Context, check checkRun, requirement imageRequirement) (logEvidence, error) {
	if c == nil {
		return logEvidence{}, errors.New("Konflux log access is unavailable")
	}
	u, err := url.Parse(check.DetailsURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || !strings.HasPrefix(u.Host, "konflux-ui.apps.") || !strings.HasSuffix(u.Host, ".openshiftapps.com") {
		return logEvidence{}, errors.New("check has no supported Konflux log URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "ns" || parts[2] != "pipelinerun" || !konfluxName.MatchString(parts[1]) || !konfluxName.MatchString(parts[3]) {
		return logEvidence{}, errors.New("check has no supported PipelineRun log URL")
	}
	cluster := strings.TrimPrefix(u.Host, "konflux-ui.apps.")
	token, err := c.token(ctx, "api."+cluster)
	if err != nil {
		return logEvidence{}, err
	}
	endpoint := "https://tekton-results-tekton-results.apps." + cluster + "/apis/results.tekton.dev/v1alpha2/parents/"
	filter := `data_type in ["tekton.dev/v1.TaskRun", "tekton.dev/v1beta1.TaskRun"] && data.metadata.labels["tekton.dev/pipelineRun"] == ` + strconv.Quote(parts[3]) + ` && data.metadata.labels["tekton.dev/pipelineTask"] == "wait-for-image"`
	if !check.StartedAt.IsZero() {
		filter += ` && data.metadata.creationTimestamp > ` + strconv.Quote(check.StartedAt.Add(-time.Minute).UTC().Format(time.RFC3339))
	}
	query := url.Values{"filter": {filter}, "page_size": {"5"}}
	data, err := c.get(ctx, endpoint+parts[1]+"/results/-/records?"+query.Encode(), token)
	if err != nil {
		return logEvidence{}, err
	}
	var response struct {
		Records []struct {
			Name string `json:"name"`
			Data struct {
				Value []byte `json:"value"`
			} `json:"data"`
		} `json:"records"`
		NextPageToken string `json:"next_page_token"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return logEvidence{}, errors.New("invalid Konflux archive response")
	}
	if len(response.Records) != 1 || response.NextPageToken != "" {
		return logEvidence{}, errors.New("latest wait-for-image log is not available unambiguously in the archive")
	}
	record := response.Records[0]
	var task konfluxTask
	if err := json.Unmarshal(record.Data.Value, &task); err != nil {
		return logEvidence{}, errors.New("invalid archived Konflux task")
	}
	labels := task.Metadata.Labels
	if task.Metadata.Namespace != parts[1] || labels["tekton.dev/pipelineRun"] != parts[3] || labels["tekton.dev/pipelineTask"] != "wait-for-image" || labels["pipelinesascode.tekton.dev/sha"] != requirement.revision || labels["pipelinesascode.tekton.dev/check-run-id"] != strconv.FormatInt(check.ID, 10) {
		return logEvidence{}, errors.New("archived task does not match the latest check and PR head")
	}
	params, err := paramValues(task.Spec.Params)
	if err != nil || params["IMAGE"] != requirement.image() {
		return logEvidence{}, errors.New("archived task does not match the required image")
	}
	recordParts := strings.Split(record.Name, "/")
	if len(recordParts) != 5 || recordParts[0] != parts[1] || recordParts[1] != "results" || recordParts[3] != "records" || !konfluxName.MatchString(recordParts[2]) || !konfluxName.MatchString(recordParts[4]) {
		return logEvidence{}, errors.New("invalid Konflux archive record path")
	}
	logs, err := c.get(ctx, endpoint+strings.Replace(record.Name, "/records/", "/logs/", 1), token)
	if err != nil {
		return logEvidence{}, err
	}
	return interpretImageLog(string(logs), requirement.image()), nil
}

func (c *konfluxClient) get(ctx context.Context, address, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("invalid Konflux archive request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("Konflux log network error or timeout")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Konflux logs returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, registryResponseLimit+1))
	if err != nil || len(data) > registryResponseLimit {
		return nil, errors.New("Konflux log response is incomplete or too large")
	}
	return data, nil
}

// interpretImageLog retains the last observed wait state. The task's not-found
// message can mask registry errors, so it is evidence of a blocked consumer,
// not proof that the image is currently absent from the registry.
func interpretImageLog(log, image string) logEvidence {
	var evidence logEvidence
	for line := range strings.SplitSeq(log, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "INFO: Image not found yet: "+image+".":
			evidence = logEvidence{reason: "Konflux reported image not found"}
		case line == "Image "+image+" found.":
			evidence = logEvidence{}
		case strings.HasPrefix(line, "Attestation for "+image+"@sha256:") && strings.Contains(line, "not (yet) found"):
			evidence = logEvidence{reason: "Konflux was waiting for the build attestation"}
		case strings.HasPrefix(line, "WARNING: The found image "+image+"@sha256:") && strings.Contains(line, "built for a different commit"):
			evidence = logEvidence{reason: "Konflux reported a source revision mismatch"}
		case strings.HasPrefix(line, "ERROR: At least one task has failed during build of "+image+"@sha256:"):
			evidence = logEvidence{reason: "Konflux reported an unsuccessful attested build"}
		case line == "Accepting this image.":
			evidence = logEvidence{accepted: true}
		}
	}
	return evidence
}

// konfluxTokens reuses a matching oc context without changing the current context.
// Credentials stay in memory and are never included in errors or subprocess args.
func konfluxTokens() func(context.Context, string) (string, error) {
	tokens := make(map[string]string)
	return func(ctx context.Context, host string) (string, error) {
		if token := tokens[host]; token != "" {
			return token, nil
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		data, err := exec.CommandContext(ctx, "oc", "config", "view", "-o", "json").Output()
		if err != nil {
			return "", errors.New("Konflux logs require oc and a matching saved login")
		}
		var config struct {
			CurrentContext string `json:"current-context"`
			Clusters       []struct {
				Name    string
				Cluster struct{ Server string }
			}
			Contexts []struct {
				Name    string
				Context struct{ Cluster string }
			}
		}
		if json.Unmarshal(data, &config) != nil {
			return "", errors.New("cannot read Konflux oc contexts")
		}
		clusters := make(map[string]bool)
		for _, cluster := range config.Clusters {
			server, err := url.Parse(cluster.Cluster.Server)
			if err == nil && server.Scheme == "https" && server.Hostname() == host {
				clusters[cluster.Name] = true
			}
		}
		var contextName string
		for _, candidate := range config.Contexts {
			if clusters[candidate.Context.Cluster] {
				if contextName == "" || candidate.Name == config.CurrentContext {
					contextName = candidate.Name
				}
			}
		}
		if contextName == "" {
			return "", errors.New("no saved oc context matches the Konflux cluster")
		}
		data, err = exec.CommandContext(ctx, "oc", "--context", contextName, "whoami", "-t").Output()
		if err != nil || strings.TrimSpace(string(data)) == "" {
			return "", errors.New("cannot read the saved Konflux login")
		}
		token := strings.TrimSpace(string(data))
		tokens[host] = token
		return token, nil
	}
}
