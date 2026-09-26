// Copyright (c) 2026 MiniStack Contributors. SPDX-License-Identifier: MIT
// Copies or substantial portions, including AI-assisted ports or rewrites, must retain this notice (see LICENSE).
//
// Go port of the validator unit cases of MiniStack 1.5.9 tests/test_dsql.py.
// MiniStack's LICENSE is reproduced in THIRD_PARTY_NOTICES at the root of this
// repository.

package pgproxy

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTxn() *TxnState { return &TxnState{} }

func requireRejected(t *testing.T, sql string, txn *TxnState) *DsqlError {
	t.Helper()
	err, rw := validate(sql, txn)
	require.Nil(t, rw, sql)
	require.NotNil(t, err, "expected %q to be rejected", sql)
	return err
}

func requireAccepted(t *testing.T, sql string, txn *TxnState) {
	t.Helper()
	err, rw := validate(sql, txn)
	require.Nil(t, err, "expected %q to be accepted", sql)
	require.Nil(t, rw, sql)
}

func requireRewrite(t *testing.T, sql string) *Rewrite {
	t.Helper()
	err, rw := validate(sql, newTxn())
	require.Nil(t, err, sql)
	require.NotNil(t, rw, "expected %q to be rewritten", sql)
	return rw
}

func TestClassifyStatement(t *testing.T) {
	cases := []struct {
		sql  string
		want stmtClass
	}{
		{"CREATE TABLE t (a int)", classDDL},
		{"alter table t add column b int", classDDL},
		{"DROP INDEX idx", classDDL},
		{"INSERT INTO t VALUES (1)", classDML},
		{"update t set a = 1", classDML},
		{"DELETE FROM t", classDML},
		{"BEGIN", classBegin},
		{"START TRANSACTION", classBegin},
		{"COMMIT", classEnd},
		{"END", classEnd},
		{"ROLLBACK", classEnd},
		{"ABORT", classEnd},
		{"SELECT 1", classOther},
		{"SET search_path TO public", classOther},
		{"SHOW server_version", classOther},
		{"GRANT SELECT ON t TO r", classOther},
		{"REVOKE SELECT ON t FROM r", classOther},
		{"-- migration 004\nCREATE TABLE t (a int)", classDDL},
		{"/* flyway */ INSERT INTO t VALUES (1)", classDML},
		{"/* a /* nested */ b */ COMMIT", classEnd},
		{"\n\n-- one\n-- two\nBEGIN", classBegin},
		{"WITH x AS (SELECT 1) INSERT INTO t SELECT 1", classDML},
		{"WITH x AS (SELECT 1) UPDATE t SET a = 1", classDML},
		{"WITH x AS (SELECT 1) DELETE FROM t", classDML},
		{"WITH RECURSIVE x AS (SELECT 1) INSERT INTO t SELECT 1", classDML},
		{"WITH x (a, b) AS (SELECT 1, 2) INSERT INTO t SELECT 1", classDML},
		{"WITH x AS MATERIALIZED (SELECT 1) INSERT INTO t SELECT 1", classDML},
		{"WITH a AS (SELECT 1), b AS (SELECT 2) INSERT INTO t SELECT 1", classDML},
		{"WITH x AS (SELECT 1) SELECT * FROM x", classOther},
		{"EXPLAIN ANALYZE INSERT INTO t VALUES (1)", classDML},
		{"EXPLAIN (ANALYZE, BUFFERS) DELETE FROM t", classDML},
		{"EXPLAIN (ANALYZE false) DELETE FROM t", classOther},
		{"EXPLAIN INSERT INTO t VALUES (1)", classOther},
		{"EXPLAIN SELECT 1", classOther},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			require.Equal(t, tc.want, classifyStatement(tc.sql))
		})
	}
}

func TestSplitStatements(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
	}{
		{"SELECT 1; SELECT 2", []string{"SELECT 1", "SELECT 2"}},
		{"SELECT 'a;b'", []string{"SELECT 'a;b'"}},
		{"SELECT 'it''s; fine'", []string{"SELECT 'it''s; fine'"}},
		{"SELECT $$a;b$$", []string{"SELECT $$a;b$$"}},
		{"SELECT $tag$a;b$tag$", []string{"SELECT $tag$a;b$tag$"}},
		{`SELECT "we;ird"`, []string{`SELECT "we;ird"`}},
		{"SELECT 1 -- c;omment\n; SELECT 2", []string{"SELECT 1 -- c;omment", "SELECT 2"}},
		{"SELECT 1 /* c;omment */; SELECT 2", []string{"SELECT 1 /* c;omment */", "SELECT 2"}},
		{`SELECT E'a\';b'`, []string{`SELECT E'a\';b'`}},
		{"SELECT 1;", []string{"SELECT 1"}},
		{"  ;  ", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			require.Equal(t, tc.want, splitStatements(tc.sql))
		})
	}
}

