package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/9star-academy/PGReady/internal/command"
)

func TestUpdateInHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	runner := command.Func(func(context.Context, command.Call) error {
		t.Fatal("help ran a system command")
		return nil
	})
	if code := run(context.Background(), []string{"--help"}, strings.NewReader(""), &out, &errOut, runner); code != 0 ||
		!strings.Contains(out.String(), "pgready update") {
		t.Fatalf("help returned %d with output %q and stderr %q", code, &out, &errOut)
	}
}

func TestLatestStableRelease(t *testing.T) {
	const mirror = "https://mirror.example/https://api.github.com/repos/9star-academy/PGReady/releases/latest"
	const direct = "https://api.github.com/repos/9star-academy/PGReady/releases/latest"
	for _, tc := range []struct {
		name   string
		first  string
		status int
	}{
		{"HTTP failure", "", http.StatusServiceUnavailable},
		{"invalid JSON", "<html>error</html>", http.StatusOK},
		{"incomplete metadata", `{"tag_name":"v9.0.0"}`, http.StatusOK},
		{"draft", `{"tag_name":"v9.0.0","draft":true,"prerelease":false}`, http.StatusOK},
		{"prerelease", `{"tag_name":"v9.0.0","draft":false,"prerelease":true}`, http.StatusOK},
		{"invalid tag", `{"tag_name":"v9.0.0-rc.1","draft":false,"prerelease":false}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requested []string
			client := &http.Client{Transport: releaseTransport(func(req *http.Request) (*http.Response, error) {
				requested = append(requested, req.URL.String())
				if req.URL.String() == mirror {
					return releaseResponse(tc.status, tc.first), nil
				}
				return releaseResponse(http.StatusOK, `{"tag_name":"v0.1.10","draft":false,"prerelease":false}`), nil
			})}
			release, err := latestRelease(context.Background(), client, []string{mirror, direct})
			if err != nil || release.Tag != "v0.1.10" || !reflect.DeepEqual(requested, []string{mirror, direct}) {
				t.Fatalf("latestRelease = %+v, %v, requests = %v", release, err, requested)
			}
		})
	}
	t.Run("all sources fail", func(t *testing.T) {
		client := &http.Client{Transport: releaseTransport(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("offline")
		})}
		if release, err := latestRelease(context.Background(), client, []string{direct}); err == nil || release.Tag != "" {
			t.Fatalf("latestRelease = %+v, %v", release, err)
		}
	})
}

func TestCompareReleaseVersions(t *testing.T) {
	for _, tc := range []struct {
		current string
		latest  string
		want    int
	}{
		{"v0.1.9", "v0.1.10", -1},
		{"v0.1.10", "v0.1.10", 0},
		{"v1.0.0", "v0.20.1", 1},
	} {
		current, _ := parseReleaseVersion(tc.current)
		latest, _ := parseReleaseVersion(tc.latest)
		if got := slices.Compare(current[:], latest[:]); got != tc.want {
			t.Errorf("compare %s to %s = %d, want %d", tc.current, tc.latest, got, tc.want)
		}
	}
}

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func releaseResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}
