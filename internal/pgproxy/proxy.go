// Copyright (c) 2026 MiniStack Contributors. SPDX-License-Identifier: MIT
// Copies or substantial portions, including AI-assisted ports or rewrites, must retain this notice (see LICENSE).
//
// Go port of the connection handling of ministack/core/pgproxy.py (MiniStack
// 1.5.9), rebuilt on github.com/jackc/pgx/v5/pgproto3. MiniStack's LICENSE is
// reproduced in THIRD_PARTY_NOTICES at the root of this repository.

// Package pgproxy is a PostgreSQL wire-protocol proxy that enforces the
// Aurora DSQL PostgreSQL-compatibility subset in front of a real PostgreSQL
// server. It is a Go port of MiniStack's pgproxy.py; see the repository
// README for the documented limitations.
package pgproxy

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// Config describes the backend PostgreSQL server a Proxy forwards to.
type Config struct {
	BackendHost     string
	BackendPort     uint16
	BackendUser     string
	BackendPassword string
	// Logger receives connection-level errors. Nil discards them.
	Logger *log.Logger
}

// Proxy accepts PostgreSQL clients and forwards them to the backend after
// DSQL validation. One Proxy plays the part of one DSQL cluster: it owns the
// sys.jobs registry and the catalog version behind OC001.
type Proxy struct {
	cfg Config
	ln  net.Listener

	mu             sync.Mutex
	jobs           []Job
	catalogVersion int
	open           map[net.Conn]struct{}
	closed         bool

	wg sync.WaitGroup
}

// Listen starts a proxy on addr (for example "127.0.0.1:0").
func Listen(addr string, cfg Config) (*Proxy, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("pgproxy: listen on %s: %w", addr, err)
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	p := &Proxy{cfg: cfg, ln: ln, open: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.acceptLoop()
	return p, nil
}

// Addr is the address the proxy listens on.
func (p *Proxy) Addr() *net.TCPAddr {
	addr, ok := p.ln.Addr().(*net.TCPAddr)
	if !ok {
		return &net.TCPAddr{}
	}
	return addr
}

// Close stops accepting clients, closes every open connection and waits for
// their goroutines to finish.
func (p *Proxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	conns := make([]net.Conn, 0, len(p.open))
	for c := range p.open {
		conns = append(conns, c)
	}
	p.mu.Unlock()

	err := p.ln.Close()
	var errs []error
	if err != nil && !errors.Is(err, net.ErrClosed) {
		errs = append(errs, err)
	}
	for _, c := range conns {
		if cerr := c.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			errs = append(errs, cerr)
		}
	}
	p.wg.Wait()
	return errors.Join(errs...)
}

func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.open[c] = struct{}{}
	return true
}

func (p *Proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.open, c)
	p.mu.Unlock()
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		p.cfg.Logger.Printf("pgproxy: close: %v", err)
	}
}

func (p *Proxy) acceptLoop() {
	defer p.wg.Done()
	for {
		nc, err := p.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			p.cfg.Logger.Printf("pgproxy: accept: %v", err)
			return
		}
		if !p.track(nc) {
			if cerr := nc.Close(); cerr != nil {
				p.cfg.Logger.Printf("pgproxy: close: %v", cerr)
			}
			continue
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.untrack(nc)
			if err := p.serve(nc); err != nil && !isDisconnect(err) {
				p.cfg.Logger.Printf("pgproxy: connection: %v", err)
			}
		}()
	}
}

func isDisconnect(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, errBackendGone) ||
		strings.Contains(err.Error(), "connection reset by peer") ||
		strings.Contains(err.Error(), "broken pipe")
}

var errBackendGone = errors.New("pgproxy: backend connection closed")

// ---------------------------------------------------------------------------
// Message helpers
// ---------------------------------------------------------------------------

const (
	textOID = 25
	boolOID = 16
)

// startupParams are the parameters announced to clients, as MiniStack does.
var startupParams = [][2]string{
	{"server_version", "16.4"},
	{"server_encoding", "UTF8"},
	{"client_encoding", "UTF8"},
	{"DateStyle", "ISO"},
	{"integer_datetimes", "on"},
	{"standard_conforming_strings", "on"},
}

