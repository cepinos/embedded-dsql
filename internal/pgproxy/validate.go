// Copyright (c) 2026 MiniStack Contributors. SPDX-License-Identifier: MIT
// Copies or substantial portions, including AI-assisted ports or rewrites, must retain this notice (see LICENSE).
//
// Go port of the DSQL validator of ministack/core/pgproxy.py (MiniStack
// 1.5.9). MiniStack's LICENSE is reproduced in THIRD_PARTY_NOTICES at the root
// of this repository.

package pgproxy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DsqlError is a validator rejection, rendered as a PostgreSQL ErrorResponse.
type DsqlError struct {
	SQLState string
	Message  string
}

func (e *DsqlError) Error() string { return e.SQLState + ": " + e.Message }

func dsqlErr(sqlstate, message string) *DsqlError {
	return &DsqlError{SQLState: sqlstate, Message: message}
}

// Rewrite says the statement must be rewritten before it reaches the backend.
type Rewrite struct {
	SQL        string
	Kind       string
	ObjectName string
	Table      string
	JobType    string
}

const (
	kindIndexAsync = "index_async"
	jobIndexBuild  = "INDEX_BUILD"
	jobValidate    = "VALIDATE_CONSTRAINT"
)

type stmtClass int

const (
	classOther stmtClass = iota
	classDDL
	classDML
	classBegin
	classEnd
)

// TxnState tracks per-connection transaction discipline.
//
// InTxn is authoritative from the backend's ReadyForQuery status byte (see
// noteBackendStatus): inferring it from statement text goes wrong the moment a
// client sends BEGIN or COMMIT over the extended protocol. The
// per-transaction counters still come from statement text, since the wire
// protocol does not report them.
type TxnState struct {
	InTxn          bool
	DDLSeen        bool
	DMLSeen        bool
	StartedAt      time.Time // zero when no transaction is open
	RowsEst        int       // static row estimate (VALUES tuples) in this txn
	BytesEst       int       // cumulative DML payload bytes in this txn
	Aborted        bool      // backend reported a failed transaction block
	SyntheticAbort bool      // the proxy rejected a statement mid-transaction
}

func (t *TxnState) begin() {
	*t = TxnState{InTxn: true, StartedAt: time.Now()}
}

func (t *TxnState) reset() {
	*t = TxnState{}
}

// noteBackendStatus reconciles with a backend ReadyForQuery status byte.
func (t *TxnState) noteBackendStatus(status byte) {
	switch status {
	case 'I':
		t.reset()
	case 'T':
		if !t.InTxn {
			t.begin()
		}
		t.Aborted = false
	case 'E':
		t.InTxn = true
		t.Aborted = true
	}
}

func (t *TxnState) apply(sql string) {
	switch classifyStatement(sql) {
	case classBegin:
		t.begin()
	case classEnd:
		t.reset()
	case classDDL:
		t.DDLSeen = true
	case classDML:
		t.DMLSeen = true
		if t.InTxn {
			t.RowsEst += valuesTupleCount(sql)
			t.BytesEst += len(sql)
		}
	}
}

var (
	ddlKeywords = map[string]bool{"CREATE": true, "ALTER": true, "DROP": true}
	dmlKeywords = map[string]bool{"INSERT": true, "UPDATE": true, "DELETE": true}
	tclBegin    = map[string]bool{"BEGIN": true, "START": true}
	tclEnd      = map[string]bool{"COMMIT": true, "END": true, "ROLLBACK": true, "ABORT": true}

	withRE        = regexp.MustCompile(`(?i)^\s*WITH\s+(?:RECURSIVE\s+)?`)
	leadWordRE    = regexp.MustCompile(`^([A-Za-z]+)`)
	firstWordRE   = regexp.MustCompile(`^\s*([A-Za-z]+)`)
	explainRE     = regexp.MustCompile(`(?i)^\s*EXPLAIN\s*`)
	explainWordRE = regexp.MustCompile(`(?i)^\s*(ANALYZE|ANALYSE|VERBOSE)\b`)
	analyzeOptRE  = regexp.MustCompile(`(?i)\bANALYZ?[ES]E?\b`)
	analyzeOffRE  = regexp.MustCompile(`(?i)^\s+(?:false|off|0)`)
)

// cteMainKeyword returns the leading keyword of the statement a WITH clause
// feeds, so CTE-leading writes do not escape every DML rule.
func cteMainKeyword(sql string) string {
	loc := withRE.FindStringIndex(sql)
	if loc == nil {
		return ""
	}
	i := loc[1]
	for {
		opening := findChar(sql, i, "(")
		if opening < 0 {
			return ""
		}
		closing := matchParen(sql, opening)
		if closing < 0 {
			return ""
		}
		tail := strings.TrimLeft(sql[closing+1:], " \t\n\r\f\v")
		if strings.HasPrefix(tail, ",") {
			i = closing + 1
			continue
		}
		km := leadWordRE.FindString(tail)
		if km == "" {
			return ""
		}
		word := strings.ToUpper(km)
		// Column list or AS [NOT] MATERIALIZED: the body is still ahead.
		if word == "AS" || word == "NOT" || word == "MATERIALIZED" {
			i = closing + 1
			continue
		}
		return word
	}
}

