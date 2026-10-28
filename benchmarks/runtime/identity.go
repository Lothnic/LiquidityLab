package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
)

const maxExecutableBytes int64 = 512 << 20

type moduleIdentity struct {
	Path        string          `json:"path"`
	Version     string          `json:"version"`
	Sum         string          `json:"sum"`
	Replacement *moduleIdentity `json:"replacement,omitempty"`
}
type buildIdentity struct {
	Available    bool              `json:"available"`
	GoVersion    string            `json:"go_version"`
	Path         string            `json:"package_path"`
	Main         moduleIdentity    `json:"main_module"`
	Dependencies []moduleIdentity  `json:"dependencies"`
	Settings     map[string]string `json:"settings"`
	VCSStatus    string            `json:"vcs_status"`
}
type executableIdentity struct {
	Status string `json:"status"`
	SHA256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

func moduleInfo(m debug.Module) moduleIdentity {
	path := m.Path
	if filepath.IsAbs(path) || strings.HasPrefix(path, ".") {
		path = "[local replacement]"
	}
	out := moduleIdentity{Path: path, Version: m.Version, Sum: m.Sum}
	if m.Replace != nil {
		// Replacement chains are not used by Go; only copy the direct metadata.
		replacement := *m.Replace
		replacement.Replace = nil
		r := moduleInfo(replacement)
		out.Replacement = &r
	}
	return out
}

func sanitizedBuildInfo(info *debug.BuildInfo, available bool) buildIdentity {
	out := buildIdentity{Available: available && info != nil, Dependencies: []moduleIdentity{}, Settings: map[string]string{}, VCSStatus: "unavailable"}
	if !out.Available {
		return out
	}
	out.GoVersion, out.Path, out.Main = info.GoVersion, info.Path, moduleInfo(info.Main)
	for _, dep := range info.Deps {
		if dep != nil {
			out.Dependencies = append(out.Dependencies, moduleInfo(*dep))
		}
	}
	// Build settings can contain environment values and arbitrary linker flags.
	// Retain only this fixed set of toolchain/target and VCS identity fields.
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs", "vcs.revision", "vcs.time", "vcs.modified", "-buildmode", "-compiler", "CGO_ENABLED", "GOOS", "GOARCH", "GOAMD64", "GOARM", "GOARM64":
			out.Settings[setting.Key] = setting.Value
		}
	}
	if out.Settings["vcs.revision"] != "" {
		out.VCSStatus = "available"
	}
	return out
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > maxExecutableBytes {
		return "", 0, errors.New("executable is not a bounded regular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxExecutableBytes+1))
	if err != nil {
		return "", n, err
	}
	if n > maxExecutableBytes {
		return "", n, errors.New("executable exceeds checksum size bound")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func currentExecutableIdentity() executableIdentity {
	path, err := os.Executable()
	if err != nil {
		return executableIdentity{Status: "unavailable"}
	}
	sum, n, err := fileSHA256(path)
	if err != nil {
		return executableIdentity{Status: "unavailable"}
	}
	return executableIdentity{Status: "available", SHA256: sum, Bytes: n}
}

func currentBuildIdentity() buildIdentity {
	info, ok := debug.ReadBuildInfo()
	return sanitizedBuildInfo(info, ok)
}
