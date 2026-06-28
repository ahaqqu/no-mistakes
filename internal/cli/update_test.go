package cli

import (
	"strings"
	"testing"
)

func TestUpdateCommandNoRepoDir(t *testing.T) {
	_, err := executeCmd("update")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cannot find repo directory") &&
		!strings.Contains(msg, "NM_REPO_DIR") {
		t.Fatalf("expected repo-dir error, got: %s", msg)
	}
}
