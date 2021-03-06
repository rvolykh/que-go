package que

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx"
	"github.com/jackc/pgx/pgtype"
	"github.com/satori/go.uuid"
)

// Job is a single unit of work for Que to perform.
type Job struct {
	// ID is the unique database ID of the Job. It is ignored on job creation.
	ID int64

	// Queue is the name of the queue. It defaults to the empty queue "".
	Queue string

	// Priority is the priority of the Job. The default priority is 100, and a
	// lower number means a higher priority. A priority of 5 would be very
	// important.
	Priority int16

	// RunAt is the time that this job should be executed. It defaults to now(),
	// meaning the job will execute immediately. Set it to a value in the future
	// to delay a job's execution.
	RunAt time.Time

	// Type corresponds to the Ruby job_class. If you are interoperating with
	// Ruby, you should pick suitable Ruby class names (such as MyJob).
	Type string

	// Args must be the bytes of a valid JSON string
	Args []byte

	// ErrorCount is the number of times this job has attempted to run, but
	// failed with an error. It is ignored on job creation.
	ErrorCount int32

	// LastError is the error message or stack trace from the last time the job
	// failed. It is ignored on job creation.
	LastError pgtype.Text

	// Callback is channel name to which notification will be posted when job
	// is finished. Overridden on job creation.
	Callback string

	// Correlation ID
	Cid string

	delayFunction func(int32) int

	mu      sync.Mutex
	deleted bool
	pool    *pgx.ConnPool
	conn    *pgx.Conn

}

// DelayFunction returns the amount of seconds to wait as a function of
// the number of retries.
var DelayFunction func(int32) int
var defaultDelayFunction = func(errorCount int32) int {
	return intPow(int(errorCount), 4) + 3
}

// Conn returns the pgx connection that this job is locked to. You may initiate
// transactions on this connection or use it as you please until you call
// Done(). At that point, this conn will be returned to the pool and it is
// unsafe to keep using it. This function will return nil if the Job's
// connection has already been released with Done().
func (j *Job) Conn() *pgx.Conn {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.conn
}

// Delete marks this job as complete by deleting it form the database.
//
// You must also later call Done() to return this job's database connection to
// the pool.
func (j *Job) Delete() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.deleted {
		return nil
	}

	_, err := j.conn.Exec("que_destroy_job", j.Queue, j.Priority, j.RunAt, j.ID)
	if err != nil {
		return err
	}

	j.deleted = true
	return nil
}

// Done releases the Postgres advisory lock on the job and returns the database
// connection to the pool.
func (j *Job) Done() {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.conn == nil || j.pool == nil {
		// already marked as done
		return
	}

	var ok bool
	// Swallow this error because we don't want an unlock failure to cause work to
	// stop.
	_ = j.conn.QueryRow("que_unlock_job", j.ID).Scan(&ok)

	j.pool.Release(j.conn)
	j.pool = nil
	j.conn = nil
}

// Error marks the job as failed and schedules it to be reworked. An error
// message or backtrace can be provided as msg, which will be saved on the job.
// It will also increase the error count.
//
// You must also later call Done() to return this job's database connection to
// the pool.
func (j *Job) Error(msg string) error {
	errorCount := j.ErrorCount + 1
	var delay int
	if j.delayFunction == nil {
		delay = defaultDelayFunction(j.ErrorCount)
	} else {
		delay = j.delayFunction(j.ErrorCount)
	}

	_, err := j.conn.Exec("que_set_error", errorCount, delay, msg, j.Queue, j.Priority, j.RunAt, j.ID)
	if err != nil {
		return err
	}
	return nil
}

// Client is a Que client that can add jobs to the queue and remove jobs from
// the queue.
type Client struct {
	pool *pgx.ConnPool

	// TODO: add a way to specify default queueing options
}

// NewClient creates a new Client that uses the pgx pool.
func NewClient(pool *pgx.ConnPool) *Client {
	return &Client{pool: pool}
}

// ErrMissingType is returned when you attempt to enqueue a job with no Type
// specified.
var ErrMissingType = errors.New("job type must be specified")

// Enqueue adds a job to the queue.
func (c *Client) Enqueue(j *Job) error {
	return execEnqueue(j, c.pool)
}

// EnqueueInTx adds a job to the queue within the scope of the transaction tx.
// This allows you to guarantee that an enqueued job will either be committed or
// rolled back atomically with other changes in the course of this transaction.
//
// It is the caller's responsibility to Commit or Rollback the transaction after
// this function is called.
func (c *Client) EnqueueInTx(j *Job, tx *pgx.Tx) error {
	return execEnqueue(j, tx)
}

// EnqueueAndWait does the same as Enqueue, but additionally listen for
// callback notification from PostgreSQL. It is processor implementation
// responsibility to send notification to callback channel.
func (c *Client) EnqueueAndWait(ctx context.Context, j *Job) (string, error) {
	conn, err := c.pool.Acquire()
	if err != nil {
		return "", err
	}
	defer c.pool.Release(conn)

	callback := uuid.NewV4().String()

	err = conn.Listen(callback)
	if err != nil {
		return "", err
	}
	defer conn.Unlisten(callback)

	j.Callback = callback

	err = execEnqueue(j, c.pool)
	if err != nil {
		return "", err
	}

	event, err := conn.WaitForNotification(ctx)
	if err != nil {
		return "", err
	}

	if event.Channel != callback {
		return "", fmt.Errorf("unexpected notification from a channel %s", event.Channel)
	}

	return event.Payload, nil
}

