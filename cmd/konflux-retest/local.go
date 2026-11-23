package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type (
	konfluxLogin struct {
		connections sync.Map
		transport   http.RoundTripper
	}
	konfluxConnection struct{ token, address string }
)

// token uses the API address and credential from the same saved OpenShift context.
func (l *konfluxLogin) token(ctx context.Context, host string) (string, error) {
	if connection, ok := l.connections.Load(host); ok {
		return connection.(konfluxConnection).token, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "oc", "config", "view", "-o", "json").Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", errors.New("Konflux lookups require oc and a matching saved login")
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
	clusters := make(map[string]string)
	for _, cluster := range config.Clusters {
		server, err := url.Parse(cluster.Cluster.Server)
		if err == nil && server.Scheme == "https" && server.Hostname() == host && server.User == nil {
			clusters[cluster.Name] = server.Host
		}
	}
	var contextName, address string
	for _, candidate := range config.Contexts {
		if matched := clusters[candidate.Context.Cluster]; matched != "" {
			if contextName == "" || candidate.Name == config.CurrentContext {
				contextName, address = candidate.Name, matched
			}
		}
	}
	if contextName == "" {
		return "", errors.New("no saved oc context matches the Konflux cluster")
	}
	data, err = exec.CommandContext(ctx, "oc", "--context", contextName, "whoami", "-t").Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	token := strings.TrimSpace(string(data))
	if err != nil || token == "" {
		return "", errors.New("cannot read the saved Konflux login")
	}
	l.connections.Store(host, konfluxConnection{token: token, address: address})
	return token, nil
}

// RoundTrip uses the saved API port for live PipelineRun requests on the matched host.
func (l *konfluxLogin) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" && req.URL.Port() == "" && strings.HasPrefix(req.URL.Path, "/apis/tekton.dev/") {
		if connection, ok := l.connections.Load(req.URL.Hostname()); ok {
			req = req.Clone(req.Context())
			req.URL.Host = connection.(konfluxConnection).address
			req.Host = req.URL.Host
		}
	}
	return l.transport.RoundTrip(req)
}

// dockerPull uses the local Docker login only when explicitly enabled. Ambiguous
// access-denied responses never count as missing images, and output is not echoed.
func dockerPull(ctx context.Context, image string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "pull", "--platform=linux/amd64", image).CombinedOutput()
	if err == nil {
		return "", nil
	}
	if ctx.Err() != nil {
		return "", errors.New("docker pull timed out or was cancelled")
	}
	return dockerPullFailure(output)
}

func dockerPullFailure(output []byte) (string, error) {
	text := strings.ToLower(string(output))
	if strings.Contains(text, "unauthorized") || strings.Contains(text, "denied") || strings.Contains(text, "authentication required") {
		return "", errors.New("docker pull could not authenticate")
	}
	if strings.Contains(text, "manifest unknown") || strings.Contains(text, "manifest for ") && strings.Contains(text, " not found") {
		return "image is missing (docker pull)", nil
	}
	return "", errors.New("docker pull could not verify the image")
}
