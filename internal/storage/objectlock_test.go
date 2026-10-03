package storage

import (
	"errors"
	"testing"

	"github.com/minio/minio-go/v7"
)

func TestParseObjectLock(t *testing.T) {
	cases := []struct {
		cfg      map[string]string
		wantMode minio.RetentionMode
		wantDays int
	}{
		{map[string]string{}, "", 0},                            // unset = off
		{map[string]string{"object_lock": "compliance"}, "", 0}, // no days = off
		{map[string]string{"object_lock": "compliance", "lock_days": "30"}, minio.Compliance, 30},
		{map[string]string{"object_lock": "Governance", "lock_days": "7"}, minio.Governance, 7}, // case-insensitive
		{map[string]string{"object_lock": "nope", "lock_days": "30"}, "", 0},                    // unknown mode = off
		{map[string]string{"object_lock": "compliance", "lock_days": "0"}, "", 0},               // zero days = off
		{map[string]string{"object_lock": "compliance", "lock_days": "-5"}, "", 0},              // negative = off
	}
	for _, c := range cases {
		gotMode, gotDays := parseObjectLock(c.cfg)
		if gotMode != c.wantMode || gotDays != c.wantDays {
			t.Errorf("parseObjectLock(%v) = %q/%d, want %q/%d", c.cfg, gotMode, gotDays, c.wantMode, c.wantDays)
		}
	}
}

func TestS3ImmutableReporting(t *testing.T) {
	s, err := NewS3(map[string]string{"endpoint": "s3.example.com", "bucket": "b", "object_lock": "compliance", "lock_days": "14"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Immutable() || s.LockDays() != 14 {
		t.Fatalf("immutable=%v days=%d, want true/14", s.Immutable(), s.LockDays())
	}
	plain, _ := NewS3(map[string]string{"endpoint": "s3.example.com", "bucket": "b"})
	if plain.Immutable() || plain.LockDays() != 0 {
		t.Fatalf("plain bucket should not be immutable")
	}
}

func TestObjectLockStatus(t *testing.T) {
	// Bucket enforces Object Lock → verified.
	if st := objectLockStatus("Enabled", nil, nil, nil, nil); !st.Enforced || !st.Checked {
		t.Fatalf("Enabled bucket should be Enforced+Checked, got %+v", st)
	}
	// Bucket does NOT have Object Lock (empty config, no error) → loud "not enabled".
	if st := objectLockStatus("", nil, nil, nil, nil); st.Enforced || !st.Checked {
		t.Fatalf("disabled bucket should be Checked but not Enforced, got %+v", st)
	}
	// S3/MinIO report a lock-less bucket as an ERROR — that specific code must be
	// the loud "not enabled" case, not a soft "couldn't verify".
	notCfg := minio.ErrorResponse{Code: "ObjectLockConfigurationNotFoundError"}
	if st := objectLockStatus("", nil, nil, nil, notCfg); st.Enforced || !st.Checked {
		t.Fatalf("not-configured error must be Checked+not-Enforced (loud), got %+v", st)
	}
	// A permission/other error (e.g. append-only key) → unchecked, no false alarm.
	if st := objectLockStatus("", nil, nil, nil, errors.New("AccessDenied")); st.Enforced || st.Checked {
		t.Fatalf("read error should be unchecked+not-enforced, got %+v", st)
	}
}
