package updater

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestLaunchArgumentsRoundTrip(t *testing.T) {
	for _, args := range [][]string{nil, {}, {""}, {"--connect", "gpu", "--config-root", "/a b/雪", "", "quote'\"line\n", string([]byte{0xff, 'x'})}} {
		got, err := decodeLaunchArgs(encodeLaunchArgs(args))
		if err != nil || !slices.Equal(got, args) {
			t.Fatalf("argument round trip failed: %v", err)
		}
	}
	for _, raw := range []string{"!", "YQ=="} {
		if _, err := decodeLaunchArgs(raw); err == nil {
			t.Fatal("malformed launch state accepted")
		}
	}
}

func TestRelaunchCommandPreservesArgvOnEveryPlatform(t *testing.T) {
	args := []string{"--connect", "gpu with spaces", "--config-root", "/a'\"b", ""}
	for _, platform := range []string{"darwin", "linux", "windows"} {
		for _, path := range []string{"/Applications/My App.app", "/opt/My App/bin/app"} {
			want := append([]string{path}, args...)
			if platform == "darwin" && strings.HasSuffix(path, ".app") {
				want = append([]string{"open", "-n", path, "--args"}, args...)
			}
			if got := relaunchCommand(path, args, platform).Args; !reflect.DeepEqual(got, want) {
				t.Fatalf("%s argv mismatch: %v", platform, got)
			}
		}
	}
	if got := relaunchCommand("/Applications/My App.app", nil, "darwin").Args; !reflect.DeepEqual(got, []string{"open", "-n", "/Applications/My App.app"}) {
		t.Fatalf("argument-free bundle launch: %v", got)
	}
}

func TestHelperClearsLaunchMetadataEvenBeforeAFailedSwap(t *testing.T) {
	keys := []string{envHelperMode, envHelperTarget, envHelperNew, envHelperPID, envHelperLog, envHelperArgs}
	for _, key := range keys {
		t.Setenv(key, "fixture")
	}
	if code := runHelperSwap(t.TempDir()+"/missing", "", 0, "", instantWaiter, &fakeLauncher{}); code != 10 {
		t.Fatalf("unexpected failure code %d", code)
	}
	for _, key := range keys {
		if os.Getenv(key) != "" {
			t.Errorf("helper metadata %s survived failure", key)
		}
	}
}

func TestConfigRefusesNULArguments(t *testing.T) {
	cfg := Config{CurrentVersion: "1.0.0", RelaunchArgs: []string{"x\x00y"}}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("NUL argument validation: %v", err)
	}
}
