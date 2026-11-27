package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"windshift/internal/database"
	"windshift/internal/mailbox"
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
	client := &fakeMailbox{link: "https://graph.example/users/box/mailFolders/inbox/messages/delta?$deltatoken=done"}
	intake := &models.Intake{ID: 7}
	if err := es.commitGraphBaseline(context.Background(), channelInfo{ID: 3}, intake, client, true); err != nil {
		t.Fatalf("commit done baseline: %v", err)
	}
	if db.writes != 1 {
		t.Fatalf("writes = %d, want one atomic update", db.writes)
	}
	if !strings.Contains(db.query, "graph_delta_link") || !strings.Contains(db.query, "uid_validity") {
		t.Fatalf("query = %q, want the resume link and validity in the same update", db.query)
	}
	db.err = errors.New("disk full")
	if err := es.commitGraphBaseline(context.Background(), channelInfo{ID: 3}, intake, client, true); err == nil {
		t.Fatal("baseline write failure must surface so the next tick retries instead of discarding incremental mail")
	}
	db.err = nil
	db.writes = 0
	if err := es.commitGraphBaseline(context.Background(), channelInfo{ID: 3}, intake, client, false); err != nil {
		t.Fatalf("commit partial baseline: %v", err)
	}
	if strings.Contains(db.query, "uid_validity") {
		t.Fatalf("unfinished baseline set validity: %s", db.query)
	}
}

// fakeMailbox is a minimal Mailbox whose checkpoint is a fixed string. It
// stands in for any transport: the baseline commit only cares that the
// checkpoint is one opaque value.
type fakeMailbox struct {
	link string
}

func (f *fakeMailbox) Open(context.Context, string, bool) (mailbox.Info, error) {
	return mailbox.Info{}, nil
}
func (f *fakeMailbox) Fetch(context.Context, int) ([]mailbox.Delivery, error) { return nil, nil }
func (f *fakeMailbox) MarkRead(context.Context, string) error                 { return nil }
func (f *fakeMailbox) Delete(context.Context, string) error                   { return nil }
func (f *fakeMailbox) Expunge(context.Context) error                          { return nil }
func (f *fakeMailbox) Restore(string)                                         {}
func (f *fakeMailbox) Acknowledge()                                           {}
func (f *fakeMailbox) Close() error                                           { return nil }
func (f *fakeMailbox) Checkpoint() string                                     { return f.link }

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
func (d *graphBaselineDB) QueryReadContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, nil
}
func (d *graphBaselineDB) QueryRowReadContext(context.Context, string, ...any) *sql.Row {
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
func (d *graphBaselineDB) NamedPools() []database.NamedPool {
	return nil
}

type graphBaselineResult int64

func (r graphBaselineResult) LastInsertId() (int64, error) { return 0, nil }
func (r graphBaselineResult) RowsAffected() (int64, error) { return int64(r), nil }

// TestResumeCheckpointCoversBothTransports pins that a mailbox is restored from
// whatever the previous tick committed. Graph keeps an opaque link; IMAP used to
// keep numeric columns, and rows written before the checkpoint column carried a
// link must still resume rather than refetch from UID 0.
func TestResumeCheckpointCoversBothTransports(t *testing.T) {
	graph := &models.EmailIntakeState{
		GraphDeltaLink: "ws-graph-v1\nfolder=inbox\nhttps://graph.example/delta?$deltatoken=x",
	}
	if got := resumeCheckpoint(graph); got != graph.GraphDeltaLink {
		t.Fatalf("graph checkpoint = %q, want the stored link", got)
	}

	legacyIMAP := &models.EmailIntakeState{LastUID: 480, UIDValidity: 100}
	if got := resumeCheckpoint(legacyIMAP); got != "imap-v1:100:480" {
		t.Fatalf("legacy IMAP checkpoint = %q, want the watermark encoded for the mailbox", got)
	}

	if got := resumeCheckpoint(&models.EmailIntakeState{}); got != "" {
		t.Fatalf("empty state = %q, want no checkpoint", got)
	}
}
