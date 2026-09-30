package goinvoicecollection

import "time"

// InvoiceStatus 发票的结清状态。
type InvoiceStatus string

const (
	InvoiceUnpaid   InvoiceStatus = "unpaid"   // 未付
	InvoicePartial  InvoiceStatus = "partial"  // 部分已付
	InvoicePaid     InvoiceStatus = "paid"     // 已全额结清
	InvoiceOverpaid InvoiceStatus = "overpaid" // 正常流程不会出现，仅防御
)

// AllocationMode 收款的分配方式。
type AllocationMode string

const (
	// AllocationManual 调用方显式指定发票与金额。
	AllocationManual AllocationMode = "manual"
	// AllocationAuto 系统按发票到期时间从早到晚自动分配。
	AllocationAuto AllocationMode = "auto"
)

// Invoice 应收发票。金额一律以最小货币单位的 int64 存储。
type Invoice struct {
	ID         string
	Number     string // 发票号，唯一
	CustomerID string
	IssueDate  time.Time
	DueDate    time.Time // 到期时间，自动分配的排序依据
	Amount     Money     // 应收总额
	PaidAmount Money     // 已分配（结清）金额，含尚未撤销的部分
	Status     InvoiceStatus
	Version    int64 // 乐观版本号，每次余额变动 +1
	CreatedAt  time.Time
}

// OpenAmount 返回未付余额。
func (inv Invoice) OpenAmount() Money { return inv.Amount.Sub(inv.PaidAmount) }

// InvoiceBalance 发票余额查询结果。
type InvoiceBalance struct {
	InvoiceID  string
	Number     string
	CustomerID string
	Amount     Money
	PaidAmount Money
	OpenAmount Money
	Status     InvoiceStatus
	Version    int64
}

// Payment 一笔收款。
type Payment struct {
	ID                string
	ExternalNo        string // 外部收款号，幂等键
	CustomerID        string
	Amount            Money // 收款总额
	AllocationMode    AllocationMode
	AllocatedAmount   Money // 已分配到发票的金额
	RefundedAmount    Money // 已撤销总额
	UnallocatedAmount Money // 明确保留的未分配余款（不静默丢弃）
	CreatedAt         time.Time
	Version           int64
}

// RefundableAmount 返回当前仍可撤销的金额（分配与余款中尚未撤销的部分）。
func (p Payment) RefundableAmount() Money { return p.Amount.Sub(p.RefundedAmount) }

// Allocation 一条收款到发票的分配明细。
type Allocation struct {
	ID              string
	PaymentID       string
	InvoiceID       string
	InvoiceNumber   string // 联表带出，便于展示
	Amount          Money
	RemainingAmount Money // 该条分配尚未被撤销冲回的剩余金额
	Sequence        int   // 同一次收款内的顺序（自动分配=到期日顺序）
	CreatedAt       time.Time
}

// Refund 一次撤销。撤销必须引用原收款；支持全额或部分撤销。
type Refund struct {
	ID         string
	ExternalNo string // 外部撤销号，可为空；非空时作为幂等键
	PaymentID  string
	Amount     Money
	Reason     string
	Itemized   bool // true=调用方逐项指定冲回；false=系统按 FIFO 冲回
	CreatedAt  time.Time
}

// RefundItem 撤销对单条原分配的逐项冲回明细。
// 对未分配余款的冲回使用空 InvoiceID / AllocationID 的条目表示。
type RefundItem struct {
	ID           string
	RefundID     string
	AllocationID string // 引用的原分配；冲回余款时为空
	InvoiceID    string // 冲回余款时为空
	Amount       Money
	Sequence     int
}

// ---- 输入参数 ----

// NewInvoice 创建发票的输入。
type NewInvoice struct {
	Number     string
	CustomerID string
	IssueDate  time.Time
	DueDate    time.Time
	Amount     Money
}

// AllocationInstruction 手工分配的一条指令。
type AllocationInstruction struct {
	InvoiceID string
	Amount    Money
}

// RegisterPaymentInput 登记收款的输入。
type RegisterPaymentInput struct {
	ExternalNo string // 必填，幂等键
	CustomerID string
	Amount     Money
	// Mode 为 manual 时 Instructions 必填；auto 时忽略 Instructions。
	Mode         AllocationMode
	Instructions []AllocationInstruction
}

// RegisterPaymentResult 登记结果。幂等重放时 Replayed 为 true。
type RegisterPaymentResult struct {
	Payment     Payment
	Allocations []Allocation
	Replayed    bool
}

// RefundInstruction 逐项撤销时对某条原分配的冲回指令。
// AllocationID 必填（引用原收款的一条分配）；冲回未分配余款不由
// 逐项指令表达，而是使用 FIFO 模式（Instructions 留空）时由系统
// 在分配冲完后自动冲回。
type RefundInstruction struct {
	AllocationID string
	Amount       Money
}

// CreateRefundInput 撤销输入。
type CreateRefundInput struct {
	ExternalNo   string // 可空；非空时按“同号同内容”幂等
	PaymentID    string
	Amount       Money // 为 0 时按 Instructions 之和；FIFO 模式必填正数
	Reason       string
	Instructions []RefundInstruction // 空切片=系统按 FIFO 冲回
}

// CreateRefundResult 撤销结果。幂等重放时 Replayed 为 true。
type CreateRefundResult struct {
	Refund   Refund
	Items    []RefundItem
	Replayed bool
}

// LedgerEntry 发票账本流水：一次分配或一次冲回。
type LedgerEntry struct {
	At           time.Time
	InvoiceID    string
	PaymentID    string
	PaymentNo    string
	RefundID     string
	Kind         string // "allocation" 或 "refund"
	Amount       Money  // 分配为正、冲回为负
	BalanceAfter Money  // 该发票在本条流水后的已付金额
}
