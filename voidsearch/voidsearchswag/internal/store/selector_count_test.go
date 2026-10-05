package store

import (
	"context"
	"testing"
)

func TestSelectorCount(t *testing.T) {
	st, err := Open(t.TempDir() + "/sc.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := st.SelectorCount(ctx); err != nil || n != 0 {
		t.Errorf("пустой реестр: n=%d err=%v", n, err)
	}
	if err := st.UpsertSelector(ctx, Selector{URLPattern: "example.com", Field: "title", Selector: "h1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSelector(ctx, Selector{URLPattern: "example.com", Field: "price", Selector: ".p"}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.SelectorCount(ctx); err != nil || n != 2 {
		t.Errorf("после двух записей: n=%d err=%v", n, err)
	}
}
