package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPiDiscoveryFallsBackFromBrokenSavedInstallation(t *testing.T) {
	if result := findPi(context.Background(), nil); result.Status != "missing" || result.Path != "" {
		t.Fatalf("empty search was not reported missing: %+v", result)
	}
	directory := t.TempDir()
	broken := writePiProbe(t, filepath.Join(directory, "broken"), "exit 1", 0700)
	unsupported := writePiProbe(t, filepath.Join(directory, "unsupported"), "printf '1.3.0\\n'", 0700)
	working := writePiProbe(t, filepath.Join(directory, "working"), "printf '1.4.0\\n'", 0700)
	installation := findPi(context.Background(), []string{filepath.Join(directory, "removed"), broken, unsupported, working})
	if installation.Status != "ready" || installation.Path != working || installation.Version != "1.4.0" {
		t.Fatalf("did not select the working installation: %+v", installation)
	}
	if result := findPi(context.Background(), []string{broken}); result.Status != "unavailable" {
		t.Fatalf("broken installation was reported missing: %+v", result)
	}
	if result := findPi(context.Background(), []string{unsupported}); result.Status != "unsupported" || result.Version != "1.3.0" {
		t.Fatalf("incompatible version was reported missing: %+v", result)
	}
}

func TestPiDiscoveryFindsKnownUserInstallsWithoutShellPATH(t *testing.T) {
	home := t.TempDir()
	standalone := writePiProbe(t, filepath.Join(home, ".pi/agent/bin/pi"), "printf '1.4.0\\n'", 0700)
	nvm := writePiProbe(t, filepath.Join(home, ".nvm/versions/node/v22.20.0/bin/pi"), "printf '1.4.0\\n'", 0700)
	fnm := writePiProbe(t, filepath.Join(home, "Library/Application Support/fnm/node-versions/v22.20.0/installation/bin/pi"), "printf '1.4.0\\n'", 0700)
	candidates := piCandidates(standalone, home, []string{"PATH=/usr/bin:/bin:" + filepath.Dir(standalone)})
	if candidates[0] != standalone || slices.Index(candidates[1:], standalone) >= 0 {
		t.Fatalf("saved installation was not preferred and deduplicated: %v", candidates)
	}
	for _, path := range []string{standalone, nvm, fnm} {
		if !slices.Contains(candidates, path) {
			t.Fatalf("Finder environment missed %s", path)
		}
	}
	for _, path := range candidates {
		if !filepath.IsAbs(path) {
			t.Fatalf("discovery searched the current project: %s", path)
		}
	}
}

func TestPiProbeReportsUnusableInstallations(t *testing.T) {
	directory := t.TempDir()
	t.Run("not executable", func(t *testing.T) {
		path := writePiProbe(t, filepath.Join(directory, "not-executable"), "printf '1.4.0\\n'", 0600)
		if result := probePi(context.Background(), path); result.Status != "unavailable" {
			t.Fatalf("wrong result: %+v", result)
		}
	})
	t.Run("node absent from desktop environment", func(t *testing.T) {
		t.Setenv("PATH", directory)
		path := filepath.Join(directory, "npm-pi")
		if err := os.WriteFile(path, []byte("#!/usr/bin/env node\n"), 0700); err != nil {
			t.Fatal(err)
		}
		result := probePi(context.Background(), path)
		if result.Status != "unavailable" || !strings.Contains(result.Detail, "Node.js") {
			t.Fatalf("missing Node.js was not explained: %+v", result)
		}
	})
	t.Run("unexpected version output", func(t *testing.T) {
		path := writePiProbe(t, filepath.Join(directory, "invalid"), "printf 'not pi\\n'", 0700)
		if result := probePi(context.Background(), path); result.Status != "unavailable" || result.Version != "" {
			t.Fatalf("unrecognized executable was accepted: %+v", result)
		}
	})
	t.Run("output bounded", func(t *testing.T) {
		path := writePiProbe(t, filepath.Join(directory, "large"), "printf '%05000d' 0", 0700)
		if result := probePi(context.Background(), path); result.Status != "unavailable" {
			t.Fatalf("oversized version output was accepted: %+v", result)
		}
	})
	t.Run("timeout bounded", func(t *testing.T) {
		path := writePiProbe(t, filepath.Join(directory, "hanging"), "exec /bin/sleep 30", 0700)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		defer cancel()
		started := time.Now()
		result := probePi(ctx, path)
		if result.Status != "unavailable" || !strings.Contains(result.Detail, "超时") || time.Since(started) > time.Second {
			t.Fatalf("version probe did not time out promptly: %+v (%s)", result, time.Since(started))
		}
	})
}

func TestDesktopAutoDetectsPiAfterSavedPathDisappears(t *testing.T) {
	executable := fakePi(t)
	t.Setenv("PATH", filepath.Dir(executable))
	d, err := newDesktop(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.disconnect() })
	d.data.PiPath = filepath.Join(t.TempDir(), "removed-pi")
	installation := d.DetectPi()
	if installation.Status != "ready" || installation.Path != executable {
		t.Fatalf("stale saved path blocked detection: %+v", installation)
	}
	snapshot, err := d.Start(t.TempDir(), "", false)
	if err != nil || !snapshot.Connected {
		t.Fatalf("automatic connection failed: %+v, %v", snapshot, err)
	}
	if d.GetSettings().PiPath != executable {
		t.Fatal("working installation was not saved")
	}
	invalid := writePiProbe(t, filepath.Join(t.TempDir(), "unsupported"), "printf '1.3.0\\n'", 0700)
	if _, err := d.Start(t.TempDir(), invalid, false); err == nil {
		t.Fatal("explicit path silently fell back to another installation")
	}
}

func writePiProbe(t *testing.T, path, body string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	return path
}