// explainInner returns the statement EXPLAIN ANALYZE executes. Plain EXPLAIN
// only plans, so it reports false.
func explainInner(sql string) (string, bool) {
	loc := explainRE.FindStringIndex(sql)
	if loc == nil {
		return "", false
	}
	i, analyze := loc[1], false
	if i < len(sql) && sql[i] == '(' {
		closing := matchParen(sql, i)
		if closing < 0 {
			return "", false
		}
		analyze = analyzeEnabled(sql[i+1 : closing])
		i = closing + 1
	} else {
		for {
			wm := explainWordRE.FindStringSubmatchIndex(sql[i:])
			if wm == nil {
				break
			}
			word := strings.ToUpper(sql[i+wm[2] : i+wm[3]])
			if word == "ANALYZE" || word == "ANALYSE" {
				analyze = true
			}
			i += wm[1]
		}
	}
	if !analyze {
		return "", false
	}
	return sql[i:], true
}

// analyzeEnabled reports whether an EXPLAIN option list turns ANALYZE on
// (ANALYZE not followed by false/off/0).
func analyzeEnabled(opts string) bool {
	for _, loc := range analyzeOptRE.FindAllStringIndex(opts, -1) {
		if !analyzeOffRE.MatchString(opts[loc[1]:]) {
			return true
		}
	}
	return false
}

func classifyStatement(sql string) stmtClass {
	s := stripLeadingComments(sql)
	m := firstWordRE.FindStringSubmatch(s)
	if m == nil {
		return classOther
	}
	kw := strings.ToUpper(m[1])
	switch kw {
	case "WITH":
		if inner := cteMainKeyword(s); inner != "" {
			kw = inner
		}
	case "EXPLAIN":
		inner, ok := explainInner(s)
		if !ok {
			return classOther
		}
		return classifyStatement(inner)
	}
	switch {
	case ddlKeywords[kw]:
		return classDDL
	case dmlKeywords[kw]:
		return classDML
	case tclBegin[kw]:
		return classBegin
	case tclEnd[kw]:
		return classEnd
	}
	return classOther
}

var (
	valuesRE      = regexp.MustCompile(`(?i)\bVALUES\b`)
	insertStartRE = regexp.MustCompile(`(?i)^\s*INSERT\b`)
)

// valuesTupleCount counts row tuples in INSERT ... VALUES (...), (...). It is
// the static estimate behind the 3,000-rows-per-transaction limit; INSERT ...
// SELECT, UPDATE and DELETE row counts cannot be known without executing.
func valuesTupleCount(sql string) int {
	m := valuesRE.FindStringIndex(sql)
	if m == nil || !insertStartRE.MatchString(sql) {
		return 0
	}
	rel := strings.IndexByte(sql[m[1]:], '(')
	if rel < 0 {
		return 0
	}
	start := m[1] + rel
	count, depth, inStr := 0, 0, false
	for i := start; i < len(sql); i++ {
		ch := sql[i]
		switch {
		case ch == '\'':
			inStr = !inStr
		case inStr:
			continue
		case ch == '(':
			if depth == 0 {
				count++
			}
			depth++
		case ch == ')':
			depth--
			if depth == 0 && i+1 < len(sql) && nextNonSpace(sql, i+1) != ',' {
				return count
			}
		}
	}
	return count
}

func nextNonSpace(s string, i int) byte {
	for ; i < len(s); i++ {
		if !isSpace(s[i]) {
			return s[i]
		}
	}
	return 0
}

// --- Denylist (rule 1) ------------------------------------------------------

type denyRule struct {
	re    *regexp.Regexp
	thing string
}

func deny(pattern, thing string) denyRule {
	return denyRule{regexp.MustCompile(`(?i)^\s*` + pattern), thing}
}

var denylist = []denyRule{
	deny(`CREATE\s+DATABASE\b`, "CREATE DATABASE"),
	deny(`CREATE\s+TYPE\b`, "CREATE TYPE"),
	deny(`CREATE\s+EXTENSION\b`, "CREATE EXTENSION"),
	deny(`CREATE\s+TRIGGER\b`, "CREATE TRIGGER"),
	deny(`CREATE\s+(OR\s+REPLACE\s+)?PROCEDURE\b`, "CREATE PROCEDURE"),
	deny(`CREATE\s+TABLESPACE\b`, "CREATE TABLESPACE"),
	deny(`CREATE\s+MATERIALIZED\s+VIEW\b`, "materialized views"),
	deny(`CREATE\s+(TEMP|TEMPORARY|UNLOGGED)\s+TABLE\b`, "temporary/unlogged tables"),
	deny(`COPY\b`, "COPY"),
	deny(`LISTEN\b`, "LISTEN"),
	deny(`NOTIFY\b`, "NOTIFY"),
	deny(`UNLISTEN\b`, "UNLISTEN"),
	deny(`TRUNCATE\b`, "TRUNCATE"),
	deny(`DO\b`, "DO"),
	deny(`SAVEPOINT\b`, "SAVEPOINT"),
	deny(`LOCK\b`, "LOCK TABLE"),
	deny(`CREATE\s+RULE\b`, "CREATE RULE"),
	deny(`CREATE\s+(OR\s+REPLACE\s+)?AGGREGATE\b`, "CREATE AGGREGATE"),
	deny(`CREATE\s+DOMAIN\b`, "CREATE DOMAIN"),
	deny(`CREATE\s+CAST\b`, "CREATE CAST"),
	deny(`CREATE\s+COLLATION\b`, "CREATE COLLATION"),
	deny(`CREATE\s+(OR\s+REPLACE\s+)?OPERATOR\b`, "CREATE OPERATOR"),
	deny(`CREATE\s+PUBLICATION\b`, "CREATE PUBLICATION"),
	deny(`CREATE\s+SUBSCRIPTION\b`, "CREATE SUBSCRIPTION"),
	deny(`CREATE\s+(FOREIGN\s+TABLE|SERVER|FOREIGN\s+DATA\s+WRAPPER)\b`, "foreign data wrappers"),
	deny(`VACUUM\b`, "VACUUM"),
	deny(`CLUSTER\b`, "CLUSTER"),
	deny(`REINDEX\b`, "REINDEX"),
	deny(`ALTER\s+SYSTEM\b`, "ALTER SYSTEM"),
	deny(`PREPARE\s+TRANSACTION\b`, "prepared transactions"),
	deny(`(COMMIT|ROLLBACK)\s+PREPARED\b`, "prepared transactions"),
}

