package hubstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAuditBurstCannotGrowPastRowCapWithinRetentionDay(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "hub.db"), DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	// This verifies the row bound, not insertion speed. SQLite's race
	// instrumentation and shared CI CPUs can make the 100005-row fixture slow.
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Minute)
	defer stop()
	_, e = s.db.ExecContext(ctx, `WITH RECURSIVE fixture(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM fixture WHERE n<?)
 INSERT INTO audit(at,principal,action,outcome) SELECT ?, 'fixture', 'read', 'ok' FROM fixture`, MaxAuditRows+5, time.Now().Unix())
	if e != nil {
		t.Fatal(e)
	}
	var count, first, last int64
	if e = s.db.QueryRowContext(ctx, "SELECT count(*),min(id),max(id) FROM audit").Scan(&count, &first, &last); e != nil {
		t.Fatal(e)
	}
	if count != MaxAuditRows || first != 6 || last != MaxAuditRows+5 {
		t.Fatal("audit burst not bounded", count, first, last)
	}
	if e = s.Record(ctx, AuditEntry{Principal: "latest", Action: "confirmed", Outcome: "ok"}); e != nil {
		t.Fatal(e)
	}
	rows, e := s.Audit(ctx, 1, 0)
	if e != nil || len(rows) != 1 || rows[0].Principal != "latest" {
		t.Fatal("latest audit lost", rows, e)
	}
	if e = s.db.QueryRowContext(ctx, "SELECT count(*) FROM audit").Scan(&count); e != nil || count != MaxAuditRows {
		t.Fatal("normal record broke cap", count, e)
	}
}
