package konfluxretest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const registryResponseLimit = 16 << 20

var (
	errRegistryMissing     = errors.New("registry artifact not found")
	imageRepositoryPattern = regexp.MustCompile(`^quay\.io/[a-z0-9_-]+/[a-z0-9._/-]+$`)
	imageTagPattern        = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)
	digestPattern          = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	bearerParameter        = regexp.MustCompile(`([a-z]+)="([^"\r\n]*)"`)
)

type (
	imageRequirement struct {
		repository string
		tag        string
		sourceURL  string
		revision   string
	}
	registryClient struct {
		baseURL  string
		http     *http.Client
		username string
		password string
	}
	registryDescriptor struct {
		Digest   string `json:"digest"`
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	}
	registryManifest struct {
		SchemaVersion int                  `json:"schemaVersion"`
		Config        registryDescriptor   `json:"config"`
		Layers        []registryDescriptor `json:"layers"`
		Manifests     []registryDescriptor `json:"manifests"`
	}
)

func (r imageRequirement) image() string { return r.repository + ":" + r.tag }

// readiness mirrors the pinned wait-for-image task's label and provenance checks.
// It inspects metadata only; neither image layers nor signatures are verified.
func (c registryClient) readiness(ctx context.Context, r imageRequirement) (string, error) {
	if !imageRepositoryPattern.MatchString(r.repository) || strings.Contains(r.repository, "..") || !imageTagPattern.MatchString(r.tag) {
		return "", errors.New("unsupported image reference; expected a Quay repository and a literal tag")
	}
	repo := strings.TrimPrefix(r.repository, "quay.io/")
	manifest, digest, err := c.manifest(ctx, repo, r.tag)
	if errors.Is(err, errRegistryMissing) {
		return "image is missing", nil
	}
	if err != nil {
		return "", err
	}
	if len(manifest.Manifests) > 0 {
		// Konflux's wait task runs on Linux/amd64, regardless of this CLI's host.
		var child string
		for _, descriptor := range manifest.Manifests {
			if descriptor.Platform.OS == "linux" && descriptor.Platform.Architecture == "amd64" {
				child = descriptor.Digest
				break
			}
		}
		if child == "" {
			return "image has no linux/amd64 manifest", nil
		}
		manifest, _, err = c.manifest(ctx, repo, child)
		if err != nil {
			return "", fmt.Errorf("read platform manifest: %w", err)
		}
	}
	config, err := c.blob(ctx, repo, manifest.Config.Digest)
	if err != nil {
		return "", fmt.Errorf("read image configuration: %w", err)
	}
	var image struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(config, &image); err != nil {
		return "", fmt.Errorf("decode image configuration: %w", err)
	}
	labels := image.Config.Labels
	if len(strings.Fields(labels["vcs-ref"])) != 1 || len(strings.Fields(labels["source-location"])) != 1 {
		return "image lacks required vcs-ref or source-location labels", nil
	}
	if labels["source-location"] == r.sourceURL && labels["vcs-ref"] != r.revision {
		return fmt.Sprintf("image source revision %q does not match PR head %q", labels["vcs-ref"], r.revision), nil
	}
	attestationTag := strings.Replace(digest, ":", "-", 1) + ".att"
	attestation, _, err := c.manifest(ctx, repo, attestationTag)
	if errors.Is(err, errRegistryMissing) {
		return "build attestation is missing for " + digest, nil
	}
	if err != nil {
		return "", fmt.Errorf("read attestation: %w", err)
	}
	// The consumer compares a single predicateType string. Multiple downloaded
	// statements produce a multi-line value and do not satisfy that comparison.
	if len(attestation.Layers) != 1 {
		return "expected one build attestation", nil
	}
	envelope, err := c.blob(ctx, repo, attestation.Layers[0].Digest)
	if err != nil {
		return "", fmt.Errorf("read attestation payload: %w", err)
	}
	return attestationReadiness(envelope), nil
}