var (
	createFunctionRE = regexp.MustCompile(`(?i)^\s*CREATE\s+(OR\s+REPLACE\s+)?FUNCTION\b`)
	languageRE       = regexp.MustCompile(`(?i)\bLANGUAGE\s+['"]?([A-Za-z0-9_]+)`)
	createOrAlterRE  = regexp.MustCompile(`(?i)^\s*(CREATE|ALTER)\s+TABLE\b`)
	createTempRE     = regexp.MustCompile(`(?i)^\s*CREATE\s+(TEMP|TEMPORARY|UNLOGGED)\s+TABLE\b`)
	partitionRE      = regexp.MustCompile(`(?i)\bPARTITION\s+BY\b|\bPARTITION\s+OF\b|\bINHERITS\b`)
	excludeRE        = regexp.MustCompile(`(?i)\bEXCLUDE\b`)
	alterTableRE     = regexp.MustCompile(`(?i)^\s*ALTER\s+TABLE\b`)
)

func checkDenylist(sql string) *DsqlError {
	for _, rule := range denylist {
		if rule.re.MatchString(sql) {
			return dsqlErr("0A000", rule.thing+" is not supported")
		}
	}
	if createFunctionRE.MatchString(sql) {
		if lang := languageRE.FindStringSubmatch(sql); lang != nil && strings.ToLower(lang[1]) != "sql" {
			return dsqlErr("0A000", fmt.Sprintf("LANGUAGE %s is not supported", lang[1]))
		}
	}
	if createOrAlterRE.MatchString(sql) || createTempRE.MatchString(sql) {
		if partitionRE.MatchString(sql) {
			return dsqlErr("0A000", "table partitioning is not supported")
		}
		// On ALTER TABLE it is an ADD CONSTRAINT form, refused as one.
		if excludeRE.MatchString(sql) && !alterTableRE.MatchString(sql) {
			return dsqlErr("0A000", "the EXCLUDE constraint is not supported")
		}
	}
	return nil
}

// --- Column types (rule 2) --------------------------------------------------

// typeAliases is the normalized allowlist of DSQL-supported column types.
var typeAliases = map[string]string{
	"smallint": "smallint", "int2": "smallint",
	"integer": "integer", "int": "integer", "int4": "integer",
	"bigint": "bigint", "int8": "bigint",
	"real": "real", "float4": "real",
	"double precision": "double precision", "float8": "double precision",
	"numeric": "numeric", "decimal": "numeric", "dec": "numeric",
	"character": "character", "char": "character", "bpchar": "character",
	"character varying": "character varying", "varchar": "character varying",
	"text": "text",
	"date": "date",
	"time": "time", "time without time zone": "time",
	"time with time zone": "time with time zone", "timetz": "time with time zone",
	"timestamp": "timestamp", "timestamp without time zone": "timestamp",
	"timestamp with time zone": "timestamp with time zone",
	"timestamptz":              "timestamp with time zone",
	"interval":                 "interval",
	"boolean":                  "boolean", "bool": "boolean",
	"bytea": "bytea", "uuid": "uuid", "json": "json", "jsonb": "jsonb",
}

// colConstraintKeywords terminate the type portion of a column definition.
var colConstraintKeywords = map[string]bool{
	"not": true, "null": true, "default": true, "primary": true, "unique": true,
	"check": true, "references": true, "collate": true, "generated": true,
	"constraint": true, "storage": true,
}

// tableConstraintStarters open a table-level constraint inside CREATE TABLE.
var tableConstraintStarters = map[string]bool{
	"primary": true, "unique": true, "check": true, "constraint": true,
	"foreign": true, "like": true, "exclude": true,
}

var (
	intervalRE   = regexp.MustCompile(`^interval(\s+(year|month|day|hour|minute|second)(\s+to\s+(year|month|day|hour|minute|second))?)?$`)
	spacesRE     = regexp.MustCompile(`\s+`)
	typeParensRE = regexp.MustCompile(`\(.*?\)`)
)

// normalizeType returns the normalized type ("" when unsupported) and its
// display form; ok is false for an empty type string.
func normalizeType(raw string) (normalized, display string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	isArray := false
	for strings.HasSuffix(strings.TrimRight(raw, " \t\n\r\f\v"), "[]") {
		isArray = true
		raw = strings.TrimRight(raw, " \t\n\r\f\v")
		raw = raw[:len(raw)-2]
	}
	display = spacesRE.ReplaceAllString(strings.TrimSpace(raw), " ")
	// Strip precision/scale and trailing modifiers: numeric(18,6), varchar(10).
	base := typeParensRE.ReplaceAllString(display, "")
	base = strings.ToLower(strings.TrimSpace(spacesRE.ReplaceAllString(base, " ")))
	if isArray {
		return "", display + "[]", true
	}
	if intervalRE.MatchString(base) {
		return "interval", display, true
	}
	return typeAliases[base], display, true
}

