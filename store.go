package goinvoicecollection

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"math/big"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store 是基于 SQLite 的持久化层。所有写操作都在 BEGIN IMMEDIATE
// 事务中完成：写事务在开始时即获取数据库级保留锁，天然串行化，
// 配合条件 UPDATE（WHERE paid_amount + ? <= amount）保证跨进程、
// 跨连接并发时同一发票余额不会被重复结清，无需任何进程内锁。
type Store struct {
	db *sql.DB
}

// OpenFile 打开（不存在则创建）一个基于文件的数据库并完成建表。
func OpenFile(ctx context.Context, path string) (*Store, error) {
	// _txlock=immediate 使驱动在 BEGIN 时直接取写锁；
	// _pragma 对连接池中每个连接生效。
	dsn := "file:" + path +
		"?_txlock=immediate" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
	return openDSN(ctx, dsn)
}

// OpenInMemory 创建一个进程内共享缓存的内存数据库，主要用于测试。
func OpenInMemory(ctx context.Context) (*Store, error) {
	name, err := randomID("mem")
	if err != nil {
		return nil, err
	}
	dsn := "file:" + name +
		"?mode=memory&cache=shared" +
		"&_txlock=immediate" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)"
	return openDSN(ctx, dsn)
}

// OpenDB 使用已建立的 *sql.DB（驱动须为 modernc.org/sqlite）创建 Store。
// 调用方负责驱动注册与连接参数；本函数仍会执行建表语句。
func OpenDB(ctx context.Context, db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func openDSN(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, wrapError(KindStorage, "open sqlite", err)
	}
	// SQLite 单写者，连接数不必无限放大；保留少量并发读连接。
	db.SetMaxOpenConns(8)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭底层连接池。
func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层 *sql.DB，便于调用方自行做只读查询或健康检查。
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return wrapError(KindStorage, "apply schema", err)
	}
	return nil
}

// txFunc 是事务体；返回 error 时回滚，nil 时提交。
type txFunc func(tx *sql.Tx) error

// runTx 运行一个立即写事务，并在遇到 SQLITE_BUSY / SQLITE_LOCKED 时
// 做有限次指数退避重试（配合 busy_timeout，双保险）。
func (s *Store) runTx(ctx context.Context, name string, fn txFunc) (err error) {
	const maxAttempts = 8
	var attempt int
	for {
		attempt++
		err = s.onceTx(ctx, fn)
		if err == nil || !isBusyLockError(err) || attempt >= maxAttempts {
			return err
		}
		// 退避：约 4ms * 2^(attempt-1) + 抖动，封顶 ~250ms
		base := time.Duration(4) * time.Millisecond * time.Duration(1<<(attempt-1))
		if base > 250*time.Millisecond {
			base = 250 * time.Millisecond
		}
		jitter, _ := rand.Int(rand.Reader, big.NewInt(int64(base/2+1)))
		timer := time.NewTimer(base/2 + time.Duration(jitter.Int64()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) onceTx(ctx context.Context, fn txFunc) error {
	tx, err := s.db.BeginTx(ctx, nil) // DSN 已配置 _txlock=immediate
	if err != nil {
		return wrapError(KindStorage, "begin tx", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return wrapError(KindStorage, "commit tx", err)
	}
	committed = true
	return nil
}

// isBusyLockError 判断是否为可重试的 SQLITE_BUSY(5) / SQLITE_LOCKED(6)。
func isBusyLockError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "SQLITE_LOCKED") ||
		strings.Contains(msg, "database table is locked")
}

// isUniqueViolation 判断是否为唯一约束冲突（SQLITE_CONSTRAINT_UNIQUE=2067）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "SQLITE_CONSTRAINT_UNIQUE")
}

func randomID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", wrapError(KindStorage, "generate id", err)
	}
	return prefix + "_" + hex.EncodeToString(b[:]), nil
}

// ---- 时间与字符串的编解码 ----

func nowUTC() time.Time { return time.Now().UTC() }

// encodeDate 只用日期部分（YYYY-MM-DD），发票到期日按日比较。
func encodeDate(t time.Time) string { return t.UTC().Format("2006-01-02") }

func parseDate(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}

// encodeTimestamp 保留纳秒的 RFC3339 UTC。
func encodeTimestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTimestamp(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// nullString / 读取 sql.NullString 的小工具。
func nullableString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
