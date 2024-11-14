package eventstore

import (
	"context"
	"database/sql"
	_ "embed"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/zitadel/logging"

	"github.com/zitadel/zitadel/internal/eventstore"
	"github.com/zitadel/zitadel/internal/telemetry/tracing"
)

func (es *Eventstore) Push(ctx context.Context, commands ...eventstore.Command) (events []eventstore.Event, err error) {
	ctx, span := tracing.NewSpan(ctx)
	defer func() { span.EndWithError(err) }()

	events, err = es.writeEvents(ctx, commands)
	if isSetupNotExecutedError(err) {
		return es.pushWithoutFunc(ctx, commands...)
	}

	return events, err
}

var (
	//go:embed push.sql
	pushStmt string
	//go:embed push2.sql
	push2Stmt string
)

func (es *Eventstore) writeEvents(ctx context.Context, commands []eventstore.Command) (_ []eventstore.Event, err error) {
	conn, err := es.client.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err = checkExecutionPlan(ctx, conn); err != nil {
		return nil, err
	}

	tx, err := conn.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelReadCommitted,
		ReadOnly:  false,
	})
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			_ = tx.Rollback()
			return
		}
		err = tx.Commit()
		if err != nil {
			logging.WithError(err).Warn("failed to commit transaction")
		}
	}()

	events, err := writeEvents(ctx, tx, commands)
	if err != nil {
		return nil, err
	}

	if err = handleUniqueConstraints(ctx, tx, commands); err != nil {
		return nil, err
	}

	// CockroachDB by default does not allow multiple modifications of the same table using ON CONFLICT
	// Thats why we enable it manually
	if es.client.Type() == "cockroach" {
		_, err = tx.Exec("SET enable_multiple_modifications_of_table = on")
		if err != nil {
			return nil, err
		}
	}

	err = handleFieldCommands(ctx, tx, commands)
	if err != nil {
		return nil, err
	}

	return events, nil
}

func writeEvents(ctx context.Context, tx *sql.Tx, commands []eventstore.Command) (_ []eventstore.Event, err error) {
	ctx, span := tracing.NewSpan(ctx)
	defer func() { span.EndWithError(err) }()

	events, cmds, err := commandsToEvents(ctx, commands)
	if err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, push2Stmt, cmds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for i := 0; rows.Next(); i++ {
		err = rows.Scan(&events[i].(*event).createdAt, &events[i].(*event).sequence, &events[i].(*event).position)
		if err != nil {
			logging.WithError(err).Warn("failed to scan events")
			return nil, err
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func checkExecutionPlan(ctx context.Context, conn *sql.Conn) error {
	return conn.Raw(func(driverConn any) error {
		conn := driverConn.(*stdlib.Conn).Conn()
		var cmd *command
		if _, ok := conn.TypeMap().TypeForValue(cmd); ok {
			return nil
		}
		return registerEventstoreTypes(ctx, conn)
	})
}