func TestStripLeadingComments(t *testing.T) {
	cases := []struct{ sql, want string }{
		{"-- x\nSELECT 1", "SELECT 1"},
		{"/* x */SELECT 1", "SELECT 1"},
		{"/* a /* b */ c */ SELECT 1", "SELECT 1"},
		{"  \n\t-- a\n  /* b */\nSELECT 1", "SELECT 1"},
		{"SELECT 1 -- trailing", "SELECT 1 -- trailing"},
		{"'-- not a comment'", "'-- not a comment'"},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			require.Equal(t, tc.want, stripLeadingComments(tc.sql))
		})
	}
}

func TestLiteralsDoNotHideABadColumn(t *testing.T) {
	for _, sql := range []string{
		"CREATE TABLE t (a text DEFAULT 'x;y', b serial)",
		"CREATE TABLE t (a text DEFAULT ')(', b serial)",
	} {
		err := requireRejected(t, sql, nil)
		require.Contains(t, err.Message, "serial")
	}
}

func TestLeadingCommentDoesNotBypassTheDenylist(t *testing.T) {
	for _, sql := range []string{
		"-- migration\nCREATE EXTENSION pgcrypto",
		"/* tooling */ TRUNCATE t",
		"\n-- a\n/* b */ CREATE TEMP TABLE t (a int)",
	} {
		requireRejected(t, sql, nil)
	}
}

func TestDenylist(t *testing.T) {
	denied := []string{
		"CREATE DATABASE foo",
		"CREATE TYPE mood AS ENUM ('sad')",
		"CREATE EXTENSION pgcrypto",
		"CREATE TRIGGER trg AFTER INSERT ON t EXECUTE FUNCTION f()",
		"CREATE PROCEDURE p() LANGUAGE sql AS $$ SELECT 1 $$",
		"CREATE TABLESPACE ts LOCATION '/tmp/x'",
		"CREATE TEMP TABLE t (a int)",
		"CREATE TEMPORARY TABLE t (a int)",
		"CREATE UNLOGGED TABLE t (a int)",
		"COPY t FROM STDIN",
		"LISTEN chan",
		"NOTIFY chan",
		"UNLISTEN chan",
		"TRUNCATE t",
		"DO $$ BEGIN NULL; END $$",
		"SAVEPOINT sp",
		"LOCK TABLE t IN ACCESS EXCLUSIVE MODE",
		"CREATE FUNCTION f() RETURNS int LANGUAGE plpgsql AS $$ BEGIN RETURN 1; END $$",
		"CREATE FUNCTION f() RETURNS int LANGUAGE C AS 'x'",
		"CREATE MATERIALIZED VIEW mv AS SELECT 1",
		"CREATE TABLE t (a int) PARTITION BY RANGE (a)",
		"CREATE TABLE p (a int, EXCLUDE (a WITH =))",
		"CREATE RULE r AS ON DELETE TO t DO INSTEAD NOTHING",
		"CREATE AGGREGATE a (int) (sfunc = f, stype = int)",
		"CREATE DOMAIN d AS int",
		"CREATE CAST (int AS text) WITH FUNCTION f(int)",
		"CREATE COLLATION c (locale = 'en_US')",
		"CREATE OPERATOR + (leftarg = int, rightarg = int, function = f)",
		"CREATE PUBLICATION p FOR ALL TABLES",
		"CREATE SUBSCRIPTION s CONNECTION 'x' PUBLICATION p",
		"CREATE FOREIGN TABLE ft (a int) SERVER s",
		"CREATE SERVER s FOREIGN DATA WRAPPER w",
		"VACUUM t",
		"VACUUM FULL ANALYZE t",
		"CLUSTER t USING idx",
		"REINDEX TABLE t",
		"ALTER SYSTEM SET work_mem = '4MB'",
		"PREPARE TRANSACTION 'gid'",
		"COMMIT PREPARED 'gid'",
		"ROLLBACK PREPARED 'gid'",
	}
	for _, sql := range denied {
		t.Run(sql, func(t *testing.T) {
			err := requireRejected(t, sql, newTxn())
			require.Equal(t, "0A000", err.SQLState)
			require.Contains(t, err.Message, "is not supported")
		})
	}
	t.Run("language sql function allowed", func(t *testing.T) {
		requireAccepted(t, "CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$", newTxn())
	})
	t.Run("plain table allowed", func(t *testing.T) {
		requireAccepted(t, "CREATE TABLE t (a int)", newTxn())
	})
}

func TestAddColumnTakesNoConstraint(t *testing.T) {
	const withConstraint = "ALTER TABLE ADD COLUMN with constraint not supported"
	cases := []struct{ sql, message string }{
		{"ALTER TABLE t ADD COLUMN c int DEFAULT 0", withConstraint},
		{"ALTER TABLE t ADD COLUMN c text NOT NULL", withConstraint},
		{"ALTER TABLE t ADD COLUMN c int UNIQUE", withConstraint},
		{"ALTER TABLE t ADD COLUMN c int CHECK (c > 0)", withConstraint},
		{"ALTER TABLE t ADD COLUMN c int REFERENCES o", withConstraint},
		{"ALTER TABLE t ADD COLUMN c text GENERATED ALWAYS AS (lower(e)) STORED", withConstraint},
		{"ALTER TABLE t ADD COLUMN c bigint GENERATED BY DEFAULT AS IDENTITY (CACHE 1)", withConstraint},
		{`ALTER TABLE t ADD COLUMN c text COLLATE "C"`, "COLLATE clause not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			err := requireRejected(t, tc.sql, newTxn())
			require.Equal(t, "0A000", err.SQLState)
			require.Equal(t, tc.message, err.Message)
		})
	}
}

