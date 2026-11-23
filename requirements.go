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
	retagRecipe struct {
		versionFile string
		pattern     *regexp.Regexp
	}
)

var (
	// ErrUnsupportedRetagContract indicates that the source does not describe an input the planner can verify.
	ErrUnsupportedRetagContract = errors.New("unsupported retag input contract")
	retagRecipes                = map[string]retagRecipe{
		"scanner-tag":   newRetagRecipe("scanner-tag", "SCANNER_VERSION"),
		"collector-tag": newRetagRecipe("collector-tag", "COLLECTOR_VERSION"),
		"fact-tag":      newRetagRecipe("fact-tag", "FACT_VERSION"),
	}
)

// newRetagRecipe matches a literal version-file recipe so source Makefiles need not be executed.
func newRetagRecipe(target, versionFile string) retagRecipe {
	return retagRecipe{
		versionFile: versionFile,
		pattern: regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `:[ \t]*\n\t@echo "\$\$\(cat ` +
			regexp.QuoteMeta(versionFile) + `\)"[ \t]*\n(?:\n|\z)`),
	}
}

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
			return document, fmt.Errorf("%w: could not parse %s", ErrUnsupportedRetagContract, path)
		}
		return document, nil
	}
	run, err := readYAML(".tekton/" + job + ".yaml")
	if err != nil {
		return result, err
	}
	if run.Kind != "PipelineRun" || run.Spec.PipelineRef.Name != "retag-pipeline" || run.Spec.PipelineRef.Resolver != "" || run.Spec.PipelineSpec != nil || run.Metadata.Annotations["pipelinesascode.tekton.dev/pipeline"] != "" {
		return result, fmt.Errorf("%w: unsupported retag PipelineRun definition", ErrUnsupportedRetagContract)
	}
	pipeline, err := readYAML(".tekton/retag-pipeline.yaml")
	if err != nil {
		return result, err
	}
	if pipeline.Kind != "Pipeline" {
		return result, fmt.Errorf("%w: expected retag Pipeline definition", ErrUnsupportedRetagContract)
	}
	params := make(map[string]string)
	for _, param := range pipeline.Spec.Params {
		if param.Default != nil {
			params[param.Name] = *param.Default
		}
	}
	overrides, err := paramValues(run.Spec.Params)
	if err != nil {
		return result, fmt.Errorf("%w: invalid parameters in .tekton/%s.yaml: %v", ErrUnsupportedRetagContract, job, err)
	}
	for name, value := range overrides {
		params[name] = value
	}
	if params["revision"] != "{{revision}}" || (params["git-url"] != "{{source_url}}" && params["git-url"] != "{{repo_url}}") {
		return result, fmt.Errorf("%w: unsupported retag source repository or revision binding", ErrUnsupportedRetagContract)
	}
	if err := validateRetagTasks(pipeline.Spec.Tasks); err != nil {
		return result, fmt.Errorf("%w: invalid .tekton/retag-pipeline.yaml: %v", ErrUnsupportedRetagContract, err)
	}
	target := params["input-image-tag-makefile-target"]
	recipe, ok := retagRecipes[target]
	if !ok {
		return result, fmt.Errorf("%w: unsupported input image Makefile target", ErrUnsupportedRetagContract)
	}
	makefile, err := readFile("Makefile")
	if err != nil {
		return result, err
	}
	if len(recipe.pattern.FindAll(makefile, -1)) != 1 {
		return result, fmt.Errorf("%w: unsupported Makefile recipe for %s; cannot resolve the exact input tag", ErrUnsupportedRetagContract, target)
	}
	version, err := readFile(recipe.versionFile)
	if err != nil {
		return result, err
	}
	suffix, ok := params["image-tag-suffix"]
	if !ok {
		return result, fmt.Errorf("%w: missing image-tag-suffix parameter", ErrUnsupportedRetagContract)
	}
	result = imageRequirement{
		repository: params["input-image-repo"], tag: strings.TrimRight(string(version), "\n") + suffix,
		sourceURL: sourceURL, revision: revision,
	}
	if !imageRepositoryPattern.MatchString(result.repository) || strings.Contains(result.repository, "..") || !imageTagPattern.MatchString(result.tag) {
		return imageRequirement{}, fmt.Errorf("%w: unsupported input image repository or tag; expected a literal Quay image reference", ErrUnsupportedRetagContract)
	}
	return result, nil
}

func paramValues(params []tektonParam) (map[string]string, error) {
	values := make(map[string]string)
	for _, param := range params {
		if _, exists := values[param.Name]; exists || param.Value == nil {
			return nil, errors.New("duplicate or nonliteral Tekton parameter")
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
		knownBundle := false
		switch digest {
		case "sha256:cc0e455bf2f74a2f5b3dac0a08a6db9dd1586848b6d949c350164aaf6821faa3",
			"sha256:ad73f11475690a5d16d31ab4da48bb514da97954bdc2681439d76193a927e41c",
			"sha256:48cf64993928085331f0bd5171bc87ec2fd488cfbf4ca2b66627ea6040e011ef",
			"sha256:6d1341c9d70c94f484668b9af3f618654d4743ce151a4b529ad19572960f8b82",
			"sha256:53208915a4703f2fb6b0bee29baf993698537ec42c35aa74057b91d1636faa92":
			knownBundle = true
		}
		if task.TaskSpec != nil || task.TaskRef.Resolver != "bundles" || ref["name"] != contract.task || ref["kind"] != "task" || !pinned || !knownBundle {
			return fmt.Errorf("unsupported pinned task implementation for %s; update the consumer contract before suggesting a retest", task.Name)
		}
	}
	if len(seen) != len(expected) {
		return errors.New("retag pipeline lacks the supported input-tag or wait-for-image task")
	}
	return nil
}