func attestationReadiness(data []byte) string {
	var envelope struct {
		Payload []byte `json:"payload"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || len(envelope.Payload) == 0 {
		return "build attestation has an invalid envelope"
	}
	var statement struct {
		PredicateType string `json:"predicateType"`
		Predicate     struct {
			BuildConfig struct {
				Tasks []struct {
					Status string `json:"status"`
				} `json:"tasks"`
			} `json:"buildConfig"`
			BuildDefinition struct {
				BuildType            string `json:"buildType"`
				ResolvedDependencies []struct {
					Name    string `json:"name"`
					Content string `json:"content"`
				} `json:"resolvedDependencies"`
			} `json:"buildDefinition"`
		} `json:"predicate"`
	}
	if err := json.Unmarshal(envelope.Payload, &statement); err != nil {
		return "build attestation has an invalid statement"
	}
	switch statement.PredicateType {
	case "https://slsa.dev/provenance/v0.2":
		tasks := statement.Predicate.BuildConfig.Tasks
		if tasks == nil {
			return "build attestation has no task results"
		}
		for _, task := range tasks {
			if task.Status != "Succeeded" {
				return "build attestation contains an unsuccessful task"
			}
		}
	case "https://slsa.dev/provenance/v1":
		definition := statement.Predicate.BuildDefinition
		if definition.BuildType != "https://tekton.dev/chains/v2/slsa-tekton" {
			return "build attestation has an unsupported build type"
		}
		if definition.ResolvedDependencies == nil {
			return "build attestation has no resolved dependencies"
		}
		for _, dependency := range definition.ResolvedDependencies {
			if dependency.Name != "pipelineTask" {
				continue
			}
			content, err := base64.StdEncoding.DecodeString(dependency.Content)
			if err != nil {
				return "build attestation has an invalid task result"
			}
			var task struct {
				Status struct {
					Conditions []struct{ Type, Status string } `json:"conditions"`
				} `json:"status"`
			}
			if err := json.Unmarshal(content, &task); err != nil {
				return "build attestation has an invalid task result"
			}
			succeeded := false
			for _, condition := range task.Status.Conditions {
				if condition.Type == "Succeeded" && condition.Status == "True" {
					succeeded = true
				}
			}
			if !succeeded {
				return "build attestation contains an unsuccessful task"
			}
		}
	default:
		return "build attestation has an unsupported predicate type"
	}
	return ""
}

func (c registryClient) manifest(ctx context.Context, repo, ref string) (registryManifest, string, error) {
	var manifest registryManifest
	if !imageTagPattern.MatchString(ref) && !digestPattern.MatchString(ref) {
		return manifest, "", errors.New("invalid manifest reference")
	}
	data, err := c.get(ctx, "/v2/"+repo+"/manifests/"+ref)
	if err != nil {
		return manifest, "", err
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	if strings.HasPrefix(ref, "sha256:") && digest != ref {
		return manifest, "", errors.New("manifest digest mismatch")
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, "", fmt.Errorf("decode registry manifest: %w", err)
	}
	if manifest.SchemaVersion != 2 {
		return manifest, "", errors.New("unsupported registry manifest schema")
	}
	return manifest, digest, nil
}

func (c registryClient) blob(ctx context.Context, repo, digest string) ([]byte, error) {
	if !digestPattern.MatchString(digest) {
		return nil, errors.New("invalid blob digest")
	}
	data, err := c.get(ctx, "/v2/"+repo+"/blobs/"+digest)
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != digest {
		return nil, errors.New("blob digest mismatch")
	}
	return data, nil
}

// get uses registry authentication, never the GitHub credential.
// Only explicit registry not-found codes count as missing artifacts.
func (c registryClient) get(ctx context.Context, path string) ([]byte, error) {
	resp, err := c.request(ctx, c.baseURL+path, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		token, err := c.registryToken(ctx, challenge)
		if err != nil {
			return nil, err
		}
		resp, err = c.request(ctx, c.baseURL+path, "Bearer "+token)
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("registry authentication/access denied (HTTP %d); image availability is unknown", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, registryResponseLimit+1))
	if err != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("network error reading registry response")
	}
	if len(data) > registryResponseLimit {
		return nil, errors.New("registry metadata exceeds size limit")
	}
	if resp.StatusCode == http.StatusNotFound {
		var result struct {
			Errors []struct {
				Code string `json:"code"`
			} `json:"errors"`
		}
		if json.Unmarshal(data, &result) == nil {
			for _, detail := range result.Errors {
				if detail.Code == "MANIFEST_UNKNOWN" || detail.Code == "NAME_UNKNOWN" || detail.Code == "BLOB_UNKNOWN" {
					return nil, errRegistryMissing
				}
			}
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned HTTP %d; image availability is unknown", resp.StatusCode)
	}
	return data, nil
}

func (c registryClient) request(ctx context.Context, address, authorization string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("invalid registry request URL")
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Redirect URLs can carry signed credentials; do not include them in errors.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("registry network/TLS error or timeout; image availability is unknown")
	}
	return resp, nil
}

func (c registryClient) registryToken(ctx context.Context, challenge string) (string, error) {
	if !strings.HasPrefix(challenge, "Bearer ") {
		return "", errors.New("registry authentication required; bearer access unavailable")
	}
	params := make(map[string]string)
	for _, match := range bearerParameter.FindAllStringSubmatch(challenge, -1) {
		params[match[1]] = match[2]
	}
	realm, err := url.Parse(params["realm"])
	base, baseErr := url.Parse(c.baseURL)
	if err != nil || baseErr != nil || realm.Host != base.Host || realm.Scheme != base.Scheme || realm.User != nil || realm.Fragment != "" {
		return "", errors.New("registry authentication uses an unsupported token endpoint")
	}
	q := realm.Query()
	q.Set("service", params["service"])
	q.Set("scope", params["scope"])
	realm.RawQuery = q.Encode()
	if (c.username == "") != (c.password == "") {
		return "", errors.New("registry authentication requires both QUAY_USERNAME and QUAY_PASSWORD")
	}
	var authorization string
	if c.username != "" {
		authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(c.username+":"+c.password))
	}
	resp, err := c.request(ctx, realm.String(), authorization)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry authentication failed (HTTP %d); image availability is unknown", resp.StatusCode)
	}
	var result struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, registryResponseLimit)).Decode(&result); err != nil {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", errors.New("invalid registry authentication response")
	}
	if result.Token != "" {
		return result.Token, nil
	}
	if result.AccessToken != "" {
		return result.AccessToken, nil
	}
	return "", errors.New("registry authentication returned no token")
}
