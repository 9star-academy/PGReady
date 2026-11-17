package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

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
	const direct = canonicalLatestReleaseURL
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
			requested := make(chan string, 2)
			client := &http.Client{Transport: releaseTransport(func(req *http.Request) (*http.Response, error) {
				requested <- req.URL.String()
				if req.URL.String() == direct {
					return releaseResponse(tc.status, tc.first), nil
				}
				return releaseResponse(http.StatusOK, `{"tag_name":"v0.1.10","draft":false,"prerelease":false}`), nil
			})}
			release, err := latestRelease(context.Background(), client, []string{mirror, direct})
			if err != nil || release.Tag != "v0.1.10" || len(requested) != 2 {
				t.Fatalf("latestRelease = %+v, %v, request count = %d", release, err, len(requested))
			}
			got := []string{<-requested, <-requested}
			slices.Sort(got)
			if !slices.Equal(got, []string{direct, mirror}) {
				t.Fatalf("requests = %v, want direct and mirror", got)
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

func TestLatestStableReleaseComparesConcurrentSources(t *testing.T) {
	const mirror = "https://mirror.example/latest"
	const direct = canonicalLatestReleaseURL
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	directStarted := make(chan struct{})
	mirrorResponded := make(chan struct{})
	client := &http.Client{Transport: releaseTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == mirror {
			select {
			case <-directStarted:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			response := releaseResponse(http.StatusOK, `{"tag_name":"v0.1.9","draft":false,"prerelease":false}`)
			response.Body = releaseBody{ReadCloser: response.Body, onClose: func() { close(mirrorResponded) }}
			return response, nil
		}
		close(directStarted)
		select {
		case <-mirrorResponded:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		return releaseResponse(http.StatusOK, `{"tag_name":"v0.1.10","draft":false,"prerelease":false}`), nil
	})}
	release, err := latestRelease(ctx, client, []string{mirror, direct})
	if err != nil || release.Tag != "v0.1.10" {
		t.Fatalf("latestRelease = %+v, %v, want v0.1.10", release, err)
	}
}

func TestLatestStableReleaseTrustsCanonicalGitHubResponse(t *testing.T) {
	const mirror = "https://mirror.example/latest"
	const direct = canonicalLatestReleaseURL
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	mirrorResponded := make(chan struct{})
	client := &http.Client{Transport: releaseTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == mirror {
			response := releaseResponse(http.StatusOK, `{"tag_name":"v9.0.0","draft":false,"prerelease":false}`)
			response.Body = releaseBody{ReadCloser: response.Body, onClose: func() { close(mirrorResponded) }}
			return response, nil
		}
		select {
		case <-mirrorResponded:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		return releaseResponse(http.StatusOK, `{"tag_name":"v0.1.10","draft":false,"prerelease":false}`), nil
	})}
	release, err := latestRelease(ctx, client, []string{direct, mirror})
	if err != nil || release.Tag != "v0.1.10" {
		t.Fatalf("latestRelease = %+v, %v, want canonical v0.1.10", release, err)
	}
}

func TestLatestStableReleaseReturnsCanonicalBeforeSlowMirror(t *testing.T) {
	const mirror = "https://mirror.example/latest"
	const direct = canonicalLatestReleaseURL
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	mirrorStarted := make(chan struct{})
	client := &http.Client{Transport: releaseTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == mirror {
			close(mirrorStarted)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		select {
		case <-mirrorStarted:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		return releaseResponse(http.StatusOK, `{"tag_name":"v0.1.10","draft":false,"prerelease":false}`), nil
	})}
	release, err := latestRelease(ctx, client, []string{direct, mirror})
	if err != nil || release.Tag != "v0.1.10" {
		t.Fatalf("latestRelease = %+v, %v, want canonical v0.1.10", release, err)
	}
}

func TestLatestStableReleaseCancelsSlowComparison(t *testing.T) {
	const mirror = "https://mirror.example/latest"
	const direct = canonicalLatestReleaseURL
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	comparisonCanceled := make(chan struct{})
	client := &http.Client{Transport: releaseTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == mirror {
			return releaseResponse(http.StatusOK, `{"tag_name":"v0.1.10","draft":false,"prerelease":false}`), nil
		}
		<-req.Context().Done()
		close(comparisonCanceled)
		return nil, req.Context().Err()
	})}
	release, err := latestRelease(ctx, client, []string{direct, mirror})
	if err != nil || release.Tag != "v0.1.10" {
		t.Fatalf("latestRelease = %+v, %v, want v0.1.10", release, err)
	}
	select {
	case <-comparisonCanceled:
	case <-ctx.Done():
		t.Fatal("slow comparison query was not canceled")
	}
}

func TestLatestStableReleaseHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &http.Client{Transport: releaseTransport(func(req *http.Request) (*http.Response, error) {
		cancel()
		return releaseResponse(http.StatusOK, `{"tag_name":"v0.1.10","draft":false,"prerelease":false}`), nil
	})}
	if release, err := latestRelease(ctx, client, []string{"https://direct.example/latest"}); !errors.Is(err, context.Canceled) || release.Tag != "" {
		t.Fatalf("latestRelease = %+v, %v, want cancellation", release, err)
	}
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

type releaseBody struct {
	io.ReadCloser
	onClose func()
}

func (b releaseBody) Close() error {
	b.onClose()
	return b.ReadCloser.Close()
}
