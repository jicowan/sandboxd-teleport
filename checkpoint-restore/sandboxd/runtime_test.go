//go:build linux

package main

import "testing"

// TestCheckRestoreCompat covers the generalized {runtime, version} restore guard:
// cross-runtime is refused unconditionally; version is checked only within the
// same runtime; back-compat (empty runtime, runscVersion alias, empty resolved
// version) preserves the pre-microVM behavior.
func TestCheckRestoreCompat(t *testing.T) {
	cases := []struct {
		name                                     string
		wantRuntime, wantEngineVer, wantRunscVer string
		haveRuntime, haveVer                     string
		wantOK                                   bool
		wantKind                                 string
	}{
		{"same runtime + same version → ok",
			"gvisor", "v1", "", "gvisor", "v1", true, ""},
		{"cross-runtime refused even if version strings match",
			"microvm", "v1", "", "gvisor", "v1", false, "runtime mismatch"},
		{"cross-runtime refused regardless of version",
			"microvm", "", "", "gvisor", "v1", false, "runtime mismatch"},
		{"same runtime, version mismatch → refused",
			"gvisor", "v2", "", "gvisor", "v1", false, "engine version mismatch"},
		{"empty caller runtime is treated as compatible (old caller)",
			"", "v1", "", "gvisor", "v1", true, ""},
		{"back-compat: runscVersion alias used when engineVersion empty (match)",
			"", "", "v1", "gvisor", "v1", true, ""},
		{"back-compat: runscVersion alias used when engineVersion empty (mismatch)",
			"", "", "v2", "gvisor", "v1", false, "engine version mismatch"},
		{"engineVersion wins over runscVersion alias",
			"gvisor", "v1", "v-stale", "gvisor", "v1", true, ""},
		{"worker version unresolvable but caller demands one → refused (can't prove compat, matches prior behavior)",
			"gvisor", "v1", "", "gvisor", "", false, "engine version mismatch"},
		{"empty CALLER version skips the check even if worker has one (the guard-off path: snapshot recorded no version)",
			"gvisor", "", "", "gvisor", "v1", true, ""},
		{"empty caller version skips version check",
			"gvisor", "", "", "gvisor", "v1", true, ""},
		{"microvm same runtime + version → ok",
			"microvm", "ch-1.0", "", "microvm", "ch-1.0", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := checkRestoreCompat(c.wantRuntime, c.wantEngineVer, c.wantRunscVer, c.haveRuntime, c.haveVer)
			if v.OK != c.wantOK {
				t.Fatalf("OK = %v, want %v (verdict=%+v)", v.OK, c.wantOK, v)
			}
			if !c.wantOK && v.Kind != c.wantKind {
				t.Fatalf("Kind = %q, want %q", v.Kind, c.wantKind)
			}
		})
	}
}

// TestRunscDriverSatisfiesInterface is a compile-time-ish assertion that the
// gVisor driver exposes the runtime-neutral identity used by the guard.
func TestRunscDriverRuntimeName(t *testing.T) {
	r := &runscDriver{ver: "release-x"}
	if r.runtimeName() != "gvisor" {
		t.Fatalf("runtimeName = %q, want gvisor", r.runtimeName())
	}
	if r.version() != "release-x" {
		t.Fatalf("version = %q, want release-x", r.version())
	}
}
