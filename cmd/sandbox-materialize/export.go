package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/kubeswift-io/kubeswift/internal/sandbox/export"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

// exportOutput is a SwiftSandbox output export Job: verify the packaged
// output, push it with the platform credential, report the result in the
// termination message. A definitive failure (the archive is refused, the
// credential is refused) is reported and exits 0 so the Job does not retry
// it; anything else exits 1 and the Job retries.
func exportOutput(args []string) int {
	fs := flag.NewFlagSet("export-output", flag.ExitOnError)
	dockerConfig := fs.String("docker-config", "", "the export credential (a docker config.json)")
	resultFile := fs.String("result-file", "/dev/termination-log", "where to write the JSON result")
	_ = fs.Parse(args)

	report := func(res export.Result) {
		b, _ := json.Marshal(res)
		_ = os.WriteFile(*resultFile, b, 0o644)
	}
	// The spec comes from the controller only, in the pod's environment.
	var spec export.Spec
	if err := json.Unmarshal([]byte(os.Getenv("KUBESWIFT_EXPORT_SPEC")), &spec); err != nil {
		report(export.Result{Reason: export.ReasonInvalidEntry, Message: "unreadable export spec: " + err.Error()})
		return 0
	}
	auth := authn.Anonymous
	if *dockerConfig != "" {
		raw, err := os.ReadFile(*dockerConfig)
		if err != nil {
			report(export.Result{Message: "read the export credential: " + err.Error()})
			return 1
		}
		if raw, err = materialize.SanitizeDockerConfig(raw); err != nil {
			report(export.Result{Reason: export.ReasonUnauthorized, Message: "the export credential: " + err.Error()})
			return 0
		}
		if auth, err = materialize.AuthFromDockerConfigJSON(raw, spec.Repository+":"+spec.Tag); err != nil {
			report(export.Result{Reason: export.ReasonUnauthorized, Message: "the export credential: " + err.Error()})
			return 0
		}
	}
	res, err := export.Push(context.Background(), spec, auth, materialize.RegistryTransport())
	if err != nil {
		var e *export.Error
		if errors.As(err, &e) {
			report(export.Result{Reason: e.Reason, Message: e.Message})
			return 0
		}
		report(export.Result{Message: err.Error()})
		fmt.Fprintln(os.Stderr, "export-output:", err)
		return 1
	}
	report(res)
	return 0
}

var sandboxUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// cleanOutputs removes one sandbox's spool directory.
func cleanOutputs(args []string) int {
	fs := flag.NewFlagSet("clean-outputs", flag.ExitOnError)
	root := fs.String("root", "/spool", "the node spool directory")
	uid := fs.String("sandbox-uid", "", "the sandbox whose directory to remove")
	_ = fs.Parse(args)
	if !sandboxUID.MatchString(*uid) {
		fmt.Fprintf(os.Stderr, "clean-outputs: %q is not a sandbox UID\n", *uid)
		return 2
	}
	if err := os.RemoveAll(filepath.Join(*root, *uid)); err != nil {
		fmt.Fprintln(os.Stderr, "clean-outputs:", err)
		return 1
	}
	return 0
}
