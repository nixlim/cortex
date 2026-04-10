package trail

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
)

// fakeAppender captures every datom group it is asked to append. The
// flat slice of groups makes assertions trivial: tests can index into
// the i-th group and inspect attribute names directly.
type fakeAppender struct {
	groups [][]datom.Datom
	err    error
}

func (f *fakeAppender) Append(group []datom.Datom) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	cp := make([]datom.Datom, len(group))
	copy(cp, group)
	f.groups = append(f.groups, cp)
	return cp[0].Tx, nil
}

func fixedClock(ts string) func() time.Time {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return t }
}

// ---------------------------------------------------------------------
// Begin
// ---------------------------------------------------------------------

func TestBeginWritesKindAgentNameStartedAt(t *testing.T) {
	app := &fakeAppender{}
	id, err := Begin(app, "tester", "inv-1", "grill-spec", "auth review",
		fixedClock("2026-04-10T12:00:00Z"))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if !strings.HasPrefix(id, EntityPrefix) {
		t.Errorf("trail id %q missing prefix %q", id, EntityPrefix)
	}
	if len(app.groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(app.groups))
	}
	got := app.groups[0]
	wantAttrs := []string{AttrKind, AttrAgent, AttrName, AttrStartedAt}
	if len(got) != len(wantAttrs) {
		t.Fatalf("group has %d datoms, want %d", len(got), len(wantAttrs))
	}
	for i, want := range wantAttrs {
		if got[i].A != want {
			t.Errorf("datom[%d].A = %q, want %q", i, got[i].A, want)
		}
		if got[i].E != id {
			t.Errorf("datom[%d].E = %q, want %q", i, got[i].E, id)
		}
		if got[i].Op != datom.OpAdd {
			t.Errorf("datom[%d].Op = %q, want add", i, got[i].Op)
		}
		if got[i].Src != Source {
			t.Errorf("datom[%d].Src = %q, want %q", i, got[i].Src, Source)
		}
		if got[i].Checksum == "" {
			t.Errorf("datom[%d] not sealed", i)
		}
	}
	// Every datom in a group shares the same tx ULID.
	tx := got[0].Tx
	for i, d := range got {
		if d.Tx != tx {
			t.Errorf("datom[%d].Tx = %q, want %q", i, d.Tx, tx)
		}
	}
	// kind value is "Trail".
	var kind string
	if err := json.Unmarshal(got[0].V, &kind); err != nil || kind != KindTrail {
		t.Errorf("kind value = %q (err=%v), want %q", kind, err, KindTrail)
	}
}

func TestBeginRejectsMissingAgent(t *testing.T) {
	app := &fakeAppender{}
	_, err := Begin(app, "tester", "inv-1", "", "label", nil)
	requireValidation(t, err, "MISSING_AGENT")
	if len(app.groups) != 0 {
		t.Errorf("group written despite validation failure: %+v", app.groups)
	}
}

func TestBeginRejectsMissingName(t *testing.T) {
	app := &fakeAppender{}
	_, err := Begin(app, "tester", "inv-1", "agent", "", nil)
	requireValidation(t, err, "MISSING_NAME")
}

func TestBeginPropagatesAppenderError(t *testing.T) {
	app := &fakeAppender{err: errors.New("disk full")}
	_, err := Begin(app, "tester", "inv-1", "a", "b", nil)
	requireOperational(t, err, "LOG_APPEND_FAILED")
}

// ---------------------------------------------------------------------
// End
// ---------------------------------------------------------------------

func TestEndWritesEndedAtAndSummary(t *testing.T) {
	app := &fakeAppender{}
	err := End(app, "tester", "inv-1", "trail:abc", "narrative goes here",
		fixedClock("2026-04-10T13:00:00Z"))
	if err != nil {
		t.Fatalf("End: %v", err)
	}
	if len(app.groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(app.groups))
	}
	got := app.groups[0]
	if len(got) != 2 || got[0].A != AttrEndedAt || got[1].A != AttrSummary {
		t.Errorf("attrs = [%s, %s], want [%s, %s]",
			got[0].A, got[1].A, AttrEndedAt, AttrSummary)
	}
	for _, d := range got {
		if d.E != "trail:abc" {
			t.Errorf("E = %q, want trail:abc", d.E)
		}
	}
	var summary string
	if err := json.Unmarshal(got[1].V, &summary); err != nil || summary == "" {
		t.Errorf("summary value invalid: %v / %q", err, summary)
	}
}

func TestEndRejectsEmptySummary(t *testing.T) {
	app := &fakeAppender{}
	err := End(app, "tester", "inv-1", "trail:abc", "", nil)
	requireValidation(t, err, "EMPTY_SUMMARY")
}

func TestEndRejectsMissingTrailID(t *testing.T) {
	app := &fakeAppender{}
	err := End(app, "tester", "inv-1", "", "summary", nil)
	requireValidation(t, err, "MISSING_TRAIL_ID")
}

// ---------------------------------------------------------------------
// helpers shared with read_test.go
// ---------------------------------------------------------------------

func requireValidation(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", code)
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Kind != errs.KindValidation || e.Code != code {
		t.Fatalf("err = %v, want validation %s", err, code)
	}
}

func requireOperational(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", code)
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Kind != errs.KindOperational || e.Code != code {
		t.Fatalf("err = %v, want operational %s", err, code)
	}
}

// makeTx returns a deterministic-looking but unique ULID for tests
// that need to assert on tx ordering across synthetic datoms.
func makeTx() string { return ulid.Make().String() }