func TestColumnTypes(t *testing.T) {
	unsupported := []string{
		"serial", "bigserial", "smallserial",
		"money", "xml", "inet", "cidr", "macaddr", "macaddr8",
		"bit", "bit varying", "varbit",
		"point", "line", "lseg", "box", "path", "polygon", "circle",
		"hstore",
		"int[]", "text[]",
		"interval foo",
	}
	for _, typ := range unsupported {
		t.Run("denied "+typ, func(t *testing.T) {
			err := requireRejected(t, fmt.Sprintf("CREATE TABLE t (id int, c %s)", typ), newTxn())
			require.Equal(t, "0A000", err.SQLState)
			require.Contains(t, err.Message, `type "`)
			require.Contains(t, err.Message, "is not supported")
		})
	}
	supported := []string{
		"smallint", "int2", "integer", "int", "int4", "bigint", "int8",
		"real", "float4", "double precision", "float8",
		"numeric", "decimal", "dec", "numeric(18,6)", "dec(10,2)",
		"char(1)", "character(1)", "bpchar", "varchar(255)",
		"character varying(10)", "text",
		"date", "time", "time with time zone", "timetz",
		"timestamp", "timestamp with time zone", "timestamptz",
		"interval", "interval year", "interval day to second",
		"interval year to month", "interval second", "boolean", "bool", "bytea", "uuid", "json", "jsonb",
	}
	for _, typ := range supported {
		t.Run("allowed "+typ, func(t *testing.T) {
			requireAccepted(t, fmt.Sprintf("CREATE TABLE t (id int, c %s)", typ), newTxn())
		})
	}
	t.Run("alter add column serial", func(t *testing.T) {
		err := requireRejected(t, "ALTER TABLE t ADD COLUMN c serial", newTxn())
		require.Equal(t, "0A000", err.SQLState)
		require.Contains(t, err.Message, `"serial"`)
	})
	t.Run("table constraints skipped", func(t *testing.T) {
		requireAccepted(t, "CREATE TABLE t (id int PRIMARY KEY, c text UNIQUE, CHECK (id > 0))", newTxn())
	})
	t.Run("storage modifier", func(t *testing.T) {
		requireAccepted(t, "CREATE TABLE t (id int, j jsonb STORAGE PLAIN)", newTxn())
		requireAccepted(t, "ALTER TABLE t ADD COLUMN j jsonb STORAGE EXTERNAL", newTxn())
		requireAccepted(t, "ALTER TABLE t ADD COLUMN j jsonb STORAGE DEFAULT", newTxn())
	})
}

func TestAlterTableSubset(t *testing.T) {
	denied := []string{
		"ALTER TABLE t ALTER COLUMN c TYPE text",
		"ALTER TABLE t ALTER COLUMN c SET DATA TYPE text",
		"ALTER TABLE t ALTER COLUMN c SET NOT NULL",
		"ALTER TABLE t ADD CONSTRAINT pk PRIMARY KEY (id)",
		"ALTER TABLE t ADD PRIMARY KEY (id)",
		"ALTER TABLE t ADD CONSTRAINT u UNIQUE (a)",
		"ALTER TABLE t ADD CONSTRAINT c CHECK (a > 0)",
		"ALTER TABLE t ALTER COLUMN c ADD GENERATED BY DEFAULT AS IDENTITY",
	}
	for _, sql := range denied {
		t.Run("denied "+sql, func(t *testing.T) {
			require.Equal(t, "0A000", requireRejected(t, sql, newTxn()).SQLState)
		})
	}
	allowed := []string{
		"ALTER TABLE t DROP COLUMN c",
		"ALTER TABLE t DROP COLUMN IF EXISTS c CASCADE",
		"ALTER TABLE t ALTER COLUMN c SET DEFAULT 0",
		"ALTER TABLE t ALTER COLUMN c DROP DEFAULT",
		"ALTER TABLE t ALTER COLUMN c DROP NOT NULL",
		"ALTER TABLE t ALTER COLUMN c SET STORAGE PLAIN",
		"ALTER TABLE t RENAME COLUMN a TO b",
		"ALTER TABLE t RENAME TO t2",
		"ALTER TABLE t SET SCHEMA app",
		"ALTER TABLE t ADD CONSTRAINT c CHECK (a > 0) NOT VALID",
		"ALTER TABLE t ADD CONSTRAINT u UNIQUE USING INDEX idx",
		"ALTER TABLE t DROP CONSTRAINT c",
		"ALTER TABLE t ALTER COLUMN c ADD GENERATED BY DEFAULT AS IDENTITY (CACHE 1)",
		"ALTER TABLE t ADD COLUMN c integer",
		"ALTER TABLE t ADD COLUMN j jsonb STORAGE PLAIN",
		"ALTER TABLE t ADD COLUMN c int NULL",
		"ALTER TABLE t ADD COLUMN IF NOT EXISTS c numeric(18,6)",
		"ALTER TABLE t ADD COLUMN a int, ADD COLUMN b text",
	}
	for _, sql := range allowed {
		t.Run("allowed "+sql, func(t *testing.T) {
			requireAccepted(t, sql, newTxn())
		})
	}
}

