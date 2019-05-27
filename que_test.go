package que

import (
	"log"
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx"
)

var testConnConfig = pgx.ConnConfig{
	Host:     env("DB_HOST", "localhost"),
	Port:     envInt("DB_PORT", 5432),
	Database: env("DB_DATABASE", "que-go-test"),
	User:     env("DB_USER", "postgres"),
	Password: env("DB_PASSWORD", "postgres"),
}

func env(key string, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

func envInt(key string, defaultValue uint16) uint16 {
	if v := os.Getenv(key); v != "" {
		num, err := strconv.ParseUint(v, 10, 16)
		if err != nil {
			log.Fatalln(err.Error())
		}
		return uint16(num)
	}
	return defaultValue
}

func openTestClientMaxConns(t testing.TB, maxConnections int) *Client {
	err := Migrate(testConnConfig)
	if err != nil {
		t.Fatal(err)
	}

	connPoolConfig := pgx.ConnPoolConfig{
		ConnConfig:     testConnConfig,
		MaxConnections: maxConnections,
		AfterConnect:   PrepareStatements,
	}
	pool, err := pgx.NewConnPool(connPoolConfig)
	if err != nil {
		t.Fatal(err)
	}

	return NewClient(pool)
}

func openTestClient(t testing.TB) *Client {
	return openTestClientMaxConns(t, 5)
}

func truncateAndClose(pool *pgx.ConnPool) {
	if _, err := pool.Exec("TRUNCATE TABLE que_jobs"); err != nil {
		panic(err)
	}
	pool.Close()
}

func findOneJob(q queryable) (*Job, error) {
	findSQL := `
	SELECT priority, run_at, job_id, job_class, args, error_count, last_error, queue
	FROM que_jobs LIMIT 1`

	j := &Job{}
	err := q.QueryRow(findSQL).Scan(
		&j.Priority,
		&j.RunAt,
		&j.ID,
		&j.Type,
		&j.Args,
		&j.ErrorCount,
		&j.LastError,
		&j.Queue,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return j, nil
}
