package mihomo

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const releaseAPI = "https://api.github.com/repos/MetaCubeX/mihomo/releases/latest"

type release struct {
	TagName string  `json:"tag_name"`
	Assets  []asset `json:"assets"`
}
type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// DownloadLatest downloads and caches the official Mihomo release for the
// current OS and architecture.
func DownloadLatest(ctx context.Context, cacheDir string, client *http.Client, logger *log.Logger) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if logger == nil {
		logger = log.Default()
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		platform += ".exe"
	}
	binaryPath := filepath.Join(cacheDir, platform, "mihomo")
	if runtime.GOOS == "windows" {
		binaryPath += ".exe"
	}
	if info, err := os.Stat(binaryPath); err == nil && info.Mode().IsRegular() {
		return binaryPath, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("get Mihomo release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("GitHub release API returned %s", resp.Status)
	}
	var latest release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&latest); err != nil {
		return "", fmt.Errorf("decode Mihomo release: %w", err)
	}
	selected := selectAsset(latest.Assets, runtime.GOOS, runtime.GOARCH)
	if selected.URL == "" {
		return "", fmt.Errorf("Mihomo has no release asset for %s", platform)
	}
	logger.Printf("downloading Mihomo %s for %s", latest.TagName, platform)
	assetReq, err := http.NewRequestWithContext(ctx, http.MethodGet, selected.URL, nil)
	if err != nil {
		return "", err
	}
	assetReq.Header.Set("Accept", "application/octet-stream")
	assetResp, err := client.Do(assetReq)
	if err != nil {
		return "", fmt.Errorf("download Mihomo: %w", err)
	}
	defer assetResp.Body.Close()
	if assetResp.StatusCode/100 != 2 {
		return "", fmt.Errorf("Mihomo download returned %s", assetResp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(assetResp.Body, 100<<20))
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(selected.Name, ".gz") {
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return "", fmt.Errorf("open Mihomo archive: %w", err)
		}
		data, err = io.ReadAll(io.LimitReader(reader, 100<<20))
		reader.Close()
		if err != nil {
			return "", fmt.Errorf("extract Mihomo: %w", err)
		}
	} else if strings.HasSuffix(selected.Name, ".zip") {
		data, err = extractZipBinary(data)
		if err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(binaryPath), ".mihomo-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0755); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, binaryPath); err != nil {
		return "", fmt.Errorf("install Mihomo: %w", err)
	}
	return binaryPath, nil
}

func selectAsset(assets []asset, goos, goarch string) asset {
	for _, item := range assets {
		name := strings.ToLower(item.Name)
		if strings.Contains(name, goos) && strings.Contains(name, goarch) && (strings.HasSuffix(name, ".gz") || strings.HasSuffix(name, ".zip")) {
			return item
		}
	}
	return asset{}
}

func extractZipBinary(data []byte) ([]byte, error) {
	tmp, err := os.CreateTemp("", "mihomo-*.zip")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	archive, err := zip.OpenReader(name)
	if err != nil {
		return nil, fmt.Errorf("open Mihomo zip: %w", err)
	}
	defer archive.Close()
	for _, file := range archive.File {
		if file.FileInfo().Mode().IsRegular() {
			h, err := file.Open()
			if err != nil {
				return nil, err
			}
			result, readErr := io.ReadAll(io.LimitReader(h, 100<<20))
			h.Close()
			if readErr != nil {
				return nil, readErr
			}
			return result, nil
		}
	}
	return nil, fmt.Errorf("Mihomo zip contains no executable")
}

func Start(binary, workDir, configPath string, logger *log.Logger) (*exec.Cmd, error) {
	cmd := exec.Command(binary, "-d", workDir, "-f", configPath)
	cmd.Stdout = logger.Writer()
	cmd.Stderr = logger.Writer()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Mihomo: %w", err)
	}
	return cmd, nil
}
