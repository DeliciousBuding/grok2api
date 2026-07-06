package admission

import "testing"

func TestControllerEnforcesLimitAndReleases(t *testing.T) {
	ctrl := NewController()

	release, ok := ctrl.TryAcquire("global", 1)
	if !ok {
		t.Fatal("first acquire should pass")
	}
	if release == nil {
		t.Fatal("release function should be non-nil")
	}
	if _, ok := ctrl.TryAcquire("global", 1); ok {
		t.Fatal("second acquire should be rejected at limit")
	}

	release()

	if _, ok := ctrl.TryAcquire("global", 1); !ok {
		t.Fatal("acquire should pass after release")
	}
}

func TestControllerTreatsNonPositiveLimitAsDisabled(t *testing.T) {
	ctrl := NewController()

	for i := 0; i < 10; i++ {
		release, ok := ctrl.TryAcquire("global", 0)
		if !ok {
			t.Fatal("non-positive limit should disable admission checks")
		}
		release()
	}
}

func TestControllerSeparatesKeys(t *testing.T) {
	ctrl := NewController()

	if _, ok := ctrl.TryAcquire("model:a", 1); !ok {
		t.Fatal("first model acquire should pass")
	}
	if _, ok := ctrl.TryAcquire("model:b", 1); !ok {
		t.Fatal("different keys should not share capacity")
	}
	if _, ok := ctrl.TryAcquire("model:a", 1); ok {
		t.Fatal("same key should be limited independently")
	}
}
