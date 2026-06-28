package update

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

const (
	appName            = "no-mistakes"
	repoName           = "kunchenguid/no-mistakes"
	backgroundFlag     = "--update-check"
	noUpdateCheckEnv   = "NO_MISTAKES_NO_UPDATE_CHECK"
	checksumsAssetName = "checksums.txt"
	cacheTTL           = 24 * time.Hour
	maxAPIResponseSize = 5 << 20
	maxDownloadSize    = 100 << 20
	maxExtractedSize   = 100 << 20
)

var allowInsecureDownloads bool
var githubAPIBaseURL = "https://api.github.com"
var currentGOOS = runtime.GOOS
var daemonIsRunning = daemon.IsRunning
var daemonExecutablePath = runningDaemonExecutablePath
var daemonStop = daemon.Stop
var daemonStart = daemon.Start
var windowsExecutablePathForPID = defaultWindowsExecutablePathForPID

type platformSpec struct {
	GOOS   string
	GOARCH string
}

type updater struct {
	appName            string
	repo               string
	currentVersion     string
	platform           platformSpec
	apiBaseURL         string
	httpClient         *http.Client
	cachePath          string
	executablePath     string
	stdin              io.Reader
	stdout             io.Writer
	stderr             io.Writer
	now                func() time.Time
	spawnBackground    func(currentVersion string) error
	resetDaemon        func() error
	paths              *paths.Paths
	disableBackground  bool
	noColor            bool
	includePrereleases bool
	assumeYes          bool
	repoDir            string
}

type RunOptions struct {
	Stdin io.Reader
}

func Run(ctx context.Context, stdout, stderr io.Writer, opts RunOptions) error {
	u, err := defaultUpdater(stdout, stderr)
	if err != nil {
		return err
	}
	if opts.Stdin != nil {
		u.stdin = opts.Stdin
	}
	return u.run(ctx)
}

func MaybeHandleBackgroundCheck(args []string) (bool, error) {
	if len(args) != 2 || args[0] != backgroundFlag {
		return false, nil
	}
	u, err := defaultUpdater(io.Discard, io.Discard)
	if err != nil {
		return true, err
	}
	u.currentVersion = args[1]
	return true, u.refreshCache(context.Background())
}

func MaybeNotifyAndCheck(args []string, stderr io.Writer) {
	u, err := defaultUpdater(io.Discard, stderr)
	if err != nil {
		return
	}
	u.maybeNotifyAndCheck(args)
}

func CachedLatestVersion() string {
	u, err := defaultUpdater(io.Discard, io.Discard)
	if err != nil {
		return ""
	}
	return u.cachedLatestVersion()
}

func defaultUpdater(stdout, stderr io.Writer) (*updater, error) {
	p, err := paths.New()
	if err != nil {
		return nil, fmt.Errorf("resolve paths: %w", err)
	}
	execPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	return &updater{
		appName:         appName,
		repo:            repoName,
		currentVersion:  buildinfo.CurrentVersion(),
		platform:        platformSpec{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH},
		apiBaseURL:      githubAPIBaseURL,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		cachePath:       p.UpdateCheckFile(),
		executablePath:  execPath,
		stdin:           os.Stdin,
		stdout:          stdout,
		stderr:          stderr,
		now:             time.Now,
		paths:           p,
		spawnBackground: defaultSpawnBackground,
		resetDaemon: func() error {
			return defaultResetDaemon(p)
		},
		repoDir: os.Getenv("NM_REPO_DIR"),
	}, nil
}

func (u *updater) refreshCache(ctx context.Context) error {
	plan, err := u.checkLatest(ctx)
	if err != nil {
		return err
	}
	return writeCache(u.cachePath, &checkCache{
		CheckedAt:     u.now(),
		LatestVersion: plan.LatestVersion,
	})
}

func (u *updater) maybeNotifyAndCheck(args []string) {
	if u.disableBackground || isDevVersion(u.currentVersion) || os.Getenv(noUpdateCheckEnv) == "1" {
		return
	}
	if len(args) > 0 && (args[0] == "update" || args[0] == backgroundFlag) {
		return
	}
	cache := readCache(u.cachePath)
	if cache != nil {
		cmp, err := compareVersions(u.currentVersion, cache.LatestVersion)
		if err == nil && cmp < 0 {
			fmt.Fprintf(u.stderrWriter(), "%sA new version of %s is available: %s -> %s\nRun \"%s update\" to update%s\n", u.yellow(), u.appName, u.currentVersion, cache.LatestVersion, u.appName, u.reset())
		}
	}
	if cacheStale(cache, u.currentVersion, u.now()) && u.spawnBackground != nil {
		_ = u.spawnBackground(u.currentVersion)
	}
}

func (u *updater) cachedLatestVersion() string {
	if u == nil || u.disableBackground || isDevVersion(u.currentVersion) || os.Getenv(noUpdateCheckEnv) == "1" {
		return ""
	}
	cache := readCache(u.cachePath)
	if cache == nil {
		return ""
	}
	cmp, err := compareVersions(u.currentVersion, cache.LatestVersion)
	if err != nil || cmp >= 0 {
		return ""
	}
	return cache.LatestVersion
}