func encode(msgs ...pgproto3.Message) ([]byte, error) {
	var buf []byte
	for _, m := range msgs {
		var err error
		buf, err = m.Encode(buf)
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

func errorMessage(e *DsqlError) *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{
		Severity:            "ERROR",
		SeverityUnlocalized: "ERROR",
		Code:                e.SQLState,
		Message:             e.Message,
	}
}

type column struct {
	name string
	oid  uint32
}

func rowDescription(cols []column, formats []int16) *pgproto3.RowDescription {
	fields := make([]pgproto3.FieldDescription, len(cols))
	for i, c := range cols {
		fields[i] = pgproto3.FieldDescription{
			Name:         []byte(c.name),
			DataTypeOID:  c.oid,
			DataTypeSize: -1,
			TypeModifier: -1,
			Format:       formatFor(formats, i),
		}
	}
	return &pgproto3.RowDescription{Fields: fields}
}

// formatFor applies the protocol's result-format rules: no codes means text,
// one code applies to every column, otherwise one code per column.
func formatFor(formats []int16, i int) int16 {
	switch {
	case len(formats) == 0:
		return 0
	case len(formats) == 1:
		return formats[0]
	case i < len(formats):
		return formats[i]
	}
	return 0
}

// ---------------------------------------------------------------------------
// Connection state
// ---------------------------------------------------------------------------

// synthetic is a statement the proxy answers itself: a sys.jobs query, a
// sys.wait_for_job call, or an ASYNC DDL rewrite that returns a job id.
type synthetic struct {
	jobs    *jobsQuery
	cols    []string
	wait    *waitQuery
	rewrite *Rewrite
}

func (s *synthetic) columns() []column {
	switch {
	case s.jobs != nil:
		out := make([]column, len(s.cols))
		for i, c := range s.cols {
			out[i] = column{c, textOID}
		}
		return out
	case s.wait != nil:
		return []column{{"wait_for_job", boolOID}}
	}
	return []column{{"job_id", textOID}}
}

func (s *synthetic) paramCount() int {
	switch {
	case s.jobs != nil:
		return s.jobs.FilterParam
	case s.wait != nil:
		return s.wait.Param
	}
	return 0
}

func paramValue(params [][]byte, n int) (string, bool) {
	if n < 1 || n > len(params) || params[n-1] == nil {
		return "", false
	}
	return string(params[n-1]), true
}

// rows computes the result set of a sys.jobs or wait_for_job statement.
func (s *synthetic) rows(p *Proxy, params [][]byte) ([][]string, string) {
	jobs := p.Jobs()
	if s.wait != nil {
		id := s.wait.JobID
		if s.wait.Param > 0 {
			id, _ = paramValue(params, s.wait.Param)
		}
		known := "f"
		for _, j := range jobs {
			if j.JobID == id && j.Status == "completed" {
				known = "t"
				break
			}
		}
		return [][]string{{known}}, "SELECT 1"
	}
	filter, hasFilter := s.jobs.Filter, s.jobs.HasFilter
	if s.jobs.FilterParam > 0 {
		filter, hasFilter = paramValue(params, s.jobs.FilterParam)
	}
	var out [][]string
	for _, j := range jobs {
		if hasFilter && filter != "" && j.JobID != filter {
			continue
		}
		row := make([]string, len(s.cols))
		for i, c := range s.cols {
			row[i] = j.column(c)
		}
		out = append(out, row)
	}
	return out, "SELECT " + strconv.Itoa(len(out))
}

func dataRow(values []string, cols []column, formats []int16) *pgproto3.DataRow {
	out := make([][]byte, len(values))
	for i, v := range values {
		if formatFor(formats, i) == 1 && i < len(cols) && cols[i].oid == boolOID {
			if v == "t" {
				out[i] = []byte{1}
			} else {
				out[i] = []byte{0}
			}
			continue
		}
		out[i] = []byte(v)
	}
	return &pgproto3.DataRow{Values: out}
}

type stmtEntry struct {
	sql   string
	synth *synthetic
}

type portal struct {
	entry   *stmtEntry
	params  [][]byte
	formats []int16
}

type conn struct {
	p      *Proxy
	client net.Conn
	cb     *pgproto3.Backend
	wmu    sync.Mutex // serializes writes to the client

	be        net.Conn
	fe        *pgproto3.Frontend
	relayDone chan struct{}

	mu         sync.Mutex // guards the fields below; shared with the relay
	cond       *sync.Cond
	txn        TxnState
	capture    chan []byte
	pending    int // ReadyForQuery messages the backend still owes
	dead       bool
	catalog    int
	hasCatalog bool

	// Extended-protocol state, handler goroutine only, reset at each Sync.
	extSkip      bool
	extForwarded bool
	stmts        map[string]*stmtEntry
	portals      map[string]*portal
}

func (c *conn) writeClient(msgs ...pgproto3.Message) error {
	buf, err := encode(msgs...)
	if err != nil {
		return err
	}
	return c.writeClientRaw(buf)
}

func (c *conn) writeClientRaw(buf []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.client.Write(buf)
	return err
}

// forward sends a client message to the backend unchanged.
func (c *conn) forward(msg pgproto3.FrontendMessage) error {
	buf, err := msg.Encode(nil)
	if err != nil {
		return err
	}
	switch msg.(type) {
	case *pgproto3.Query, *pgproto3.Sync, *pgproto3.FunctionCall:
		c.mu.Lock()
		c.pending++
		c.mu.Unlock()
	}
	_, err = c.be.Write(buf)
	return err
}

// relay forwards backend messages to the client, or to the capture channel
// while the proxy runs a query of its own. ReadyForQuery carries the
// backend's own view of the transaction block, so it is the authoritative
// source for InTxn whichever protocol the client used.
func (c *conn) relay() {
	defer func() {
		c.mu.Lock()
		c.dead = true
		c.cond.Broadcast()
		c.mu.Unlock()
		close(c.relayDone)
		if err := c.client.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			c.p.cfg.Logger.Printf("pgproxy: close client: %v", err)
		}
	}()
	for {
		msg, err := c.fe.Receive()
		if err != nil {
			return
		}
		raw, err := msg.Encode(nil)
		if err != nil {
			c.p.cfg.Logger.Printf("pgproxy: encode backend message: %v", err)
			return
		}
		c.mu.Lock()
		if rfq, ok := msg.(*pgproto3.ReadyForQuery); ok {
			c.txn.noteBackendStatus(rfq.TxStatus)
			if c.pending > 0 {
				c.pending--
			}
			c.cond.Broadcast()
		}
		ch := c.capture
		c.mu.Unlock()
		if ch != nil {
			ch <- raw
			continue
		}
		if err := c.writeClientRaw(raw); err != nil {
			return
		}
	}
}

