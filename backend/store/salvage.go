// File overview: Best-effort recovery of a corrupt per-user SQLite database.
// SQLITE_CORRUPT usually damages a bounded set of pages, so the rows that are
// still readable can be copied into a freshly migrated database. This file
// implements that copy: it walks every table of the new schema in rowid order,
// skips the rows whose pages cannot be read, repairs the foreign key graph the
// lost rows leave behind, and reports exactly what survived. It issues no write
// against the corrupt file; "rolltop recover-db" quarantines that file intact
// once a recovered database exists.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"

	"rolltop/backend/plugins"
)

const (
	// Rows are streamed, so the batch size only bounds how much work a corrupt
	// page can cost: a failing batch is retried one row at a time.
	salvageBatchRows = 200
	// Committing per table would hold one transaction over a multi-gigabyte
	// messages table; committing per row would fsync millions of times.
	salvageCommitRows = 2000
	// A deleted orphan can orphan another row, but the schema is only a few
	// levels deep, so a bounded number of passes always converges.
	salvageForeignKeyPasses = 10
	// Long tables report intermediate progress so an operator can see a
	// multi-gigabyte recovery advancing.
	salvageProgressRows = 25000
	// Blind probing doubles its stride, so this budget steps over any damaged
	// rowid range a real table can hold before the scan gives up.
	salvageMaxProbes      = 64
	salvageMaxProbeStride = 1 << 20
)

// TableSalvage is the per-table outcome of a recovery run.
type TableSalvage struct {
	Table   string `json:"table"`
	Copied  int64  `json:"copied"`
	Skipped int64  `json:"skipped"`
	Dropped int64  `json:"dropped"`
	// Gaps counts rowid ranges the scan stepped over because neither the rows
	// nor the index entries around them could be read. Each gap can hide any
	// number of lost rows.
	Gaps int64 `json:"gaps"`
	// Failure is set when the table could not be read to the end. Rows counted
	// in Copied are still present in the recovered database.
	Failure string `json:"failure,omitempty"`
}

// SalvageReport summarizes one recovery run for the operator.
type SalvageReport struct {
	SourcePath    string         `json:"source_path"`
	DestPath      string         `json:"dest_path"`
	Tables        []TableSalvage `json:"tables,omitempty"`
	MissingTables []string       `json:"missing_tables,omitempty"`
	RowsCopied    int64          `json:"rows_copied"`
	RowsSkipped   int64          `json:"rows_skipped"`
	RowsDropped   int64          `json:"rows_dropped"`
	Gaps          int64          `json:"gaps"`
}

// Incomplete reports whether any row was lost, so callers can tell the operator
// that a search rebuild is needed.
func (r SalvageReport) Incomplete() bool {
	return r.RowsSkipped > 0 || r.RowsDropped > 0 || r.Gaps > 0 || len(r.MissingTables) > 0 || r.FailedTables() > 0
}

// FailedTables counts tables whose scan stopped early on an unreadable page.
func (r SalvageReport) FailedTables() int {
	failed := 0
	for _, table := range r.Tables {
		if table.Failure != "" {
			failed++
		}
	}
	return failed
}

