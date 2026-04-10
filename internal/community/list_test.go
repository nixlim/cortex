package community

import (
	"context"
	"errors"
	"testing"

	"github.com/nixlim/cortex/internal/errs"
)

// fakeReader is a Reader double that returns canned QueryGraph rows
// keyed by a substring of the cypher statement. It lets list tests
// assert against the read seam without a live Bolt connection.
type fakeReader struct {
	// rows[matchSubstring] → rows returned when the executed cypher
	// contains matchSubstring. The first matching key wins.
	rows map[string][]map[string]any
	err  error

	calls    int
	lastArgs map[string]any
}

func (f *fakeReader) QueryGraph(_ context.Context, cypher string, params map[string]any) ([]map[string]any, error) {
	f.calls++
	f.lastArgs = params
	if f.err != nil {
		return nil, f.err
	}
	for needle, rows := range f.rows {
		if contains(cypher, needle) {
			return rows, nil
		}
	}
	return nil, nil
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestListCommunitiesFiltersBelowMinimumSize(t *testing.T) {
	r := &fakeReader{
		rows: map[string][]map[string]any{
			"MATCH (c:Community": {
				{"community_id": int64(1), "member_count": int64(5), "summary": "five"},
				{"community_id": int64(2), "member_count": int64(1), "summary": "tiny"},
				{"community_id": int64(3), "member_count": int64(2), "summary": "ok"},
			},
		},
	}
	got, err := ListCommunities(context.Background(), r, 0)
	if err != nil {
		t.Fatalf("ListCommunities: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != 1 || got[1].ID != 3 {
		t.Errorf("got = %+v, want IDs 1 and 3", got)
	}
	if got[0].Level != 0 {
		t.Errorf("level = %d, want 0", got[0].Level)
	}
	if got[0].Summary != "five" {
		t.Errorf("summary = %q, want %q", got[0].Summary, "five")
	}
}

func TestListCommunitiesEmptyResultIsNotAnError(t *testing.T) {
	r := &fakeReader{rows: map[string][]map[string]any{}}
	got, err := ListCommunities(context.Background(), r, 0)
	if err != nil {
		t.Fatalf("ListCommunities: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0 (empty list, not error)", len(got))
	}
}

func TestListCommunitiesPropagatesQueryError(t *testing.T) {
	r := &fakeReader{err: errors.New("bolt down")}
	_, err := ListCommunities(context.Background(), r, 0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "NEO4J_QUERY_FAILED" {
		t.Errorf("err = %v, want NEO4J_QUERY_FAILED", err)
	}
}

func TestListCommunitiesNilReader(t *testing.T) {
	_, err := ListCommunities(context.Background(), nil, 0)
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "NEO4J_UNAVAILABLE" {
		t.Errorf("err = %v, want NEO4J_UNAVAILABLE", err)
	}
}

func TestShowCommunityReturnsDetailWithMembers(t *testing.T) {
	r := &fakeReader{
		rows: map[string][]map[string]any{
			"RETURN c.member_count": {
				{"member_count": int64(3), "summary": "cluster"},
			},
			"RETURN n.entry_id": {
				{"entry_id": "entry:01AAA"},
				{"entry_id": "entry:01BBB"},
				{"entry_id": "entry:01CCC"},
			},
		},
	}
	got, err := ShowCommunity(context.Background(), r, 1, 42)
	if err != nil {
		t.Fatalf("ShowCommunity: %v", err)
	}
	if got.ID != 42 || got.Level != 1 {
		t.Errorf("id/level = %d/%d, want 42/1", got.ID, got.Level)
	}
	if got.MemberCount != 3 {
		t.Errorf("member_count = %d, want 3", got.MemberCount)
	}
	if got.Summary != "cluster" {
		t.Errorf("summary = %q, want %q", got.Summary, "cluster")
	}
	if len(got.MemberIDs) != 3 {
		t.Fatalf("members len = %d, want 3", len(got.MemberIDs))
	}
	if got.MemberIDs[0] != "entry:01AAA" {
		t.Errorf("first member = %q, want entry:01AAA", got.MemberIDs[0])
	}
}

func TestShowCommunityNotFound(t *testing.T) {
	r := &fakeReader{
		rows: map[string][]map[string]any{
			"RETURN c.member_count": {},
		},
	}
	_, err := ShowCommunity(context.Background(), r, 0, 999)
	if err == nil {
		t.Fatal("expected NOT_FOUND, got nil")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "NOT_FOUND" || e.Kind != errs.KindOperational {
		t.Errorf("err = %v, want operational NOT_FOUND", err)
	}
}

func TestShowCommunityEmptyMembersIsOK(t *testing.T) {
	r := &fakeReader{
		rows: map[string][]map[string]any{
			"RETURN c.member_count": {
				{"member_count": int64(0), "summary": ""},
			},
			"RETURN n.entry_id": {},
		},
	}
	got, err := ShowCommunity(context.Background(), r, 0, 7)
	if err != nil {
		t.Fatalf("ShowCommunity: %v", err)
	}
	if len(got.MemberIDs) != 0 {
		t.Errorf("members = %v, want empty", got.MemberIDs)
	}
}

func TestFormatParseIDRoundTrip(t *testing.T) {
	s := FormatID(2, 137)
	if s != "L2:C137" {
		t.Errorf("FormatID = %q, want L2:C137", s)
	}
	level, id, err := ParseID(s)
	if err != nil {
		t.Fatalf("ParseID: %v", err)
	}
	if level != 2 || id != 137 {
		t.Errorf("Parse = %d, %d, want 2, 137", level, id)
	}
}

func TestParseIDRejectsBareNumeric(t *testing.T) {
	if _, _, err := ParseID("42"); err == nil {
		t.Error("expected error on bare numeric, got nil")
	}
}
