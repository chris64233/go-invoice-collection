# go-invoice-collection

应收发票收款的**多发票分配**与**原路撤销**服务。基于 SQLite 持久化，金额用整数
最小货币单位精确表示，正确性由数据库事务保证（不依赖进程内锁），可安全用于
多连接 / 多进程并发场景。

## 能力概览

| 需求 | 实现 |
| --- | --- |
| 登记收款并指定发票与金额 | `Service.RegisterPayment`，`Mode=manual` |
| 按到期时间从早到晚自动分配 | `Mode=auto`（同日按发票号），见 `allocations.sequence` |
| 一次收款全部分配整体写入 | 单条 `BEGIN IMMEDIATE` 事务，发票、收款、分配明细一起提交或一起回滚 |
| 累计不超过收款额 / 发票未付余额 | 输入校验 + 发票条件更新 `WHERE paid_amount + ? <= amount`，影响行数为 0 即冲突回滚 |
| 并发不重复结清同一余额 | 立即写事务 + 条件 UPDATE；跨 `*sql.DB` / 跨进程同样成立，`SQLITE_BUSY` 自动退避重试 |
| 外部收款号幂等 | `payments.external_no` 唯一 + 归一化内容指纹（SHA-256）：同号同金额同分配返回首次记录；同号改内容返回 `KindConflict` |
| 精确金额 | `Money` 为 `int64` 最小货币单位（如分），提供 `ParseMoney` / `String`（两位小数），全程无浮点 |
| 未分配余款明确保留 | `payments.unallocated_amount`，恒等式 `allocated + unallocated + refunded = amount`（数据库 CHECK 约束） |
| 撤销引用原收款并逐项冲回 | `Service.CreateRefund`，逐条回写发票与原分配（`allocations.remaining_amount`）；逐项模式可用空 `AllocationID` 冲回未分配余款 |
| 发票恢复部分未付 / 未付 | 冲回时按剩余已付额回算 `status = unpaid | partial` |
| 多次部分撤销累计不超限 | 收款条件更新 `WHERE refunded_amount + ? <= amount` 及每条分配剩余额条件更新 |
| 撤销与新收款并发的最终一致 | 全部余额变更只走数据库行上的条件更新，数据库为唯一事实来源 |
| 查询 | 发票余额、收款、分配历史（含剩余可冲额）、撤销列表与明细、发票账本流水 |

## 快速开始

```go
store, err := goinvoicecollection.OpenFile(ctx, "/var/lib/ledger/ar.db")
if err != nil { return err }
defer store.Close()

svc := goinvoicecollection.NewService(store)

inv, err := svc.CreateInvoice(ctx, goinvoicecollection.NewInvoice{
    Number: "INV-1001", CustomerID: "C-7",
    IssueDate: time.Now(), DueDate: time.Now().AddDate(0, 0, 30),
    Amount: goinvoicecollection.MustParseMoney("1000.00"),
})

// 1) 手工分配：收款 600，付 INV-1001 全部，其余留作未分配余款。
pay, err := svc.RegisterPayment(ctx, goinvoicecollection.RegisterPaymentInput{
    ExternalNo: "EXT-PAY-20260928-001",
    CustomerID: "C-7",
    Amount:     goinvoicecollection.MustParseMoney("600.00"),
    Mode:       goinvoicecollection.AllocationManual,
    Instructions: []goinvoicecollection.AllocationInstruction{
        {InvoiceID: inv.ID, Amount: goinvoicecollection.MustParseMoney("600.00")},
    },
})
// pay.Payment.UnallocatedAmount == 0；若收款 700 则余款 100.00 被明确保留

// 2) 自动分配：按客户名下发票到期日从早到晚结清。
pay2, err := svc.RegisterPayment(ctx, goinvoicecollection.RegisterPaymentInput{
    ExternalNo: "EXT-PAY-20260928-002",
    CustomerID: "C-7",
    Amount:     goinvoicecollection.MustParseMoney("5000.00"),
    Mode:       goinvoicecollection.AllocationAuto,
})
```

### 撤销

```go
// FIFO 部分撤销：按原分配顺序冲回仍有剩余的分配；
// 若冲回额超过已分配总额，其余冲回未分配余款。
rf, err := svc.CreateRefund(ctx, goinvoicecollection.CreateRefundInput{
    ExternalNo: "EXT-REF-0001", // 可空；非空时作为撤销幂等键
    PaymentID:  pay.Payment.ID,
    Amount:     goinvoicecollection.MustParseMoney("200.00"),
})

// 逐项撤销：明确指定冲回哪条原分配。
rf2, err := svc.CreateRefund(ctx, goinvoicecollection.CreateRefundInput{
    PaymentID: pay.Payment.ID,
    Instructions: []goinvoicecollection.RefundInstruction{
        {AllocationID: pay.Allocations[0].ID, Amount: goinvoicecollection.MustParseMoney("100.00")},
    },
})

// 逐项冲回未分配余款：AllocationID 留空即表示冲回该收款明确保留的余款
//（一次撤销至多一条，金额不得超过余款余额），可与普通分配冲回条目混用。
rf3, err := svc.CreateRefund(ctx, goinvoicecollection.CreateRefundInput{
    ExternalNo: "EXT-REF-0003",
    PaymentID:  pay.Payment.ID,
    Instructions: []goinvoicecollection.RefundInstruction{
        {AllocationID: pay.Allocations[0].ID, Amount: goinvoicecollection.MustParseMoney("10.00")},
        {AllocationID: "",                          Amount: goinvoicecollection.MustParseMoney("20.00")},
    },
})
```