func TestDroppedColumns(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
	}{
		{"ALTER TABLE t DROP COLUMN c", []string{"c"}},
		{"ALTER TABLE t DROP c", []string{"c"}},
		{"ALTER TABLE t DROP COLUMN IF EXISTS c CASCADE", []string{"c"}},
		{"ALTER TABLE t DROP COLUMN a, DROP COLUMN b", []string{"a", "b"}},
		{"ALTER TABLE t DROP a, DROP COLUMN b, DROP IF EXISTS c", []string{"a", "b", "c"}},
		{`ALTER TABLE t DROP COLUMN "MixedCase"`, []string{"MixedCase"}},
		{"ALTER TABLE t DROP CONSTRAINT c", nil},
		{"ALTER TABLE t ALTER COLUMN c DROP DEFAULT", nil},
		{"ALTER TABLE t ALTER COLUMN c DROP NOT NULL", nil},
		{"ALTER TABLE t ALTER COLUMN c DROP EXPRESSION", nil},
		{"ALTER TABLE t ALTER COLUMN c DROP IDENTITY", nil},
		{"ALTER TABLE t ADD COLUMN c int", nil},
		{"DROP TABLE t", nil},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			require.Equal(t, tc.want, droppedColumns(tc.sql))
		})
	}
	t.Run("drop column passes text-only validation", func(t *testing.T) {
		requireAccepted(t, "ALTER TABLE t DROP COLUMN c", nil)
	})
}

func TestValidateConstraintAsyncRewrite(t *testing.T) {
	rw := requireRewrite(t, "ALTER TABLE ASYNC t VALIDATE CONSTRAINT c")
	require.Equal(t, "ALTER TABLE t VALIDATE CONSTRAINT c", rw.SQL)
	require.Equal(t, jobValidate, rw.JobType)
	require.Equal(t, "public.t", rw.ObjectName)
}

func TestForeignKeys(t *testing.T) {
	allowed := []string{
		"CREATE TABLE orders (id int PRIMARY KEY, p int REFERENCES products)",
		"CREATE TABLE orders (id int PRIMARY KEY, p int REFERENCES products (product_no))",
		"CREATE TABLE orders (id int PRIMARY KEY, p int CONSTRAINT fk_product REFERENCES products)",
		"CREATE TABLE shipments (id int PRIMARY KEY, w int, p int, FOREIGN KEY (w, p) REFERENCES inventory (warehouse_id, product_no))",
		"CREATE TABLE tree (id int PRIMARY KEY, parent int REFERENCES tree)",
		"CREATE TABLE o (p int REFERENCES products ON DELETE NO ACTION)",
		"CREATE TABLE o (p int REFERENCES products ON DELETE RESTRICT)",
		"CREATE TABLE o (p int REFERENCES products ON DELETE CASCADE)",
		"CREATE TABLE o (p int REFERENCES products ON DELETE SET NULL)",
		"CREATE TABLE o (p int REFERENCES products ON DELETE SET DEFAULT)",
		"CREATE TABLE o (p int REFERENCES products ON DELETE CASCADE ON UPDATE CASCADE)",
		"CREATE TABLE s (w int, p int, FOREIGN KEY (w, p) REFERENCES inventory (w, p) MATCH FULL)",
		"CREATE TABLE s (w int, p int, FOREIGN KEY (w, p) REFERENCES inventory (w, p) MATCH SIMPLE)",
		"CREATE TABLE o (p int REFERENCES products DEFERRABLE)",
		"CREATE TABLE o (p int REFERENCES products DEFERRABLE INITIALLY DEFERRED)",
		"CREATE TABLE o (p int REFERENCES products NOT DEFERRABLE)",
		"ALTER TABLE o ALTER CONSTRAINT fk DEFERRABLE INITIALLY DEFERRED",
		"ALTER TABLE o ADD CONSTRAINT fk FOREIGN KEY (p) REFERENCES products NOT VALID",
		"ALTER TABLE o ADD FOREIGN KEY (p) REFERENCES products ON DELETE CASCADE NOT VALID",
		"ALTER TABLE o ADD CONSTRAINT fk FOREIGN KEY (p) REFERENCES products DEFERRABLE INITIALLY DEFERRED NOT VALID",
		"ALTER TABLE o DROP CONSTRAINT fk",
		"SET CONSTRAINTS ALL DEFERRED",
	}
	for _, sql := range allowed {
		t.Run("allowed "+sql, func(t *testing.T) { requireAccepted(t, sql, newTxn()) })
	}
	for _, sql := range []string{
		"ALTER TABLE o ADD CONSTRAINT fk FOREIGN KEY (p) REFERENCES products",
		"ALTER TABLE o ADD FOREIGN KEY (p) REFERENCES products ON DELETE CASCADE",
	} {
		t.Run("needs NOT VALID "+sql, func(t *testing.T) {
			err := requireRejected(t, sql, newTxn())
			require.Equal(t, "0A000", err.SQLState)
			require.Equal(t, "unsupported ALTER TABLE ADD CONSTRAINT statement", err.Message)
		})
	}
}

