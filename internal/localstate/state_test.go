package localstate_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cwei9203/pi-desktop-gui/internal/localstate"
)

func TestSettingsSurviveSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "settings.json")
	want := localstate.Data{
		PiPath:      "/Users/test/.local/bin/pi",
		Projects:    []string{"/projects/中文项目"},
		LastProject: "/projects/中文项目",
		Sessions: []localstate.Session{{
			Project: "/projects/中文项目",
			Path:    "/sessions/conversation.jsonl",
			Name:    "修复输入",
			Updated: "2026-10-22T12:00:00+08:00",
		}},
	}
	if err := localstate.Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := localstate.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded settings = %#v, want %#v", got, want)
	}
	for target, wantMode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != wantMode {
			t.Fatalf("%s permissions = %o, want %o", target, info.Mode().Perm(), wantMode)
		}
	}
}

func TestMissingSettingsStartEmpty(t *testing.T) {
	got, err := localstate.Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, localstate.Data{}) {
		t.Fatalf("missing settings = %#v, want empty data", got)
	}
}

func TestCorruptSettingsAreReportedAndPreserved(t *testing.T) {
	for _, contents := range []string{"{broken", "null", `{"piPath":7}`, `{} {}`} {
		t.Run(contents, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := localstate.Load(path); err == nil {
				t.Fatal("loading corrupt settings succeeded")
			}
			if err := localstate.Save(path, localstate.Data{PiPath: "/new/pi"}); err == nil {
				t.Fatal("overwriting corrupt settings succeeded")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != contents {
				t.Fatal("corrupt settings were modified")
			}
		})
	}
}

func TestFailedSaveKeepsPreviousSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	want := localstate.Data{PiPath: "/working/pi"}
	if err := localstate.Save(path, want); err != nil {
		t.Fatal(err)
	}
	tooLarge := localstate.Data{PiPath: strings.Repeat("x", 4*1024*1024)}
	if err := localstate.Save(path, tooLarge); err == nil {
		t.Fatal("saving more than 4 MiB succeeded")
	}
	got, err := localstate.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("failed save changed settings: %#v", got)
	}
}

func TestOversizedSettingsCannotBeLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	contents := `{"piPath":"` + strings.Repeat("x", 4*1024*1024) + `"}`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := localstate.Load(path); err == nil {
		t.Fatal("loading more than 4 MiB succeeded")
	}
}
