package store

import (
	"testing"
)

func TestMigrateV17ToV18InstallsUniqueRepairRoundIndex(t *testing.T) {
	s := tempDB(t)
	root := s.rootDir
	if _, err := s.db.Exec(`DROP INDEX idx_revision_candidates_v8_repair_round`); err != nil {
		t.Fatalf("remove V18 repair round index: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_version WHERE version = 18`); err != nil {
		t.Fatalf("rewind schema fixture from V18: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(root)
	if err != nil {
		t.Fatalf("migrate V17 store to V18: %v", err)
	}
	defer migrated.Close()
	var version int
	var indexSQL string
	if err := migrated.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := migrated.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_revision_candidates_v8_repair_round'`).Scan(&indexSQL); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || indexSQL == "" {
		t.Fatalf("V18 migration version=%d repair_round_index=%q", version, indexSQL)
	}
}
