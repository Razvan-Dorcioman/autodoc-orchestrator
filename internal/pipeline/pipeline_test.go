package pipeline

import (
	"strings"
	"testing"

	"autodoc/internal/gofacts"
)

func TestEntrypoint(t *testing.T) {
	f := &gofacts.Facts{Mains: []gofacts.Main{
		{File: "cmd/api/main.go", ServesRoutes: true},
		{File: "cmd/worker/main.go"},
	}}
	for _, tt := range []struct {
		override, want, err string
	}{
		{override: "", want: "cmd/api/main.go"},
		{override: "cmd/worker/main.go", want: "cmd/worker/main.go"},
		{override: "./cmd/worker/main.go", want: "cmd/worker/main.go"},
		{override: "main.go", err: "cmd/api/main.go, cmd/worker/main.go"},
	} {
		got, err := entrypoint(f, tt.override)
		if got != tt.want || (err == nil) != (tt.err == "") || (err != nil && !strings.Contains(err.Error(), tt.err)) {
			t.Errorf("entrypoint(%q) = %q, %v; want %q, error containing %q", tt.override, got, err, tt.want, tt.err)
		}
	}
}
