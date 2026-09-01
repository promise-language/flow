package common

// The three freeBytes implementations, and the one thing nobody else checks.
//
// This machine compiles exactly one of them. The other two are compiled by
// nothing here — not by `go build ./...`, not by `go vet`, not by any test —
// so a typo in the Windows syscall, or an import the fallback stopped using,
// ships and stays. The first person to find out is on that platform, watching
// this whole package fail to build: the other five gates, verify and the guard
// all live in it, and none of them are about disks at all.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Every platform the toolchain knows gets exactly one freeBytes, and the file
// it gets compiles.
//
// Compiling is the check because both failures ARE compile failures: a tag
// expression covering a platform twice is a redefinition, one covering it
// nowhere is an undefined reference. The three expressions are complements
// maintained by hand — platform_nostatfs.go negates, name by name, the list
// platform_statfs.go declares — and nothing but this keeps them complementary.
func TestEveryPlatformGetsExactlyOneFreeBytes(t *testing.T) {
	for _, p := range toolchainPlatforms(t) {
		t.Run(p.goos, func(t *testing.T) {
			t.Parallel()
			// The package, not a binary. Linking is a second toolchain's job —
			// android and ios want a cgo linker for it — and it answers nothing
			// about which source file was selected.
			cmd := exec.Command("go", "build", ".")
			cmd.Env = p.env()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("this package does not build for %s/%s, so nothing in it runs there: %v\n%s",
					p.goos, p.goarch, err, out)
			}
		})
	}
}

type platform struct{ goos, goarch string }

// env is this process's environment with the target substituted in. GOOS and
// GOARCH are REMOVED before being set: two entries for one name leave which
// one wins to the operating system, and a sweep that quietly rebuilt the host
// fifteen times would pass whatever the tags said.
func (p platform) env() []string {
	env := []string{}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GOOS=") || strings.HasPrefix(kv, "GOARCH=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GOOS="+p.goos, "GOARCH="+p.goarch)
}

// toolchainPlatforms lists every GOOS the installed toolchain can target, with
// one architecture each. Asked of the toolchain rather than written down: a
// list in this file would go stale exactly when it mattered, since the platform
// added after it was written is the one nobody weighed the tags against.
func toolchainPlatforms(t *testing.T) []platform {
	t.Helper()
	out, err := exec.Command("go", "tool", "dist", "list").Output()
	if err != nil {
		t.Fatalf("asking the toolchain which platforms it targets: %v", err)
	}
	var platforms []platform
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		goos, goarch, ok := strings.Cut(strings.TrimSpace(line), "/")
		if !ok || seen[goos] {
			continue
		}
		seen[goos] = true
		platforms = append(platforms, platform{goos, goarch})
	}
	if len(platforms) < 2 {
		t.Fatalf("read %d platforms from `go tool dist list` — the probe is broken, not the code", len(platforms))
	}
	return platforms
}
