package projects

import (
	"context"
	"errors"
	"testing"
)

type stubStore struct{ err error }

func (s stubStore) List(ctx context.Context, w ID) ([]Project, error) { return nil, s.err }
func (s stubStore) Create(ctx context.Context, w ID, k, n string) (Project, error) {
	return Project{}, s.err
}
func (s stubStore) Update(ctx context.Context, w, p ID, n string) (Project, error) {
	return Project{}, s.err
}
func (s stubStore) Archive(ctx context.Context, w, p ID) error { return s.err }

func TestServicePropagatesStoreErrors(t *testing.T) {
	for _, want := range []error{ErrConflict, ErrNotFound, errors.New("synthetic")} {
		svc := New(stubStore{err: want})
		ctx := context.Background()
		if _, err := svc.List(ctx, ID{}); !errors.Is(err, want) {
			t.Fatal("list")
		}
		if _, err := svc.Create(ctx, ID{}, "k", "n"); !errors.Is(err, want) {
			t.Fatal("create")
		}
		if _, err := svc.Update(ctx, ID{}, ID{}, "n"); !errors.Is(err, want) {
			t.Fatal("update")
		}
		if err := svc.Archive(ctx, ID{}, ID{}); !errors.Is(err, want) {
			t.Fatal("archive")
		}
	}
}