func (u *updater) run(ctx context.Context) error {
	repoDir, err := u.resolveRepoDir()
	if err != nil {
		return err
	}

	if err := u.verifyRepoPreconditions(repoDir); err != nil {
		return err
	}

	fmt.Fprintf(u.stdoutWriter(), "fetching upstream...\n")
	if err := u.runGit(ctx, repoDir, "fetch", "upstream"); err != nil {
		return fmt.Errorf("fetch upstream failed: %w", err)
	}

	behind, err := u.behindUpstream(repoDir)
	if err != nil {
		return err
	}

	if behind == 0 {
		fmt.Fprintf(u.stdoutWriter(), "%s is already up to date with upstream\n", u.appName)
		return nil
	}

	fmt.Fprintf(u.stdoutWriter(), "rebasing onto upstream/main (%d commits behind)...\n", behind)
	if err := u.runGit(ctx, repoDir, "rebase", "upstream/main"); err != nil {
		return fmt.Errorf(
			"rebase failed — resolve conflicts manually, then:\n"+
				"  git rebase --continue\n"+
				"  ./scripts/install-local.sh",
		)
	}

	fmt.Fprintf(u.stdoutWriter(), "pushing to origin/main (force-with-lease after rebase)...\n")
	if err := u.runGit(ctx, repoDir, "push", "--force-with-lease", "origin", "main"); err != nil {
		return fmt.Errorf("push failed: %w", err)
	}

	fmt.Fprintf(u.stdoutWriter(), "building and installing...\n")
	installScript := filepath.Join(repoDir, "scripts", "install-local.sh")
	cmd := exec.CommandContext(ctx, installScript)
	cmd.Dir = repoDir
	cmd.Stdout = u.stdoutWriter()
	cmd.Stderr = u.stderrWriter()
	cmd.Stdin = u.stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("install-local failed: %w", err)
	}

	if u.resetDaemon != nil {
		if err := u.resetDaemon(); err != nil {
			return fmt.Errorf("update succeeded, but daemon restart failed: %w", err)
		}
	}

	fmt.Fprintf(u.stdoutWriter(), "%s updated to latest upstream\n", u.appName)
	return nil
}

func (u *updater) stdoutWriter() io.Writer {
	if u.stdout == nil {
		return io.Discard
	}
	return u.stdout
}

func (u *updater) stderrWriter() io.Writer {
	if u.stderr == nil {
		return io.Discard
	}
	return u.stderr
}

func (u *updater) yellow() string {
	if u.noColor {
		return ""
	}
	return "\033[33m"
}

func (u *updater) reset() string {
	if u.noColor {
		return ""
	}
	return "\033[0m"
}

func (u *updater) resolveRepoDir() (string, error) {
	if u.repoDir != "" {
		if fi, err := os.Stat(filepath.Join(u.repoDir, ".git")); err == nil && fi.IsDir() {
			return u.repoDir, nil
		}
		return "", fmt.Errorf("NM_REPO_DIR=%s is not a git repository", u.repoDir)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cannot determine current directory: %w", err)
	}
	if isGitRepoWithRemote(cwd) {
		return cwd, nil
	}

	return "", fmt.Errorf("cannot find repo directory: set NM_REPO_DIR or run from the repo directory")
}

func isGitRepoWithRemote(dir string) bool {
	dotGit := filepath.Join(dir, ".git")
	if fi, err := os.Stat(dotGit); err != nil || !fi.IsDir() {
		return false
	}
	cmd := exec.Command("git", "remote", "get-url", "upstream")
	cmd.Dir = dir
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

func (u *updater) verifyRepoPreconditions(repoDir string) error {
	branch, err := u.gitOutput(repoDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("check branch: %w", err)
	}
	if strings.TrimSpace(branch) != "main" {
		return fmt.Errorf("not on main branch (current: %q) — switch to main first", strings.TrimSpace(branch))
	}

	status, err := u.gitOutput(repoDir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("check working tree: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("working tree has uncommitted changes — commit or stash first")
	}

	remote, err := u.gitOutput(repoDir, "remote", "get-url", "upstream")
	if err != nil || strings.TrimSpace(remote) == "" {
		return fmt.Errorf(
			"upstream remote not configured — add it with:\n" +
				"  git remote add upstream https://github.com/kunchenguid/no-mistakes.git",
		)
	}

	return nil
}

func (u *updater) behindUpstream(repoDir string) (int, error) {
	behind, err := u.gitOutput(repoDir, "rev-list", "--count", "HEAD..upstream/main")
	if err != nil {
		return 0, fmt.Errorf("count behind commits: %w", err)
	}
	behind = strings.TrimSpace(behind)
	if behind == "" {
		return 0, nil
	}
	var n int
	if _, err := fmt.Sscanf(behind, "%d", &n); err != nil {
		return 0, fmt.Errorf("parse behind count %q: %w", behind, err)
	}
	return n, nil
}

func (u *updater) runGit(ctx context.Context, repoDir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoDir
	cmd.Stdout = u.stdoutWriter()
	cmd.Stderr = u.stderrWriter()
	return cmd.Run()
}

func (u *updater) gitOutput(repoDir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s failed: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s failed: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
