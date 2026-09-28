-- go-invoice-collection schema (SQLite)。
-- 并发安全不只依赖应用层：所有写操作都在 BEGIN IMMEDIATE 事务中串行化，
-- 而以下 CHECK 约束与触发器在数据库层强制核心账务不变量，跨进程同样成立。

-- 应收发票
CREATE TABLE IF NOT EXISTS invoices (
    id         TEXT PRIMARY KEY,
    number     TEXT NOT NULL UNIQUE,
    total      INTEGER NOT NULL CHECK (total > 0),
    paid       INTEGER NOT NULL DEFAULT 0 CHECK (paid >= 0 AND paid <= total),
    due_at     TEXT NOT NULL,          -- RFC3339，决定自动分配先后
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_invoices_due ON invoices (due_at, id);

-- 收款（一次收款一行；未分配余款显式保存在 unallocated 列）
-- allocated 只随分配增加、从不因撤销而减少；reversed 单独累计，
-- 因此"已撤销的部分不能再次使用"由 reversed <= allocated 等约束在库内保证。
CREATE TABLE IF NOT EXISTS payments (
    id          TEXT PRIMARY KEY,
    external_no TEXT NOT NULL UNIQUE,  -- 外部收款号，幂等键
    amount      INTEGER NOT NULL CHECK (amount > 0),
    allocated   INTEGER NOT NULL DEFAULT 0 CHECK (allocated >= 0 AND allocated <= amount),
    reversed    INTEGER NOT NULL DEFAULT 0 CHECK (reversed >= 0 AND reversed <= allocated),
    unallocated INTEGER NOT NULL CHECK (unallocated >= 0 AND unallocated = amount - allocated),
    mode        TEXT NOT NULL CHECK (mode IN ('manual', 'auto')),
    fingerprint TEXT NOT NULL,         -- 金额 + 分配方式的规范化摘要，用于冲突判定
    created_at  TEXT NOT NULL
);

-- 收款对发票的分配。同一收款对同一发票至多一条（部分撤销通过 reversed 累计体现）
CREATE TABLE IF NOT EXISTS allocations (
    id         TEXT PRIMARY KEY,
    payment_id TEXT NOT NULL REFERENCES payments (id),
    invoice_id TEXT NOT NULL REFERENCES invoices (id),
    amount     INTEGER NOT NULL CHECK (amount > 0),
    reversed   INTEGER NOT NULL DEFAULT 0 CHECK (reversed >= 0 AND reversed <= amount),
    seq        INTEGER NOT NULL,       -- 该收款内的顺序
    created_at TEXT NOT NULL,
    UNIQUE (payment_id, invoice_id)
);
CREATE INDEX IF NOT EXISTS idx_allocations_invoice ON allocations (invoice_id);
CREATE INDEX IF NOT EXISTS idx_allocations_payment ON allocations (payment_id, seq);

-- 撤销单（一次撤销一行，撤销对象为某笔原收款）
CREATE TABLE IF NOT EXISTS reversals (
    id          TEXT PRIMARY KEY,
    reversal_no TEXT NOT NULL UNIQUE,  -- 撤销幂等键
    payment_id  TEXT NOT NULL REFERENCES payments (id),
    amount      INTEGER NOT NULL CHECK (amount > 0),
    fingerprint TEXT NOT NULL,
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reversals_payment ON reversals (payment_id);

-- 撤销逐项冲回当时的分配
CREATE TABLE IF NOT EXISTS reversal_items (
    id            TEXT PRIMARY KEY,
    reversal_id   TEXT NOT NULL REFERENCES reversals (id),
    allocation_id TEXT NOT NULL REFERENCES allocations (id),
    amount        INTEGER NOT NULL CHECK (amount > 0),
    seq           INTEGER NOT NULL,
    created_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reversal_items_allocation ON reversal_items (allocation_id);

-- 插入分配前：累计分配不得超过收款额，也不得超过发票未付余额。
-- 触发器读到的是本行之前已提交的数据，因此同一事务内插入多行时逐行累计。
CREATE TRIGGER IF NOT EXISTS trg_allocation_before_insert
BEFORE INSERT ON allocations
BEGIN
    SELECT CASE
        WHEN NEW.amount + COALESCE(
                (SELECT allocated FROM payments WHERE id = NEW.payment_id), 0)
             > (SELECT amount FROM payments WHERE id = NEW.payment_id)
        THEN RAISE(ABORT, 'PAYMENT_LIMIT_EXCEEDED')
        WHEN NEW.amount + COALESCE(
                (SELECT paid FROM invoices WHERE id = NEW.invoice_id), 0)
             > (SELECT total FROM invoices WHERE id = NEW.invoice_id)
        THEN RAISE(ABORT, 'INVOICE_LIMIT_EXCEEDED')
    END;
END;

-- 插入分配后：原子地推进发票已付金额与收款已分配金额。
CREATE TRIGGER IF NOT EXISTS trg_allocation_after_insert
AFTER INSERT ON allocations
BEGIN
    UPDATE invoices
       SET paid = paid + NEW.amount
     WHERE id = NEW.invoice_id;
    UPDATE payments
       SET allocated   = allocated + NEW.amount,
           unallocated = unallocated - NEW.amount
     WHERE id = NEW.payment_id;
END;

-- 插入冲回项前：必须冲回本收款自己的分配，且该行累计冲回不得超过当时分配额。
CREATE TRIGGER IF NOT EXISTS trg_reversal_item_before_insert
BEFORE INSERT ON reversal_items
BEGIN
    SELECT CASE
        WHEN (SELECT payment_id FROM allocations WHERE id = NEW.allocation_id)
             <> (SELECT payment_id FROM reversals WHERE id = NEW.reversal_id)
        THEN RAISE(ABORT, 'REVERSAL_WRONG_PAYMENT')
        WHEN NEW.amount + COALESCE(
                (SELECT reversed FROM allocations WHERE id = NEW.allocation_id), 0)
             > (SELECT amount FROM allocations WHERE id = NEW.allocation_id)
        THEN RAISE(ABORT, 'ALLOCATION_ALREADY_REVERSED')
    END;
END;

-- 插入冲回项后：回退分配的已撤销额、累计收款的已撤销额，
-- 并把发票已付金额按实际冲回额减回（CHECK 约束保证不会冲成负数）。
CREATE TRIGGER IF NOT EXISTS trg_reversal_item_after_insert
AFTER INSERT ON reversal_items
BEGIN
    UPDATE allocations
       SET reversed = reversed + NEW.amount
     WHERE id = NEW.allocation_id;
    UPDATE payments
       SET reversed = reversed + NEW.amount
     WHERE id = (SELECT payment_id FROM reversals WHERE id = NEW.reversal_id);
    UPDATE invoices
       SET paid = paid - NEW.amount
     WHERE id = (SELECT invoice_id FROM allocations WHERE id = NEW.allocation_id);
END;
