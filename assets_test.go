package assets

import (
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Treat the embedded filesystem as a distribution boundary: adding a helper
// requires an intentional allowlist change, and private files must never ride
// along with an overly broad embed pattern.
func TestEmbeddedAssetsAreOnlyRequiredPublicRuntimeFiles(t *testing.T) {
	var names []string
	err := fs.WalkDir(Files, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			names = append(names, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		".env.example",
		"deploy/journald@mcs-bootloader.conf",
		"deploy/mcs-bootloader-backend.service",
		"deploy/mcs-bootloader.service",
		"scripts/env_config.py",
		"scripts/install.py",
		"scripts/manage.py",
		"scripts/remove.py",
		"scripts/service_config.py",
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("embedded files = %v; want explicit public allowlist %v", names, want)
	}
	example, err := Files.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(example), "replace-with-a-random-private-password") != 1 {
		t.Fatal("embedded example must contain exactly the safe initialization password marker")
	}
}