func execEnqueue(j *Job, q queryable) error {
	if j.Type == "" {
		return ErrMissingType
	}

	queue := &pgtype.Text{
		String: j.Queue,
		Status: pgtype.Null,
	}
	if j.Queue != "" {
		queue.Status = pgtype.Present
	}

	priority := &pgtype.Int2{
		Int:    j.Priority,
		Status: pgtype.Null,
	}
	if j.Priority != 0 {
		priority.Status = pgtype.Present
	}

	runAt := &pgtype.Timestamptz{
		Time:   j.RunAt,
		Status: pgtype.Null,
	}
	if !j.RunAt.IsZero() {
		runAt.Status = pgtype.Present
	}

	args := &pgtype.Bytea{
		Bytes:  j.Args,
		Status: pgtype.Null,
	}
	if len(j.Args) != 0 {
		args.Status = pgtype.Present
	}

	_, err := q.Exec("que_insert_job", queue, priority, runAt, j.Type, args, j.Callback, j.Cid)
	return err
}

type queryable interface {
	Exec(sql string, arguments ...interface{}) (commandTag pgx.CommandTag, err error)
	Query(sql string, args ...interface{}) (*pgx.Rows, error)
	QueryRow(sql string, args ...interface{}) *pgx.Row
}

// Maximum number of loop iterations in LockJob before giving up.  This is to
// avoid looping forever in case something is wrong.
const maxLockJobAttempts = 10

// Returned by LockJob if a job could not be retrieved from the queue after
// several attempts because of concurrently running transactions.  This error
// should not be returned unless the queue is under extremely heavy
// concurrency.
var ErrAgain = errors.New("maximum number of LockJob attempts reached")

// TODO: consider an alternate Enqueue func that also returns the newly
// enqueued Job struct. The query sqlInsertJobAndReturn was already written for
// this.

// LockJob attempts to retrieve a Job from the database in the specified queue.
// If a job is found, a session-level Postgres advisory lock is created for the
// Job's ID. If no job is found, nil will be returned instead of an error.
//
// Because Que uses session-level advisory locks, we have to hold the
// same connection throughout the process of getting a job, working it,
// deleting it, and removing the lock.
//
// After the Job has been worked, you must call either Done() or Error() on it
// in order to return the database connection to the pool and remove the lock.
func (c *Client) LockJob(queue string) (*Job, error) {
	conn, err := c.pool.Acquire()
	if err != nil {
		return nil, err
	}

	j := Job{pool: c.pool, conn: conn, delayFunction: DelayFunction}

	for i := 0; i < maxLockJobAttempts; i++ {
		err = conn.QueryRow("que_lock_job", queue).Scan(
			&j.Queue,
			&j.Priority,
			&j.RunAt,
			&j.ID,
			&j.Type,
			&j.Args,
			&j.ErrorCount,
			&j.Callback,
			&j.Cid,
		)
		if err != nil {
			c.pool.Release(conn)
			if err == pgx.ErrNoRows {
				return nil, nil
			}
			return nil, err
		}

		// Deal with race condition. Explanation from the Ruby Que gem:
		//
		// Edge case: It's possible for the lock_job query to have
		// grabbed a job that's already been worked, if it took its MVCC
		// snapshot while the job was processing, but didn't attempt the
		// advisory lock until it was finished. Since we have the lock, a
		// previous worker would have deleted it by now, so we just
		// double check that it still exists before working it.
		//
		// Note that there is currently no spec for this behavior, since
		// I'm not sure how to reliably commit a transaction that deletes
		// the job in a separate thread between lock_job and check_job.
		var ok bool
		err = conn.QueryRow("que_check_job", j.Queue, j.Priority, j.RunAt, j.ID).Scan(&ok)
		if err == nil {
			return &j, nil
		} else if err == pgx.ErrNoRows {
			// Encountered job race condition; start over from the beginning.
			// We're still holding the advisory lock, though, so we need to
			// release it before resuming.  Otherwise we leak the lock,
			// eventually causing the server to run out of locks.
			//
			// Also swallow the possible error, exactly like in Done.
			_ = conn.QueryRow("que_unlock_job", j.ID).Scan(&ok)
			continue
		} else {
			c.pool.Release(conn)
			return nil, err
		}
	}
	c.pool.Release(conn)
	return nil, ErrAgain
}

var preparedStatements = map[string]string{
	"que_check_job":   sqlCheckJob,
	"que_destroy_job": sqlDeleteJob,
	"que_insert_job":  sqlInsertJob,
	"que_lock_job":    sqlLockJob,
	"que_set_error":   sqlSetError,
	"que_unlock_job":  sqlUnlockJob,
}

// PrepareStatements prepares the required statements to run que on the provided
// *pgx.Conn. Typically it is used as an AfterConnect func for a
// *pgx.ConnPool. Every connection used by que must have the statements prepared
// ahead of time.
func PrepareStatements(conn *pgx.Conn) error {
	return PrepareStatementsWithPreparer(conn)
}

// Preparer defines the interface for types that support preparing
// statements. This includes all of *pgx.ConnPool, *pgx.Conn, and *pgx.Tx
type Preparer interface {
	Prepare(name, sql string) (*pgx.PreparedStatement, error)
}

// PrepareStatementsWithPreparer prepares the required statements to run que on
// the provided Preparer. This func can be used to prepare statements on a
// *pgx.ConnPool after it is created, or on a *pg.Tx. Every connection used by
// que must have the statements prepared ahead of time.
func PrepareStatementsWithPreparer(p Preparer) error {
	for name, sql := range preparedStatements {
		if _, err := p.Prepare(name, sql); err != nil {
			return err
		}
	}
	return nil
}
