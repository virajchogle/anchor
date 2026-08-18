package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEmbeddedPageJavaScriptParses guards against shipping a page whose script
// does not parse.
//
// This exists because it actually happened. A stray backslash inside a
// single-quoted string ended the string early, the whole script failed to parse,
// and the page rendered completely blank. Nothing in the Go build catches that:
// the HTML is an opaque embedded blob, the binary compiles, the server returns
// 200, and the failure only appears in a browser console.
//
// The test shells out to node when it is available and skips otherwise, so it
// never blocks a build on a machine without it.
func TestEmbeddedPageJavaScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JavaScript syntax check")
	}

	page, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatalf("reading embedded page: %v", err)
	}

	scripts := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllSubmatch(page, -1)
	if len(scripts) == 0 {
		t.Fatal("no <script> block found in the embedded page")
	}

	for i, m := range scripts {
		js := m[1]
		path := filepath.Join(t.TempDir(), "panel.js")
		if err := os.WriteFile(path, js, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(node, "--check", path).CombinedOutput()
		if err != nil {
			t.Errorf("script block %d does not parse, which renders the page blank:\n%s", i, out)
		}
	}
}

// TestEmbeddedPageIsWellFormed catches truncation and unbalanced markup, which
// would also produce a blank page while the server still returns 200.
func TestEmbeddedPageIsWellFormed(t *testing.T) {
	page, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(page)

	if !strings.HasSuffix(strings.TrimSpace(s), "</html>") {
		t.Error("page does not end with </html>; it may have been truncated")
	}
	for _, tag := range []string{"script", "style", "body", "html"} {
		open := strings.Count(s, "<"+tag)
		close := strings.Count(s, "</"+tag+">")
		if open != close {
			t.Errorf("unbalanced <%s>: %d opening, %d closing", tag, open, close)
		}
	}
	// Every view the navigation offers must exist, or a nav click shows nothing.
	navs := regexp.MustCompile(`href="#/([a-z]+)"`).FindAllStringSubmatch(s, -1)
	if len(navs) == 0 {
		t.Fatal("no navigation links found")
	}
	for _, n := range navs {
		if !strings.Contains(s, `data-v="`+n[1]+`"`) {
			t.Errorf("navigation links to #/%s but no view declares data-v=%q", n[1], n[1])
		}
	}
}
