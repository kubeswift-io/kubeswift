package main

import (
	"testing"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
)

func TestParsePreferredMode(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    migrationv1alpha1.SwiftMigrationMode
		wantErr bool
	}{
		{"empty defaults to auto", "", migrationv1alpha1.SwiftMigrationModeAuto, false},
		{"auto", "auto", migrationv1alpha1.SwiftMigrationModeAuto, false},
		{"live", "live", migrationv1alpha1.SwiftMigrationModeLive, false},
		{"offline", "offline", migrationv1alpha1.SwiftMigrationModeOffline, false},
		{"invalid value", "fast", "", true},
		{"case-sensitive: Live is invalid", "Live", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePreferredMode(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePreferredMode(%q) = %q, nil; want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePreferredMode(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parsePreferredMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
