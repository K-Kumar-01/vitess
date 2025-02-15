/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package worker

import (
	"io"
	"strings"
	"time"

	"vitess.io/vitess/go/vt/vterrors"

	"context"

	"vitess.io/vitess/go/sqlescape"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/grpcclient"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/logutil"
	"vitess.io/vitess/go/vt/topo/topoproto"
	"vitess.io/vitess/go/vt/vttablet/queryservice"
	"vitess.io/vitess/go/vt/vttablet/tabletconn"

	querypb "vitess.io/vitess/go/vt/proto/query"
	tabletmanagerdatapb "vitess.io/vitess/go/vt/proto/tabletmanagerdata"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
)

// RestartableResultReader will stream all rows within a chunk.
// If the streaming query gets interrupted, it can resume the stream after
// the last row which was read.
type RestartableResultReader struct {
	ctx    context.Context
	logger logutil.Logger
	tp     tabletProvider
	// td is used to get the list of primary key columns at a restart.
	td    *tabletmanagerdatapb.TableDefinition
	chunk chunk
	// allowMultipleRetries is true if we are allowed to retry more than once.
	allowMultipleRetries bool
	// if we are running inside a transaction, this will hold a non-zero value
	txID int64

	query string

	tablet *topodatapb.Tablet
	conn   queryservice.QueryService
	fields []*querypb.Field
	output sqltypes.ResultStream

	// lastRow is used during a restart to determine after which row the restart
	// should start.
	lastRow []sqltypes.Value
}

