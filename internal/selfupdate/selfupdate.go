// Package selfupdate checks and installs tagged Callbell CLI releases.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	repositoryAPI     = "https://api.github.com/repos/castrowithcee/callbell-cli"
	maxMetadataBytes  = 1 << 20
	maxArchiveBytes   = 128 << 20
	maxExecutableSize = 64 << 20
	maxManpageSize    = 2 << 20
)

var ErrDevelopmentBuild = errors.New("self-update is unavailable in a dev build; install a tagged release first")

// UnsupportedInstallationError reports an installation that cannot be replaced safely in place.
type UnsupportedInstallationError struct{ Reason string }

func (e *UnsupportedInstallationError) Error() string {
	return "cannot self-update this installation: " + e.Reason
}

// Result is the stable payload of an update check or installation.
type Result struct {
	Current         string
	Latest          string
	UpdateAvailable bool
	Updated         bool
}

// Client checks GitHub Releases and updates one installed executable. Exported fields are test seams;
// zero values select the production repository, platform, executable, and HTTP client.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Version    string
	GOOS       string
	GOARCH     string
	Executable string
}

// New returns the production updater for version.
func New(version string) *Client { return &Client{Version: version} }

// Check reports whether GitHub has a newer stable release without downloading an artifact.
func (c *Client) Check(ctx context.Context) (Result, error) {
	current, err := c.currentVersion()
	if err != nil {
		return Result{}, err
	}
	release, err := c.latest(ctx)
	if err != nil {
		return Result{}, err
	}
	latest, err := parseSemver(release.TagName)
	if err != nil {
		return Result{}, fmt.Errorf("latest release has an invalid version: %w", err)
	}
	return Result{
		Current:         c.Version,
		Latest:          release.TagName,
		UpdateAvailable: current.compare(latest) < 0,
	}, nil
}

// Update installs the newest stable release when it is newer than the running version.
func (c *Client) Update(ctx context.Context) (Result, error) {
	result, release, err := c.checkWithRelease(ctx)
	if err != nil || !result.UpdateAvailable {
		return result, err
	}
	goos, goarch := c.platform()
	if goos == "windows" {
		return Result{}, &UnsupportedInstallationError{Reason: "safe replacement of the running Windows executable is not supported yet"}
	}
	archiveName := assetName(result.Latest, goos, goarch)
	archiveURL, checksumURL, err := releaseURLs(release, archiveName)
	if err != nil {
		return Result{}, err
	}
	checksums, err := c.download(ctx, checksumURL, maxMetadataBytes)
	if err != nil {
		return Result{}, fmt.Errorf("download checksums: %w", err)
	}
	want, err := checksumFor(checksums, archiveName)
	if err != nil {
		return Result{}, err
	}
	archive, err := c.download(ctx, archiveURL, maxArchiveBytes)
	if err != nil {
		return Result{}, fmt.Errorf("download %s: %w", archiveName, err)
	}
	got := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return Result{}, fmt.Errorf("checksum mismatch for %s", archiveName)
	}
	payload, err := extractPayload(archiveName, archive, goos)
	if err != nil {
		return Result{}, err
	}
	if err := c.install(payload, goos); err != nil {
		return Result{}, err
	}
	result.Updated = true
	return result, nil
}

type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (c *Client) checkWithRelease(ctx context.Context) (Result, release, error) {
	current, err := c.currentVersion()
	if err != nil {
		return Result{}, release{}, err
	}
	r, err := c.latest(ctx)
	if err != nil {
		return Result{}, release{}, err
	}
	latest, err := parseSemver(r.TagName)
	if err != nil {
		return Result{}, release{}, fmt.Errorf("latest release has an invalid version: %w", err)
	}
	result := Result{Current: c.Version, Latest: r.TagName, UpdateAvailable: current.compare(latest) < 0}
	return result, r, nil
}

func (c *Client) currentVersion() (semver, error) {
	if c.Version == "" || c.Version == "dev" {
		return semver{}, ErrDevelopmentBuild
	}
	parsed, err := parseSemver(c.Version)
	if err != nil {
		return semver{}, fmt.Errorf("running build has an invalid version: %w", err)
	}
	return parsed, nil
}

func (c *Client) latest(ctx context.Context) (release, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = repositoryAPI
	}
	body, err := c.request(ctx, base+"/releases/latest", maxMetadataBytes)
	if err != nil {
		return release{}, fmt.Errorf("check latest release: %w", err)
	}
	var r release
	if err := json.Unmarshal(body, &r); err != nil {
		return release{}, fmt.Errorf("decode latest release: %w", err)
	}
	if r.TagName == "" {
		return release{}, errors.New("latest release does not declare a tag")
	}
	return r, nil
}

func (c *Client) download(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	if err := c.validateAssetURL(rawURL); err != nil {
		return nil, err
	}
	return c.request(ctx, rawURL, limit)
}

func (c *Client) request(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "callbell/"+c.Version)
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return body, nil
}

func (c *Client) validateAssetURL(rawURL string) error {
	asset, err := url.Parse(rawURL)
	if err != nil || asset.Host == "" {
		return errors.New("release contains an invalid asset URL")
	}
	baseRaw := c.BaseURL
	if baseRaw == "" {
		baseRaw = repositoryAPI
	}
	base, err := url.Parse(baseRaw)
	if err != nil {
		return errors.New("updater has an invalid repository URL")
	}
	if base.Scheme == "https" {
		if asset.Scheme != "https" || !(asset.Host == "github.com" || strings.HasSuffix(asset.Host, ".github.com") || strings.HasSuffix(asset.Host, ".githubusercontent.com")) {
			return errors.New("release asset URL is outside trusted GitHub HTTPS hosts")
		}
		return nil
	}
	if asset.Scheme != base.Scheme || asset.Host != base.Host {
		return errors.New("release asset URL is outside the configured test origin")
	}
	return nil
}

