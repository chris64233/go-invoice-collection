-- 收款分配 / 撤销的账本结构。
-- 所有金额以最小货币单位（如“分”）的整数存储，禁止使用浮点。

CREATE TABLE IF NOT EXISTS invoices (
    id           TEXT PRIMARY KEY,
    number       TEXT NOT NULL UNIQUE,
    customer_id  TEXT NOT NULL,
    issue_date   TEXT NOT NULL,           -- YYYY-MM-DD
    due_date     TEXT NOT NULL,           -- YYYY-MM-DD（自动分配按此升序）
    amount       INTEGER NOT NULL,        -- 应收金额（最小货币单位）
    paid_amount  INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL DEFAULT 'unpaid',
    version      INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL,

    CONSTRAINT chk_invoice_amount_positive CHECK (amount > 0),
    CONSTRAINT chk_invoice_paid_range     CHECK (paid_amount >= 0 AND paid_amount <= amount),
    CONSTRAINT chk_invoice_status CHECK (status IN ('unpaid','partial','paid'))
);

CREATE INDEX IF NOT EXISTS idx_invoices_due ON invoices (due_date, number);
CREATE INDEX IF NOT EXISTS idx_invoices_customer ON invoices (customer_id);

CREATE TABLE IF NOT EXISTS payments (
    id                 TEXT PRIMARY KEY,
    external_no        TEXT NOT NULL UNIQUE,   -- 外部收款号：幂等键
    customer_id        TEXT NOT NULL,
    amount             INTEGER NOT NULL,       -- 收款总额
    allocation_mode    TEXT NOT NULL,          -- manual | auto
    allocated_amount   INTEGER NOT NULL DEFAULT 0,
    refunded_amount    INTEGER NOT NULL DEFAULT 0,
    unallocated_amount INTEGER NOT NULL DEFAULT 0, -- 明确保留的未分配余款
    fingerprint        TEXT NOT NULL,          -- 规范化后的请求内容指纹（冲突检测）
    created_at         TEXT NOT NULL,
    version            INTEGER NOT NULL DEFAULT 1,

    CONSTRAINT chk_payment_amount_positive CHECK (amount > 0),
    CONSTRAINT chk_payment_mode CHECK (allocation_mode IN ('manual','auto')),
    CONSTRAINT chk_payment_parts CHECK (
        allocated_amount >= 0 AND
        refunded_amount >= 0 AND
        unallocated_amount >= 0 AND
        allocated_amount + unallocated_amount + refunded_amount = amount
    )
);

CREATE TABLE IF NOT EXISTS allocations (
    id               TEXT PRIMARY KEY,
    payment_id       TEXT NOT NULL REFERENCES payments(id),
    invoice_id       TEXT NOT NULL REFERENCES invoices(id),
    amount           INTEGER NOT NULL,       -- 原始分配金额
    remaining_amount INTEGER NOT NULL,       -- 尚未被撤销冲回的金额
    sequence         INTEGER NOT NULL,       -- 同一次收款内的顺序
    created_at       TEXT NOT NULL,

    CONSTRAINT chk_alloc_amount_positive CHECK (amount > 0),
    CONSTRAINT chk_alloc_remaining_range CHECK (remaining_amount >= 0 AND remaining_amount <= amount),
    UNIQUE (payment_id, invoice_id)          -- 一次收款对一张发票至多一条
);

CREATE INDEX IF NOT EXISTS idx_allocations_invoice ON allocations (invoice_id);
CREATE INDEX IF NOT EXISTS idx_allocations_payment ON allocations (payment_id, sequence);

CREATE TABLE IF NOT EXISTS refunds (
    id          TEXT PRIMARY KEY,
    external_no TEXT UNIQUE,                  -- 可空；SQLite 中 NULL 不冲突
    payment_id  TEXT NOT NULL REFERENCES payments(id),
    amount      INTEGER NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    itemized    INTEGER NOT NULL,            -- 1=逐项指定, 0=FIFO
    fingerprint TEXT NOT NULL DEFAULT '',    -- 幂等内容指纹（external_no 非空时使用）
    created_at  TEXT NOT NULL,

    CONSTRAINT chk_refund_amount_positive CHECK (amount > 0)
);

CREATE INDEX IF NOT EXISTS idx_refunds_payment ON refunds (payment_id, created_at);

CREATE TABLE IF NOT EXISTS refund_items (
    id            TEXT PRIMARY KEY,
    refund_id     TEXT NOT NULL REFERENCES refunds(id),
    allocation_id TEXT REFERENCES allocations(id), -- 冲回余款时为 NULL
    invoice_id    TEXT REFERENCES invoices(id),    -- 冲回余款时为 NULL
    amount        INTEGER NOT NULL,
    sequence      INTEGER NOT NULL,

    CONSTRAINT chk_refund_item_amount_positive CHECK (amount > 0)
);

CREATE INDEX IF NOT EXISTS idx_refund_items_refund ON refund_items (refund_id, sequence);
CREATE INDEX IF NOT EXISTS idx_refund_items_invoice ON refund_items (invoice_id);
