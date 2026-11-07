package git

import "testing"

func TestTreeEntriesWithSizes(t *testing.T) {
	repo, ref := setupTestRepo(t)

	tree, err := repo.TreePath(ref, "")
	if err != nil {
		t.Fatalf("TreePath failed: %v", err)
	}
	ents, err := tree.EntriesWithSizes()
	if err != nil {
		t.Fatalf("EntriesWithSizes failed: %v", err)
	}
	if len(ents) != 1 || !ents[0].IsTree() {
		t.Fatalf("expected one tree entry, got %d", len(ents))
	}
	if !ents[0].sized || ents[0].Size() != 0 {
		t.Fatalf("tree entry: sized=%v size=%d, want sized and 0", ents[0].sized, ents[0].Size())
	}

	sub, err := repo.TreePath(ref, "dot_config")
	if err != nil {
		t.Fatalf("TreePath failed: %v", err)
	}
	ents, err = sub.EntriesWithSizes()
	if err != nil {
		t.Fatalf("EntriesWithSizes failed: %v", err)
	}
	if len(ents) != 1 || ents[0].Name() != "bat" {
		t.Fatalf("expected the bat entry, got %d entries", len(ents))
	}
	want := int64(len("test content"))
	if !ents[0].sized {
		t.Fatal("entry size was not loaded with the listing")
	}
	if got := ents[0].Size(); got != want {
		t.Fatalf("size = %d, want %d", got, want)
	}
}
