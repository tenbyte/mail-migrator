package updatecheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCheckerComparesReleaseVersions(t *testing.T) {
	for _, test := range []struct {
		name      string
		current   string
		latest    string
		available bool
	}{
		{name: "newer", current: "0.3.0", latest: "v0.3.1", available: true},
		{name: "equal", current: "v0.3.0", latest: "0.3.0", available: false},
		{name: "older", current: "0.3.0", latest: "v0.2.9", available: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept") != "application/vnd.github+json" {
					t.Errorf("unexpected Accept header: %q", r.Header.Get("Accept"))
				}
				if r.Header.Get("User-Agent") != "tenbyte-mail-migrator/0.3.0" {
					t.Errorf("unexpected User-Agent: %q", r.Header.Get("User-Agent"))
				}
				fmt.Fprintf(w, `{"tag_name":%q}`, test.latest)
			}))
			defer server.Close()
			checker := &Checker{Client: server.Client(), APIURL: server.URL}
			result, err := checker.Check(context.Background(), test.current)
			if err != nil {
				t.Fatal(err)
			}
			if result.CurrentVersion != "0.3.0" || result.LatestVersion != strings.TrimPrefix(test.latest, "v") || result.UpdateAvailable != test.available {
				t.Fatalf("unexpected result: %#v", result)
			}
		})
	}
}

func TestCheckerRejectsInvalidAndOversizedResponses(t *testing.T) {
	for _, body := range []string{`{"tag_name":"not-a-version"}`, strings.Repeat("x", maxResponse+1)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		checker := &Checker{Client: server.Client(), APIURL: server.URL}
		if _, err := checker.Check(context.Background(), "0.3.0"); err == nil {
			t.Fatal("expected invalid response to fail")
		}
		server.Close()
	}
}

func TestCheckerReturnsHTTPAndTimeoutErrors(t *testing.T) {
	notFound := httptest.NewServer(http.NotFoundHandler())
	checker := &Checker{Client: notFound.Client(), APIURL: notFound.URL}
	if _, err := checker.Check(context.Background(), "0.3.0"); err == nil {
		t.Fatal("expected non-200 response to fail")
	}
	notFound.Close()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"tag_name":"v0.3.1"}`))
	}))
	checker = &Checker{Client: &http.Client{Timeout: 10 * time.Millisecond}, APIURL: slow.URL}
	if _, err := checker.Check(context.Background(), "0.3.0"); err == nil {
		t.Fatal("expected timeout to fail")
	}
	slow.Close()
}

func TestRepositoryURLsAreFixed(t *testing.T) {
	if DefaultAPIURL != "https://api.github.com/repos/tenbyte/mail-migrator/releases/latest" {
		t.Fatalf("unexpected API URL %q", DefaultAPIURL)
	}
	if LatestReleaseURL != "https://github.com/tenbyte/mail-migrator/releases/latest" {
		t.Fatalf("unexpected release URL %q", LatestReleaseURL)
	}
}