// SalvageUserDatabase copies every readable row of a corrupt user database into
// a new database at destPath. destPath must not exist; the caller installs it
// in place of the corrupt file only after this returns successfully. The source
// is opened read-write so SQLite can replay a hot WAL and the newest committed
// rows are salvaged, but no statement issued here changes its contents.
func SalvageUserDatabase(ctx context.Context, sourcePath, destPath string, manifests []plugins.Manifest, progress func(string)) (SalvageReport, error) {
	report := SalvageReport{SourcePath: sourcePath, DestPath: destPath}
	if strings.TrimSpace(sourcePath) == "" || strings.TrimSpace(destPath) == "" {
		return report, fmt.Errorf("salvage requires a source and destination database path")
	}
	if sourcePath == destPath {
		return report, fmt.Errorf("salvage destination must differ from the corrupt database")
	}
	// open() below would happily reuse an existing file, and INSERT OR IGNORE
	// would then count its rows as dropped duplicates, understating the loss.
	if _, err := os.Lstat(destPath); err == nil {
		return report, fmt.Errorf("salvage destination already exists: %s", destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return report, fmt.Errorf("inspect salvage destination %s: %w", destPath, err)
	}

	source, err := sql.Open("sqlite3", sourcePath+"?_busy_timeout=5000")
	if err != nil {
		return report, fmt.Errorf("open corrupt database %s: %w", sourcePath, err)
	}
	// One connection keeps the scan on a single SQLite snapshot and keeps the
	// error reported for a corrupt page stable across retries.
	source.SetMaxOpenConns(1)
	defer source.Close()

	dest, err := open(destPath, "", false, schemaUser, nil, pluginCatalogFromManifests(manifests))
	if err != nil {
		return report, fmt.Errorf("create recovered database %s: %w", destPath, err)
	}
	defer dest.Close()

	writerConn, err := dest.db.Conn(ctx)
	if err != nil {
		return report, fmt.Errorf("reserve recovered database connection: %w", err)
	}
	defer writerConn.Close()
	// Rows are inserted in schema order, not dependency order, and rows whose
	// parent was lost must still be copied before the foreign key repair pass
	// decides what to drop.
	if _, err := writerConn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return report, fmt.Errorf("disable foreign keys on recovered database: %w", err)
	}

	tables, err := listUserTables(ctx, dest.db)
	if err != nil {
		return report, fmt.Errorf("read recovered database schema: %w", err)
	}
	sourceTables, err := listUserTables(ctx, source)
	if err != nil {
		return report, fmt.Errorf("read corrupt database schema: %w", err)
	}
	sourceHas := make(map[string]bool, len(sourceTables))
	for _, table := range sourceTables {
		sourceHas[table] = true
	}
	for _, table := range sourceTables {
		if !slices.Contains(tables, table) {
			report.MissingTables = append(report.MissingTables, table)
		}
	}

	writer := &salvageWriter{conn: writerConn}
	// Any error return below can leave a batch transaction open on the reserved
	// connection; a commit later makes this a no-op.
	defer writer.rollback()
	for _, table := range tables {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if !sourceHas[table] {
			continue
		}
		columns, err := commonColumns(ctx, source, dest.db, table)
		if err != nil {
			return report, fmt.Errorf("compare %s columns: %w", table, err)
		}
		if len(columns) == 0 {
			report.Tables = append(report.Tables, TableSalvage{Table: table, Failure: "no columns in common with the current schema"})
			continue
		}
		result, err := copyTable(ctx, source, writer, table, columns, progress)
		if err != nil {
			return report, fmt.Errorf("write recovered %s rows: %w", table, err)
		}
		if err := writer.commit(ctx); err != nil {
			return report, fmt.Errorf("commit recovered %s rows: %w", table, err)
		}
		report.Tables = append(report.Tables, result)
		report.RowsCopied += result.Copied
		report.RowsSkipped += result.Skipped
		report.RowsDropped += result.Dropped
		report.Gaps += result.Gaps
		if result.Copied > 0 || result.Skipped > 0 || result.Gaps > 0 || result.Failure != "" {
			reportProgress(progress, fmt.Sprintf("%s: %d row(s) recovered, %d unreadable, %d damaged range(s)", table, result.Copied, result.Skipped, result.Gaps))
		}
	}

	copySequenceCounters(ctx, source, writer, tables)
	if err := writer.commit(ctx); err != nil {
		return report, fmt.Errorf("commit recovered sequence counters: %w", err)
	}

	reportProgress(progress, "repairing foreign key references")
	dropped, err := repairForeignKeys(ctx, writerConn)
	report.RowsDropped += dropped
	if err != nil {
		return report, fmt.Errorf("repair recovered foreign keys: %w", err)
	}
	return report, nil
}

// salvageWriter batches inserts into the recovered database. All work runs on
// one reserved connection so PRAGMA foreign_keys stays off for the whole run.
type salvageWriter struct {
	conn    *sql.Conn
	tx      *sql.Tx
	pending int
	// prepared caches the current transaction's statement for the table being
	// copied. A multi-gigabyte table is millions of identical inserts, so
	// re-parsing the statement per row would be a large share of the recovery.
	prepared     *sql.Stmt
	preparedText string
}

// exec runs a one-off statement. Row inserts go through execPrepared instead.
func (w *salvageWriter) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := w.begin(ctx); err != nil {
		return nil, err
	}
	result, err := w.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return result, w.countRow(ctx)
}

// execPrepared runs the same statement repeatedly, preparing it once per
// transaction and re-preparing after each intermediate commit.
func (w *salvageWriter) execPrepared(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := w.begin(ctx); err != nil {
		return nil, err
	}
	if w.prepared == nil || w.preparedText != query {
		w.closePrepared()
		statement, err := w.tx.PrepareContext(ctx, query)
		if err != nil {
			return nil, err
		}
		w.prepared, w.preparedText = statement, query
	}
	result, err := w.prepared.ExecContext(ctx, args...)
	if err != nil {
		return nil, err
	}
	return result, w.countRow(ctx)
}

