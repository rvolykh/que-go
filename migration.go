package que

import (
	"github.com/jackc/pgx"
)

const sqlCreateQueTable = `
	CREATE TABLE IF NOT EXISTS "que_jobs"
	(
	  priority    smallint    NOT NULL DEFAULT 100,
	  run_at      timestamptz NOT NULL DEFAULT now(),
	  job_id      bigserial   NOT NULL,
	  job_class   text        NOT NULL,
	  args        json        NOT NULL DEFAULT '[]'::json,
	  error_count integer     NOT NULL DEFAULT 0,
	  last_error  text,
	  queue       text        NOT NULL DEFAULT '',
	  callback    text        NOT NULL DEFAULT '',
	  cid         text        NOT NULL DEFAULT '',

	  CONSTRAINT que_jobs_pkey PRIMARY KEY (queue, priority, run_at, job_id)
	);

	COMMENT ON TABLE que_jobs IS '3';
`

func Migrate(config pgx.ConnConfig) error {
	c, err := pgx.Connect(config)
	if err != nil {
		return err
	}
	defer c.Close()

	_, err = c.Exec(sqlCreateQueTable)
	if err != nil {
		return err
	}

	return nil
}
