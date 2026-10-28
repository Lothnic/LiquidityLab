package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestFileSHA256FixtureAndSizeBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture")
	if err := os.WriteFile(path, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	sum, n, err := fileSHA256(path)
	if err != nil || sum != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" || n != 3 {
		t.Fatalf("sum=%s bytes=%d err=%v", sum, n, err)
	}
	if err = os.Truncate(path, maxExecutableBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err = fileSHA256(path); err == nil {
		t.Fatal("oversized input accepted")
	}
}

func TestBuildMetadataIsSanitizedAndUnknownVCSExplicit(t *testing.T) {
	info := &debug.BuildInfo{GoVersion: "go-test", Path: "example.org/tool", Main: debug.Module{Path: "example.org/main", Version: "(devel)"}, Deps: []*debug.Module{{Path: "example.org/dep", Version: "v1.0.0", Sum: "h1:test", Replace: &debug.Module{Path: "/private/local/source"}}}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "true"}, {Key: "GOOS", Value: "linux"}, {Key: "-ldflags", Value: "secret-linker-value"}, {Key: "DATABASE_PASSWORD", Value: "secret-environment-value"}}}
	identity := sanitizedBuildInfo(info, true)
	if !identity.Available || identity.VCSStatus != "available" || identity.Settings["vcs.revision"] != "abc123" || len(identity.Dependencies) != 1 {
		t.Fatalf("unexpected build identity: %+v", identity)
	}
	b, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-linker-value", "secret-environment-value", "/private/local/source"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("sensitive metadata retained: %s", secret)
		}
	}
	info.Settings = nil
	if got := sanitizedBuildInfo(info, true); !got.Available || got.VCSStatus != "unavailable" {
		t.Fatalf("unstamped binary inferred VCS: %+v", got)
	}
	if got := sanitizedBuildInfo(nil, false); got.Available || got.VCSStatus != "unavailable" {
		t.Fatalf("missing build metadata: %+v", got)
	}
}

func TestHostBuildMetadataAndExecutableHashShape(t *testing.T) {
	host := hostInfo()
	build, ok := host["binary_build"].(buildIdentity)
	if !ok {
		t.Fatal("host omitted binary build identity")
	}
	if build.VCSStatus != "available" && build.VCSStatus != "unavailable" {
		t.Fatalf("unknown VCS status: %s", build.VCSStatus)
	}
	if build.Settings == nil || build.Dependencies == nil {
		t.Fatal("build metadata omitted bounded collections")
	}
	executable, ok := host["executable"].(executableIdentity)
	if !ok || executable.Status != "available" || executable.Bytes <= 0 {
		t.Fatalf("running executable checksum unavailable: %+v", executable)
	}
	digest, err := hex.DecodeString(executable.SHA256)
	if err != nil || len(digest) != 32 {
		t.Fatalf("invalid SHA-256: %q", executable.SHA256)
	}
	for _, key := range []string{"launch_worktree_status", "launch_worktree_revision", "launch_worktree_dirty"} {
		if _, ok := host[key]; !ok {
			t.Fatalf("host omitted launch context %s", key)
		}
	}
	if _, ok := host["git_revision"]; ok {
		t.Fatal("ambiguous Git revision field retained")
	}
}