func (w *salvageWriter) begin(ctx context.Context) error {
	if w.tx != nil {
		return nil
	}
	tx, err := w.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	w.tx = tx
	w.pending = 0
	return nil
}

func (w *salvageWriter) countRow(ctx context.Context) error {
	w.pending++
	if w.pending < salvageCommitRows {
		return nil
	}
	return w.commit(ctx)
}

// closePrepared releases the cached statement. It is bound to the transaction
// that prepared it, so it cannot outlive a commit.
func (w *salvageWriter) closePrepared() {
	if w.prepared == nil {
		return
	}
	_ = w.prepared.Close()
	w.prepared, w.preparedText = nil, ""
}

// rollback abandons an unfinished batch. It is a no-op after a commit.
func (w *salvageWriter) rollback() {
	if w.tx == nil {
		return
	}
	w.closePrepared()
	tx := w.tx
	w.tx = nil
	w.pending = 0
	_ = tx.Rollback()
}

func (w *salvageWriter) commit(ctx context.Context) error {
	if w.tx == nil {
		return nil
	}
	w.closePrepared()
	tx := w.tx
	w.tx = nil
	w.pending = 0
	return tx.Commit()
}

// copyTable streams one table from the corrupt database. Read errors are
// treated as damaged pages: the batch is retried one row at a time, and the
// single row that cannot be read is skipped so the rest of the table survives.
func copyTable(ctx context.Context, source *sql.DB, writer *salvageWriter, table string, columns []string, progress func(string)) (TableSalvage, error) {
	result := TableSalvage{Table: table}
	insert := insertStatement(table, columns)
	reported := int64(0)
	announce := func() {
		if result.Copied-reported < salvageProgressRows {
			return
		}
		reported = result.Copied
		reportProgress(progress, fmt.Sprintf("%s: %d row(s) recovered so far", table, result.Copied))
	}

	if !tableHasRowID(ctx, source, table) {
		// Without rowids there is no cursor to resume from, so the table is
		// copied in one pass and truncated at the first unreadable row.
		rows, err := source.QueryContext(ctx, selectStatement(table, columns, false))
		if err != nil {
			result.Failure = err.Error()
			return result, nil
		}
		defer rows.Close()
		for rows.Next() {
			values := make([]any, len(columns))
			if err := rows.Scan(scanTargets(values)...); err != nil {
				result.Failure = err.Error()
				return result, nil
			}
			if err := insertRow(ctx, writer, insert, values, &result); err != nil {
				return result, err
			}
		}
		if err := rows.Err(); err != nil {
			result.Failure = err.Error()
		}
		return result, nil
	}

	// The highest rowid bounds the blind probing below. It is read from the
	// rightmost branch of the table, which usually survives damage further left.
	maxRowID, haveMax := tableMaxRowID(ctx, source, table)

	cursor := int64(math.MinInt64)
	batch := salvageBatchRows
	stride := int64(0)
	probes := 0
	query := selectStatement(table, columns, true)
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if haveMax && cursor > maxRowID {
			return result, nil
		}
		read, lastRowID, readErr, writeErr := copyBatch(ctx, source, writer, query, insert, columns, cursor, batch, &result)
		if writeErr != nil {
			return result, writeErr
		}
		announce()
		if readErr == nil {
			if read < batch || lastRowID == math.MaxInt64 {
				return result, nil
			}
			cursor = lastRowID + 1
			batch = salvageBatchRows
			stride, probes = 0, 0
			continue
		}
		if read > 0 {
			// The batch made progress before failing; resume after the last row
			// that was copied successfully.
			if lastRowID == math.MaxInt64 {
				return result, nil
			}
			cursor = lastRowID + 1
			stride, probes = 0, 0
			continue
		}
		if batch > 1 {
			// Narrow the read down to the single damaged row so the rest of the
			// batch is not written off with it.
			batch = 1
			continue
		}
		if next, found, nextErr := nextRowID(ctx, source, table, cursor); nextErr == nil {
			if !found {
				return result, nil
			}
			if next == math.MaxInt64 {
				result.Skipped++
				return result, nil
			}
			cursor = next + 1
			result.Skipped++
			batch = salvageBatchRows
			stride, probes = 0, 0
			continue
		}
		// Neither the row nor the index entry after it can be read, so the
		// damage covers interior pages too. Step over the damaged range in
		// growing jumps; rows inside the jump are lost, but everything after it
		// is still recoverable.
		if probes >= salvageMaxProbes {
			result.Failure = readErr.Error()
			return result, nil
		}
		probes++
		if stride == 0 {
			stride = 1
		} else if stride < salvageMaxProbeStride {
			stride *= 2
		}
		if math.MaxInt64-stride < cursor {
			result.Failure = readErr.Error()
			return result, nil
		}
		cursor += stride
		result.Gaps++
		batch = salvageBatchRows
	}
}

