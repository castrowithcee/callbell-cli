package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSemverOrdering(t *testing.T) {
	tests := []struct {
		left, right string
		want        int
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.1", "v1.0.0", 1},
		{"v1.2.0", "v1.10.0", -1},
		{"v2.0.0-rc.1", "v2.0.0", -1},
		{"v2.0.0-rc.2", "v2.0.0-rc.10", -1},
		{"v2.0.0-beta", "v2.0.0-rc.1", -1},
	}
	for _, tt := range tests {
		t.Run(tt.left+"_"+tt.right, func(t *testing.T) {
			left, err := parseSemver(tt.left)
			if err != nil {
				t.Fatal(err)
			}
			right, err := parseSemver(tt.right)
			if err != nil {
				t.Fatal(err)
			}
			if got := left.compare(right); got != tt.want {
				t.Fatalf("compare() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSemverRejectsInvalidTags(t *testing.T) {
	for _, value := range []string{"1.0.0", "v1", "v1.0", "v1.0.0-", "v01.0.0", "v1.0.0-01"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseSemver(value); err == nil {
				t.Fatalf("parseSemver(%q) = nil error", value)
			}
		})
	}
}

func TestCheckReportsNewerStableRelease(t *testing.T) {
	server := releaseServer(t, "v1.2.0", nil, "")
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTPClient: server.Client(), Version: "v1.1.0"}
	result, err := client.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Current != "v1.1.0" || result.Latest != "v1.2.0" || !result.UpdateAvailable || result.Updated {
		t.Fatalf("Check() = %+v", result)
	}
}

func TestCheckRefusesDevelopmentBuild(t *testing.T) {
	_, err := (&Client{Version: "dev"}).Check(context.Background())
	if !errors.Is(err, ErrDevelopmentBuild) {
		t.Fatalf("Check() error = %v, want ErrDevelopmentBuild", err)
	}
}

func TestUpdateReplacesBinaryAndManpage(t *testing.T) {
	archive := releaseArchive(t, []byte("new-binary"), []byte("new-manpage"))
	server := releaseServer(t, "v1.1.0", archive, "")
	defer server.Close()

	prefix := t.TempDir()
	executable := filepath.Join(prefix, "bin", "callbell")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := &Client{
		BaseURL: server.URL, HTTPClient: server.Client(), Version: "v1.0.0",
		GOOS: "linux", GOARCH: "amd64", Executable: executable,
	}
	result, err := client.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.UpdateAvailable || !result.Updated || result.Latest != "v1.1.0" {
		t.Fatalf("Update() = %+v", result)
	}
	assertFile(t, executable, "new-binary")
	assertFile(t, filepath.Join(prefix, "share", "man", "man1", "callbell.1"), "new-manpage")
	info, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o500 != 0o500 {
		t.Fatalf("updated executable mode = %v", info.Mode())
	}
}

func TestUpdateLeavesInstallationUntouchedOnChecksumMismatch(t *testing.T) {
	archive := releaseArchive(t, []byte("new-binary"), []byte("new-manpage"))
	server := releaseServer(t, "v1.1.0", archive, "0000000000000000000000000000000000000000000000000000000000000000")
	defer server.Close()
	prefix := t.TempDir()
	executable := filepath.Join(prefix, "bin", "callbell")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := &Client{
		BaseURL: server.URL, HTTPClient: server.Client(), Version: "v1.0.0",
		GOOS: "linux", GOARCH: "amd64", Executable: executable,
	}
	if _, err := client.Update(context.Background()); err == nil {
		t.Fatal("Update() = nil error, want checksum failure")
	}
	assertFile(t, executable, "old-binary")
	if _, err := os.Stat(filepath.Join(prefix, "share", "man", "man1", "callbell.1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manpage exists after failed update: %v", err)
	}
}

func TestUpdateDoesNotDowngrade(t *testing.T) {
	server := releaseServer(t, "v1.0.0", nil, "")
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTPClient: server.Client(), Version: "v1.1.0"}
	result, err := client.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.UpdateAvailable || result.Updated {
		t.Fatalf("Update() = %+v, want no downgrade", result)
	}
}

func releaseServer(t *testing.T, version string, archive []byte, checksumOverride string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asset := assetName(version, "linux", "amd64")
		switch r.URL.Path {
		case "/releases/latest":
			assets := []map[string]string{}
			if archive != nil {
				assets = append(assets,
					map[string]string{"name": asset, "browser_download_url": server.URL + "/asset"},
					map[string]string{"name": "checksums.txt", "browser_download_url": server.URL + "/checksums"},
				)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": version, "assets": assets})
		case "/asset":
			_, _ = w.Write(archive)
		case "/checksums":
			sum := sha256.Sum256(archive)
			encoded := hex.EncodeToString(sum[:])
			if checksumOverride != "" {
				encoded = fmt.Sprintf("%-64s", checksumOverride)
			}
			fmt.Fprintf(w, "%s  %s\n", encoded, asset)
		default:
			http.NotFound(w, r)
		}
	}))
	return server
}

func releaseArchive(t *testing.T, executable, manpage []byte) []byte {
	t.Helper()
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	tarWriter := tar.NewWriter(gz)
	for name, content := range map[string][]byte{
		"bin/callbell":              executable,
		"share/man/man1/callbell.1": manpage,
	} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("%s = %q, want %q", path, body, want)
	}
}
