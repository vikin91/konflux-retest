package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestParseOptions(t *testing.T) {
	tests := map[string]struct {
		args    []string
		want    options
		invalid bool
	}{
		"should accept the requested tag syntax":           {args: []string{"--repo", "stackrox/scanner", "--tags", "2.39.27"}, want: options{repo: "stackrox/scanner", tags: []string{"2.39.27"}}},
		"should deduplicate tags in input order":           {args: []string{"--repo", "scanner", "--tags", "2.39.27,2.39.28", "2.39.27", "v2.40.0"}, want: options{repo: "stackrox/scanner", tags: []string{"2.39.27", "2.39.28", "v2.40.0"}}},
		"should accept quoted tag lists and namespaces":    {args: []string{"--tags", "release/v1 v2"}, want: options{repo: "stackrox/stackrox", tags: []string{"release/v1", "v2"}}},
		"should allow optional verification with tags":     {args: []string{"--verify-pull", "--tags", "v1"}, want: options{repo: "stackrox/stackrox", tags: []string{"v1"}, verifyPull: true}},
		"should enable posting for PRs":                    {args: []string{"--full-auto", "--prs", "1"}, want: options{repo: "stackrox/stackrox", prs: []int{1}, fullAuto: true}},
		"should enable posting for tags":                   {args: []string{"--full-auto", "--tags", "v1"}, want: options{repo: "stackrox/stackrox", tags: []string{"v1"}, fullAuto: true}},
		"should reject mixed PR and tag lists":             {args: []string{"--prs", "1", "--tags", "v1"}, invalid: true},
		"should reject mixed lists even when one is empty": {args: []string{"--prs=", "--tags", "v1"}, invalid: true},
		"should reject empty tags":                         {args: []string{"--tags="}, invalid: true},
		"should reject separator-only tags":                {args: []string{"--tags", ", \t"}, invalid: true},
		"should reject flags after positional tags":        {args: []string{"--tags", "v1", "v2", "--prs", "1"}, invalid: true},
		"should reject tag revision expressions":           {args: []string{"--tags", "v1~1"}, invalid: true},
		"should reject invalid tag namespaces":             {args: []string{"--tags", "release//v1"}, invalid: true},
		"should reject control characters in tags":         {args: []string{"--tags", "v1\x1b"}, invalid: true},
		"should enable optional Docker verification":       {args: []string{"--verify-pull", "--prs", "23958"}, want: options{repo: "stackrox/stackrox", prs: []int{23958}, verifyPull: true}},
		"should accept the requested syntax":               {args: []string{"--repo", "stackrox", "--prs", "2345", "3456", "4567"}, want: options{repo: "stackrox/stackrox", prs: []int{2345, 3456, 4567}}},
		"should accept qualified repos and comma lists":    {args: []string{"--repo", "other/repo", "--prs=1,2,1"}, want: options{repo: "other/repo", prs: []int{1, 2}}},
		"should accept quoted PR lists":                    {args: []string{"--prs", "1 2"}, want: options{repo: "stackrox/stackrox", prs: []int{1, 2}}},
		"should reject missing PRs":                        {invalid: true},
		"should reject empty PR lists":                     {args: []string{"--prs", ","}, invalid: true},
		"should reject zero PR numbers":                    {args: []string{"--prs", "0"}, invalid: true},
		"should reject negative PR numbers":                {args: []string{"--prs", "-1"}, invalid: true},
		"should reject text PR numbers":                    {args: []string{"--prs", "1", "bad"}, invalid: true},
		"should reject malformed repos":                    {args: []string{"--repo", "https://github.com/a/b", "--prs", "1"}, invalid: true},
		"should reject unknown flags":                      {args: []string{"--bogus"}, invalid: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parseOptions(tt.args, io.Discard)
			if tt.invalid {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v; want %+v", got, tt.want)
			}
		})
	}
}

func TestHelpExplainsAuthenticationAndMilestones(t *testing.T) {
	var out strings.Builder
	_, err := parseOptions([]string{"--help"}, &out)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("got %v", err)
	}
	for _, text := range []string{"GH_TOKEN", "GITHUB_TOKEN", "gh auth token --hostname github.com", "stderr", "operator-bundle-on-push", "--tags", "--full-auto", "scanner-db-slim-on-push"} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("help missing %q", text)
		}
	}
}

func TestResolveToken(t *testing.T) {
	tests := map[string]struct {
		env                   map[string]string
		stored                string
		fail                  bool
		wantToken, wantSource string
		wantSaved             bool
	}{
		"should prefer GH_TOKEN":                {env: map[string]string{"GH_TOKEN": "first", "GITHUB_TOKEN": "second"}, wantToken: "first", wantSource: "GH_TOKEN"},
		"should fall back to GITHUB_TOKEN":      {env: map[string]string{"GH_TOKEN": " ", "GITHUB_TOKEN": "second"}, wantToken: "second", wantSource: "GITHUB_TOKEN"},
		"should use saved CLI login":            {stored: "saved\n", wantToken: "saved", wantSource: "saved gh login", wantSaved: true},
		"should explain missing authentication": {fail: true, wantSaved: true},
		"should reject empty saved tokens":      {wantSaved: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			called := false
			token, source, err := resolveToken(t.Context(), func(k string) string { return tt.env[k] }, func(context.Context) (string, error) {
				called = true
				if tt.fail {
					return "", errors.New("do not expose secret subprocess output")
				}
				return tt.stored, nil
			})
			if called != tt.wantSaved {
				t.Fatalf("saved token called=%v; want %v", called, tt.wantSaved)
			}
			if tt.wantToken == "" {
				if err == nil || !strings.Contains(err.Error(), "gh auth login") || strings.Contains(err.Error(), "secret") {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err != nil || token != tt.wantToken || !strings.HasPrefix(source, tt.wantSource) {
				t.Fatalf("unexpected authentication result; error=%v source=%q", err, source)
			}
		})
	}
}
