package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func DecompressArtifact(zipPath string, targetDir string) (string, error) {
	// 1. Open the source zip file
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("failed to open zip file: %w", err)
	}
	defer reader.Close()

	// 2. Derive the folder name by stripping ".zip" from the file name
	zipFileName := filepath.Base(zipPath)
	folderName := strings.TrimSuffix(zipFileName, filepath.Ext(zipFileName))

	// Create the full destination path (e.g., targetDir/my-artifact)
	extractionDir := filepath.Join(targetDir, folderName)

	// delete directory if it exists
	if _, err := os.Stat(extractionDir); err == nil {
		os.RemoveAll(extractionDir)
	}

	// 3. Loop through all files within the archive
	for _, file := range reader.File {
		// Clean up the file path to prevent Zip Slip directory traversal attacks
		cleanedPath := filepath.Clean(file.Name)
		if strings.HasPrefix(cleanedPath, "..") || strings.HasPrefix(cleanedPath, "/") {
			return "", fmt.Errorf("legal violation: file path attempts directory traversal: %s", file.Name)
		}

		// Construct the absolute path where this specific file should live
		extractedFilePath := filepath.Join(extractionDir, cleanedPath)

		// 4. Handle Directories
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(extractedFilePath, file.Mode()); err != nil {
				return "", fmt.Errorf("failed creating directory tree: %w", err)
			}
			continue
		}

		// 5. Handle Files - Ensure parent directory exists before creating the file
		if err := os.MkdirAll(filepath.Dir(extractedFilePath), 0755); err != nil {
			return "", fmt.Errorf("failed creating parent directory structure: %w", err)
		}

		// Open the file inside the zip
		zippedFile, err := file.Open()
		if err != nil {
			return "", fmt.Errorf("failed opening zipped file reader: %w", err)
		}

		// Create the target file on your local disk
		targetFile, err := os.OpenFile(extractedFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
		if err != nil {
			zippedFile.Close()
			return "", fmt.Errorf("failed creating output file on disk: %w", err)
		}

		// Stream the data from the zip entry to the local file
		_, err = io.Copy(targetFile, zippedFile)

		// Explicitly close open descriptors for this loop iteration
		zippedFile.Close()
		targetFile.Close()

		if err != nil {
			return "", fmt.Errorf("failed writing decompressed bytes to disk: %w", err)
		}
	}

	return extractionDir, nil
}
