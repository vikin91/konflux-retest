package konfluxretest

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

type (
	tektonParam struct {
		Name    string  `yaml:"name"`
		Value   *string `yaml:"value"`
		Default *string `yaml:"default"`
	}
	tektonRef struct {
		Name     string        `yaml:"name"`
		Resolver string        `yaml:"resolver"`
		Params   []tektonParam `yaml:"params"`
	}
	tektonTask struct {
		Name     string        `yaml:"name"`
		Params   []tektonParam `yaml:"params"`
		TaskRef  tektonRef     `yaml:"taskRef"`
		TaskSpec *yaml.Node    `yaml:"taskSpec"`
	}
	tektonDocument struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
		Spec struct {
			Params       []tektonParam `yaml:"params"`
			PipelineRef  tektonRef     `yaml:"pipelineRef"`
			PipelineSpec *yaml.Node    `yaml:"pipelineSpec"`
			Tasks        []tektonTask  `yaml:"tasks"`
		} `yaml:"spec"`
	}
)

// resolveImage interprets the supported retag contract without executing PR code.
// Unknown task versions or parameter wiring fail closed instead of guessing.
func resolveImage(readFile func(string) ([]byte, error), job, sourceURL, revision string) (imageRequirement, error) {
	var result imageRequirement
	readYAML := func(path string) (tektonDocument, error) {
		var document tektonDocument
		data, err := readFile(path)
		if err != nil {
			return document, err
		}
		if err := yaml.Unmarshal(data, &document); err != nil {
			return document, fmt.Errorf("decode %s: %w", path, err)
		}
		return document, nil
	}
	run, err := readYAML(".tekton/" + job + ".yaml")
	if err != nil {
		return result, err
	}
	if run.Kind != "PipelineRun" || run.Spec.PipelineRef.Name != "retag-pipeline" || run.Spec.PipelineRef.Resolver != "" || run.Spec.PipelineSpec != nil || run.Metadata.Annotations["pipelinesascode.tekton.dev/pipeline"] != "" {
		return result, errors.New("unsupported retag PipelineRun definition")
	}
	pipeline, err := readYAML(".tekton/retag-pipeline.yaml")
	if err != nil {
		return result, err
	}
	if pipeline.Kind != "Pipeline" {
		return result, errors.New("expected retag Pipeline definition")
	}
	params := make(map[string]string)
	for _, param := range pipeline.Spec.Params {
		if param.Default != nil {
			params[param.Name] = *param.Default
		}
	}
	overrides, err := paramValues(run.Spec.Params)
	if err != nil {
		return result, err
	}
	for name, value := range overrides {
		params[name] = value
	}
	if params["revision"] != "{{revision}}" || (params["git-url"] != "{{source_url}}" && params["git-url"] != "{{repo_url}}") {
		return result, errors.New("unsupported retag source repository or revision binding")
	}
	if err := validateRetagTasks(pipeline.Spec.Tasks); err != nil {
		return result, err
	}
	versionFiles := map[string]string{"scanner-tag": "SCANNER_VERSION", "collector-tag": "COLLECTOR_VERSION", "fact-tag": "FACT_VERSION"}
	target := params["input-image-tag-makefile-target"]
	versionFile, ok := versionFiles[target]
	if !ok {
		return result, fmt.Errorf("unsupported input image Makefile target %q", target)
	}
	makefile, err := readFile("Makefile")
	if err != nil {
		return result, err
	}
	// Restrict interpretation to the literal version-file recipe used by StackRox.
	// Running make here would execute arbitrary code from the PR.
	recipe := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `:[ \t]*\n\t@echo "\$\$\(cat ` + versionFile + `\)"[ \t]*\n(?:\n|\z)`)
	if len(recipe.FindAll(makefile, -1)) != 1 {
		return result, fmt.Errorf("unsupported Makefile recipe for %s; cannot resolve the exact input tag", target)
	}
	version, err := readFile(versionFile)
	if err != nil {
		return result, err
	}
	suffix, ok := params["image-tag-suffix"]
	if !ok {
		return result, errors.New("missing image-tag-suffix parameter")
	}
	result = imageRequirement{
		repository: params["input-image-repo"], tag: strings.TrimRight(string(version), "\n") + suffix,
		sourceURL: sourceURL, revision: revision,
	}
	if !imageRepositoryPattern.MatchString(result.repository) || strings.Contains(result.repository, "..") || !imageTagPattern.MatchString(result.tag) {
		return imageRequirement{}, errors.New("unsupported input image repository or tag; expected a literal Quay image reference")
	}
	return result, nil
}

func paramValues(params []tektonParam) (map[string]string, error) {
	values := make(map[string]string)
	for _, param := range params {
		if _, exists := values[param.Name]; exists || param.Value == nil {
			return nil, fmt.Errorf("duplicate or nonliteral Tekton parameter %q", param.Name)
		}
		values[param.Name] = *param.Value
	}
	return values, nil
}

func validateRetagTasks(tasks []tektonTask) error {
	expected := map[string]struct {
		task   string
		params map[string]string
	}{
		"determine-input-image-tag": {"determine-dependency-image-tag", map[string]string{
			"MAKEFILE_TARGET": "$(params.input-image-tag-makefile-target)",
			"TAG_SUFFIX":      "$(params.image-tag-suffix)",
			"SOURCE_ARTIFACT": "$(tasks.clone-repository.results.SOURCE_ARTIFACT)",
		}},
		"wait-for-image": {"wait-for-image", map[string]string{
			"IMAGE": "$(params.input-image-repo):$(tasks.determine-input-image-tag.results.IMAGE_TAG)",
		}},
	}
	seen := make(map[string]bool)
	for _, task := range tasks {
		contract, relevant := expected[task.Name]
		if !relevant {
			continue
		}
		if seen[task.Name] {
			return fmt.Errorf("duplicate task %q", task.Name)
		}
		seen[task.Name] = true
		params, err := paramValues(task.Params)
		if err != nil {
			return err
		}
		if len(params) != len(contract.params) {
			return fmt.Errorf("unsupported parameters for %s", task.Name)
		}
		for name, value := range contract.params {
			if params[name] != value {
				return fmt.Errorf("unsupported %s parameter binding for %s", name, task.Name)
			}
		}
		ref, err := paramValues(task.TaskRef.Params)
		if err != nil {
			return err
		}
		_, digest, pinned := strings.Cut(ref["bundle"], "@")
		// These bundles have identical tag-generation and readiness scripts.
		// A new implementation needs inspection before its inputs can be approved.
		knownBundle := digest == "sha256:cc0e455bf2f74a2f5b3dac0a08a6db9dd1586848b6d949c350164aaf6821faa3" || digest == "sha256:ad73f11475690a5d16d31ab4da48bb514da97954bdc2681439d76193a927e41c"
		if task.TaskSpec != nil || task.TaskRef.Resolver != "bundles" || ref["name"] != contract.task || ref["kind"] != "task" || !pinned || !knownBundle {
			return fmt.Errorf("unsupported pinned task implementation for %s; update the consumer contract before suggesting a retest", task.Name)
		}
	}
	if len(seen) != len(expected) {
		return errors.New("retag pipeline lacks the supported input-tag or wait-for-image task")
	}
	return nil
}
