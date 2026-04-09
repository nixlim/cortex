package pagination

import (
	"encoding/json"
	"errors"
	"testing"
)

func makeList(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func TestFirstPage(t *testing.T) {
	page, err := Page(makeList(100), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 10 || page[0] != 0 || page[9] != 9 {
		t.Errorf("first page wrong: %v", page)
	}
}

func TestTailPage(t *testing.T) {
	page, err := Page(makeList(100), 10, 95)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 5 || page[0] != 95 || page[4] != 99 {
		t.Errorf("tail page wrong: %v", page)
	}
}

func TestDeterministicJSONOutput(t *testing.T) {
	list := makeList(100)
	a, _ := Page(list, 10, 0)
	b, _ := Page(list, 10, 0)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("JSON outputs differ: %s vs %s", ja, jb)
	}
}

func TestNegativeOffset(t *testing.T) {
	page, err := Page(makeList(100), 10, -1)
	if !errors.Is(err, ErrBadPagination) {
		t.Fatalf("expected ErrBadPagination, got %v", err)
	}
	if len(page) != 0 {
		t.Errorf("expected zero results, got %d", len(page))
	}
}

func TestZeroLimit(t *testing.T) {
	_, err := Page(makeList(10), 0, 0)
	if !errors.Is(err, ErrBadPagination) {
		t.Fatal("expected ErrBadPagination for limit=0")
	}
}

func TestOffsetPastEndReturnsEmpty(t *testing.T) {
	page, err := Page(makeList(10), 5, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 0 {
		t.Errorf("expected empty page, got %v", page)
	}
}
