package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// ArtifactResponse maps the minimalist payload returned by GitHub's endpoint
type ArtifactResponse struct {
	Artifacts []struct {
		ID                 int64  `json:"id"`
		Name               string `json:"name"`
		ArchiveDownloadURL string `json:"archive_download_url"`
		CreatedAt          string `json:"created_at"`
	} `json:"artifacts"`
}

// DownloadLatestArtifact fetches the absolute latest zip build bundle from GitHub Actions.
// It places the file under destinationDir/artifact-build-{run_number}.zip
func DownloadLatestArtifact(repo, artifactName, token, destDir string) (string, error) {
	client := &http.Client{}
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/actions/artifacts?per_page=1", repo)

	// 1. Fetch metadata for the single most recent execution payload
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create metadata request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("metadata request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata API returned status: %s", resp.Status)
	}

	var artResp ArtifactResponse
	if err := json.NewDecoder(resp.Body).Decode(&artResp); err != nil {
		return "", fmt.Errorf("failed to decode JSON metadata payload: %w", err)
	}

	if len(artResp.Artifacts) == 0 {
		return "", fmt.Errorf("no artifacts found matching the designation: %s", artifactName)
	}

	target := artResp.Artifacts[0]

	dlReq, err := http.NewRequest("GET", target.ArchiveDownloadURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create download pointer request: %w", err)
	}
	dlReq.Header.Set("Accept", "application/vnd.github+json")
	dlReq.Header.Set("Authorization", "Bearer "+token)

	finalPath := filepath.Join(destDir, target.Name+".zip")
	if _, err := os.Stat(finalPath); err == nil {
		os.Remove(finalPath)
	}

	out, err := os.Create(finalPath)
	if err != nil {
		return "", fmt.Errorf("failed to create local disk output signature: %w", err)
	}
	defer out.Close()

	dlResp, err := client.Do(dlReq)
	if err != nil {
		return "", fmt.Errorf("failed to download artifact: %w", err)
	}
	defer dlResp.Body.Close()

	_, err = io.Copy(out, dlResp.Body)
	if err != nil {
		return "", fmt.Errorf("failed streaming payload blocks to file destination: %w", err)
	}

	return finalPath, nil
}