// runCapture sends a query to the backend and swallows its frames up to and
// including ReadyForQuery. It first waits until the backend owes no response
// to the client, so no client frame is ever swallowed.
func (c *conn) runCapture(sql string) ([][]byte, error) {
	c.mu.Lock()
	for c.pending > 0 && !c.dead {
		c.cond.Wait()
	}
	if c.dead {
		c.mu.Unlock()
		return nil, errBackendGone
	}
	ch := make(chan []byte, 16)
	c.capture = ch
	c.pending++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.capture = nil
		c.mu.Unlock()
	}()

	buf, err := (&pgproto3.Query{String: sql}).Encode(nil)
	if err != nil {
		return nil, err
	}
	if _, err := c.be.Write(buf); err != nil {
		return nil, err
	}
	var frames [][]byte
	for {
		select {
		case raw := <-ch:
			frames = append(frames, raw)
			if raw[0] == 'Z' {
				return frames, nil
			}
		case <-c.relayDone:
			return nil, errBackendGone
		}
	}
}

func decodeError(raw []byte) (*pgproto3.ErrorResponse, error) {
	var e pgproto3.ErrorResponse
	if err := e.Decode(raw[5:]); err != nil {
		return nil, err
	}
	return &e, nil
}

// captureValues returns the single-column values of a captured result, or
// false when the probe failed.
func captureValues(frames [][]byte) ([]string, bool) {
	var out []string
	for _, raw := range frames {
		switch raw[0] {
		case 'E':
			return nil, false
		case 'D':
			var row pgproto3.DataRow
			if err := row.Decode(raw[5:]); err != nil || len(row.Values) != 1 {
				return nil, false
			}
			out = append(out, string(row.Values[0]))
		}
	}
	return out, true
}

func (c *conn) status() byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}

func (c *conn) statusLocked() byte {
	switch {
	case c.txn.Aborted || c.txn.SyntheticAbort:
		return 'E'
	case c.txn.InTxn:
		return 'T'
	}
	return 'I'
}

func (c *conn) ready() *pgproto3.ReadyForQuery {
	return &pgproto3.ReadyForQuery{TxStatus: c.status()}
}

// reject sends an ErrorResponse for a statement the backend never saw. In an
// explicit transaction it poisons the block the way a real error would; the
// backend is still in a clean transaction, so the proxy tracks the abort.
func (c *conn) reject(e *DsqlError) error {
	c.mu.Lock()
	if c.txn.InTxn {
		c.txn.SyntheticAbort = true
	}
	st := c.statusLocked()
	c.mu.Unlock()
	return c.writeClient(errorMessage(e), &pgproto3.ReadyForQuery{TxStatus: st})
}

var errAborted = dsqlErr("25P02", "current transaction is aborted, commands ignored until end of transaction block")

