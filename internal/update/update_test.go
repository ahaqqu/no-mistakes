package update

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdaterCheckLatestAndRefreshCache(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	tests := []struct {
		name        string
		platform    platformSpec
		archiveName string
	}{
		{
			name:        "darwin tarball",
			platform:    platformSpec{GOOS: "darwin", GOARCH: "arm64"},
			archiveName: "no-mistakes-v1.2.3-darwin-arm64.tar.gz",
		},
		{
			name:        "windows zip",
			platform:    platformSpec{GOOS: "windows", GOARCH: "amd64"},
			archiveName: "no-mistakes-v1.2.3-windows-amd64.zip",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/kunchenguid/no-mistakes/releases/latest" {
					t.Fatalf("unexpected path %q", r.URL.Path)
				}
				fmt.Fprintf(w, `{"tag_name":"v1.2.3","assets":[{"name":%q,"browser_download_url":"http://example.com/archive"},{"name":"checksums.txt","browser_download_url":"http://example.com/checksums"}]}`,
					tt.archiveName,
				)
			}))
			defer server.Close()

			cachePath := filepath.Join(t.TempDir(), "update-check.json")
			u := &updater{
				appName:        "no-mistakes",
				repo:           "kunchenguid/no-mistakes",
				currentVersion: "v1.2.2",
				platform:       tt.platform,
				apiBaseURL:     server.URL,
				httpClient:     server.Client(),
				cachePath:      cachePath,
				now:            func() time.Time { return time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC) },
			}

			plan, err := u.checkLatest(context.Background())
			if err != nil {
				t.Fatalf("checkLatest error = %v", err)
			}
			if !plan.UpdateAvailable {
				t.Fatal("expected update to be available")
			}
			if plan.LatestVersion != "v1.2.3" {
				t.Fatalf("LatestVersion = %q", plan.LatestVersion)
			}
			if plan.ArchiveName != tt.archiveName {
				t.Fatalf("ArchiveName = %q, want %q", plan.ArchiveName, tt.archiveName)
			}
			if plan.Archive.Name != tt.archiveName {
				t.Fatalf("Archive.Name = %q, want %q", plan.Archive.Name, tt.archiveName)
			}

			if err := u.refreshCache(context.Background()); err != nil {
				t.Fatalf("refreshCache error = %v", err)
			}
			cache := readCache(cachePath)
			if cache == nil || cache.LatestVersion != "v1.2.3" {
				t.Fatalf("cache = %#v", cache)
			}
		})
	}
}

func TestUpdaterRunNoRepoDir(t *testing.T) {
	u := &updater{
		currentVersion: "v1.2.2",
	}
	err := u.run(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "cannot find repo directory") {
		t.Fatalf("error = %v", err)
	}
}

func TestUpdaterRunAlreadyUpToDate(t *testing.T) {
	repoDir := t.TempDir()
	upstreamDir := filepath.Join(t.TempDir(), "upstream.git")
	originDir := filepath.Join(t.TempDir(), "origin.git")

	runGitCmd(repoDir, "init", "-b", "main")
	runGitCmd(repoDir, "config", "user.email", "test@test.com")
	runGitCmd(repoDir, "config", "user.name", "Test")
	runGitCmd(repoDir, "commit", "--allow-empty", "-m", "initial")

	runGitCmd("", "init", "--bare", "-b", "main", upstreamDir)
	runGitCmd("", "init", "--bare", "-b", "main", originDir)
	runGitCmd(repoDir, "remote", "add", "upstream", upstreamDir)
	runGitCmd(repoDir, "remote", "add", "origin", originDir)
	runGitCmd(repoDir, "push", "-u", "upstream", "main")
	runGitCmd(repoDir, "push", "-u", "origin", "main")

	stdout := new(bytes.Buffer)
	resetCalled := false
	u := &updater{
		appName:  "no-mistakes",
		repoDir:  repoDir,
		stdout:   stdout,
		stdin:    os.Stdin,
		resetDaemon: func() error {
			resetCalled = true
			return nil
		},
	}

	if err := u.run(context.Background()); err != nil {
		t.Fatalf("run error = %v", err)
	}
	if !strings.Contains(stdout.String(), "already up to date") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if resetCalled {
		t.Fatal("expected no daemon reset when already up to date")
	}
}

func TestUpdaterRunNotOnMainBranch(t *testing.T) {
	repoDir := t.TempDir()
	upstreamDir := filepath.Join(t.TempDir(), "upstream.git")

	runGitCmd(repoDir, "init", "-b", "main")
	runGitCmd(repoDir, "config", "user.email", "test@test.com")
	runGitCmd(repoDir, "config", "user.name", "Test")
	runGitCmd(repoDir, "commit", "--allow-empty", "-m", "initial")

	runGitCmd("", "init", "--bare", upstreamDir)
	runGitCmd(repoDir, "remote", "add", "upstream", upstreamDir)
	runGitCmd(repoDir, "checkout", "-b", "feature")

	u := &updater{
		appName:  "no-mistakes",
		repoDir:  repoDir,
	}

	err := u.run(context.Background())
	if err == nil {
		t.Fatal("expected error for non-main branch")
	}
	if !strings.Contains(err.Error(), "switch to main") {
		t.Fatalf("error = %v", err)
	}
}

