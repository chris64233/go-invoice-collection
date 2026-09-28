package goinvoicecollection

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// SQLite 主错误码（modernc.org/sqlite 与 C 版 SQLite 一致，取 Code()&0xff）。
const (
	sqliteErrConstraint = 19
	sqliteErrBusy       = 5
	sqliteErrLocked     = 6
)

// queryer 是只读执行抽象，*sql.DB、*sql.Conn 都实现它。
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// execer 在 queryer 之上增加写能力。
type execer interface {
	queryer
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// writeTx 是一条通过 BEGIN IMMEDIATE 开启的写事务，固定占用一个底层连接。
type writeTx struct {
	conn *sql.Conn
	done bool
}

// Store 是基于 SQLite 的持久化存储。多个 Store（甚至多个进程）打开同一文件
// 也能通过数据库事务与触发器保持账务一致；正确性不依赖任何进程内锁。
type Store struct {
	db *sql.DB
}

// OpenFile 打开（不存在则创建）文件型 SQLite 数据库并初始化表结构。
// busyTimeoutMS 让写锁竞争时等待而不是立即返回 SQLITE_BUSY。
func OpenFile(ctx context.Context, path string, busyTimeoutMS int) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)",
		path, busyTimeoutMS)
	return openStore(ctx, dsn, 1)
}

// OpenMemory 打开一个可被池内多个连接共享的内存数据库，主要用于测试。
// name 相同的调用共享同一份数据；不同 name 相互隔离。
func OpenMemory(ctx context.Context, name string) (*Store, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: memory db name is empty", ErrInvalidInput)
	}
	dsn := fmt.Sprintf(
		"file:%s?mode=memory&cache=shared&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)",
		name)
	s, err := openStore(ctx, dsn, 8)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func openStore(ctx context.Context, dsn string, maxConns int) (*Store, error) {
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(maxConns)
	if err := conn.PingContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, schemaSQL); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("init schema: %w", mapDBError(err))
	}
	return &Store{db: conn}, nil
}

// Close 关闭底层连接池。
func (s *Store) Close() error { return s.db.Close() }

// QueryContext 透传只读查询（*Store 因而实现 queryer/execer）。
func (s *Store) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, query, args...)
}

// QueryRowContext 透传单行查询。
func (s *Store) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, query, args...)
}

// beginTx 以 BEGIN IMMEDIATE 立即获取 RESERVED 写锁，使所有写事务在全库范围
// 串行执行。事务内读到的发票已付金额一定是已提交的最新值，没有读后写竞态；
// 跨进程时由 SQLite 文件锁提供同样保证。
func (s *Store) beginTx(ctx context.Context) (*writeTx, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, mapDBError(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, mapDBError(err)
	}
	return &writeTx{conn: conn}, nil
}

func (t *writeTx) commit(ctx context.Context) error {
	if t.done {
		return nil
	}
	_, err := t.conn.ExecContext(ctx, "COMMIT")
	t.done = true
	_ = t.conn.Close()
	return mapDBError(err)
}

// ExecContext 在事务连接上执行写语句。
func (t *writeTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.conn.ExecContext(ctx, query, args...)
}

// QueryContext 在事务连接上执行查询。
func (t *writeTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.conn.QueryContext(ctx, query, args...)
}

// QueryRowContext 在事务连接上执行单行查询。
func (t *writeTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.conn.QueryRowContext(ctx, query, args...)
}

// rollback 在 defer 中调用；提交后再调用为空操作。
func (t *writeTx) rollback(ctx context.Context) {
	if t.done {
		return
	}
	_, _ = t.conn.ExecContext(ctx, "ROLLBACK")
	t.done = true
	_ = t.conn.Close()
}

// mapDBError 把 SQLite 约束/触发器错误翻译成稳定的领域错误。
func mapDBError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	code := sqliteErrorCode(err)

	if strings.Contains(msg, "PAYMENT_LIMIT_EXCEEDED") {
		return fmt.Errorf("%w: %v", ErrExceedsPayment, err)
	}
	if strings.Contains(msg, "INVOICE_LIMIT_EXCEEDED") {
		return fmt.Errorf("%w: %v", ErrExceedsInvoice, err)
	}
	if strings.Contains(msg, "ALLOCATION_ALREADY_REVERSED") {
		return fmt.Errorf("%w: %v", ErrAlreadyReversed, err)
	}
	if strings.Contains(msg, "REVERSAL_EXCEEDS_PAYMENT") {
		return fmt.Errorf("%w: %v", ErrReversalExceeds, err)
	}
	if strings.Contains(msg, "REVERSAL_WRONG_PAYMENT") {
		return fmt.Errorf("%w: reversal item belongs to another payment", ErrInvalidInput)
	}
	if code == sqliteErrConstraint {
		switch {
		case strings.Contains(msg, "UNIQUE"):
			return fmt.Errorf("%w: unique constraint: %v", ErrConflict, err)
		case strings.Contains(msg, "FOREIGN KEY"):
			return fmt.Errorf("%w: foreign key: %v", ErrNotFound, err)
		default:
			return fmt.Errorf("%w: constraint: %v", ErrInvalidInput, err)
		}
	}
	if code == sqliteErrBusy || code == sqliteErrLocked {
		// busy_timeout 正常情况下会等到锁释放；仍失败说明竞争超时。
		return fmt.Errorf("database busy: %w", err)
	}
	return err
}

// sqliteErrorCode 尽量从驱动错误取 SQLite 主错误码；取不到返回 -1，
// 此时调用方退化为错误信息字符串匹配。不直接 import 驱动错误类型以减少耦合。
func sqliteErrorCode(err error) int {
	type codeErr interface{ Code() int }
	var ce codeErr
	if errors.As(err, &ce) {
		return ce.Code() & 0xff
	}
	return -1
}
