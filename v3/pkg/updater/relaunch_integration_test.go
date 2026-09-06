package updater_test

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

func TestRestartCarriesTheConfiguredLaunchArguments(t *testing.T) {
	original := []string{"--connect", "gpu", "--config-root", "/a b/雪", ""}
	for _, tc := range []struct {
		name     string
		override []string
	}{
		{"original", nil}, {"normalized", []string{"--frontend", "--config-root", "/a b"}}, {"cleared", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(updater.SetSelfExecutableForTest(os.Executable))
			t.Cleanup(updater.SetSelfArgumentsForTest(func() []string { return original }))
			var spawned *exec.Cmd
			t.Cleanup(updater.SetNewDetachedCommandForTest(func(path string) *exec.Cmd {
				spawned = exec.Command(path, "-test.run=^$")
				return spawned
			}))
			body := []byte("payload")
			provider := &fakeProvider{name: "p", body: body, rel: &updater.Release{Version: "2.0.0", Artifact: updater.Artifact{Filename: "app.bin", Size: int64(len(body))}}}
			host := &fakeHost{}
			u := updater.New(host)
			want := slices.Clone(original)
			if tc.override != nil {
				want = slices.Clone(tc.override)
			}
			if err := u.Init(updater.Config{CurrentVersion: "1.0.0", Providers: []updater.Provider{provider}, RelaunchArgs: tc.override}); err != nil {
				t.Fatal(err)
			}
			// Init must own its argument snapshot, not a caller's mutable slice.
			if len(tc.override) > 0 {
				tc.override[0] = "mutated"
			}
			if _, err := u.Check(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := u.DownloadAndInstall(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := u.Restart(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = spawned.Wait() })
			var raw string
			for _, entry := range spawned.Env {
				if value, ok := strings.CutPrefix(entry, "WAILS_UPDATER_HELPER_ARGS="); ok {
					raw = value
				}
			}
			var got []string
			if raw != "" {
				decoded, err := base64.StdEncoding.DecodeString(raw)
				if err != nil || len(decoded) == 0 || decoded[len(decoded)-1] != 0 {
					t.Fatal("invalid helper launch metadata")
				}
				got = strings.Split(string(decoded[:len(decoded)-1]), "\x00")
			}
			if !slices.Equal(got, want) {
				t.Fatalf("relaunch lost arguments: %v", got)
			}
		})
	}
}
