package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "# comment\n\nAUTODOC_T_PLAIN=a=b\nexport AUTODOC_T_QUOTED=\"q v\"\nAUTODOC_T_SHELL=from-file\nAUTODOC_T_EMPTY=\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTODOC_T_SHELL", "from-shell")
	for _, k := range []string{"AUTODOC_T_PLAIN", "AUTODOC_T_QUOTED", "AUTODOC_T_EMPTY"} {
		t.Setenv(k, "") // registers cleanup
		os.Unsetenv(k)
	}

	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"AUTODOC_T_PLAIN":  "a=b",
		"AUTODOC_T_QUOTED": "q v",
		"AUTODOC_T_SHELL":  "from-shell",
		"AUTODOC_T_EMPTY":  "",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Errorf("missing file: %v", err)
	}
}

func TestLoadDotEnvBadLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("JUSTAKEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadDotEnv(path); err == nil {
		t.Error("want error for a line without =")
	}
}
