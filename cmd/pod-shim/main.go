// pod-shim is the one container of a pod-mode stand-in Pod (see
// internal/sandbox/podmode). Its image has no shell: sh, bash and env are this
// binary, so `kubectl exec <stand-in> -- sh` runs in the microVM, never here.
//
//	pod-shim pod-shim [flags]   the stand-in's process: port forwarder + logs
//	pod-shim exec -- CMD...     run CMD in the microVM
//	sh|bash|env ARGS...         run the same command in the microVM
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	name := filepath.Base(os.Args[0])
	args := os.Args[1:]
	if name != "pod-shim" {
		os.Exit(podExec(os.Args))
	}
	if len(args) > 0 {
		switch args[0] {
		case "pod-shim":
			os.Exit(podShim(args[1:]))
		case "exec":
			argv := args[1:]
			if len(argv) > 0 && argv[0] == "--" {
				argv = argv[1:]
			}
			os.Exit(podExec(argv))
		}
	}
	fmt.Fprintln(os.Stderr, "usage: pod-shim pod-shim [flags] | pod-shim exec -- CMD...")
	os.Exit(2)
}