func TestDeferrable(t *testing.T) {
	const alterUnique = "ALTER TABLE / ADD CONSTRAINT UNIQUE / DEFERRED / INITIALLY DEFERRED not supported"
	denied := []struct{ sql, message string }{
		{"CREATE TABLE t (a int, UNIQUE (a) DEFERRABLE)", "DEFERRABLE constraint not supported"},
		{"CREATE TABLE t (a int PRIMARY KEY DEFERRABLE INITIALLY DEFERRED)", "DEFERRABLE constraint not supported"},
		{"CREATE TABLE t (a int, UNIQUE (a) INITIALLY DEFERRED)", "DEFERRABLE constraint not supported"},
		{"ALTER TABLE t ADD CONSTRAINT u UNIQUE USING INDEX idx DEFERRABLE", alterUnique},
		{"ALTER TABLE t ADD CONSTRAINT u UNIQUE USING INDEX idx INITIALLY DEFERRED", alterUnique},
		{"ALTER TABLE t ADD CONSTRAINT c CHECK (a > 0) DEFERRABLE NOT VALID", "CHECK constraints cannot be marked DEFERRABLE"},
		{"CREATE TABLE t (a int, CONSTRAINT c CHECK (a > 0) DEFERRABLE INITIALLY IMMEDIATE)", "CHECK constraints cannot be marked DEFERRABLE"},
	}
	for _, tc := range denied {
		t.Run("denied "+tc.sql, func(t *testing.T) {
			err := requireRejected(t, tc.sql, newTxn())
			require.Equal(t, "0A000", err.SQLState)
			require.Equal(t, tc.message, err.Message)
		})
	}
	accepted := []string{
		"CREATE TABLE t (a int, UNIQUE (a) NOT DEFERRABLE)",
		"CREATE TABLE t (a int PRIMARY KEY NOT DEFERRABLE)",
		"CREATE TABLE t (a int, CHECK (a > 0) NOT DEFERRABLE)",
		"CREATE TABLE t (p int UNIQUE REFERENCES o DEFERRABLE)",
		"CREATE TABLE t (a int UNIQUE, p int REFERENCES o DEFERRABLE INITIALLY DEFERRED)",
		"ALTER TABLE t ADD CONSTRAINT u UNIQUE USING INDEX idx INITIALLY IMMEDIATE",
		"ALTER TABLE t ADD CONSTRAINT u UNIQUE USING INDEX idx NOT DEFERRABLE",
		"ALTER TABLE t ADD CONSTRAINT c CHECK (a > 0) NOT DEFERRABLE NOT VALID",
		"ALTER TABLE t ADD CONSTRAINT f FOREIGN KEY (p) REFERENCES o NOT VALID DEFERRABLE",
		"CREATE TABLE t (a int, s text, CHECK (s <> 'DEFERRABLE'))",
	}
	for _, sql := range accepted {
		t.Run("accepted "+sql, func(t *testing.T) { requireAccepted(t, sql, newTxn()) })
	}
}

func TestIdentityAndSequences(t *testing.T) {
	for _, sql := range []string{
		"CREATE TABLE t (id bigint GENERATED BY DEFAULT AS IDENTITY (CACHE 1) PRIMARY KEY)",
		"CREATE TABLE t (id bigint GENERATED ALWAYS AS IDENTITY (CACHE 65536))",
		"CREATE TABLE t (id bigint GENERATED BY DEFAULT AS IDENTITY)",
		"CREATE SEQUENCE s CACHE 1",
		"CREATE SEQUENCE s CACHE 65536",
		"CREATE SEQUENCE s CACHE 1000000",
		"CREATE SEQUENCE s",
	} {
		t.Run("allowed "+sql, func(t *testing.T) { requireAccepted(t, sql, newTxn()) })
	}
	for _, sql := range []string{
		"CREATE TABLE t (id int GENERATED BY DEFAULT AS IDENTITY (CACHE 1))",
		"CREATE TABLE t (id integer GENERATED ALWAYS AS IDENTITY (CACHE 65536))",
		"CREATE TABLE t (id bigint GENERATED BY DEFAULT AS IDENTITY (CACHE 100))",
		"CREATE SEQUENCE s CACHE 100",
		"CREATE SEQUENCE s CACHE 65535",
	} {
		t.Run("denied "+sql, func(t *testing.T) {
			require.Equal(t, "0A000", requireRejected(t, sql, newTxn()).SQLState)
		})
	}
}

