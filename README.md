# go-invoice-collection

在 Go 服务中实现**收款在多张应收发票之间的分配与原路撤销**。存储使用 SQLite
（纯 Go 驱动 [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)，无需 CGO），
并发安全由数据库事务、`CHECK` 约束与触发器保证，不依赖进程内锁——多个进程打开
同一个数据库文件也能得到一致结果。

## 概念模型

| 表 | 含义 |
| --- | --- |
| `invoices` | 应收发票：`total` 应收额、`paid` 已付额，状态由二者派生（未付 / 部分未付 / 已结清） |
| `payments` | 收款：`amount` 收款额、`allocated` 累计已分配、`reversed` 累计已撤销、`unallocated` 显式保留的余款 |
| `allocations` | 收款对某张发票的一条分配；`reversed` 记录其中已被冲回的金额，有效占用为 `amount - reversed` |
| `reversals` / `reversal_items` | 撤销单及其逐项冲回明细，每个冲回项引用原收款下的一条分配 |

发票已付额恒等于该发票所有 `allocation.amount` 减去所有 `reversal_items.amount`，
由触发器原子维护；`Service.InvoiceHistory` 可重放这本不可变台账。

## 金额

金额使用 `Money` 类型——以**分**为单位的 `int64`，固定两位小数，全程不出现浮点数：

```go
m, err := goinvoicecollection.ParseMoney("100.50") // 精确解析
m.Cents()                                          // 10050
m.String()                                         // "100.50"
```

超过两位小数（如 `"1.234"`）或非法格式会返回错误而不是被舍入。

## 快速开始

```go
ctx := context.Background()

// 文件库（生产）；测试可用 OpenMemory(ctx, "name")。
store, err := goinvoicecollection.OpenFile(ctx, "data.db", 10000 /* busyTimeoutMS */)
if err != nil { log.Fatal(err) }
defer store.Close()
svc := goinvoicecollection.NewService(store)

// 1) 登记应收发票
inv, _ := svc.CreateInvoice(ctx, goinvoicecollection.CreateInvoiceInput{
    Number: "INV-001", Total: goinvoicecollection.MustParseMoney("100.00"),
    DueAt:  time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
})

// 2a) 手工分配：120 元中分给 INV-001 100 元，余款 20 元保留在 Payment.Unallocated
res, err := svc.RegisterPayment(ctx, goinvoicecollection.RegisterPaymentInput{
    ExternalNo: "EXT-2026-0001",
    Amount:     goinvoicecollection.MustParseMoney("120.00"),
    Mode:       goinvoicecollection.AllocManual,
    Allocations: []goinvoicecollection.PaymentAllocationInput{
        {InvoiceID: inv.ID, Amount: goinvoicecollection.MustParseMoney("100.00")},
    },
})

// 2b) 自动分配：忽略 Allocations，按到期日从早到晚逐张填满，分不完的留作余款
// res, _ := svc.RegisterPayment(ctx, goinvoicecollection.RegisterPaymentInput{
//     ExternalNo: "EXT-2026-0002", Amount: ..., Mode: goinvoicecollection.AllocAuto,
// })

// 3) 查询
got, _ := svc.GetInvoice(ctx, inv.ID)   // got.Paid / got.Balance() / got.Status()

// 4) 原路撤销（三种等价写法）
//    全额撤销：逐项冲回当时所有尚未撤销的分配
svc.ReversePayment(ctx, goinvoicecollection.ReversePaymentInput{
    ReversalNo: "RV-1", PaymentID: res.Payment.ID,
})
//    部分撤销（按原分配顺序 FIFO 冲回 30）
svc.ReversePayment(ctx, goinvoicecollection.ReversePaymentInput{
    ReversalNo: "RV-2", PaymentID: res.Payment.ID,
    Amount:     goinvoicecollection.MustParseMoney("30.00"),
})
//    部分撤销（显式逐项指定）
svc.ReversePayment(ctx, goinvoicecollection.ReversePaymentInput{
    ReversalNo: "RV-3", PaymentID: res.Payment.ID,
    Items: []goinvoicecollection.ReversalItemInput{
        {InvoiceID: inv.ID, Amount: goinvoicecollection.MustParseMoney("10.00")},
    },
})
```

## 关键规则

