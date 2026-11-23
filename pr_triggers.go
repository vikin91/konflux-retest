package konfluxretest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// missingPRTriggers verifies commands from the head repository without running source code.
func (c githubClient) missingPRTriggers(ctx context.Context, sourceRepo, sha string, jobs []string) (verified, warnings []string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, job := range jobs {
		if ctx.Err() != nil {
			warnings = append(warnings, "could not finish verifying missing PR build triggers: "+ctx.Err().Error())
			break
		}
		// shortcut: check the supported repositories' filename conventions; discover files if those conventions diverge.
		filename := job + ".yaml"
		if component, build := strings.CutSuffix(job, "-on-push"); build {
			filename = component + "-build.yaml"
		}
		path := ".tekton/" + filename
		data, err := c.file(ctx, sourceRepo, sha, path)
		if err != nil {
			if !errors.Is(err, githubStatusError(http.StatusNotFound)) {
				warnings = append(warnings, fmt.Sprintf("could not verify comment trigger for %s: %v", job, err))
			}
			continue
		}
		matches, err := matchesPRTrigger(data, job)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("could not verify comment trigger for %s in %s: %v", job, path, err))
		} else if matches {
			verified = append(verified, job)
		}
	}
	return verified, warnings
}

// matchesPRTrigger accepts comment triggers without unverified path or label filters.
func matchesPRTrigger(data []byte, job string) (bool, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	matched := false
	for {
		var document struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
			Metadata   struct {
				Name        string            `yaml:"name"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
		}
		if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
			return matched, nil
		} else if err != nil {
			return false, errors.New("invalid PipelineRun YAML")
		}
		if document.Kind != "PipelineRun" || document.Metadata.Name != job ||
			(document.APIVersion != "tekton.dev/v1" && document.APIVersion != "tekton.dev/v1beta1") {
			continue
		}
		annotations := document.Metadata.Annotations
		pattern := annotations[pacAnnotationPrefix+"on-comment"]
		labels := strings.TrimSpace(annotations[pacAnnotationPrefix+"on-label"])
		if pattern == "" || (labels != "" && labels != "[]") ||
			annotations[pacAnnotationPrefix+"on-path-changed"] != "" || annotations[pacAnnotationPrefix+"on-path-changed-ignore"] != "" {
			continue
		}
		trigger, err := regexp.Compile(pattern)
		if err != nil {
			return false, errors.New("invalid on-comment pattern")
		}
		matched = matched || trigger.MatchString("/konflux-retest "+job)
	}
}
