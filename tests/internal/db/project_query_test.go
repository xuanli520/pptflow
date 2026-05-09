package db_test

import (
	"context"
	"slices"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/db"
)

func TestProjectSearchDoesNotTreatInputAsSQL(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	injectionLikeTaskID := `TASK%' OR 1=1 --`
	upsertTaskIDs(t, store, "TASK-1", "TASK-2", injectionLikeTaskID)

	projects, total, err := store.ListProjectsPaginated(ctx, db.ProjectQuery{
		Limit: 20,
		Search: db.ProjectSearch{Terms: []db.ProjectSearchTerm{{
			Text: injectionLikeTaskID,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1; projects=%#v", total, projects)
	}
	if got := projectIDs(projects); !slices.Equal(got, []string{injectionLikeTaskID}) {
		t.Fatalf("search should match literal task id only, got %#v", got)
	}
}