func TestUpdaterRunDirtyWorkingTree(t *testing.T) {
	repoDir := t.TempDir()
	upstreamDir := filepath.Join(t.TempDir(), "upstream.git")

	runGitCmd(repoDir, "init", "-b", "main")
	runGitCmd(repoDir, "config", "user.email", "test@test.com")
	runGitCmd(repoDir, "config", "user.name", "Test")
	runGitCmd(repoDir, "commit", "--allow-empty", "-m", "initial")

	runGitCmd("", "init", "--bare", upstreamDir)
	runGitCmd(repoDir, "remote", "add", "upstream", upstreamDir)

	os.WriteFile(filepath.Join(repoDir, "dirty.txt"), []byte("untracked"), 0o644)
	runGitCmd(repoDir, "add", "dirty.txt")

	u := &updater{
		appName:  "no-mistakes",
		repoDir:  repoDir,
	}

	err := u.run(context.Background())
	if err == nil {
		t.Fatal("expected error for dirty working tree")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("error = %v", err)
	}
}

func TestUpdaterRunRebaseAndInstall(t *testing.T) {
	repoDir := t.TempDir()
	upstreamDir := filepath.Join(t.TempDir(), "upstream.git")
	originDir := filepath.Join(t.TempDir(), "origin.git")

	runGitCmd(repoDir, "init", "-b", "main")
	runGitCmd(repoDir, "config", "user.email", "test@test.com")
	runGitCmd(repoDir, "config", "user.name", "Test")

	runGitCmd("", "init", "--bare", "-b", "main", upstreamDir)
	runGitCmd("", "init", "--bare", "-b", "main", originDir)

	// Create scripts/install-local.sh and commit it as part of the initial commit
	scriptsDir := filepath.Join(repoDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	installScript := filepath.Join(scriptsDir, "install-local.sh")
	if err := os.WriteFile(installScript, []byte("#!/bin/sh\necho installed"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitCmd(repoDir, "add", ".")
	runGitCmd(repoDir, "commit", "-m", "initial")

	runGitCmd(repoDir, "remote", "add", "upstream", upstreamDir)
	runGitCmd(repoDir, "remote", "add", "origin", originDir)
	runGitCmd(repoDir, "push", "-u", "upstream", "main")
	runGitCmd(repoDir, "push", "-u", "origin", "main")

	// Add a custom commit on local
	os.WriteFile(filepath.Join(repoDir, "local.txt"), []byte("custom"), 0o644)
	runGitCmd(repoDir, "add", ".")
	runGitCmd(repoDir, "commit", "-m", "local custom change")

	// Push custom commit to origin (simulate user's fork)
	runGitCmd(repoDir, "push", "-u", "origin", "main")

	// Now upstream has a new commit: clone, add commit, push back
	upstreamClone := t.TempDir()
	runGitCmd(upstreamClone, "clone", "-b", "main", upstreamDir, ".")
	runGitCmd(upstreamClone, "config", "user.email", "test@test.com")
	runGitCmd(upstreamClone, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(upstreamClone, "upstream.txt"), []byte("new"), 0o644)
	runGitCmd(upstreamClone, "add", ".")
	runGitCmd(upstreamClone, "commit", "-m", "upstream change")
	runGitCmd(upstreamClone, "push")

	stdout := new(bytes.Buffer)
	resetCalled := false
	u := &updater{
		appName:  "no-mistakes",
		repoDir:  repoDir,
		stdout:   stdout,
		stdin:    os.Stdin,
		resetDaemon: func() error {
			resetCalled = true
			return nil
		},
	}

	if err := u.run(context.Background()); err != nil {
		t.Fatalf("run error = %v", err)
	}
	if !strings.Contains(stdout.String(), "updated to latest upstream") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !resetCalled {
		t.Fatal("expected daemon reset after update")
	}

	// Verify custom commit is on top after rebase
	log, err := u.gitOutput(repoDir, "log", "--oneline", "-3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log, "local custom change") {
		t.Fatalf("expected custom commit on top, got:\n%s", log)
	}
}

func TestUpdaterMaybeNotifyAndCheck(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "update-check.json")
	if err := writeCache(cachePath, &checkCache{
		CheckedAt:     time.Date(2026, 4, 8, 12, 0, 0, 0, time.UTC),
		LatestVersion: "v1.2.3",
	}); err != nil {
		t.Fatal(err)
	}

	stderr := new(bytes.Buffer)
	spawned := false
	u := &updater{
		appName:        "no-mistakes",
		currentVersion: "v1.2.2",
		cachePath:      cachePath,
		stderr:         stderr,
		now:            func() time.Time { return time.Date(2026, 4, 9, 13, 0, 0, 0, time.UTC) },
		spawnBackground: func(currentVersion string) error {
			spawned = true
			if currentVersion != "v1.2.2" {
				t.Fatalf("currentVersion = %q", currentVersion)
			}
			return nil
		},
	}

	u.maybeNotifyAndCheck([]string{"status"})

	if !strings.Contains(stderr.String(), "A new version of no-mistakes is available: v1.2.2 -> v1.2.3") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if !spawned {
		t.Fatal("expected stale cache to trigger background refresh")
	}

	stderr.Reset()
	spawned = false
	u.maybeNotifyAndCheck([]string{"update"})
	if stderr.Len() != 0 {
		t.Fatalf("update command should not notify, got %q", stderr.String())
	}
	if spawned {
		t.Fatal("update command should not spawn background refresh")
	}
}

func TestUpdaterCheckLatestBetaUsesReleasesList(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	archiveName := "no-mistakes-v1.3.0-beta.1-darwin-arm64.tar.gz"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kunchenguid/no-mistakes/releases":
			fmt.Fprintf(w, `[
				{"tag_name":"v1.3.0-beta.1","draft":false,"prerelease":true,"assets":[{"name":%q,"browser_download_url":"http://example.com/archive"},{"name":"checksums.txt","browser_download_url":"http://example.com/checksums"}]},
				{"tag_name":"v1.2.3","draft":false,"prerelease":false,"assets":[]},
				{"tag_name":"v1.4.0-draft","draft":true,"prerelease":true,"assets":[]}
			]`, archiveName)
		case "/repos/kunchenguid/no-mistakes/tags":
			fmt.Fprint(w, `[{"name":"v1.3.0-beta.1"},{"name":"v1.2.3"}]`)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	u := &updater{
		appName:            "no-mistakes",
		repo:               "kunchenguid/no-mistakes",
		currentVersion:     "v1.2.3",
		platform:           platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		apiBaseURL:         server.URL,
		httpClient:         server.Client(),
		cachePath:          filepath.Join(t.TempDir(), "update-check.json"),
		now:                func() time.Time { return time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC) },
		includePrereleases: true,
	}

	plan, err := u.checkLatest(context.Background())
	if err != nil {
		t.Fatalf("checkLatest error = %v", err)
	}
	if !plan.UpdateAvailable {
		t.Fatal("expected update to be available")
	}
	if plan.LatestVersion != "v1.3.0-beta.1" {
		t.Fatalf("LatestVersion = %q", plan.LatestVersion)
	}
	if plan.ArchiveName != archiveName {
		t.Fatalf("ArchiveName = %q", plan.ArchiveName)
	}
}

func TestUpdaterCheckLatestBetaPicksHighestSemver(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	archiveName := "no-mistakes-v1.3.0-beta.2-darwin-arm64.tar.gz"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kunchenguid/no-mistakes/releases":
			fmt.Fprintf(w, `[
				{"tag_name":"v1.3.0-beta.1","draft":false,"prerelease":true,"assets":[]},
				{"tag_name":"v1.3.0-beta.2","draft":false,"prerelease":true,"assets":[{"name":%q,"browser_download_url":"http://example.com/archive"},{"name":"checksums.txt","browser_download_url":"http://example.com/checksums"}]},
				{"tag_name":"v1.2.3","draft":false,"prerelease":false,"assets":[]}
			]`, archiveName)
		case "/repos/kunchenguid/no-mistakes/tags":
			fmt.Fprint(w, `[{"name":"v1.3.0-beta.2"},{"name":"v1.3.0-beta.1"},{"name":"v1.2.3"}]`)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	u := &updater{
		appName:            "no-mistakes",
		repo:               "kunchenguid/no-mistakes",
		currentVersion:     "v1.2.3",
		platform:           platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		apiBaseURL:         server.URL,
		httpClient:         server.Client(),
		cachePath:          filepath.Join(t.TempDir(), "update-check.json"),
		now:                func() time.Time { return time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC) },
		includePrereleases: true,
	}

	plan, err := u.checkLatest(context.Background())
	if err != nil {
		t.Fatalf("checkLatest error = %v", err)
	}
	if plan.LatestVersion != "v1.3.0-beta.2" {
		t.Fatalf("LatestVersion = %q", plan.LatestVersion)
	}
}