func (c *Client) platform() (string, string) {
	goos, goarch := c.GOOS, c.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

func assetName(version, goos, goarch string) string {
	extension := ".tar.gz"
	if goos == "windows" {
		extension = ".zip"
	}
	return fmt.Sprintf("callbell_%s_%s_%s%s", version, goos, goarch, extension)
}

func releaseURLs(r release, archiveName string) (string, string, error) {
	var archiveURL, checksumURL string
	for _, asset := range r.Assets {
		switch asset.Name {
		case archiveName:
			archiveURL = asset.URL
		case "checksums.txt":
			checksumURL = asset.URL
		}
	}
	if archiveURL == "" || checksumURL == "" {
		return "", "", fmt.Errorf("release %s does not contain %s and checksums.txt", r.TagName, archiveName)
	}
	return archiveURL, checksumURL, nil
}

func checksumFor(body []byte, filename string) (string, error) {
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != filename {
			continue
		}
		if decoded, err := hex.DecodeString(fields[0]); err == nil && len(decoded) == sha256.Size {
			return strings.ToLower(fields[0]), nil
		}
		return "", fmt.Errorf("checksums.txt contains an invalid SHA-256 for %s", filename)
	}
	return "", fmt.Errorf("checksums.txt does not contain %s", filename)
}

type payload struct {
	executable []byte
	manpage    []byte
}

func extractPayload(filename string, body []byte, goos string) (payload, error) {
	if strings.HasSuffix(filename, ".zip") {
		return extractZip(body, goos)
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return payload{}, fmt.Errorf("open release archive: %w", err)
	}
	defer gz.Close()
	return extractTar(tar.NewReader(gz), goos)
}

func extractTar(reader *tar.Reader, goos string) (payload, error) {
	var out payload
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return payload{}, fmt.Errorf("read release archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if err := capturePayload(&out, filepath.ToSlash(strings.TrimPrefix(header.Name, "./")), reader, goos); err != nil {
			return payload{}, err
		}
	}
	return requirePayload(out, goos)
}

func extractZip(body []byte, goos string) (payload, error) {
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return payload{}, fmt.Errorf("open release archive: %w", err)
	}
	var out payload
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		entry, err := file.Open()
		if err != nil {
			return payload{}, fmt.Errorf("open %s in release archive: %w", file.Name, err)
		}
		err = capturePayload(&out, filepath.ToSlash(strings.TrimPrefix(file.Name, "./")), entry, goos)
		entry.Close()
		if err != nil {
			return payload{}, err
		}
	}
	return requirePayload(out, goos)
}

func capturePayload(out *payload, name string, reader io.Reader, goos string) error {
	executableName := "bin/callbell"
	if goos == "windows" {
		executableName += ".exe"
	}
	switch name {
	case executableName:
		body, err := readEntry(reader, maxExecutableSize)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		out.executable = body
	case "share/man/man1/callbell.1":
		body, err := readEntry(reader, maxManpageSize)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		out.manpage = body
	}
	return nil
}

func readEntry(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("entry exceeds %d bytes", limit)
	}
	return body, nil
}

func requirePayload(out payload, goos string) (payload, error) {
	if len(out.executable) == 0 {
		return payload{}, errors.New("release archive does not contain the callbell executable")
	}
	if goos != "windows" && len(out.manpage) == 0 {
		return payload{}, errors.New("release archive does not contain callbell.1")
	}
	return out, nil
}

func (c *Client) install(files payload, goos string) error {
	executable := c.Executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return fmt.Errorf("locate running executable: %w", err)
		}
	}
	absolute, err := filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return fmt.Errorf("inspect installed executable: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return &UnsupportedInstallationError{Reason: "the running executable must be a regular file, not a symlink"}
	}
	wantName := "callbell"
	if goos == "windows" {
		wantName += ".exe"
	}
	binDir := filepath.Dir(absolute)
	if filepath.Base(absolute) != wantName || filepath.Base(binDir) != "bin" {
		return &UnsupportedInstallationError{Reason: "install the release as <prefix>/bin/" + wantName}
	}
	prefix := filepath.Dir(binDir)
	manDir := filepath.Join(prefix, "share", "man", "man1")
	if err := os.MkdirAll(manDir, 0o755); err != nil {
		return fmt.Errorf("create man directory: %w", err)
	}
	stagedMan, err := stageFile(manDir, ".callbell-man-*", files.manpage, 0o644)
	if err != nil {
		return fmt.Errorf("stage manpage: %w", err)
	}
	defer os.Remove(stagedMan)
	stagedBinary, err := stageFile(binDir, ".callbell-bin-*", files.executable, info.Mode().Perm()|0o500)
	if err != nil {
		return fmt.Errorf("stage executable: %w", err)
	}
	defer os.Remove(stagedBinary)
	if err := os.Rename(stagedMan, filepath.Join(manDir, "callbell.1")); err != nil {
		return fmt.Errorf("install manpage: %w", err)
	}
	if err := os.Rename(stagedBinary, absolute); err != nil {
		return fmt.Errorf("replace executable: %w", err)
	}
	return nil
}

func stageFile(dir, pattern string, body []byte, mode os.FileMode) (path string, err error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	path = file.Name()
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(path)
		}
	}()
	if err = file.Chmod(mode); err != nil {
		return "", err
	}
	if _, err = file.Write(body); err != nil {
		return "", err
	}
	return path, file.Sync()
}