// copyBatch reads and inserts up to limit rows starting at cursor. It reports
// the number of rows copied and the last rowid copied, so a caller that hits a
// damaged page can resume after the rows that did survive. Read errors describe
// the corrupt source and are recoverable; write errors are not, and abort the
// whole salvage.
func copyBatch(ctx context.Context, source *sql.DB, writer *salvageWriter, query, insert string, columns []string, cursor int64, limit int, result *TableSalvage) (copied int, lastRowID int64, readErr, writeErr error) {
	rows, err := source.QueryContext(ctx, query, cursor, limit)
	if err != nil {
		return 0, cursor, err, nil
	}
	defer rows.Close()
	lastRowID = cursor
	for rows.Next() {
		var rowID int64
		values := make([]any, len(columns))
		pointers := append([]any{&rowID}, scanTargets(values)...)
		if err := rows.Scan(pointers...); err != nil {
			return copied, lastRowID, err, nil
		}
		if err := insertRow(ctx, writer, insert, values, result); err != nil {
			return copied, lastRowID, nil, err
		}
		lastRowID = rowID
		copied++
	}
	if err := rows.Err(); err != nil {
		return copied, lastRowID, err, nil
	}
	return copied, lastRowID, nil, nil
}

func insertRow(ctx context.Context, writer *salvageWriter, insert string, values []any, result *TableSalvage) error {
	outcome, err := writer.execPrepared(ctx, insert, values...)
	if err != nil {
		return err
	}
	affected, err := outcome.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		// A salvaged row can collide with an earlier one when a unique index
		// page was damaged; the first copy wins.
		result.Dropped++
		return nil
	}
	result.Copied++
	return nil
}

// repairForeignKeys deletes rows whose parent row was lost, so the recovered
// database satisfies the constraints the schema declares.
func repairForeignKeys(ctx context.Context, conn *sql.Conn) (int64, error) {
	var dropped int64
	// The extra iteration is check-only: without it a final pass that removed
	// the last violations would still report non-convergence, and the caller
	// would discard a database that is in fact repaired.
	for pass := 0; pass <= salvageForeignKeyPasses; pass++ {
		type violation struct {
			table string
			rowID int64
		}
		rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
		if err != nil {
			return dropped, err
		}
		var violations []violation
		// One row can violate several foreign keys at once and is then reported
		// once per key. Deleting it repeatedly would inflate the loss the report
		// shows the operator.
		seen := map[violation]bool{}
		for rows.Next() {
			var table string
			var rowID sql.NullInt64
			var parent string
			var fkID int64
			if err := rows.Scan(&table, &rowID, &parent, &fkID); err != nil {
				rows.Close()
				return dropped, err
			}
			if !rowID.Valid {
				continue
			}
			item := violation{table: table, rowID: rowID.Int64}
			if seen[item] {
				continue
			}
			seen[item] = true
			violations = append(violations, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return dropped, err
		}
		rows.Close()
		if len(violations) == 0 {
			return dropped, nil
		}
		if pass == salvageForeignKeyPasses {
			break
		}
		for _, item := range violations {
			if _, err := conn.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE rowid = ?`, quoteIdentifier(item.table)), item.rowID); err != nil {
				return dropped, err
			}
			dropped++
		}
	}
	return dropped, fmt.Errorf("foreign key repair did not converge after %d passes", salvageForeignKeyPasses)
}

// copySequenceCounters keeps AUTOINCREMENT counters ahead of the highest
// salvaged row so recovered identifiers are never handed out twice, even when
// the tail of a table was lost.
func copySequenceCounters(ctx context.Context, source *sql.DB, writer *salvageWriter, tables []string) {
	rows, err := source.QueryContext(ctx, `SELECT name, seq FROM sqlite_sequence`)
	if err != nil {
		return
	}
	defer rows.Close()
	type counter struct {
		name string
		seq  int64
	}
	var counters []counter
	for rows.Next() {
		var item counter
		if err := rows.Scan(&item.name, &item.seq); err != nil {
			return
		}
		if slices.Contains(tables, item.name) {
			counters = append(counters, item)
		}
	}
	if err := rows.Err(); err != nil {
		return
	}
	for _, item := range counters {
		// A table whose rows were all lost has no counter row in the recovered
		// database yet, and must still not hand out an identifier that the lost
		// rows already used.
		_, _ = writer.exec(ctx, `INSERT INTO sqlite_sequence (name, seq)
			SELECT ?, ? WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = ?)`, item.name, item.seq, item.name)
		_, _ = writer.exec(ctx, `UPDATE sqlite_sequence SET seq = ? WHERE name = ? AND seq < ?`, item.seq, item.name, item.seq)
	}
}

// migrationBookkeepingTables record which schema and plugin migrations ran.
// The recovered database applies its own migrations, so its own rows are
// authoritative and copying the corrupt file's rows would only risk reviving
// stale checksums.
var migrationBookkeepingTables = map[string]bool{
	"schema_migrations": true,
	"plugin_migrations": true,
}

func listUserTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if migrationBookkeepingTables[name] {
			continue
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

// commonColumns returns the destination column order restricted to columns the
// corrupt database also has. Columns added by later migrations keep their
// schema default in the recovered file.
func commonColumns(ctx context.Context, source, dest *sql.DB, table string) ([]string, error) {
	sourceColumns, err := tableColumns(ctx, source, table)
	if err != nil {
		return nil, err
	}
	destColumns, err := tableColumns(ctx, dest, table)
	if err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(sourceColumns))
	for _, column := range sourceColumns {
		have[column] = true
	}
	columns := make([]string, 0, len(destColumns))
	for _, column := range destColumns {
		if have[column] {
			columns = append(columns, column)
		}
	}
	return columns, nil
}

func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, quoteIdentifier(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid int64
		var name, columnType string
		var notNull int64
		var defaultValue any
		var primaryKey int64
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

// tableHasRowID reports whether the table can be scanned with a rowid cursor.
// WITHOUT ROWID tables have no such cursor, so they cannot skip damaged rows.
func tableHasRowID(ctx context.Context, db *sql.DB, table string) bool {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT rowid FROM %s LIMIT 1`, quoteIdentifier(table)))
	if err != nil {
		// A corrupt page is not an answer about the schema; assume a rowid
		// table so the caller keeps its skip-and-resume behavior.
		return !strings.Contains(err.Error(), "no such column")
	}
	defer rows.Close()
	return true
}

