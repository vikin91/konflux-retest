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
		"should accept the requested syntax":            {args: []string{"--repo", "stackrox", "--prs", "2345", "3456", "4567"}, want: options{repo: "stackrox/stackrox", prs: []int{2345, 3456, 4567}}},
		"should accept qualified repos and comma lists": {args: []string{"--repo", "other/repo", "--prs=1,2,1"}, want: options{repo: "other/repo", prs: []int{1, 2}}},
		"should accept quoted PR lists":                 {args: []string{"--prs", "1 2"}, want: options{repo: "stackrox/stackrox", prs: []int{1, 2}}},
		"should reject missing PRs":                     {invalid: true},
		"should reject empty PR lists":                  {args: []string{"--prs", ","}, invalid: true},
		"should reject zero PR numbers":                 {args: []string{"--prs", "0"}, invalid: true},
		"should reject negative PR numbers":             {args: []string{"--prs", "-1"}, invalid: true},
		"should reject text PR numbers":                 {args: []string{"--prs", "1", "bad"}, invalid: true},
		"should reject malformed repos":                 {args: []string{"--repo", "https://github.com/a/b", "--prs", "1"}, invalid: true},
		"should reject unknown flags":                   {args: []string{"--bogus"}, invalid: true},
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
	for _, text := range []string{"GH_TOKEN", "GITHUB_TOKEN", "gh auth token --hostname github.com", "stderr", "operator-bundle-on-push"} {
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