// abortGate lets only the statement that ends the block run after a synthetic
// abort. COMMIT becomes ROLLBACK so the aborted work is discarded and the
// client sees the ROLLBACK tag, as PostgreSQL reports a failed block.
func (c *conn) abortGate(sql string) (*DsqlError, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.txn.SyntheticAbort {
		return nil, sql, false
	}
	if classifyStatement(sql) != classEnd {
		return errAborted, sql, false
	}
	c.txn.SyntheticAbort = false
	if commitOrEndLeadingRE.MatchString(stripLeadingComments(sql)) {
		return nil, "ROLLBACK", true
	}
	return nil, sql, false
}

func (c *conn) applyTxn(sql string) {
	c.mu.Lock()
	c.txn.apply(sql)
	c.mu.Unlock()
}

func (c *conn) noteCatalog(sql string) {
	v := c.p.currentCatalog()
	if classifyStatement(sql) == classDDL {
		v = c.p.bumpCatalog()
	}
	c.mu.Lock()
	c.catalog, c.hasCatalog = v, true
	c.mu.Unlock()
}

// staleCatalog reports OC001: this connection's cached catalog is behind
// another session's DDL.
func (c *conn) staleCatalog() bool {
	current := c.p.currentCatalog()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.txn.InTxn && c.hasCatalog && c.catalog < current
}

var (
	errOC001      = dsqlErr("40001", "schema has been updated by another transaction (OC001)")
	errAsyncBatch = dsqlErr("0A000", "asynchronous DDL is not supported in multi-statement queries")
)