// typeFromTokens extracts the type from column-definition tokens that follow
// the column name.
func typeFromTokens(tokens []string) string {
	var typeTokens []string
	for _, tok := range tokens {
		word := tok
		if k := strings.IndexByte(word, '('); k >= 0 {
			word = word[:k]
		}
		if colConstraintKeywords[strings.ToLower(word)] {
			break
		}
		typeTokens = append(typeTokens, tok)
	}
	return strings.Join(typeTokens, " ")
}

func checkType(rawType string) *DsqlError {
	normalized, display, ok := normalizeType(rawType)
	if !ok {
		return nil
	}
	if normalized == "" {
		return dsqlErr("0A000", fmt.Sprintf("type %q is not supported", display))
	}
	return nil
}

var (
	generatedRE  = regexp.MustCompile(`(?i)\bGENERATED\b`)
	asIdentityRE = regexp.MustCompile(`(?i)\bAS\s+IDENTITY\b`)
)

// checkIdentity: DSQL supports identity columns on bigint columns only.
func checkIdentity(colDef, rawType string) *DsqlError {
	if generatedRE.MatchString(colDef) && asIdentityRE.MatchString(colDef) {
		normalized, _, ok := normalizeType(rawType)
		if ok && normalized != "" && normalized != "bigint" {
			return dsqlErr("0A000", "identity columns are only supported on bigint columns")
		}
	}
	return nil
}

var (
	createTableRE   = regexp.MustCompile(`(?i)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?[\w".]+\s*`)
	addColumnTypeRE = regexp.MustCompile(`(?i)\bADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?([\w"]+)\s+`)
)

