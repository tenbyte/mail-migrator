package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

const (
	DefaultAPIURL    = "https://api.github.com/repos/tenbyte/mail-migrator/releases/latest"
	LatestReleaseURL = "https://github.com/tenbyte/mail-migrator/releases/latest"
	maxResponse      = 1 << 20
)

type Checker struct {
	Client *http.Client
	APIURL string
}

func New() *Checker {
	return &Checker{Client: &http.Client{Timeout: 5 * time.Second}, APIURL: DefaultAPIURL}
}

func (c *Checker) Check(ctx context.Context, currentVersion string) (domain.UpdateInfo, error) {
	current := normalizeVersion(currentVersion)
	result := domain.UpdateInfo{CurrentVersion: strings.TrimPrefix(current, "v")}
	if !semver.IsValid(current) {
		return result, fmt.Errorf("invalid application version %q", currentVersion)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	apiURL := c.APIURL
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return result, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "tenbyte-mail-migrator/"+result.CurrentVersion)
	response, err := client.Do(req)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("GitHub release API returned %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil {
		return result, err
	}
	if len(body) > maxResponse {
		return result, errors.New("GitHub release response exceeds size limit")
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return result, fmt.Errorf("decode GitHub release response: %w", err)
	}
	latest := normalizeVersion(release.TagName)
	if !semver.IsValid(latest) {
		return result, fmt.Errorf("invalid GitHub release version %q", release.TagName)
	}
	result.LatestVersion = strings.TrimPrefix(latest, "v")
	result.UpdateAvailable = semver.Compare(latest, current) > 0
	return result, nil
}

func normalizeVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "v") {
		return value
	}
	return "v" + value
}