### 查询

```go
bal, _   := svc.InvoiceBalance(ctx, inv.ID)        // 余额与状态
allocs, _ := svc.ListAllocations(ctx, pay.Payment.ID)  // 分配历史 + 剩余可冲额
refunds, _ := svc.ListRefunds(ctx, pay.Payment.ID)
items, _   := svc.ListRefundItems(ctx, rf.Refund.ID)
ledger, _  := svc.ListInvoiceLedger(ctx, inv.ID)   // +分配 / -冲回 流水与累计已付额
```

## 金额表示

`Money` 是最小货币单位的 `int64`（人民币场景即“分”）。字符串接口按两位小数
互转：`MustParseMoney("12.34") == 1234`，`Money(1234).String() == "12.34"`。
超过两位小数直接报错，避免静默截断。数据库列全部为 `INTEGER`。

## 数据模型（schema.sql）

- `invoices`：应收发票，`amount` / `paid_amount`，CHECK 保证 `0 <= paid <= amount`；
- `payments`：收款，含 `allocated_amount`、`unallocated_amount`、`refunded_amount`
  与幂等 `fingerprint`，CHECK 保证三部分之和恒等于 `amount`；
- `allocations`：收款 → 发票的分配明细，`remaining_amount` 记录尚未冲回的部分；
- `refunds` / `refund_items`：撤销主单与逐项冲回明细，引用原分配；
  冲回未分配余款的条目（FIFO 溢出或逐项模式中空 `AllocationID` 的指令）
  `allocation_id / invoice_id` 为 NULL。

## 并发与一致性设计

1. 每个写事务使用 `BEGIN IMMEDIATE`（DSN 参数 `_txlock=immediate`），
   事务开始即获取 SQLite 保留锁，写事务天然串行；
2. 所有余额变更使用**条件 UPDATE**，而不是“读后写”：
   - 收款：`UPDATE invoices SET paid_amount = paid_amount + ? WHERE id = ? AND paid_amount + ? <= amount`；
   - 冲回发票：`... SET paid_amount = paid_amount - ? WHERE id = ? AND paid_amount - ? >= 0`；
   - 冲回分配：`... SET remaining_amount = remaining_amount - ? WHERE id = ? AND remaining_amount - ? >= 0`；
   - 收款撤销总额：`... SET refunded_amount = refunded_amount + ? WHERE id = ? AND refunded_amount + ? <= amount ...`。
   条件不满足时影响行数为 0，事务整体回滚并返回 `KindConflict`；
3. 幂等依赖 `external_no` 唯一约束，幂等检查在写事务内完成，
   并发同号请求只有一个能首次写入，其余读到同一条记录并返回（`Replayed=true`）；
4. 因此正确性不依赖 Go 进程内的 mutex —— 测试中每个 goroutine 使用**独立的
   `*sql.DB` 连接池**打开同一个数据库文件（模拟多进程），仍满足账本守恒。

冲突以业务错误类型返回，可用 `goinvoicecollection.IsKind(err, goinvoicecollection.KindConflict)`
判定，类别有 `KindValidation` / `KindNotFound` / `KindConflict` / `KindStorage`。

## 测试

```bash
go test -race -count=1 ./...
```

覆盖点：

- 金额解析 / 格式化、非法输入；
- 手工与自动分配（到期日顺序、部分结清、余款保留、超额/重票/跨客户校验）；
- 收款幂等：同号同内容重放、金额或方式改变冲突、余额不被重复结清；
- FIFO 撤销（跨分配拆分、冲回未分配余款）、逐项撤销（剩余额限制）、
  逐项撤销混合冲回分配与未分配余款（空 `AllocationID`、余款超额/重复条目拒绝、
  幂等重放）、全额撤销后发票重新开放给新收款、
  多次部分撤销累计超额拒绝、撤销幂等与冲突；
- 分配历史与发票账本流水（正负流水、累计余额、追溯原收款号）；
- 并发：独立连接池下 20 路手工收款恰好 10 成功 10 冲突、10 撤销 + 10 新收款
  交织后的最终账本与流水一致、同号并发请求只产生一条记录、
  20 路自动收款总额恰好分完不超付、10 路逐项撤销同一分配恰好 6 成功 4 冲突。

## 存储位置

默认使用文件数据库（推荐配置见 `OpenFile`：WAL、`busy_timeout=10000`、
`foreign_keys=ON`）。`OpenInMemory` 提供共享缓存内存库用于测试；
也可用 `OpenDB` 接入自行配置的 `*sql.DB`。
