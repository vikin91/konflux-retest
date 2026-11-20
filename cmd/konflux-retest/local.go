package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// konfluxTokens reuses a matching oc context without changing the current context.
// Credentials stay in memory and are never included in errors or subprocess args.
func konfluxTokens() func(context.Context, string) (string, error) {
	var tokens sync.Map
	return func(ctx context.Context, host string) (string, error) {
		if token, ok := tokens.Load(host); ok {
			return token.(string), nil
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
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err != nil || strings.TrimSpace(string(data)) == "" {
			return "", errors.New("cannot read the saved Konflux login")
		}
		token := strings.TrimSpace(string(data))
		tokens.Store(host, token)
		return token, nil
	}
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
