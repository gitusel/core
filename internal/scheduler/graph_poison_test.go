package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"

	"windshift/internal/database"
	"windshift/internal/email"
	"windshift/internal/models"
)

func TestGraphPoisonContinuesOnlyAfterRepeatedFailures(t *testing.T) {
	exhausted := &models.EmailIntakeState{
		FailedMessageUID:         7,
		FailedMessageUIDValidity: 1,
		FailedMessageCount:       maxDeliveryAttempts - 1,
	}
	if !graphPoisonExhausted(true, exhausted, 7, 1) {
		t.Fatal("graph page must skip a message that has already exhausted delivery attempts")
	}
	if graphPoisonExhausted(false, exhausted, 7, 1) {
		t.Fatal("IMAP must keep advancing its own UID watermark instead of continuing a Graph page")
	}
	if graphPoisonExhausted(true, &models.EmailIntakeState{}, 7, 1) {
		t.Fatal("first graph failure must be retried, not dropped")
	}
}

func TestGraphBaselineDonePersistsCursorAndValidityTogether(t *testing.T) {
	db := &graphBaselineDB{}
	es := &EmailScheduler{db: db}
	client := &fakeGraphCursor{link: "https://graph.example/users/box/mailFolders/inbox/messages/delta?$deltatoken=done"}
	intake := &models.Intake{ID: 7}
	if err := es.commitGraphBaseline(context.Background(), channelInfo{ID: 3}, intake, client, true, 1); err != nil {
		t.Fatalf("commit done baseline: %v", err)
	}
	if db.writes != 1 {
		t.Fatalf("writes = %d, want one atomic update", db.writes)
	}
	if !strings.Contains(db.query, "graph_delta_link") || !strings.Contains(db.query, "uid_validity") {
		t.Fatalf("query = %q, want the resume link and validity in the same update", db.query)
	}
	db.err = errors.New("disk full")
	if err := es.commitGraphBaseline(context.Background(), channelInfo{ID: 3}, intake, client, true, 1); err == nil {
		t.Fatal("baseline write failure must surface so the next tick retries instead of discarding incremental mail")
	}
	db.err = nil
	db.writes = 0
	if err := es.commitGraphBaseline(context.Background(), channelInfo{ID: 3}, intake, client, false, 1); err != nil {
		t.Fatalf("commit partial baseline: %v", err)
	}
	if strings.Contains(db.query, "uid_validity") {
		t.Fatalf("unfinished baseline set validity: %s", db.query)
	}
}

type fakeGraphCursor struct {
	link string
}

func (f *fakeGraphCursor) SelectMailbox(string) (*imap.SelectData, error) { return nil, nil }
func (f *fakeGraphCursor) FetchMessages(uint32, int) ([]*email.FetchedMessage, error) {
	return nil, nil
}
func (f *fakeGraphCursor) MarkAsRead(uint32) error    { return nil }
func (f *fakeGraphCursor) DeleteMessage(uint32) error { return nil }
func (f *fakeGraphCursor) Expunge() error             { return nil }
func (f *fakeGraphCursor) Close() error               { return nil }
func (f *fakeGraphCursor) RestoreGraphDelta(string)   {}
func (f *fakeGraphCursor) GraphDelta() string         { return f.link }

type graphBaselineDB struct {
	query  string
	writes int
	err    error
}

func (d *graphBaselineDB) ExecWriteContext(_ context.Context, query string, _ ...any) (sql.Result, error) {
	d.query = query
	d.writes++
	if d.err != nil {
		return nil, d.err
	}
	return graphBaselineResult(1), nil
}
func (d *graphBaselineDB) Query(string, ...any) (*sql.Rows, error) { return nil, nil }
func (d *graphBaselineDB) QueryRow(string, ...any) *sql.Row        { return nil }
func (d *graphBaselineDB) Exec(string, ...any) (sql.Result, error) { return nil, nil }
func (d *graphBaselineDB) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, nil
}
func (d *graphBaselineDB) QueryRowContext(context.Context, string, ...any) *sql.Row {
	return nil
}
func (d *graphBaselineDB) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, nil
}
func (d *graphBaselineDB) ExecWrite(string, ...any) (sql.Result, error) { return nil, nil }
func (d *graphBaselineDB) Begin() (database.Tx, error)                  { return nil, errors.New("unused") }
func (d *graphBaselineDB) BeginTx(context.Context, *sql.TxOptions) (database.Tx, error) {
	return nil, errors.New("unused")
}
func (d *graphBaselineDB) Close() error          { return nil }
func (d *graphBaselineDB) Initialize() error     { return nil }
func (d *graphBaselineDB) GetDB() *sql.DB        { return nil }
func (d *graphBaselineDB) GetDriverName() string { return "sqlite" }

type graphBaselineResult int64

func (r graphBaselineResult) LastInsertId() (int64, error) { return 0, nil }
func (r graphBaselineResult) RowsAffected() (int64, error) { return int64(r), nil }
