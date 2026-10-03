package rawretention

import (
	"context"
	"strings"
	"testing"

	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/jackc/pgx/v5"
)

// Read the initialized catalog rather than matching SQL source strings. These
// requirements mirror database_schema_catalog.validate(strict=True), with
// explicit documentation of the additional fence/approval provenance states.
const rawRetentionMetadataProblems = `
WITH raw_tables AS (
 SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relkind IN ('r','p') AND c.relname ~ '^billing_raw_retention_'
), required_columns AS (
 SELECT c.oid,c.relname,a.attnum,a.attname,
   EXISTS(SELECT 1 FROM pg_constraint k WHERE k.conrelid=c.oid AND k.contype IN ('p','f') AND a.attnum=ANY(k.conkey)) AS is_key,
   (a.attname ~ '(^|_)(status|state|count|bytes)$' OR a.attname ~ '_minor$'
     OR (c.relname='billing_raw_retention_policies' AND a.attname='active')
     OR (c.relname='billing_raw_retention_clearances' AND a.attname='revoked')
     OR (c.relname='billing_raw_retention_operations' AND a.attname='decision_origin')) AS is_semantic
 FROM raw_tables c JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
)
SELECT 'table:'||c.relname FROM raw_tables c
LEFT JOIN schema_metadata m ON m.object_kind='table' AND m.schema_name='public' AND m.object_name=c.relname
LEFT JOIN schema_metadata g ON g.object_kind='group' AND g.schema_name='public' AND g.object_name=m.details->>'group'
WHERE m.meta_version IS DISTINCT FROM 1 OR g.meta_version IS DISTINCT FROM 1
 OR m.details->>'group' IS DISTINCT FROM 'Raw billing retention'
 OR coalesce(m.details->>'purpose','')='' OR coalesce(m.details->>'scenario','')=''
 OR coalesce(g.details->>'purpose','')='' OR coalesce(g.details->>'scenario','')=''
 OR obj_description(c.oid,'pg_class') IS DISTINCT FROM m.details->>'purpose'
UNION ALL
SELECT 'column:'||c.relname||'.'||c.attname FROM required_columns c
LEFT JOIN schema_metadata m ON m.object_kind='column' AND m.schema_name='public' AND m.object_name=c.relname||'.'||c.attname
WHERE (c.is_key OR c.is_semantic) AND (
 m.meta_version IS DISTINCT FROM 1 OR coalesce(m.details->>'description','')=''
 OR col_description(c.oid,c.attnum) IS DISTINCT FROM m.details->>'description'
 OR (c.is_key AND m.details->>'critical' IS DISTINCT FROM 'key')
 OR (c.is_semantic AND coalesce(m.details->>'semantic_kind','')=''))
ORDER BY 1`

type rawMetadataQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func rawMetadataProblems(t *testing.T, db rawMetadataQuerier) []string {
	t.Helper()
	rows, err := db.Query(context.Background(), rawRetentionMetadataProblems)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var problem string
		if err := rows.Scan(&problem); err != nil {
			t.Fatal(err)
		}
		problems = append(problems, problem)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return problems
}

func TestRawRetentionSchemaMetadataDocumentsInitializedKeysAndStates(t *testing.T) {
	f := newAuthorityFixture(t)
	ctx := context.Background()
	if err := database.Migrate(ctx, f.db); err != nil {
		t.Fatal(err)
	}
	var tables int
	var applied bool
	if err := f.db.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname='public' AND c.relkind IN ('r','p') AND c.relname ~ '^billing_raw_retention_'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version='072_raw_retention_schema_metadata.sql')`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if tables != 5 || !applied {
		t.Fatalf("initialized raw authority tables=%d, metadata migration applied=%v", tables, applied)
	}
	if problems := rawMetadataProblems(t, f.db); len(problems) != 0 {
		t.Fatalf("metadata gaps: %v", problems)
	}
	for _, test := range []struct {
		name, sql, want string
	}{
		{"purpose", `UPDATE schema_metadata SET details=details-'purpose' WHERE object_kind='table' AND object_name='billing_raw_retention_operations'`, "table:billing_raw_retention_operations"},
		{"group", `DELETE FROM schema_metadata WHERE object_kind='group' AND object_name='Raw billing retention'`, "table:billing_raw_retention_policies"},
		{"primary-key", `UPDATE schema_metadata SET details=details-'description' WHERE object_kind='column' AND object_name='billing_raw_retention_operations.operation_id'`, "column:billing_raw_retention_operations.operation_id"},
		{"composite-foreign-key", `UPDATE schema_metadata SET details=details-'description' WHERE object_kind='column' AND object_name='billing_raw_retention_clearances.policy_version'`, "column:billing_raw_retention_clearances.policy_version"},
		{"status-kind", `UPDATE schema_metadata SET details=details-'semantic_kind' WHERE object_kind='column' AND object_name='billing_raw_retention_holds.status'`, "column:billing_raw_retention_holds.status"},
		{"decision-origin", `UPDATE schema_metadata SET details=details-'semantic_kind' WHERE object_kind='column' AND object_name='billing_raw_retention_operations.decision_origin'`, "column:billing_raw_retention_operations.decision_origin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := f.db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, test.sql); err != nil {
				t.Fatal(err)
			}
			if problems := rawMetadataProblems(t, tx); !strings.Contains(strings.Join(problems, "\n"), test.want) {
				t.Fatalf("negative control did not detect %s: %v", test.want, problems)
			}
		})
	}
	if problems := rawMetadataProblems(t, f.db); len(problems) != 0 {
		t.Fatalf("negative controls must roll back: %v", problems)
	}
}
