package bt

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSymlinkBypassVerify(t *testing.T) {
	allowedDir := t.TempDir()
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.go")
	if err := os.WriteFile(secret, []byte("TOP\nSECRET\nDATA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// symlink under allowed root pointing to secret outside it
	link := filepath.Join(allowedDir, "linked.go")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}

	stack := "goroutine 1 [running]:\n" +
		"main.a()\n" +
		"\t" + link + ":2 +0x1\n"

	threads, sources, _ := buildThreads([]byte(stack), sourceOptions{
		mode: SourceCodeContext, contextLines: 2, tabWidth: 8,
		roots: []string{allowedDir},
	})
	frames := threads["0"].Stacks
	got := sources[frames[0].SourceCodeID].Text
	t.Logf("embedded text via symlink: %q", got)
	if got != "" {
		t.Errorf("BYPASS CONFIRMED: symlink leaked target content: %q", got)
	}
}
