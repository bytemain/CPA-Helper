package app

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestDetectRepoRootFromPrefersProjectAncestor(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "backend", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "frontend"), 0o755); err != nil {
		t.Fatal(err)
	}

	cwd := filepath.Join(root, "backend", "bin")
	executablePath := filepath.Join(cwd, "cpa-helper.exe")
	got, err := detectRepoRootFrom(cwd, executablePath)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("repo root = %q, want %q", got, root)
	}
}

func TestDetectRepoRootFromFallsBackToExecutableDir(t *testing.T) {
	cwd := t.TempDir()
	releaseDir := t.TempDir()
	executablePath := filepath.Join(releaseDir, "cpa-helper.exe")

	got, err := detectRepoRootFrom(cwd, executablePath)
	if err != nil {
		t.Fatal(err)
	}
	if got != releaseDir {
		t.Fatalf("repo root = %q, want %q", got, releaseDir)
	}
}

func TestHandleSPAServesEmbeddedFrontendAsset(t *testing.T) {
	app := &App{frontendFS: fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<html>embedded</html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log('embedded')")},
	}}

	req := httptest.NewRequest("GET", "http://example.com/assets/app.js", nil)
	recorder := httptest.NewRecorder()
	if err := app.handleSPA(recorder, req); err != nil {
		t.Fatal(err)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "console.log('embedded')") {
		t.Fatalf("body = %q", body)
	}
}

func TestHandleSPAFallsBackToEmbeddedIndex(t *testing.T) {
	app := &App{frontendFS: fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>embedded index</html>")},
	}}

	req := httptest.NewRequest("GET", "http://example.com/settings/account", nil)
	recorder := httptest.NewRecorder()
	if err := app.handleSPA(recorder, req); err != nil {
		t.Fatal(err)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "embedded index") {
		t.Fatalf("body = %q", body)
	}
}

func TestHandleSPAFrontendDistOverrideUsesExternalFiles(t *testing.T) {
	distDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(distDir, "index.html"), []byte("<html>external</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{
		frontendDist: distDir,
		frontendEnv:  true,
		frontendFS: fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte("<html>embedded</html>")},
		},
	}

	req := httptest.NewRequest("GET", "http://example.com/", nil)
	recorder := httptest.NewRecorder()
	if err := app.handleSPA(recorder, req); err != nil {
		t.Fatal(err)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "external") || strings.Contains(body, "embedded") {
		t.Fatalf("body = %q", body)
	}
}

func TestReplaceTitleTag(t *testing.T) {
	tests := []struct {
		name  string
		input string
		title string
		want  string
	}{
		{
			name:  "replaces existing title",
			input: `<html><head><title>CPA-Helper</title></head></html>`,
			title: "My Product",
			want:  `<html><head><title>My Product</title></head></html>`,
		},
		{
			name:  "no title tag returns unchanged",
			input: `<html><head></head></html>`,
			title: "My Product",
			want:  `<html><head></head></html>`,
		},
		{
			name:  "no closing tag returns unchanged",
			input: `<html><head><title>CPA-Helper</head></html>`,
			title: "My Product",
			want:  `<html><head><title>CPA-Helper</head></html>`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(replaceTitleTag([]byte(tc.input), tc.title))
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInjectBrandingNilDB(t *testing.T) {
	// When a.db is nil (no database), injectBranding returns the HTML unchanged.
	app := &App{}
	input := `<html><head><title>CPA-Helper</title><link rel="icon" href="__CPA_HELPER_LOGO_URL__"/></head></html>`
	got := string(app.injectBranding(context.Background(), []byte(input)))
	if got != input {
		t.Fatalf("expected unchanged HTML when db is nil, got %q", got)
	}
}

func TestHandleSPAInjectsTitle(t *testing.T) {
	// When db is nil the HTML is served as-is (no injection panic).
	app := &App{frontendFS: fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(`<html><head><title>CPA-Helper</title></head></html>`)},
	}}

	req := httptest.NewRequest("GET", "http://example.com/", nil)
	recorder := httptest.NewRecorder()
	if err := app.handleSPA(recorder, req); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "<title>CPA-Helper</title>") {
		t.Fatalf("body = %q", body)
	}
}
