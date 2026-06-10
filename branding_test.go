package transcoder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNoUpstreamProductBrandingInPublicProject(t *testing.T) {
	forbidden := strings.ToLower("jelly" + "fin")
	roots := []string{".", "server", filepath.Join("cmd", "transcode-server")}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !utf8.Valid(data) {
				return nil
			}
			if strings.Contains(strings.ToLower(string(data)), forbidden) {
				t.Fatalf("forbidden upstream product branding appears in %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