func TestIndexRules(t *testing.T) {
	denied := []struct{ sql, sqlstate, message string }{
		{"CREATE INDEX ASYNC IF NOT EXISTS ON t (a)", "42601", `syntax error at or near "ON"`},
		{"CREATE INDEX CONCURRENTLY i ON t (a)", "0A000", "CONCURRENTLY not supported for CREATE INDEX"},
		{"CREATE INDEX i ON t USING gin (data)", "0A000", "USING not supported for CREATE INDEX"},
		{"CREATE INDEX ASYNC i ON t USING btree (a)", "0A000", "USING not supported for CREATE INDEX"},
		{"CREATE INDEX i ON t (a) WHERE a > 0", "0A000", "WHERE not supported for CREATE INDEX"},
		{"CREATE INDEX ASYNC i ON t (a) WHERE a > 0", "0A000", "WHERE not supported for CREATE INDEX"},
		{"CREATE INDEX i ON t (a)", "0A000", "unsupported mode. please use CREATE INDEX ASYNC."},
		{"CREATE UNIQUE INDEX i ON t (a NULLS LAST) INCLUDE (b)", "0A000", "unsupported mode"},
		{"CREATE INDEX i ON t (now())", "0A000", "unsupported mode"},
		{"CREATE INDEX ASYNC i ON t (now())", "42P17", "functions in index expression must be marked IMMUTABLE"},
		{"CREATE INDEX ASYNC i ON t (random())", "42P17", "IMMUTABLE"},
		{"CREATE INDEX ASYNC i ON t (gen_random_uuid())", "42P17", "IMMUTABLE"},
		{"CREATE INDEX ASYNC i ON t (lower(email), nextval('s'))", "42P17", "IMMUTABLE"},
		{"CREATE INDEX ASYNC i ON t (a) INCLUDE (lower(b))", "0A000", "expressions are not supported in included columns"},
		{"CREATE UNIQUE INDEX ASYNC i ON t (a) INCLUDE ((b + c))", "0A000", "included columns"},
		{"CREATE INDEX ASYNC i ON t (a,b,c,d,e,f,g,h,i)", "54011", "more than 8 column keys in an index are not supported"},
	}
	for _, tc := range denied {
		t.Run("denied "+tc.sql, func(t *testing.T) {
			err := requireRejected(t, tc.sql, newTxn())
			require.Equal(t, tc.sqlstate, err.SQLState)
			require.Contains(t, err.Message, tc.message)
		})
	}
	for _, sql := range []string{
		"CREATE INDEX ASYNC i ON t (a)",
		"CREATE INDEX ASYNC i ON t (a, b DESC) INCLUDE (c)",
		"CREATE UNIQUE INDEX ASYNC i ON t (a NULLS LAST) NULLS NOT DISTINCT",
		"CREATE INDEX ASYNC i ON t (lower(email))",
		"CREATE INDEX ASYNC i ON t ((data->>'city'))",
		"CREATE INDEX ASYNC ON t (upper(title), (a + b) NULLS LAST)",
		"CREATE INDEX ASYNC i ON t (a,b,c,d,e,f,g,h)",
	} {
		t.Run("rewritten "+sql, func(t *testing.T) { requireRewrite(t, sql) })
	}
}

func TestIndexAsyncRewrite(t *testing.T) {
	require.Equal(t, "CREATE UNIQUE INDEX IF NOT EXISTS idx ON t (a)",
		rewriteIndexAsync("CREATE UNIQUE INDEX ASYNC IF NOT EXISTS idx ON t (a)"))

	rw := requireRewrite(t, "CREATE INDEX ASYNC idx ON t (a)")
	require.Equal(t, kindIndexAsync, rw.Kind)
	require.Equal(t, "CREATE INDEX idx ON t (a)", rw.SQL)
	require.Equal(t, "public.idx", rw.ObjectName)
	require.Equal(t, "t", rw.Table)

	cases := []struct{ sql, objectName string }{
		{"CREATE INDEX ASYNC idx ON app.t (a)", "app.idx"},
		{"CREATE INDEX ASYNC app.idx ON app.t (a)", "app.idx"},
		{"CREATE INDEX ASYNC ON public.t (a, b)", "public.t_a_b_idx"},
		{"CREATE INDEX ASYNC ON t (email)", "public.t_email_idx"},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			require.Equal(t, tc.objectName, requireRewrite(t, tc.sql).ObjectName)
		})
	}
}