func TestUpdaterCheckLatestBetaFallsBackToTagsWhenListingStale(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	archiveName := "no-mistakes-v1.3.0-beta.1-darwin-arm64.tar.gz"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kunchenguid/no-mistakes/releases":
			fmt.Fprint(w, `[
				{"tag_name":"v1.2.3","draft":false,"prerelease":false,"assets":[]},
				{"tag_name":"v1.2.2","draft":false,"prerelease":false,"assets":[]}
			]`)
		case "/repos/kunchenguid/no-mistakes/tags":
			fmt.Fprint(w, `[{"name":"v1.3.0-beta.1"},{"name":"v1.2.3"},{"name":"v1.2.2"}]`)
		case "/repos/kunchenguid/no-mistakes/releases/tags/v1.3.0-beta.1":
			fmt.Fprintf(w, `{"tag_name":"v1.3.0-beta.1","draft":false,"prerelease":true,"assets":[{"name":%q,"browser_download_url":"http://example.com/archive"},{"name":"checksums.txt","browser_download_url":"http://example.com/checksums"}]}`, archiveName)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	u := &updater{
		appName:            "no-mistakes",
		repo:               "kunchenguid/no-mistakes",
		currentVersion:     "v1.2.3",
		platform:           platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		apiBaseURL:         server.URL,
		httpClient:         server.Client(),
		cachePath:          filepath.Join(t.TempDir(), "update-check.json"),
		now:                func() time.Time { return time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC) },
		includePrereleases: true,
	}

	plan, err := u.checkLatest(context.Background())
	if err != nil {
		t.Fatalf("checkLatest error = %v", err)
	}
	if !plan.UpdateAvailable {
		t.Fatal("expected update to be available")
	}
	if plan.LatestVersion != "v1.3.0-beta.1" {
		t.Fatalf("LatestVersion = %q", plan.LatestVersion)
	}
	if plan.ArchiveName != archiveName {
		t.Fatalf("ArchiveName = %q", plan.ArchiveName)
	}
}

