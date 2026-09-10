package cronspec

import (
	"strings"
	"testing"
	"time"

	// Embedded zoneinfo, so the explicit-zone case does not depend on host tzdata.
	_ "time/tzdata"
)

// The defect this package exists for: a TZ=/CRON_TZ= prefix with no space
// panics cron.ParseStandard (slice bounds out of range [:-1]) rather than
// returning an error. spec.schedule is user-supplied, so these arrive from a CR.
//
// These cases would PANIC without checkTimezonePrefix, and a panic fails the
// test outright — so this does not depend on any recover() elsewhere. That
// matters: controller-runtime recovers reconciler panics by default, which
// would otherwise let a regression look like a passing test.
func TestParse_TimezonePrefixWithoutFieldsIsRejectedNotPanic(t *testing.T) {
	for _, spec := range []string{
		"TZ=Europe/Rome",
		"CRON_TZ=UTC",
		"TZ=",
		"CRON_TZ=",
	} {
		t.Run(spec, func(t *testing.T) {
			sched, err := Parse(spec)
			if err == nil {
				t.Fatalf("Parse(%q) returned no error; a prefix with no cron fields must be rejected", spec)
			}
			if sched != nil {
				t.Errorf("Parse(%q) returned a schedule alongside an error", spec)
			}
			// The message has to tell the operator what to do about it.
			if !strings.Contains(err.Error(), "followed by a space") {
				t.Errorf("error should explain the fix, got: %v", err)
			}
		})
	}
}

// Shapes that LOOK like the broken one but are cron's own business: each
// contains a space, so cron finds it and errors by itself. The guard must not
// claim these — over-rejecting would break valid-ish input and mask real parser
// errors.
func TestParse_SimilarShapesAreLeftToTheParser(t *testing.T) {
	for _, tc := range []struct{ name, spec string }{
		{"trailing space, no fields", "TZ=Europe/Rome "},
		{"tab separator", "TZ=Zone\t0 2 * * *"},
		{"bad zone name", "CRON_TZ=Bogus/Zone 0 2 * * *"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Must not panic, and must surface the parser's own error.
			_, err := Parse(tc.spec)
			if err == nil {
				t.Fatalf("Parse(%q) unexpectedly succeeded", tc.spec)
			}
			if strings.Contains(err.Error(), "followed by a space") {
				t.Errorf("guard claimed %q; it should have been left to the parser: %v", tc.spec, err)
			}
		})
	}
}

// The guard must not disturb anything valid.
func TestParse_ValidExpressionsStillWork(t *testing.T) {
	for _, spec := range []string{
		"0 2 * * *",
		"*/15 * * * *",
		"@daily",
		"@every 30m",
		"CRON_TZ=Asia/Tokyo 0 2 * * *",
		"TZ=Asia/Tokyo 0 2 * * *",
	} {
		if _, err := Parse(spec); err != nil {
			t.Errorf("Parse(%q) = %v, want success", spec, err)
		}
	}
}

// The UTC pinning this package inherited must survive the refactor: an unzoned
// expression is evaluated in UTC no matter the process timezone.
func TestParse_UnzonedIsPinnedToUTC(t *testing.T) {
	orig := time.Local
	time.Local = time.FixedZone("TEST", 5*60*60)
	t.Cleanup(func() { time.Local = orig })

	sched, err := Parse("0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 6, 6, 0, 0, 0, 0, time.Local)
	want := time.Date(2026, 6, 6, 2, 0, 0, 0, time.UTC)
	if got := sched.Next(from); !got.Equal(want) {
		t.Errorf("next tick = %v (%v UTC), want %v", got, got.UTC(), want)
	}
}

// An explicit zone still wins over the UTC pin.
func TestParse_ExplicitZoneIsHonoured(t *testing.T) {
	sched, err := Parse("CRON_TZ=Asia/Tokyo 0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	// 00:00 UTC is already 09:00 in Tokyo, so the next 02:00 there is tomorrow's.
	from := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
	want := time.Date(2026, 6, 7, 2, 0, 0, 0, tokyo)
	if got := sched.Next(from); !got.Equal(want) {
		t.Errorf("next tick = %v (%v UTC), want %v", got, got.UTC(), want)
	}
}