### 1. 分配整体写入且不得超额

- 一次收款的收款行与所有分配行在同一个事务提交，任何一条不合法都整体回滚。
- 应用层先校验：手工分配条目不得重复、累计不得超过收款额、逐条不得超过发票未付余额。
- 数据库层再兜底：`trg_allocation_before_insert` 触发器逐行检查
  “本收款已分配 + 本行 ≤ 收款额”“发票已付 + 本行 ≤ 发票应收”，超额即 `RAISE(ABORT)`。
  因此即使绕过应用层，也不可能把同一余额重复结清。

### 2. 外部收款号幂等与冲突

- `payments.external_no` 唯一。同号请求先比对**内容指纹**（金额 + 分配方式 +
  手工模式下的发票-金额集合）：
  - 完全相同：返回首次记录，`RegisterResult.Replayed == true`，不重复写余额；
  - 内容改变（金额、方式或分配目标变化）：返回 `ErrConflict`。

### 3. 未分配余款显式保留

- 收款 `amount` 与 `allocated` 的差额保存在 `unallocated`（`CHECK unallocated = amount - allocated`），
  自动/手工分配后仍有剩余时可在 `Payment.Unallocated` 查到，绝不静默丢弃。
- 余款不属于任何发票，因此不参与撤销冲回；可撤销金额为 `Payment.Reversible() = Allocated - Reversed`。

### 4. 撤销逐项冲回、累计受限

- 撤销必须引用原收款；冲回项写入 `reversal_items` 并引用原 `allocations` 行。
- 触发器把冲回金额加回 `allocations.reversed`、`payments.reversed`，并从
  `invoices.paid` 减去，发票据此从已结清回到部分未付或未付。
- `CHECK (reversed <= amount)`、`CHECK (payments.reversed <= allocated)` 与
  `trg_reversal_item_before_insert` 共同保证：多次部分撤销累计不超过原收款，
  已撤销的部分不能再次使用；越界即返回 `ErrAlreadyReversed` / `ErrReversalExceeds`。
- `reversals.reversal_no` 唯一，撤销同样幂等：同号同内容返回首次撤销，同号异内容冲突。

### 5. 并发：只信数据库，不信进程内锁

- 每个写事务以 `BEGIN IMMEDIATE` 立即获取全库 RESERVED 写锁，写事务在库级别串行；
  事务内读到的 `invoices.paid` 一定是已提交的最新值，没有读后写竞态。
- 文件库开启 WAL 与 `busy_timeout`，锁竞争时等待而不是立即失败。
- 跨进程测试 `TestCrossProcessConcurrency` 用两个独立 `Store` 句柄打开同一个库
  竞争同一张发票，验证恰好一笔成功、另一笔收到 `ErrExceedsInvoice`，且发票
  已付额不会超过应收额；`TestConcurrentReversalAndPayment` 验证撤销与新收款
  并发时发票余额始终等于台账（全部分配 − 全部冲回）。

## API 速览

| 方法 | 说明 |
| --- | --- |
| `OpenFile` / `OpenMemory` | 打开并初始化存储 |
| `NewService` | 创建业务服务 |
| `CreateInvoice` | 登记应收发票 |
| `RegisterPayment` | 登记收款并整体写入分配（手工/自动），支持幂等重放 |
| `ReversePayment` | 全额 / FIFO 金额 / 显式逐项撤销原收款 |
| `GetInvoice` / `GetInvoiceByNumber` | 发票与余额、状态 |
| `GetPayment` / `GetPaymentByExternalNo` | 收款及其分配 |
| `GetReversal` / `GetReversalByNo` | 撤销单及冲回明细 |
| `InvoiceHistory` | 发票的分配/冲回台账（带每笔之后的累计已付额） |

常见错误：`ErrInvalidInput`、`ErrNotFound`、`ErrConflict`、`ErrExceedsPayment`、
`ErrExceedsInvoice`、`ErrReversalExceeds`、`ErrAlreadyReversed`，用 `errors.Is` 判断。

## 测试

```sh
go test -race -count=1 ./...
```

覆盖：金额精确解析、手工/自动分配与余款保留、幂等重放与冲突、全额/部分/超额撤销、
已撤销部分不可复用、台账重放，以及同进程与跨“进程”（独立连接句柄）的并发场景。