// NewRestartableResultReader creates a new RestartableResultReader for
// the provided tablet and chunk.
// It will automatically create the necessary query to read all rows within
// the chunk.
// NOTE: We assume that the Columns field in "td" was ordered by a preceding
// call to reorderColumnsPrimaryKeyFirst().
func NewRestartableResultReader(ctx context.Context, logger logutil.Logger, tp tabletProvider, td *tabletmanagerdatapb.TableDefinition, chunk chunk, allowMultipleRetries bool) (*RestartableResultReader, error) {
	r := &RestartableResultReader{
		ctx:                  ctx,
		logger:               logger,
		tp:                   tp,
		td:                   td,
		chunk:                chunk,
		allowMultipleRetries: allowMultipleRetries,
	}

	err := tryToConnect(r)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func tryToConnect(r *RestartableResultReader) error {

	// If the initial connection fails we retry once.
	// Note: The first retry will be the second attempt.
	attempt := 0
	for {
		attempt++
		var err error
		var retryable bool
		if retryable, err = r.getTablet(); err != nil {
			err = vterrors.Wrap(err, "tablet=unknown")
			goto retry
		}
		if retryable, err = r.startStream(); err != nil {
			err = vterrors.Wrapf(err, "tablet=%v", topoproto.TabletAliasString(r.tablet.Alias))
			goto retry
		}
		return nil

	retry:
		if !retryable || attempt > 1 {
			return vterrors.Wrapf(err, "failed to initialize tablet connection: retryable %v", retryable)
		}
		statsRetryCount.Add(1)
		log.Infof("retrying after error: %v", err)
	}

}

// NewTransactionalRestartableResultReader does the same thing that NewRestartableResultReader does,
// but works inside of a single transaction
func NewTransactionalRestartableResultReader(ctx context.Context, logger logutil.Logger, tp tabletProvider, td *tabletmanagerdatapb.TableDefinition, chunk chunk, allowMultipleRetries bool, txID int64) (*RestartableResultReader, error) {
	r := &RestartableResultReader{
		ctx:                  ctx,
		logger:               logger,
		tp:                   tp,
		td:                   td,
		chunk:                chunk,
		allowMultipleRetries: allowMultipleRetries,
		txID:                 txID,
	}
	err := tryToConnect(r)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// getTablet (re)sets the tablet which is used for the streaming query.
// If the method returns an error, the first return value specifies if it is
// okay to retry.
func (r *RestartableResultReader) getTablet() (bool, error) {
	if r.tablet != nil {
		// If there was a tablet before, return it to the tabletProvider.
		r.Close(r.ctx)

		r.tablet = nil
		r.conn = nil
		r.fields = nil
		r.output = nil
	}

	// Get a tablet from the tablet provider.
	tablet, err := r.tp.getTablet()
	if err != nil {
		return true /* retryable */, vterrors.Wrap(err, "failed get tablet for streaming query")
	}

	// Connect (dial) to the tablet.
	conn, err := tabletconn.GetDialer()(tablet, grpcclient.FailFast(false))
	if err != nil {
		return false /* retryable */, vterrors.Wrap(err, "failed to get dialer for tablet")
	}
	r.tablet = tablet
	r.conn = conn
	return false /* retryable */, nil
}

// startStream assumes that getTablet() was successfully called before and now
// tries to connect to the set tablet and start the streaming query.
// If the method returns an error, the first return value specifies if it is
// okay to retry.
func (r *RestartableResultReader) startStream() (bool, error) {
	// Start the streaming query.
	r.generateQuery()
	var stream sqltypes.ResultStream

	if r.txID == 0 {
		stream = queryservice.ExecuteWithStreamer(r.ctx, r.conn, &querypb.Target{
			Keyspace:   r.tablet.Keyspace,
			Shard:      r.tablet.Shard,
			TabletType: r.tablet.Type,
		}, r.query, make(map[string]*querypb.BindVariable), nil)
	} else {
		stream = queryservice.ExecuteWithTransactionalStreamer(r.ctx, r.conn, &querypb.Target{
			Keyspace:   r.tablet.Keyspace,
			Shard:      r.tablet.Shard,
			TabletType: r.tablet.Type,
		}, r.query, make(map[string]*querypb.BindVariable), r.txID, nil)
	}

	// Read the fields information.
	cols, err := stream.Recv()
	if err != nil {
		return true /* retryable */, vterrors.Wrapf(err, "cannot read Fields for query '%v'", r.query)
	}
	r.fields = cols.Fields
	r.output = stream

	alias := topoproto.TabletAliasString(r.tablet.Alias)
	statsStreamingQueryCounters.Add(alias, 1)
	log.V(2).Infof("tablet=%v table=%v chunk=%v: Starting to stream rows using query '%v'.", alias, r.td.Name, r.chunk, r.query)
	return false, nil
}

// Next returns the next result on the stream. It implements ResultReader.
func (r *RestartableResultReader) Next() (*sqltypes.Result, error) {
	result, err := r.output.Recv()
	if err != nil && err != io.EOF {
		// We start the retries only on the second attempt to avoid the cost
		// of starting a timer (for the retry timeout) for every Next() call
		// when no error occurs.
		alias := topoproto.TabletAliasString(r.tablet.Alias)
		statsStreamingQueryErrorsCounters.Add(alias, 1)
		log.V(2).Infof("tablet=%v table=%v chunk=%v: Failed to read next rows from active streaming query. Trying to restart stream on the same tablet. Original Error: %v", alias, r.td.Name, r.chunk, err)
		result, err = r.nextWithRetries()
	}
	if result != nil && len(result.Rows) > 0 {
		r.lastRow = result.Rows[len(result.Rows)-1]
	}
	return result, err
}

func (r *RestartableResultReader) nextWithRetries() (*sqltypes.Result, error) {
	// In case of errors we will keep retrying until retryCtx is done.
	retryCtx, retryCancel := context.WithTimeout(r.ctx, *retryDuration)
	defer retryCancel()

	// The first retry is the second attempt because we already tried once in Next()
	attempt := 1
	start := time.Now()
	for {
		attempt++

		var result *sqltypes.Result
		var retryable bool
		var err error

		if attempt > 2 {
			// Do not failover to a different tablet at the 2nd attempt, but always
			// after that.
			// That's because the first restart is meant to fix transient problems
			// which go away at the next retry. For example, when MySQL killed the
			// vttablet connection due to net_write_timeout being reached.
			retryable, err = r.getTablet()
			if err != nil {
				if !retryable {
					r.logger.Errorf2(err, "table=%v chunk=%v: Failed to restart streaming query (attempt %d) and failover to a different tablet (%v) due to a non-retryable error", r.td.Name, r.chunk, attempt, r.tablet)
					return nil, err
				}
				goto retry
			}
		}

		if attempt > 1 {
			// Restart streaming query.
			retryable, err = r.startStream()
			if err != nil {
				if !retryable {
					r.logger.Errorf2(err, "tablet=%v table=%v chunk=%v: Failed to restart streaming query (attempt %d) with query '%v' and stopped due to a non-retryable error", topoproto.TabletAliasString(r.tablet.Alias), r.td.Name, r.chunk, attempt, r.query)
					return nil, err
				}
				goto retry
			}
		}

		result, err = r.output.Recv()
		if err == nil || err == io.EOF {
			alias := topoproto.TabletAliasString(r.tablet.Alias)
			log.V(2).Infof("tablet=%v table=%v chunk=%v: Successfully restarted streaming query with query '%v' after %.1f seconds.", alias, r.td.Name, r.chunk, r.query, time.Since(start).Seconds())
			if attempt == 2 {
				statsStreamingQueryRestartsSameTabletCounters.Add(alias, 1)
			} else {
				statsStreamingQueryRestartsDifferentTablet.Add(1)
			}

			// Recv() was successful.
			return result, err
		}

	retry:
		if attempt == 2 && !r.allowMultipleRetries {
			// Offline source tablets must not be retried forever. Fail early.
			return nil, vterrors.Wrapf(err, "%v: first retry to restart the streaming query on the same tablet failed. We're failing at this point because we're not allowed to keep retrying. err", r.tp.description())
		}

		alias := "unknown"
		if r.tablet != nil {
			alias = topoproto.TabletAliasString(r.tablet.Alias)
			// tablet may be nil if e.g. the HealthCheck module currently does not
			// return a tablet.
			statsStreamingQueryErrorsCounters.Add(alias, 1)
		}

		deadline, _ := retryCtx.Deadline()
		log.V(2).Infof("tablet=%v table=%v chunk=%v: Failed to restart streaming query (attempt %d) with query '%v'. Retrying to restart stream on a different tablet (for up to %.1f minutes). Next retry is in %.1f seconds. Error: %v", alias, r.td.Name, r.chunk, attempt, r.query, time.Until(deadline).Minutes(), executeFetchRetryTime.Seconds(), err)

		select {
		case <-retryCtx.Done():
			err := retryCtx.Err()
			if err == context.DeadlineExceeded {
				return nil, vterrors.Wrapf(err, "%v: failed to restart the streaming connection after retrying for %v", r.tp.description(), *retryDuration)
			}
			return nil, vterrors.Wrapf(err, "%v: interrupted while trying to restart the streaming connection (%.1f minutes elapsed so far)", r.tp.description(), time.Since(start).Minutes())
		case <-time.After(*executeFetchRetryTime):
			// Make a pause between the retries to avoid hammering the servers.
		}
	}
}

// Fields returns the field data. It implements ResultReader.
func (r *RestartableResultReader) Fields() []*querypb.Field {
	return r.fields
}

// Close closes the connection to the tablet.
func (r *RestartableResultReader) Close(ctx context.Context) {
	if r.conn != nil {
		r.conn.Close(ctx)
		r.tp.returnTablet(r.tablet)
	}
}

func (r *RestartableResultReader) generateQuery() {
	query := "SELECT " + strings.Join(escapeAll(r.td.Columns), ",") + " FROM " + sqlescape.EscapeID(r.td.Name)

	// Build WHERE clauses.
	var clauses []string

	// start value.
	if r.lastRow == nil {
		// Initial query.
		if !r.chunk.start.IsNull() {
			var b strings.Builder
			sqlescape.WriteEscapeID(&b, r.td.PrimaryKeyColumns[0])
			b.WriteString(">=")
			r.chunk.start.EncodeSQL(&b)
			clauses = append(clauses, b.String())
		}
	} else {
		// This is a restart. Read after the last row.
		// Note that we don't have to be concerned that the new start might be > end
		// because lastRow < end is always true. That's because the initial query
		// had the clause 'WHERE PrimaryKeyColumns[0] < end'.
		// TODO(mberlin): Write an e2e test to verify that restarts also work with
		// string types and MySQL collation rules.
		clauses = append(clauses, greaterThanTupleWhereClause(r.td.PrimaryKeyColumns, r.lastRow)...)
	}

	// end value.
	if !r.chunk.end.IsNull() {
		var b strings.Builder
		sqlescape.WriteEscapeID(&b, r.td.PrimaryKeyColumns[0])
		b.WriteString("<")
		r.chunk.end.EncodeSQL(&b)
		clauses = append(clauses, b.String())
	}

	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	if len(r.td.PrimaryKeyColumns) > 0 {
		query += " ORDER BY " + strings.Join(escapeAll(r.td.PrimaryKeyColumns), ",")
	}
	r.query = query
}

// greaterThanTupleWhereClause builds a greater than (">") WHERE clause
// expression for the first "columns" in "row".
// The caller has to ensure that "columns" matches with the values in "row".
// Examples:
// one column:    a > 1
// two columns:   a>=1 AND (a,b) > (1,2)
//   (Input for that would be: columns{"a", "b"}, row{1, 2}.)
// three columns: a>=1 AND (a,b,c) > (1,2,3)
//
// Note that we are using the short-form for row comparisons. This is defined
// by MySQL. See: http://dev.mysql.com/doc/refman/5.5/en/comparison-operators.html
// <quote>
//   For row comparisons, (a, b) < (x, y) is equivalent to:
//   (a < x) OR ((a = x) AND (b < y))
// </quote>
//
// NOTE: If there is more than one column, we add an extra clause for the
// first column because older MySQL versions seem to do a full table scan
// when we use the short-form. With the additional clause we skip the full
// table scan up the primary key we're interested it.
func greaterThanTupleWhereClause(columns []string, row []sqltypes.Value) []string {
	var clauses []string

	// Additional clause on the first column for multi-columns.
	if len(columns) > 1 {
		var b strings.Builder
		sqlescape.WriteEscapeID(&b, columns[0])
		b.WriteString(">=")
		row[0].EncodeSQL(&b)
		clauses = append(clauses, b.String())
	}

	var b strings.Builder
	// List of columns.
	if len(columns) > 1 {
		b.WriteByte('(')
	}
	b.WriteString(strings.Join(escapeAll(columns), ","))
	if len(columns) > 1 {
		b.WriteByte(')')
	}

	// Operator.
	b.WriteString(">")

	// List of values.
	if len(columns) > 1 {
		b.WriteByte('(')
	}
	for i := 0; i < len(columns); i++ {
		if i != 0 {
			b.WriteByte(',')
		}
		row[i].EncodeSQL(&b)
	}
	if len(columns) > 1 {
		b.WriteByte(')')
	}
	clauses = append(clauses, b.String())

	return clauses
}
