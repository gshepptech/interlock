package state

import (
	"errors"
	"testing"
)

func TestValidateAcceptsLegacyAndCurrent(t *testing.T) {
	for _, version := range []int{0, CurrentVersion} {
		if err := Validate("session", "record.json", version); err != nil {
			t.Errorf("Validate version %d: %v", version, err)
		}
	}
}

func TestValidateRejectsFutureVersionWithTypedError(t *testing.T) {
	err := Validate("lease", "lease.json", CurrentVersion+1)
	var unsupported *UnsupportedVersionError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %v, want UnsupportedVersionError", err)
	}
	if unsupported.Store != "lease" || unsupported.Path != "lease.json" ||
		unsupported.Found != CurrentVersion+1 || unsupported.Supported != CurrentVersion {
		t.Errorf("error fields = %#v", unsupported)
	}
}

func TestValidateRejectsNegativeVersionAsMalformed(t *testing.T) {
	err := Validate("session", "record.json", -1)
	if err == nil {
		t.Fatal("negative version accepted")
	}
	var unsupported *UnsupportedVersionError
	if errors.As(err, &unsupported) {
		t.Fatalf("negative version reported as future state: %v", err)
	}
}