func TestUpdaterCheckLatestBetaChecksListedReleaseAfterMissingTags(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	archiveName := "no-mistakes-v1.3.0-beta.1-darwin-arm64.tar.gz"
	tagFetches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kunchenguid/no-mistakes/releases":
			fmt.Fprintf(w, `[
				{"tag_name":"v1.3.0-beta.1","draft":false,"prerelease":true,"assets":[{"name":%q,"browser_download_url":"http://example.com/archive"},{"name":"checksums.txt","browser_download_url":"http://example.com/checksums"}]},
				{"tag_name":"v1.2.3","draft":false,"prerelease":false,"assets":[]}
			]`, archiveName)
		case "/repos/kunchenguid/no-mistakes/tags":
			fmt.Fprint(w, `[
				{"name":"v1.3.0-beta.6"},
				{"name":"v1.3.0-beta.5"},
				{"name":"v1.3.0-beta.4"},
				{"name":"v1.3.0-beta.3"},
				{"name":"v1.3.0-beta.2"},
				{"name":"v1.3.0-beta.1"},
				{"name":"v1.2.3"}
			]`)
		default:
			if strings.HasPrefix(r.URL.Path, "/repos/kunchenguid/no-mistakes/releases/tags/") {
				tagFetches++
				http.NotFound(w, r)
				return
			}
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	u := &updater{
		appName:            "no-mistakes",
		repo:               "kunchenguid/no-mistakes",
		currentVersion:     "v1.2.3",
		platform:           platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		apiBaseURL:         server.URL,
		httpClient:         server.Client(),
		cachePath:          filepath.Join(t.TempDir(), "update-check.json"),
		now:                func() time.Time { return time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC) },
		includePrereleases: true,
	}

	plan, err := u.checkLatest(context.Background())
	if err != nil {
		t.Fatalf("checkLatest error = %v", err)
	}
	if plan.LatestVersion != "v1.3.0-beta.1" {
		t.Fatalf("LatestVersion = %q", plan.LatestVersion)
	}
	if tagFetches != 5 {
		t.Fatalf("tagFetches = %d, want 5", tagFetches)
	}
}

func TestUpdaterCachedLatestVersion(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "update-check.json")
	if err := writeCache(cachePath, &checkCache{
		CheckedAt:     time.Date(2026, 4, 8, 12, 0, 0, 0, time.UTC),
		LatestVersion: "v1.2.3",
	}); err != nil {
		t.Fatal(err)
	}

	u := &updater{
		currentVersion: "v1.2.2",
		cachePath:      cachePath,
	}

	if got := u.cachedLatestVersion(); got != "v1.2.3" {
		t.Fatalf("cachedLatestVersion() = %q, want %q", got, "v1.2.3")
	}

	u.currentVersion = "v1.2.3"
	if got := u.cachedLatestVersion(); got != "" {
		t.Fatalf("cachedLatestVersion() = %q, want empty when already current", got)
	}
}

func runGitCmd(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("git %s in %s: %s\n%s", strings.Join(args, " "), dir, err, string(out)))
	}
}