func (c *conn) rollbackStale() error {
	if _, err := c.runCapture("ROLLBACK"); err != nil {
		return err
	}
	current := c.p.currentCatalog()
	c.mu.Lock()
	c.txn.apply("ROLLBACK")
	c.catalog, c.hasCatalog = current, true
	c.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// Statement planning (shared by both protocols)
// ---------------------------------------------------------------------------

type planKind int

const (
	planForward planKind = iota
	planError
	planSynthetic
)

type plan struct {
	kind  planKind
	err   *DsqlError
	synth *synthetic
}

func quoteLiteral(s string) string { return strings.ReplaceAll(s, "'", "''") }

// indexCount counts the table's existing indexes; false when the probe failed.
func (c *conn) indexCount(table string) (int, bool, error) {
	schema, bare := rpartitionDot(table)
	schema = strings.Trim(schema, `"`)
	if schema == "" {
		schema = "public"
	}
	bare = strings.Trim(bare, `"`)
	frames, err := c.runCapture(fmt.Sprintf(
		"SELECT count(*) FROM pg_indexes WHERE schemaname = '%s' AND tablename = '%s'",
		quoteLiteral(schema), quoteLiteral(bare)))
	if err != nil {
		return 0, false, err
	}
	vals, ok := captureValues(frames)
	if !ok || len(vals) != 1 {
		return 0, false, nil
	}
	n, convErr := strconv.Atoi(vals[0])
	if convErr != nil {
		return 0, false, nil
	}
	return n, true, nil
}

// relationLiteral quotes a normalized name path back into SQL, keeping the
// exact case the catalog holds.
func relationLiteral(parts []string) string {
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(quoted, ".")
}

// primaryKeyColumns returns the primary key columns of relation. It uses
// to_regclass, so an unknown relation yields no rows instead of aborting the
// client's transaction; the backend then answers the statement itself.
func (c *conn) primaryKeyColumns(relation []string) ([]string, error) {
	if len(relation) == 0 {
		return nil, nil
	}
	frames, err := c.runCapture(
		"SELECT a.attname FROM pg_index i JOIN pg_attribute a " +
			"ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey) " +
			"WHERE i.indrelid = to_regclass('" + quoteLiteral(relationLiteral(relation)) + "') AND i.indisprimary")
	if err != nil {
		return nil, err
	}
	vals, ok := captureValues(frames)
	if !ok {
		return nil, nil
	}
	return vals, nil
}

// checkDropColumn refuses to drop a primary key column.
func (c *conn) checkDropColumn(sql string) (*DsqlError, error) {
	cols := droppedColumns(sql)
	if len(cols) == 0 {
		return nil, nil
	}
	m := alterTableTargetRE.FindStringSubmatch(sql)
	if m == nil {
		return nil, nil
	}
	pk, err := c.primaryKeyColumns(identifierPath(m[1]))
	if err != nil || len(pk) == 0 {
		return nil, err
	}
	for _, col := range cols {
		for _, k := range pk {
			if col == k {
				return dsqlErr("0A000", "cannot drop primary key column "+col), nil
			}
		}
	}
	return nil, nil
}

// planStatement decides what to do with one statement, for either protocol.
// allowProbe is false when the backend is mid extended-protocol sequence,
// where injecting a probe query would break the protocol; only the rules
// that look at backend state are then skipped.
func (c *conn) planStatement(s string, allowProbe bool) (plan, error) {
	if q, ok := matchSysJobs(s); ok {
		cols := q.Columns
		if cols == nil {
			cols = jobColumns
		}
		for _, col := range cols {
			if !isJobColumn(col) {
				return plan{kind: planError, err: dsqlErr("42703", fmt.Sprintf("column %q does not exist", col))}, nil
			}
		}
		return plan{kind: planSynthetic, synth: &synthetic{jobs: &q, cols: cols}}, nil
	}
	if w, ok := matchWaitForJob(s); ok {
		return plan{kind: planSynthetic, synth: &synthetic{wait: &w}}, nil
	}

	c.mu.Lock()
	derr, rw := validate(s, &c.txn)
	c.mu.Unlock()
	if derr != nil {
		return plan{kind: planError, err: derr}, nil
	}
	if rw != nil && rw.Kind == kindIndexAsync {
		if !allowProbe {
			return plan{kind: planError, err: errAsyncBatch}, nil
		}
		if rw.JobType == jobIndexBuild && rw.Table != "" {
			n, ok, err := c.indexCount(rw.Table)
			if err != nil {
				return plan{}, err
			}
			if ok && n >= 24 {
				return plan{kind: planError, err: dsqlErr("0A000",
					fmt.Sprintf("table %s already has the maximum of 24 indexes", rw.Table))}, nil
			}
		}
		return plan{kind: planSynthetic, synth: &synthetic{rewrite: rw}}, nil
	}
	if allowProbe {
		derr, err := c.checkDropColumn(s)
		if err != nil {
			return plan{}, err
		}
		if derr != nil {
			return plan{kind: planError, err: derr}, nil
		}
	}
	return plan{kind: planForward}, nil
}

// runRewrite executes a rewritten ASYNC statement. It returns the job, or the
// raw backend ErrorResponse to relay to the client.
func (c *conn) runRewrite(rw *Rewrite) (Job, []byte, error) {
	frames, err := c.runCapture(rw.SQL)
	if err != nil {
		return Job{}, nil, err
	}
	for _, raw := range frames {
		if raw[0] != 'E' {
			continue
		}
		e, err := decodeError(raw)
		if err != nil {
			return Job{}, nil, err
		}
		c.mu.Lock()
		inTxn := c.txn.InTxn
		c.mu.Unlock()
		// Data that does not satisfy the constraint (or a duplicate key for a
		// unique index) fails the job, not its submission: DSQL answers with
		// a job id and reports the failure through sys.jobs. Anything else is
		// refused on the spot. In a transaction the backend is left aborted,
		// so relay there either way.
		if strings.HasPrefix(e.Code, "23") && !inTxn {
			job, err := c.p.registerJob(rw.ObjectName, rw.JobType, "failed", e.Message)
			return job, nil, err
		}
		return Job{}, raw, nil
	}
	v := c.p.bumpCatalog()
	c.mu.Lock()
	c.catalog, c.hasCatalog = v, true
	c.mu.Unlock()
	job, err := c.p.registerJob(rw.ObjectName, rw.JobType, "completed", "")
	return job, nil, err
}

// ---------------------------------------------------------------------------
// Simple query protocol
// ---------------------------------------------------------------------------

func (c *conn) handleQuery(q *pgproto3.Query) error {
	sql := q.String
	stmts := splitStatements(sql)

	if len(stmts) == 1 {
		s := stripLeadingComments(stmts[0])

		// A statement rejected earlier in this transaction poisoned the block.
		gateErr, fwd, replaced := c.abortGate(s)
		if gateErr != nil {
			return c.writeClient(errorMessage(gateErr), c.ready())
		}
		if replaced { // COMMIT of an aborted block -> ROLLBACK
			c.applyTxn(fwd)
			return c.forward(&pgproto3.Query{String: fwd})
		}

		// OC001: abort like real DSQL (40001); the next attempt sees the
		// fresh catalog and succeeds.
		if c.staleCatalog() {
			if err := c.rollbackStale(); err != nil {
				return err
			}
			return c.reject(errOC001)
		}

		pl, err := c.planStatement(s, true)
		if err != nil {
			return err
		}
		switch pl.kind {
		case planError:
			return c.reject(pl.err)
		case planSynthetic:
			return c.runSyntheticSimple(s, pl.synth)
		}
		c.noteCatalog(s)
		c.applyTxn(s)
		return c.forward(q)
	}

	// Multi-statement batch (implicit transaction): validate every statement
	// up front, then forward the batch verbatim.
	if len(stmts) > 0 {
		if handled, err := c.validateBatch(stmts); handled {
			return err
		}
	}
	return c.forward(q)
}

func (c *conn) runSyntheticSimple(s string, synth *synthetic) error {
	cols := synth.columns()
	if synth.rewrite == nil {
		rows, tag := synth.rows(c.p, nil)
		msgs := []pgproto3.Message{rowDescription(cols, nil)}
		for _, r := range rows {
			msgs = append(msgs, dataRow(r, cols, nil))
		}
		msgs = append(msgs, &pgproto3.CommandComplete{CommandTag: []byte(tag)}, c.ready())
		return c.writeClient(msgs...)
	}
	// Run the rewritten statement on the backend, swallow its response and
	// synthesize the DSQL job_id result set.
	c.applyTxn(s)
	job, backendErr, err := c.runRewrite(synth.rewrite)
	if err != nil {
		return err
	}
	if backendErr != nil {
		if err := c.writeClientRaw(backendErr); err != nil {
			return err
		}
		return c.writeClient(c.ready())
	}
	return c.writeClient(
		rowDescription(cols, nil),
		dataRow([]string{job.JobID}, cols, nil),
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		c.ready(),
	)
}

// validateBatch applies the DSQL rules to a multi-statement batch. handled
// is true when the batch was answered by the proxy and must not be forwarded.
func (c *conn) validateBatch(stmts []string) (bool, error) {
	c.mu.Lock()
	// An aborted block only accepts an explicit rollback; forwarding a batch
	// that opened with COMMIT would commit work the client was told failed.
	if c.txn.SyntheticAbort {
		if !rollbackOrAbortLeadRE.MatchString(stripLeadingComments(stmts[0])) {
			st := c.statusLocked()
			c.mu.Unlock()
			return true, c.writeClient(errorMessage(errAborted), &pgproto3.ReadyForQuery{TxStatus: st})
		}
		c.txn.SyntheticAbort = false
	}
	sim := c.txn
	c.mu.Unlock()

	explicit := false
	if !sim.InTxn && len(stmts) > 1 {
		sim.InTxn = true // implicit transaction
	}
	for _, s := range stmts {
		if classifyStatement(s) == classBegin {
			explicit = true
		}
		derr, rw := validate(s, &sim)
		if rw != nil {
			// ASYNC rewrites are only supported for single statements.
			derr = errAsyncBatch
		}
		if derr != nil {
			return true, c.reject(derr)
		}
		sim.apply(s)
	}

	ddl := false
	for _, s := range stmts {
		if classifyStatement(s) == classDDL {
			ddl = true
		}
	}
	v := c.p.currentCatalog()
	if ddl {
		v = c.p.bumpCatalog()
	}

	c.mu.Lock()
	if c.txn.InTxn || explicit {
		c.txn.InTxn, c.txn.DDLSeen, c.txn.DMLSeen = sim.InTxn, sim.DDLSeen, sim.DMLSeen
		c.txn.StartedAt, c.txn.RowsEst, c.txn.BytesEst = sim.StartedAt, sim.RowsEst, sim.BytesEst
	}
	// Otherwise the implicit batch commits and the connection stays idle.
	c.catalog, c.hasCatalog = v, true
	c.mu.Unlock()
	return false, nil
}

// ---------------------------------------------------------------------------
// Extended query protocol (Parse / Bind / Describe / Execute / Close / Sync)
//
// pgx, pgjdbc, asyncpg, psycopg3 and every ORM built on prepared statements
// send SQL this way. Validating only the simple protocol would let all of
// them past the DSQL subset entirely.
// ---------------------------------------------------------------------------

func (c *conn) extForward(msg pgproto3.FrontendMessage) error {
	c.extForwarded = true
	return c.forward(msg)
}

// extError reports an error and enters skip-until-Sync, per the protocol.
func (c *conn) extError(e *DsqlError) error {
	c.mu.Lock()
	if c.txn.InTxn {
		c.txn.SyntheticAbort = true
	}
	c.mu.Unlock()
	c.extSkip = true
	return c.writeClient(errorMessage(e))
}

func (c *conn) extParse(m *pgproto3.Parse) error {
	s := strings.TrimSpace(stripLeadingComments(m.Query))
	entry := &stmtEntry{sql: s}
	if s == "" {
		c.stmts[m.Name] = entry
		return c.extForward(m)
	}

	gateErr, fwd, replaced := c.abortGate(s)
	if gateErr != nil {
		return c.extError(gateErr)
	}
	if replaced { // COMMIT of an aborted block -> ROLLBACK
		entry.sql = fwd
		c.stmts[m.Name] = entry
		return c.extForward(&pgproto3.Parse{Name: m.Name, Query: fwd})
	}

	// Probes and rewrites need the backend to be idle; it is not once
	// anything in this pipelined batch has been forwarded.
	allowProbe := !c.extForwarded
	if allowProbe && c.staleCatalog() {
		if err := c.rollbackStale(); err != nil {
			return err
		}
		return c.extError(errOC001)
	}

	pl, err := c.planStatement(s, allowProbe)
	if err != nil {
		return err
	}
	switch pl.kind {
	case planError:
		return c.extError(pl.err)
	case planSynthetic:
		entry.synth = pl.synth
		c.stmts[m.Name] = entry
		return c.writeClient(&pgproto3.ParseComplete{})
	}
	c.stmts[m.Name] = entry
	return c.extForward(m)
}

// bindGate re-checks, at Bind, the rules that depend on the transaction state.
// Clients such as pgx cache prepared statements, so a statement parsed in one
// transaction context is executed in others without another Parse.
func (c *conn) bindGate(sql string) *DsqlError {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.txn.SyntheticAbort && classifyStatement(sql) != classEnd {
		return errAborted
	}
	return checkTxnRules(sql, &c.txn)
}

func (c *conn) extBind(m *pgproto3.Bind) error {
	entry := c.stmts[m.PreparedStatement]
	if entry == nil {
		return c.extForward(m)
	}
	if entry.synth == nil && entry.sql != "" && !c.extForwarded {
		if gateErr := c.bindGate(entry.sql); gateErr != nil {
			return c.extError(gateErr)
		}
	}
	params := make([][]byte, len(m.Parameters))
	for i, v := range m.Parameters {
		if v != nil {
			params[i] = append([]byte{}, v...)
		}
	}
	c.portals[m.DestinationPortal] = &portal{
		entry:   entry,
		params:  params,
		formats: append([]int16(nil), m.ResultFormatCodes...),
	}
	if entry.synth == nil {
		return c.extForward(m)
	}
	return c.writeClient(&pgproto3.BindComplete{})
}

func (c *conn) extDescribe(m *pgproto3.Describe) error {
	var entry *stmtEntry
	var formats []int16
	if m.ObjectType == 'S' {
		entry = c.stmts[m.Name]
	} else if pt := c.portals[m.Name]; pt != nil {
		entry, formats = pt.entry, pt.formats
	}
	if entry == nil || entry.synth == nil {
		return c.extForward(m)
	}
	var msgs []pgproto3.Message
	if m.ObjectType == 'S' {
		oids := make([]uint32, entry.synth.paramCount())
		for i := range oids {
			oids[i] = textOID
		}
		msgs = append(msgs, &pgproto3.ParameterDescription{ParameterOIDs: oids})
	}
	msgs = append(msgs, rowDescription(entry.synth.columns(), formats))
	return c.writeClient(msgs...)
}

func (c *conn) extExecute(m *pgproto3.Execute) error {
	pt := c.portals[m.Portal]
	if pt == nil || pt.entry.synth == nil {
		if pt != nil && pt.entry.sql != "" {
			c.noteCatalog(pt.entry.sql)
			c.applyTxn(pt.entry.sql)
		}
		return c.extForward(m)
	}

	synth := pt.entry.synth
	cols := synth.columns()
	if synth.rewrite == nil {
		rows, tag := synth.rows(c.p, pt.params)
		msgs := make([]pgproto3.Message, 0, len(rows)+1)
		for _, r := range rows {
			msgs = append(msgs, dataRow(r, cols, pt.formats))
		}
		msgs = append(msgs, &pgproto3.CommandComplete{CommandTag: []byte(tag)})
		return c.writeClient(msgs...)
	}

	if c.extForwarded {
		return c.extError(errAsyncBatch)
	}
	c.applyTxn(pt.entry.sql)
	job, backendErr, err := c.runRewrite(synth.rewrite)
	if err != nil {
		return err
	}
	if backendErr != nil {
		c.extSkip = true
		return c.writeClientRaw(backendErr)
	}
	return c.writeClient(
		dataRow([]string{job.JobID}, cols, pt.formats),
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
	)
}

func (c *conn) extClose(m *pgproto3.Close) error {
	var entry *stmtEntry
	if m.ObjectType == 'S' {
		entry = c.stmts[m.Name]
		delete(c.stmts, m.Name)
	} else {
		if pt := c.portals[m.Name]; pt != nil {
			entry = pt.entry
		}
		delete(c.portals, m.Name)
	}
	if entry == nil || entry.synth == nil {
		return c.extForward(m)
	}
	return c.writeClient(&pgproto3.CloseComplete{})
}

func (c *conn) extSync(m *pgproto3.Sync) error {
	forwarded := c.extForwarded
	c.extSkip = false
	c.extForwarded = false
	delete(c.portals, "") // the unnamed portal dies at Sync
	if forwarded {
		// The backend answers with its own ReadyForQuery, which also
		// refreshes the transaction state via relay.
		return c.forward(m)
	}
	return c.writeClient(c.ready())
}

func (c *conn) handleExtended(msg pgproto3.FrontendMessage) error {
	if _, isSync := msg.(*pgproto3.Sync); c.extSkip && !isSync {
		return nil // after an error, everything is ignored until Sync
	}
	switch m := msg.(type) {
	case *pgproto3.Parse:
		return c.extParse(m)
	case *pgproto3.Bind:
		return c.extBind(m)
	case *pgproto3.Describe:
		return c.extDescribe(m)
	case *pgproto3.Execute:
		return c.extExecute(m)
	case *pgproto3.Close:
		return c.extClose(m)
	case *pgproto3.Sync:
		return c.extSync(m)
	case *pgproto3.Flush:
		if c.extForwarded {
			return c.extForward(m)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Connection lifecycle
// ---------------------------------------------------------------------------

func (p *Proxy) connectBackend(params map[string]string) (*pgconn.HijackedConn, error) {
	database := params["database"]
	if database == "" {
		database = "postgres"
	}
	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(p.cfg.BackendUser, p.cfg.BackendPassword),
		Host:     net.JoinHostPort(p.cfg.BackendHost, strconv.Itoa(int(p.cfg.BackendPort))),
		Path:     "/" + database,
		RawQuery: "sslmode=disable",
	}).String()
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	for k, v := range params {
		switch k {
		case "user", "database", "replication":
			continue
		}
		cfg.RuntimeParams[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pg, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pg.SyncConn(ctx); err != nil {
		return nil, err
	}
	return pg.Hijack()
}

func greeting() ([]pgproto3.Message, error) {
	var key [8]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, err
	}
	msgs := []pgproto3.Message{&pgproto3.AuthenticationOk{}}
	for _, kv := range startupParams {
		msgs = append(msgs, &pgproto3.ParameterStatus{Name: kv[0], Value: kv[1]})
	}
	msgs = append(msgs,
		&pgproto3.BackendKeyData{ProcessID: binary.BigEndian.Uint32(key[:4]) >> 1, SecretKey: key[4:]},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)
	return msgs, nil
}

func (p *Proxy) serve(nc net.Conn) error {
	c := &conn{
		p:         p,
		client:    nc,
		cb:        pgproto3.NewBackend(nc, nc),
		relayDone: make(chan struct{}),
		stmts:     map[string]*stmtEntry{},
		portals:   map[string]*portal{},
	}
	c.cond = sync.NewCond(&c.mu)

	// Startup phase: SSL/GSS negotiation, then StartupMessage.
	var params map[string]string
	for params == nil {
		msg, err := c.cb.ReceiveStartupMessage()
		if err != nil {
			return err
		}
		switch m := msg.(type) {
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			if _, err := nc.Write([]byte{'N'}); err != nil {
				return err
			}
		case *pgproto3.StartupMessage:
			params = map[string]string{}
			for k, v := range m.Parameters {
				params[k] = v
			}
		default:
			return nil // cancel request or unknown packet: close
		}
	}

	// Any user and password is accepted: DSQL IAM tokens are client-side
	// SigV4 presigning, so the token is just a password to the proxy.
	hc, err := p.connectBackend(params)
	if err != nil {
		werr := c.writeClient(&pgproto3.ErrorResponse{
			Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "08006",
			Message: "embedded-dsql: backend unavailable: " + err.Error(),
		})
		return errors.Join(fmt.Errorf("connect backend: %w", err), werr)
	}
	c.be, c.fe = hc.Conn, hc.Frontend
	if !p.track(c.be) {
		return errors.Join(errBackendGone, c.be.Close())
	}
	defer p.untrack(c.be)

	msgs, err := greeting()
	if err != nil {
		return err
	}
	if err := c.writeClient(msgs...); err != nil {
		return err
	}

	go c.relay()
	defer func() { <-c.relayDone }()
	defer func() {
		if err := c.be.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			p.cfg.Logger.Printf("pgproxy: close backend: %v", err)
		}
	}()

	for {
		msg, err := c.cb.Receive()
		if err != nil {
			return err
		}
		switch m := msg.(type) {
		case *pgproto3.Terminate:
			return c.forward(m)
		case *pgproto3.Query:
			err = c.handleQuery(m)
		case *pgproto3.Parse, *pgproto3.Bind, *pgproto3.Describe, *pgproto3.Execute,
			*pgproto3.Close, *pgproto3.Sync, *pgproto3.Flush:
			err = c.handleExtended(m)
		default:
			// Everything else relays verbatim (COPY frames, FunctionCall...).
			err = c.forward(m)
		}
		if err != nil {
			return err
		}
	}
}