func checkColumnTypes(sql string) *DsqlError {
	if m := createTableRE.FindStringIndex(sql); m != nil {
		rest := sql[m[1]:]
		if !strings.HasPrefix(strings.TrimLeft(rest, " \t\n\r\f\v"), "(") {
			return nil // CREATE TABLE ... AS / PARTITION OF / LIKE-only
		}
		start := m[1] + strings.IndexByte(rest, '(')
		end := matchParen(sql, start)
		if end < 0 {
			return nil
		}
		for _, part := range splitTopLevel(sql[start+1 : end]) {
			tokens := strings.Fields(part)
			if len(tokens) == 0 {
				continue
			}
			if tableConstraintStarters[strings.ToLower(strings.Trim(tokens[0], `"`))] {
				continue
			}
			rawType := typeFromTokens(tokens[1:])
			if err := checkType(rawType); err != nil {
				return err
			}
			if err := checkIdentity(part, rawType); err != nil {
				return err
			}
		}
		return nil
	}
	if alterTableRE.MatchString(sql) {
		// One action per part: a statement can add several columns at once.
		for _, part := range splitTopLevel(sql) {
			add := addColumnTypeRE.FindStringIndex(part)
			if add == nil {
				continue
			}
			tail := part[add[1]:]
			rawType := typeFromTokens(strings.Fields(tail))
			if err := checkType(rawType); err != nil {
				return err
			}
			if err := checkIdentity(tail, rawType); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- ALTER TABLE subset (AWS alter-table-syntax-support) ---------------------

// DSQL supports only a subset of ALTER TABLE actions: ADD COLUMN (no inline
// constraint), DROP COLUMN (not on a primary key column; that needs a catalog
// probe, so it lives in the proxy), SET/DROP DEFAULT, DROP NOT NULL, DROP
// EXPRESSION, identity actions, SET STORAGE, ADD CONSTRAINT ... CHECK /
// FOREIGN KEY ... NOT VALID, ADD CONSTRAINT ... UNIQUE USING INDEX, ALTER
// CONSTRAINT, DROP CONSTRAINT, RENAME, SET SCHEMA, OWNER TO, and the async
// VALIDATE CONSTRAINT form (a Rewrite in validate).

var (
	alterColumnTypeRE = regexp.MustCompile(`(?i)\bALTER\s+COLUMN\s+[\w"]+\s+(?:SET\s+DATA\s+)?TYPE\b`)
	setNotNullRE      = regexp.MustCompile(`(?i)\bSET\s+NOT\s+NULL\b`)
	addWhatRE         = regexp.MustCompile(`(?i)\bADD\s+(?:CONSTRAINT\s+[\w"]+\s+)?(\w+)`)
	uniqueUsingIdxRE  = regexp.MustCompile(`(?i)\bUNIQUE\s+USING\s+INDEX\b`)
	notValidRE        = regexp.MustCompile(`(?i)\bNOT\s+VALID\b`)
	addGeneratedRE    = regexp.MustCompile(`(?i)\bADD\s+GENERATED\b`)
	identityRE        = regexp.MustCompile(`(?i)\bIDENTITY\b`)
	cacheNumRE        = regexp.MustCompile(`(?i)\bCACHE\s+(\d+)`)
)

func checkAlterTable(sql string) *DsqlError {
	if !alterTableRE.MatchString(sql) {
		return nil
	}
	if alterColumnTypeRE.MatchString(sql) {
		return dsqlErr("0A000", "ALTER COLUMN TYPE is not supported")
	}
	if setNotNullRE.MatchString(sql) {
		return dsqlErr("0A000", "SET NOT NULL is not supported (only DROP NOT NULL)")
	}
	if add := addWhatRE.FindStringSubmatch(sql); add != nil && strings.ToUpper(add[1]) != "COLUMN" {
		what := strings.ToUpper(add[1])
		// Only two ADD CONSTRAINT forms exist: a CHECK or FOREIGN KEY added
		// NOT VALID, and UNIQUE USING INDEX. Every other one draws the same
		// message, whatever the constraint.
		refused := what == "PRIMARY" || what == "EXCLUDE" ||
			(what == "UNIQUE" && !uniqueUsingIdxRE.MatchString(sql)) ||
			((what == "CHECK" || what == "FOREIGN") && !notValidRE.MatchString(sql))
		if refused {
			return dsqlErr("0A000", "unsupported ALTER TABLE ADD CONSTRAINT statement")
		}
	}
	if err := checkAddColumn(sql); err != nil {
		return err
	}
	// ADD GENERATED ... AS IDENTITY requires an explicit CACHE value.
	if addGeneratedRE.MatchString(sql) && identityRE.MatchString(sql) && !cacheNumRE.MatchString(sql) {
		return dsqlErr("0A000", "ADD GENERATED AS IDENTITY requires an explicit CACHE value")
	}
	return nil
}

var (
	// ALTER TABLE ADD COLUMN takes a name, a type and an optional STORAGE
	// mode, nothing else. Every column constraint draws one message, a bare
	// NULL (which constrains nothing) and COLLATE apart.
	addColumnRE      = regexp.MustCompile(`(?is)\bADD\s+(?:COLUMN\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(` + identPat + `)\s+(.+)$`)
	alterColumnAddRE = regexp.MustCompile(`(?i)\bALTER\s+(?:COLUMN\s+)?[\w"]+\s+ADD\b`)
)

func checkAddColumn(sql string) *DsqlError {
	for _, part := range splitTopLevel(sql) {
		// ALTER COLUMN x ADD GENERATED ... is an identity action, not a column.
		if alterColumnAddRE.MatchString(part) {
			continue
		}
		m := addColumnRE.FindStringSubmatch(part)
		if m == nil {
			continue
		}
		name, quoted := foldIdentifier(m[1])
		if !quoted && tableConstraintStarters[name] {
			continue // ADD <table constraint>, handled by checkAlterTable
		}
		tokens := strings.Fields(m[2])
		rawType := typeFromTokens(tokens)
		tail := tokens[len(strings.Fields(rawType)):]
		for i := 0; i < len(tail); {
			switch strings.ToUpper(strings.TrimRight(tail[i], ",")) {
			case "NULL":
				i++
			case "STORAGE":
				i += 2
			case "COLLATE":
				return dsqlErr("0A000", "COLLATE clause not supported")
			default:
				return dsqlErr("0A000", "ALTER TABLE ADD COLUMN with constraint not supported")
			}
		}
	}
	return nil
}

// --- Foreign keys and DEFERRABLE --------------------------------------------

// A foreign key is otherwise plain PostgreSQL, so the only DSQL-specific rules
// are that ALTER TABLE must add one NOT VALID (checkAlterTable) and that
// DEFERRABLE applies to foreign keys alone. The regexp is a cheap gate: only a
// statement that could carry a deferral is tokenized.
var deferrableRE = regexp.MustCompile(`(?i)\bDEFERRABLE\b|\bINITIALLY\s+DEFERRED\b`)

func checkDeferrable(sql string) *DsqlError {
	if !deferrableRE.MatchString(sql) {
		return nil
	}
	isAlter := alterTableRE.MatchString(sql)
	var body string
	if m := createTableRE.FindStringIndex(sql); m != nil && strings.HasPrefix(strings.TrimLeft(sql[m[1]:], " \t\n\r\f\v"), "(") {
		start := m[1] + strings.IndexByte(sql[m[1]:], '(')
		end := matchParen(sql, start)
		if end < 0 {
			return nil
		}
		body = sql[start+1 : end]
	} else if isAlter {
		body = sql
	} else {
		return nil
	}
	for _, part := range splitTopLevel(body) {
		var words []string
		for _, t := range sqlTokens(part) {
			if t.kind == tokName {
				words = append(words, t.text)
			}
		}
		hasPair := func(a, b string) bool {
			for i := 0; i+1 < len(words); i++ {
				if words[i] == a && words[i+1] == b {
					return true
				}
			}
			return false
		}
		has := func(w string) bool {
			for _, x := range words {
				if x == w {
					return true
				}
			}
			return false
		}
		// NOT DEFERRABLE is the default, so only an actual deferral counts.
		deferred := false
		for i, w := range words {
			if w == "deferrable" && (i == 0 || words[i-1] != "not") {
				deferred = true
				break
			}
		}
		if !deferred && !hasPair("initially", "deferred") {
			continue
		}
		// ALTER CONSTRAINT only ever targets a foreign key constraint.
		if hasPair("alter", "constraint") {
			continue
		}
		if has("references") || hasPair("foreign", "key") {
			continue
		}
		if has("check") {
			return dsqlErr("0A000", "CHECK constraints cannot be marked DEFERRABLE")
		}
		if isAlter && has("unique") {
			return dsqlErr("0A000", "ALTER TABLE / ADD CONSTRAINT UNIQUE / DEFERRED / INITIALLY DEFERRED not supported")
		}
		return dsqlErr("0A000", "DEFERRABLE constraint not supported")
	}
	return nil
}

var cacheTargetRE = regexp.MustCompile(`(?i)^\s*(CREATE|ALTER)\s+(TABLE|SEQUENCE)\b`)

// checkCacheValue: IDENTITY columns and sequences need CACHE 1 or >= 65536.
func checkCacheValue(sql string) *DsqlError {
	if !cacheTargetRE.MatchString(sql) {
		return nil
	}
	if m := cacheNumRE.FindStringSubmatch(sql); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || (n != 1 && n < 65536) {
			return dsqlErr("0A000", "CACHE must be 1 or at least 65536")
		}
	}
	return nil
}

// --- Index rules --------------------------------------------------------------

// DSQL index creation is always asynchronous: plain CREATE INDEX fails with
// "unsupported mode". No CONCURRENTLY, no USING (not even btree), no partial
// (WHERE) index. IF NOT EXISTS requires a name. Expression keys are supported
// but every function must be immutable (42P17), INCLUDE columns cannot be
// expressions, and an index has at most 8 key columns (54011); 24 indexes per
// table is enforced by the proxy with a catalog probe.
//
// Error precedence (observed on real DSQL): name grammar, CONCURRENTLY, USING,
// WHERE, mode, key-expression rules, key count.
var volatileFunctions = map[string]bool{
	"now": true, "random": true, "setseed": true, "nextval": true, "currval": true,
	"lastval": true, "setval": true, "gen_random_uuid": true, "uuid_generate_v1": true,
	"uuid_generate_v4": true, "txid_current": true, "pg_backend_pid": true,
	"pg_notification_queue_usage": true, "current_setting": true, "set_config": true,
	"version": true, "clock_timestamp": true, "statement_timestamp": true,
	"transaction_timestamp": true, "timeofday": true,
}

var (
	indexAsyncRE = regexp.MustCompile(`(?i)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+ASYNC\s+(?:IF\s+NOT\s+EXISTS\s+)?` +
		`(?:([\w".]+)\s+)?ON\s+(?:ONLY\s+)?(?:USING\s+\w+\s+)?([\w".]+)`)
	indexRE = regexp.MustCompile(`(?i)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?` +
		`(?:IF\s+NOT\s+EXISTS\s+)?(?:([\w".]+)\s+)?ON\s+(?:ONLY\s+)?` +
		`(?:USING\s+\w+\s+)?([\w".]+)`)
	ifNotExistsRE   = regexp.MustCompile(`(?i)\bIF\s+NOT\s+EXISTS\b`)
	concurrentlyRE  = regexp.MustCompile(`(?i)\bCONCURRENTLY\b`)
	usingRE         = regexp.MustCompile(`(?i)\bUSING\s+\w+`)
	whereRE         = regexp.MustCompile(`(?i)\bWHERE\b`)
	funcCallRE      = regexp.MustCompile(`(\w+)\s*\(`)
	includeRE       = regexp.MustCompile(`(?i)\bINCLUDE\s*\(`)
	indexAsyncSubRE = regexp.MustCompile(`(?i)(CREATE\s+(?:UNIQUE\s+)?INDEX)\s+ASYNC`)
	alterAsyncSubRE = regexp.MustCompile(`(?i)(ALTER\s+TABLE)\s+ASYNC`)
	alterAsyncValRE = regexp.MustCompile(`(?i)^\s*ALTER\s+TABLE\s+ASYNC\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?` +
		`([\w".]+)\s*\*?\s*VALIDATE\s+CONSTRAINT\s+([\w"]+)`)
	indexColRE = regexp.MustCompile(`^[A-Za-z_"][\w"]*`)
)

func checkIndexRules(sql string) *DsqlError {
	m := indexRE.FindStringSubmatchIndex(sql)
	if m == nil {
		m = indexAsyncRE.FindStringSubmatchIndex(sql)
	}
	if m == nil {
		return nil
	}
	// IF NOT EXISTS without a name is a grammar error on real DSQL.
	if ifNotExistsRE.MatchString(sql) && m[2] < 0 {
		return dsqlErr("42601", `syntax error at or near "ON"`)
	}
	if concurrentlyRE.MatchString(sql) {
		return dsqlErr("0A000", "CONCURRENTLY not supported for CREATE INDEX")
	}
	if usingRE.MatchString(sql) {
		return dsqlErr("0A000", "USING not supported for CREATE INDEX")
	}
	if whereRE.MatchString(sql) {
		return dsqlErr("0A000", "WHERE not supported for CREATE INDEX")
	}
	if !indexAsyncRE.MatchString(sql) {
		return dsqlErr("0A000", "unsupported mode. please use CREATE INDEX ASYNC.")
	}
	rel := strings.IndexByte(sql[m[1]:], '(')
	if rel < 0 {
		return nil
	}
	start := m[1] + rel
	end := matchParen(sql, start)
	if end < 0 {
		return nil
	}
	parts := splitTopLevel(sql[start+1 : end])
	for _, part := range parts {
		for _, fn := range funcCallRE.FindAllStringSubmatch(part, -1) {
			if volatileFunctions[strings.ToLower(fn[1])] {
				// Same code and message the backend (and real DSQL) produce;
				// failing fast rejects at submit time instead of returning a
				// job id.
				return dsqlErr("42P17", "functions in index expression must be marked IMMUTABLE")
			}
		}
	}
	// INCLUDE columns are non-key: expressions are not supported there.
	if inc := includeRE.FindStringIndex(sql[end:]); inc != nil {
		istart := end + inc[1] - 1
		iend := matchParen(sql, istart)
		if iend > 0 {
			for _, p := range splitTopLevel(sql[istart+1 : iend]) {
				if strings.Contains(p, "(") {
					return dsqlErr("0A000", "expressions are not supported in included columns")
				}
			}
		}
	}
	if len(parts) > 8 {
		return dsqlErr("54011", "more than 8 column keys in an index are not supported")
	}
	return nil
}

// replaceFirst replaces the first match of re with its first capture group.
func replaceFirst(re *regexp.Regexp, sql string) string {
	loc := re.FindStringSubmatchIndex(sql)
	if loc == nil {
		return sql
	}
	return sql[:loc[0]] + sql[loc[2]:loc[3]] + sql[loc[1]:]
}

// rewriteIndexAsync strips the DSQL-only ASYNC keyword from CREATE [UNIQUE]
// INDEX ASYNC.
func rewriteIndexAsync(sql string) string { return replaceFirst(indexAsyncSubRE, sql) }

// rewriteAlterAsync strips the DSQL-only ASYNC keyword from ALTER TABLE ASYNC.
func rewriteAlterAsync(sql string) string { return replaceFirst(alterAsyncSubRE, sql) }

// rpartitionDot splits a dotted name at its last dot.
func rpartitionDot(s string) (string, string) {
	if k := strings.LastIndexByte(s, '.'); k >= 0 {
		return s[:k], s[k+1:]
	}
	return "", s
}

// indexObjectName is the schema-qualified index name for sys.jobs. Unnamed
// indexes get DSQL's documented auto-name <table>_<col>_..._idx in the
// table's schema (default public).
func indexObjectName(sql string, onEnd int, indexName, table string) string {
	schema, bareTable := rpartitionDot(table)
	schema = strings.Trim(schema, `"`)
	if schema == "" {
		schema = "public"
	}
	bareTable = strings.Trim(bareTable, `"`)
	if indexName != "" {
		if strings.Contains(indexName, ".") {
			return indexName
		}
		return schema + "." + strings.Trim(indexName, `"`)
	}
	var cols []string
	if rel := strings.IndexByte(sql[onEnd:], '('); rel >= 0 {
		start := onEnd + rel
		if end := matchParen(sql, start); end > 0 {
			for _, part := range splitTopLevel(sql[start+1 : end]) {
				tokens := strings.Fields(part)
				if len(tokens) == 0 {
					continue
				}
				if ident := indexColRE.FindString(tokens[0]); ident != "" {
					cols = append(cols, strings.Trim(ident, `"`))
				}
			}
		}
	}
	suffix := ""
	if len(cols) > 0 {
		suffix = "_" + strings.Join(cols, "_")
	}
	return schema + "." + bareTable + suffix + "_idx"
}

// --- sys.jobs / sys.wait_for_job ----------------------------------------------

var (
	sysJobsRE     = regexp.MustCompile(`(?is)^\s*SELECT\s+(.+?)\s+FROM\s+sys\.jobs\b`)
	jobIDFilterRE = regexp.MustCompile(`(?i)job_id\s*=\s*(?:'([^']*)'|\$(\d+))`)
	waitJobRE     = regexp.MustCompile(`(?i)^\s*(?:SELECT|CALL)\s+sys\.wait_for_job\s*\(\s*` +
		`(?:job_id\s*\)\s*'([^']*)'|'([^']*)'\s*\)|\$(\d+)\s*\))`)
)

// jobsQuery is a matched sys.jobs query. Columns is nil for SELECT *. A
// filter is either a literal job id or a bound parameter ($N, 1-based).
type jobsQuery struct {
	Columns     []string
	Filter      string
	HasFilter   bool
	FilterParam int
}

// matchSysJobs reports whether sql queries sys.jobs.
func matchSysJobs(sql string) (jobsQuery, bool) {
	m := sysJobsRE.FindStringSubmatch(sql)
	if m == nil {
		return jobsQuery{}, false
	}
	var q jobsQuery
	if list := strings.TrimSpace(m[1]); list != "*" {
		for _, c := range strings.Split(list, ",") {
			q.Columns = append(q.Columns, strings.Trim(strings.TrimSpace(c), `"`))
		}
	}
	if f := jobIDFilterRE.FindStringSubmatch(sql); f != nil {
		if f[2] != "" {
			n, err := strconv.Atoi(f[2])
			if err == nil {
				q.FilterParam = n
			}
		} else {
			q.Filter, q.HasFilter = f[1], true
		}
	}
	return q, true
}

// waitQuery is a matched sys.wait_for_job call: a literal job id or a bound
// parameter ($N, 1-based).
type waitQuery struct {
	JobID string
	Param int
}

// matchWaitForJob matches both documented call forms,
// sys.wait_for_job(job_id) '<id>' and sys.wait_for_job('<id>'), plus a bound
// parameter sys.wait_for_job($1).
func matchWaitForJob(sql string) (waitQuery, bool) {
	m := waitJobRE.FindStringSubmatchIndex(sql)
	if m == nil {
		return waitQuery{}, false
	}
	switch {
	case m[2] >= 0:
		return waitQuery{JobID: sql[m[2]:m[3]]}, true
	case m[4] >= 0:
		return waitQuery{JobID: sql[m[4]:m[5]]}, true
	default:
		n, err := strconv.Atoi(sql[m[6]:m[7]])
		if err != nil {
			return waitQuery{}, false
		}
		return waitQuery{Param: n}, true
	}
}

// --- Locking clauses ------------------------------------------------------------

// DSQL supports two locking clauses, FOR UPDATE and FOR KEY SHARE, each with
// its optional OF / NOWAIT / SKIP LOCKED tail. Neither restricts the query the
// clause locks. The regexp is a cheap gate before tokenizing.
var (
	unsupportedLocks = [][]string{{"no", "key", "update"}, {"share"}}
	lockClauseRE     = regexp.MustCompile(`(?i)\bFOR\s+(?:NO\s+KEY\s+UPDATE|SHARE)\b`)
)

// checkLockingClause refuses every locking clause but FOR UPDATE and FOR KEY
// SHARE. It reads tokens, so FOR SHARE inside a literal is text. A clause
// nested in a CTE or subquery still counts, and an earlier FOR UPDATE does not
// excuse a later share clause.
func checkLockingClause(sql string) *DsqlError {
	if !lockClauseRE.MatchString(sql) {
		return nil
	}
	toks := sqlTokens(sql)
	for i, t := range toks {
		if t.kind != tokName || t.text != "for" {
			continue
		}
		var words []string
		for k := i + 1; k < i+4 && k < len(toks); k++ {
			if toks[k].kind == tokName {
				words = append(words, toks[k].text)
			}
		}
		for _, lock := range unsupportedLocks {
			if len(words) >= len(lock) && equalWords(words[:len(lock)], lock) {
				return dsqlErr("0A000", "locking clauses other than FOR UPDATE/FOR KEY SHARE are not supported")
			}
		}
	}
	return nil
}

func equalWords(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- Top-level validator ----------------------------------------------------------

const (
	maxTxnRows     = 3000
	maxTxnBytes    = 10 * 1024 * 1024
	maxTxnDuration = 300 * time.Second
)

// checkTxnRules applies the rules that depend on the transaction state: DDL /
// DML mixing, the duration limit and the row and size limits.
func checkTxnRules(s string, txn *TxnState) *DsqlError {
	if txn == nil {
		return nil
	}
	cls := classifyStatement(s)
	if txn.InTxn {
		switch {
		case cls == classDDL && txn.DDLSeen:
			return dsqlErr("25006", "only one DDL statement can be run in a transaction")
		case cls == classDDL && txn.DMLSeen:
			return dsqlErr("25006", "DDL and DML statements cannot be mixed in a transaction")
		case cls == classDML && txn.DDLSeen:
			return dsqlErr("25006", "DDL and DML statements cannot be mixed in a transaction")
		}
		// Documented DSQL transaction limits (messages are approximations).
		if !txn.StartedAt.IsZero() && time.Since(txn.StartedAt) > maxTxnDuration {
			return dsqlErr("25006", "transaction exceeded the 5 minute duration limit")
		}
	}
	// Row/size limits apply to any transaction, including an implicit
	// single-statement one. Row counting is static (VALUES tuples).
	if cls == classDML {
		rows, size := 0, 0
		if txn.InTxn {
			rows, size = txn.RowsEst, txn.BytesEst
		}
		if rows+valuesTupleCount(s) > maxTxnRows {
			return dsqlErr("25006", "transaction exceeds the 3,000 row limit")
		}
		if size+len(s) > maxTxnBytes {
			return dsqlErr("25006", "transaction exceeds the 10 MiB size limit")
		}
	}
	return nil
}

// validate checks one statement. It returns a rejection, a rewrite, or
// neither. It does not modify txn: the caller applies state changes once the
// statement is actually forwarded.
func validate(sql string, txn *TxnState) (*DsqlError, *Rewrite) {
	s := strings.TrimSpace(stripLeadingComments(sql))
	if s == "" {
		return nil, nil
	}
	if err := checkTxnRules(s, txn); err != nil {
		return err, nil
	}
	for _, check := range []func(string) *DsqlError{
		checkDenylist, checkAlterTable, checkDeferrable, checkCacheValue,
		checkColumnTypes, checkIndexRules, checkLockingClause,
	} {
		if err := check(s); err != nil {
			return err, nil
		}
	}
	if m := alterAsyncValRE.FindStringSubmatch(s); m != nil {
		table := m[1]
		schema, bare := rpartitionDot(table)
		schema = strings.Trim(schema, `"`)
		if schema == "" {
			schema = "public"
		}
		return nil, &Rewrite{
			SQL:        rewriteAlterAsync(s),
			Kind:       kindIndexAsync,
			ObjectName: schema + "." + strings.Trim(bare, `"`),
			Table:      table,
			JobType:    jobValidate,
		}
	}
	if m := indexAsyncRE.FindStringSubmatchIndex(s); m != nil {
		indexName := ""
		if m[2] >= 0 {
			indexName = s[m[2]:m[3]]
		}
		table := s[m[4]:m[5]]
		return nil, &Rewrite{
			SQL:        rewriteIndexAsync(s),
			Kind:       kindIndexAsync,
			ObjectName: indexObjectName(s, m[1], indexName, table),
			Table:      table,
			JobType:    jobIndexBuild,
		}
	}
	return nil, nil
}

// --- DROP COLUMN on a primary key --------------------------------------------------

// ALTER TABLE ... DROP COLUMN is supported (several columns at once too), but
// dropping a primary key column is not. COLUMN is optional in PostgreSQL, so
// the actions that merely start with DROP have to be told apart from a bare
// column name.
var (
	dropColumnRE          = regexp.MustCompile(`(?i)\bDROP\s+(?:COLUMN\s+)?(?:IF\s+EXISTS\s+)?(` + identPat + `)`)
	dropNonColumnActions  = map[string]bool{"constraint": true, "default": true, "not": true, "expression": true, "identity": true}
	alterTableTargetRE    = regexp.MustCompile(`(?i)^\s*ALTER\s+TABLE\s+(?:ASYNC\s+)?(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?(` + identPathPat + `)`)
	commitOrEndLeadingRE  = regexp.MustCompile(`(?i)^\s*(COMMIT|END)\b`)
	rollbackOrAbortLeadRE = regexp.MustCompile(`(?i)^\s*(ROLLBACK|ABORT)\b`)
)

// droppedColumns returns the column names an ALTER TABLE statement drops,
// normalized the way the server stores them.
func droppedColumns(sql string) []string {
	if !alterTableRE.MatchString(sql) {
		return nil
	}
	var cols []string
	for _, m := range dropColumnRE.FindAllStringSubmatch(sql, -1) {
		name, quoted := foldIdentifier(m[1])
		if !quoted && dropNonColumnActions[name] {
			continue
		}
		cols = append(cols, name)
	}
	return cols
}
