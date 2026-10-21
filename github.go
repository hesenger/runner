package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Artifact maps the fields of a GitHub Actions artifact that runner uses
type Artifact struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	ArchiveDownloadURL string `json:"archive_download_url"`
	CreatedAt          string `json:"created_at"`
	Expired            bool   `json:"expired"`
	WorkflowRun        struct {
		HeadBranch string `json:"head_branch"`
	} `json:"workflow_run"`
}

// ArtifactResponse maps the minimalist payload returned by GitHub's endpoint
type ArtifactResponse struct {
	Artifacts []Artifact `json:"artifacts"`
}

// artifactsPerPage is how many artifacts are listed per lookup, newest first,
// so older matches are still found when the newest ones are filtered out.
const artifactsPerPage = 30

// FindLatestArtifact returns the newest non-expired artifact of the repository,
// optionally restricted to an artifact name and a branch (empty means any).
// apiBase is the GitHub REST API base URL, e.g. https://api.github.com.
func FindLatestArtifact(apiBase, repo, token, name, branch string) (*Artifact, error) {
	query := url.Values{}
	query.Set("per_page", fmt.Sprint(artifactsPerPage))
	if name != "" {
		query.Set("name", name)
	}
	apiURL := fmt.Sprintf("%s/repos/%s/actions/artifacts?%s", strings.TrimSuffix(apiBase, "/"), repo, query.Encode())

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create metadata request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metadata request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata API returned status: %s", resp.Status)
	}

	var artResp ArtifactResponse
	if err := json.NewDecoder(resp.Body).Decode(&artResp); err != nil {
		return nil, fmt.Errorf("failed to decode JSON metadata payload: %w", err)
	}

	target, ok := pickArtifact(artResp.Artifacts, name, branch)
	if !ok {
		return nil, fmt.Errorf("no artifact found among the latest %d (name=%q, branch=%q, expired skipped)",
			len(artResp.Artifacts), name, branch)
	}
	return &target, nil
}

// pickArtifact returns the first artifact, in API order (newest first), that
// is not expired and matches name and branch when they are set.
func pickArtifact(artifacts []Artifact, name, branch string) (Artifact, bool) {
	for _, a := range artifacts {
		if a.Expired {
			continue
		}
		if name != "" && a.Name != name {
			continue
		}
		if branch != "" && a.WorkflowRun.HeadBranch != branch {
			continue
		}
		return a, true
	}
	return Artifact{}, false
}

// DownloadArtifact saves the artifact zip to destDir/<name>-<id>.zip and returns its path.
func DownloadArtifact(target Artifact, token, destDir string) (string, error) {
	dlReq, err := http.NewRequest("GET", target.ArchiveDownloadURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create download pointer request: %w", err)
	}
	dlReq.Header.Set("Accept", "application/vnd.github+json")
	dlReq.Header.Set("Authorization", "Bearer "+token)

	dlResp, err := http.DefaultClient.Do(dlReq)
	if err != nil {
		return "", fmt.Errorf("failed to download artifact: %w", err)
	}
	defer dlResp.Body.Close()

	// never save an error page as the zip
	if dlResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("artifact download returned status: %s", dlResp.Status)
	}

	// include the artifact ID so each version extracts to its own directory and
	// never overwrites (or gets cleaned up as) the currently running version
	finalPath := filepath.Join(destDir, fmt.Sprintf("%s-%d.zip", target.Name, target.ID))

	out, err := os.Create(finalPath)
	if err != nil {
		return "", fmt.Errorf("failed to create local disk output signature: %w", err)
	}
	defer out.Close()

	_, err = io.Copy(out, dlResp.Body)
	if err != nil {
		os.Remove(finalPath)
		return "", fmt.Errorf("failed streaming payload blocks to file destination: %w", err)
	}

	return finalPath, nil
}