func TestWaitForJob(t *testing.T) {
	cases := []struct {
		sql   string
		want  waitQuery
		match bool
	}{
		{"SELECT sys.wait_for_job(job_id) 'abc123'", waitQuery{JobID: "abc123"}, true},
		{"SELECT sys.wait_for_job('abc123')", waitQuery{JobID: "abc123"}, true},
		{"CALL sys.wait_for_job('abc123')", waitQuery{JobID: "abc123"}, true},
		{"SELECT sys.wait_for_job($1)", waitQuery{Param: 1}, true},
		{"SELECT 1", waitQuery{}, false},
		{"SELECT sys.wait_for_job(job_id)", waitQuery{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			got, ok := matchWaitForJob(tc.sql)
			require.Equal(t, tc.match, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestMatchSysJobs(t *testing.T) {
	cases := []struct {
		sql   string
		want  jobsQuery
		match bool
	}{
		{"SELECT * FROM sys.jobs", jobsQuery{}, true},
		{`SELECT job_id, "status" FROM sys.jobs`, jobsQuery{Columns: []string{"job_id", "status"}}, true},
		{"SELECT status FROM sys.jobs WHERE job_id = 'abc'", jobsQuery{Columns: []string{"status"}, Filter: "abc", HasFilter: true}, true},
		{"SELECT status FROM sys.jobs WHERE job_id = $1", jobsQuery{Columns: []string{"status"}, FilterParam: 1}, true},
		{"SELECT * FROM jobs", jobsQuery{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			got, ok := matchSysJobs(tc.sql)
			require.Equal(t, tc.match, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestTxnDiscipline(t *testing.T) {
	t.Run("second DDL in a transaction", func(t *testing.T) {
		txn := newTxn()
		txn.apply("BEGIN")
		txn.apply("CREATE TABLE a (x int)")
		err := requireRejected(t, "CREATE TABLE b (x int)", txn)
		require.Equal(t, "25006", err.SQLState)
		require.Contains(t, err.Message, "only one DDL")
	})
	t.Run("DDL after DML", func(t *testing.T) {
		txn := newTxn()
		txn.apply("BEGIN")
		txn.apply("INSERT INTO t VALUES (1)")
		err := requireRejected(t, "CREATE TABLE b (x int)", txn)
		require.Equal(t, "25006", err.SQLState)
		require.Contains(t, err.Message, "cannot be mixed")
	})
	t.Run("DML after DDL", func(t *testing.T) {
		txn := newTxn()
		txn.apply("BEGIN")
		txn.apply("CREATE TABLE a (x int)")
		err := requireRejected(t, "INSERT INTO a VALUES (1)", txn)
		require.Equal(t, "25006", err.SQLState)
		require.Contains(t, err.Message, "cannot be mixed")
	})
	t.Run("commit resets", func(t *testing.T) {
		txn := newTxn()
		txn.apply("BEGIN")
		txn.apply("CREATE TABLE a (x int)")
		txn.apply("COMMIT")
		requireAccepted(t, "CREATE TABLE b (x int)", txn)
	})
	t.Run("autocommit sequential DDL", func(t *testing.T) {
		txn := newTxn()
		requireAccepted(t, "CREATE TABLE a (x int)", txn)
		txn.apply("CREATE TABLE a (x int)")
		requireAccepted(t, "CREATE TABLE b (x int)", txn)
	})
	t.Run("select and insert outside a transaction", func(t *testing.T) {
		txn := newTxn()
		requireAccepted(t, "SELECT 1", txn)
		requireAccepted(t, "INSERT INTO t VALUES (1)", txn)
	})
	t.Run("backend status drives in-transaction state", func(t *testing.T) {
		txn := newTxn()
		txn.noteBackendStatus('T')
		require.True(t, txn.InTxn)
		txn.noteBackendStatus('E')
		require.True(t, txn.Aborted)
		txn.noteBackendStatus('I')
		require.False(t, txn.InTxn)
		require.False(t, txn.Aborted)
	})
}

func valuesInsert(n int) string {
	tuples := make([]string, n)
	for i := range tuples {
		tuples[i] = fmt.Sprintf("(%d)", i)
	}
	return "INSERT INTO t VALUES " + strings.Join(tuples, ",")
}

func TestTxnLimits(t *testing.T) {
	t.Run("over 3000 rows", func(t *testing.T) {
		err := requireRejected(t, valuesInsert(3001), newTxn())
		require.Equal(t, "25006", err.SQLState)
		require.Contains(t, err.Message, "3,000")
	})
	t.Run("exactly 3000 rows", func(t *testing.T) {
		requireAccepted(t, valuesInsert(3000), newTxn())
	})
	t.Run("cumulative rows", func(t *testing.T) {
		txn := newTxn()
		txn.apply("BEGIN")
		batch := valuesInsert(2000)
		requireAccepted(t, batch, txn)
		txn.apply(batch)
		require.Contains(t, requireRejected(t, batch, txn).Message, "3,000")
	})
	t.Run("oversized statement", func(t *testing.T) {
		sql := "INSERT INTO t VALUES ('" + strings.Repeat("x", 10*1024*1024+1) + "')"
		require.Contains(t, requireRejected(t, sql, newTxn()).Message, "10 MiB")
	})
	t.Run("duration", func(t *testing.T) {
		txn := newTxn()
		txn.apply("BEGIN")
		txn.StartedAt = txn.StartedAt.Add(-301 * time.Second)
		require.Contains(t, requireRejected(t, "INSERT INTO t VALUES (1)", txn).Message, "5 minute")
	})
	t.Run("values tuple count", func(t *testing.T) {
		require.Equal(t, 2, valuesTupleCount("INSERT INTO t VALUES (1, '(a'), (2)"))
		require.Equal(t, 0, valuesTupleCount("INSERT INTO t SELECT * FROM s"))
		require.Equal(t, 0, valuesTupleCount("UPDATE t SET a = 1"))
	})
}

func TestIdentifierNormalization(t *testing.T) {
	fold := []struct {
		raw    string
		name   string
		quoted bool
	}{
		{"id", "id", false},
		{"ID", "id", false},
		{"  id  ", "id", false},
		{`"id"`, "id", true},
		{`"ID"`, "ID", true},
		{`"we ird"`, "we ird", true},
		{`"quo""ted"`, `quo"ted`, true},
	}
	for _, tc := range fold {
		t.Run(tc.raw, func(t *testing.T) {
			name, quoted := foldIdentifier(tc.raw)
			require.Equal(t, tc.name, name)
			require.Equal(t, tc.quoted, quoted)
		})
	}
	paths := []struct {
		raw  string
		want []string
	}{
		{"id", []string{"id"}},
		{"t.id", []string{"t", "id"}},
		{`"T".id`, []string{"T", "id"}},
		{`t."ID"`, []string{"t", "ID"}},
		{`"my schema"."My Table"`, []string{"my schema", "My Table"}},
		{"Schema.Tbl.Col", []string{"schema", "tbl", "col"}},
	}
	for _, tc := range paths {
		t.Run(tc.raw, func(t *testing.T) {
			require.Equal(t, tc.want, identifierPath(tc.raw))
		})
	}
}

func TestLockingClauses(t *testing.T) {
	const message = "locking clauses other than FOR UPDATE/FOR KEY SHARE are not supported"
	for _, sql := range []string{
		"SELECT s FROM t WHERE id = '1' FOR UPDATE",
		`SELECT "id", "s" FROM "t" WHERE "t"."id" = $1 FOR UPDATE`,
		"SELECT s FROM t FOR UPDATE",
		"SELECT s FROM t WHERE s = 'a' FOR UPDATE",
		"SELECT s FROM t WHERE id > '1' FOR UPDATE",
		"SELECT s FROM t WHERE id IN (1, 2) FOR UPDATE",
		"SELECT s FROM t WHERE id = 1 OR s = 'a' FOR UPDATE",
		"SELECT a.s FROM t a JOIN t b ON a.id = b.id FOR UPDATE",
		"SELECT s FROM t, u WHERE t.id = u.id FOR UPDATE",
		"SELECT 1 FOR UPDATE",
		"SELECT s FROM t WHERE id = 1 FOR UPDATE NOWAIT",
		"SELECT s FROM t WHERE id = 1 FOR UPDATE SKIP LOCKED",
		"SELECT s FROM t WHERE id = 1 FOR UPDATE OF t NOWAIT",
		"SELECT * FROM (SELECT 1 FOR UPDATE) x FOR UPDATE",
		"SELECT s FROM t WHERE id = 1 FOR KEY SHARE",
		"SELECT s FROM t FOR KEY SHARE NOWAIT",
		"SELECT * FROM (SELECT s FROM t FOR KEY SHARE) y",
		"WITH x AS (SELECT s FROM t FOR KEY SHARE) SELECT * FROM x",
		"SELECT s FROM t WHERE s = 'FOR SHARE'",
		"SELECT 'FOR NO KEY UPDATE' AS lit",
		"DECLARE c CURSOR FOR SELECT 1",
		"CREATE POLICY p ON t FOR UPDATE TO someone",
	} {
		t.Run("allowed "+sql, func(t *testing.T) { requireAccepted(t, sql, newTxn()) })
	}
	for _, sql := range []string{
		"SELECT s FROM t WHERE id = 1 FOR SHARE",
		"SELECT s FROM t WHERE id = 1 FOR NO KEY UPDATE",
		"SELECT s FROM t for share",
		"SELECT s FROM t FOR SHARE NOWAIT",
		"WITH x AS (SELECT s FROM t WHERE id = 1 FOR SHARE) SELECT * FROM x",
		"SELECT * FROM (SELECT s FROM t FOR SHARE) y",
		"SELECT x.id FROM (SELECT id FROM t FOR UPDATE) x, (SELECT a FROM two) y FOR SHARE",
		"WITH c AS (SELECT id FROM t FOR UPDATE) SELECT * FROM two FOR SHARE",
	} {
		t.Run("denied "+sql, func(t *testing.T) {
			err := requireRejected(t, sql, newTxn())
			require.Equal(t, "0A000", err.SQLState)
			require.Equal(t, message, err.Message)
		})
	}
}
