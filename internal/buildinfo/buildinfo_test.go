package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestFromBuildInfo(t *testing.T) {
	bi := &debug.BuildInfo{
		GoVersion: "go1.27.1",
		Main:      debug.Module{Version: "v0.1.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "f8cc52db080d990b0f1718e611331db5f9d10cdc"},
			{Key: "vcs.time", Value: "2026-09-15T07:22:47Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	got := FromBuildInfo(bi)
	if got.Version != "v0.1.0" || got.Revision != "f8cc52db080d990b0f1718e611331db5f9d10cdc" || !got.Modified || got.Time != "2026-09-15T07:22:47Z" {
		t.Fatalf("unexpected info: %+v", got)
	}
	if want := "v0.1.0 (f8cc52db080d-dirty, 2026-09-15T07:22:47Z) go1.27.1"; got.String() != want {
		t.Fatalf("String() = %q, want %q", got.String(), want)
	}
	bare := FromBuildInfo(&debug.BuildInfo{GoVersion: "go1.27.1"})
	if bare.Version != "(devel)" || bare.Revision != "unknown" || bare.String() != "(devel) (unknown) go1.27.1" {
		t.Fatalf("bare info: %+v %q", bare, bare.String())
	}
}

func TestGetDoesNotPanic(t *testing.T) {
	if Get().GoVersion == "" {
		t.Fatal("GoVersion must be set")
	}
}