// tableMaxRowID reads the highest rowid so a scan that has to probe blindly
// past damaged pages knows where the table ends. Damage can hide this answer
// too, in which case the probe budget bounds the scan instead.
func tableMaxRowID(ctx context.Context, db *sql.DB, table string) (int64, bool) {
	var rowID sql.NullInt64
	if err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT max(rowid) FROM %s`, quoteIdentifier(table))).Scan(&rowID); err != nil {
		return 0, false
	}
	if !rowID.Valid {
		return 0, false
	}
	return rowID.Int64, true
}

func nextRowID(ctx context.Context, db *sql.DB, table string, cursor int64) (int64, bool, error) {
	var rowID sql.NullInt64
	err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT min(rowid) FROM %s WHERE rowid >= ?`, quoteIdentifier(table)), cursor).Scan(&rowID)
	if err != nil {
		return 0, false, err
	}
	if !rowID.Valid {
		return 0, false, nil
	}
	return rowID.Int64, true, nil
}

func selectStatement(table string, columns []string, withRowID bool) string {
	quoted := make([]string, 0, len(columns)+1)
	if withRowID {
		quoted = append(quoted, "rowid")
	}
	for _, column := range columns {
		quoted = append(quoted, quoteIdentifier(column))
	}
	if !withRowID {
		return fmt.Sprintf(`SELECT %s FROM %s`, strings.Join(quoted, ", "), quoteIdentifier(table))
	}
	return fmt.Sprintf(`SELECT %s FROM %s WHERE rowid >= ? ORDER BY rowid LIMIT ?`, strings.Join(quoted, ", "), quoteIdentifier(table))
}

func insertStatement(table string, columns []string) string {
	quoted := make([]string, 0, len(columns))
	placeholders := make([]string, 0, len(columns))
	for _, column := range columns {
		quoted = append(quoted, quoteIdentifier(column))
		placeholders = append(placeholders, "?")
	}
	return fmt.Sprintf(`INSERT OR IGNORE INTO %s (%s) VALUES (%s)`, quoteIdentifier(table), strings.Join(quoted, ", "), strings.Join(placeholders, ", "))
}

func scanTargets(values []any) []any {
	pointers := make([]any, len(values))
	for i := range values {
		pointers[i] = &values[i]
	}
	return pointers
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func reportProgress(progress func(string), message string) {
	if progress != nil {
		progress(message)
	}
}
