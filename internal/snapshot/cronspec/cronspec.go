// Package cronspec parses the cron expression on SwiftSnapshotSchedule.
//
// It exists so the controller and the admission webhook parse the same string
// the same way. They previously each called cron.ParseStandard directly, which
// meant a guard added to one would not protect the other — and the webhook is
// off by default (webhook.enabled=false), so the controller is the path that
// actually needs protecting.
package cronspec

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Parse parses a 5-field cron expression, pinned to UTC.
//
// ParseStandard defaults an unzoned expression to time.Local, and Next() then
// evaluates it in the zone of the time it is given — always local in the
// reconcile loop, since metav1.Time decodes to time.Local. A pod with a
// timezone would otherwise run every schedule on local wall-clock, while
// spec.schedule documents UTC. An explicit TZ=/CRON_TZ= prefix keeps the zone
// it names.
func Parse(spec string) (cron.Schedule, error) {
	if err := checkTimezonePrefix(spec); err != nil {
		return nil, err
	}
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return nil, err
	}
	// @every parses to a plain interval, with no location to pin.
	if ss, ok := sched.(*cron.SpecSchedule); ok && !namesTimezone(spec) {
		ss.Location = time.UTC
	}
	return sched, nil
}

// checkTimezonePrefix rejects the one input shape that makes ParseStandard
// PANIC rather than return an error.
//
// cron v3.0.1 extracts the zone with:
//
//	i := strings.Index(spec, " ")
//	eq := strings.Index(spec, "=")
//	time.LoadLocation(spec[eq+1 : i])
//
// When the spec carries a TZ=/CRON_TZ= prefix and contains no space at all, i
// is -1 and the slice expression panics with "slice bounds out of range [:-1]".
// spec.schedule is user-supplied, so "TZ=Europe/Rome" with no fields after it
// reaches that line straight from a CR.
//
// The condition here is exactly the condition that panics — a prefix plus no
// ASCII space — verified case by case against the parser, including the shapes
// that look similar but do NOT panic: a trailing space with no fields
// ("TZ=Europe/Rome ") and a tab separator ("TZ=Zone\t0 2 * * *"), both of which
// contain a space that Index finds, so cron returns an error on its own.
//
// Upstream has had this reported since at least robfig/cron#470 (also #566,
// #574), all still open, and the project was last pushed in 2024 — so guarding
// locally is the only option, not a stopgap.
func checkTimezonePrefix(spec string) error {
	if namesTimezone(spec) && !strings.Contains(spec, " ") {
		return fmt.Errorf(
			"a %s prefix must be followed by a space and the cron fields, e.g. %q; got %q",
			timezoneKeyword(spec), "CRON_TZ=Europe/Rome 0 2 * * *", spec)
	}
	return nil
}

// namesTimezone reports whether spec carries a zone prefix ParseStandard
// honours. Deliberately byte-identical to cron's own check, so the two cannot
// disagree about whether a prefix is present.
func namesTimezone(spec string) bool {
	return strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=")
}

// timezoneKeyword returns which prefix spec used, for the error message.
func timezoneKeyword(spec string) string {
	if strings.HasPrefix(spec, "CRON_TZ=") {
		return "CRON_TZ="
	}
	return "TZ="
}
