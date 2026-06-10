package transcoder

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeCodeDoesNotImportOSExec(t *testing.T) {
	roots := []string{".", "server", filepath.Join("cmd", "transcode-server")}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(root, e.Name())
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				if strings.Trim(imp.Path.Value, "\"") == "os/exec" {
					t.Fatalf("runtime file imports os/exec: %s", path)
				}
			}
		}
	}
}
