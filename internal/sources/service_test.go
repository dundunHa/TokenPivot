package sources

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// archiveFake records the order of the transaction-bound steps.
type archiveFake struct {
	calls  []string
	found  bool
	failAt string
}

func (a *archiveFake) step(name string) error {
	a.calls = append(a.calls, name)
	if a.failAt == name {
		return errors.New("synthetic step failure")
	}
	return nil
}

func (a *archiveFake) lockSource(ctx context.Context, workspaceID, sourceID ID) (bool, error) {
	return a.found, a.step("lock")
}
func (a *archiveFake) markArchived(ctx context.Context, workspaceID, sourceID ID) error {
	return a.step("archive")
}
func (a *archiveFake) revokeCredentials(ctx context.Context, workspaceID, sourceID ID) error {
	return a.step("revoke")
}

func TestArchiveOrderLockArchiveRevoke(t *testing.T) {
	tx := &archiveFake{found: true}
	if err := archive(context.Background(), tx, ID{}, ID{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tx.calls, ","); got != "lock,archive,revoke" {
		t.Fatalf("order=%s", got)
	}
}

func TestArchiveMissingSourceStopsAfterLock(t *testing.T) {
	tx := &archiveFake{}
	if err := archive(context.Background(), tx, ID{}, ID{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error=%v want ErrNotFound", err)
	}
	if got := strings.Join(tx.calls, ","); got != "lock" {
		t.Fatalf("calls=%s want only lock", got)
	}
}

func TestArchiveStopsAtFirstFailure(t *testing.T) {
	for _, failAt := range []string{"lock", "archive", "revoke"} {
		tx := &archiveFake{found: true, failAt: failAt}
		if err := archive(context.Background(), tx, ID{}, ID{}); err == nil {
			t.Fatalf("%s: failure swallowed", failAt)
		}
		order := []string{"lock", "archive", "revoke"}
		want := strings.Join(order[:indexOf(order, failAt)+1], ",")
		if got := strings.Join(tx.calls, ","); got != want {
			t.Fatalf("%s: calls=%s want %s", failAt, got, want)
		}
	}
}

func indexOf(list []string, value string) int {
	for i, item := range list {
		if item == value {
			return i
		}
	}
	return -1
}
