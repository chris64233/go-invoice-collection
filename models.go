package goinvoicecollection

import "time"

// InvoiceStatus 表示发票当前的结清状态，由 paid 与 total 的关系派生。
type InvoiceStatus string

const (
	InvoiceUnpaid  InvoiceStatus = "unpaid"  // 未付：paid == 0
	InvoicePartial InvoiceStatus = "partial" // 部分未付：0 < paid < total
	InvoicePaid    InvoiceStatus = "paid"    // 已结清：paid == total
)

// AllocationMode 是一次收款的分配方式。
type AllocationMode string

const (
	// AllocManual：调用方显式指定每张发票分配多少。
	AllocManual AllocationMode = "manual"
	// AllocAuto：系统按发票到期时间从早到晚（同日按发票 ID 稳定排序）自动分配。
	AllocAuto AllocationMode = "auto"
)

// Invoice 应收发票。
type Invoice struct {
	ID        string
	Number    string
	Total     Money
	Paid      Money // 已付（含历次收款减去历次冲回），与分配台账实时一致
	DueAt     time.Time
	CreatedAt time.Time
}

// Balance 未付余额。
func (i Invoice) Balance() Money { return i.Total - i.Paid }

// Status 当前状态。
func (i Invoice) Status() InvoiceStatus {
	switch {
	case i.Paid <= 0:
		return InvoiceUnpaid
	case i.Paid >= i.Total:
		return InvoicePaid
	default:
		return InvoicePartial
	}
}

// Payment 收款主记录。
type Payment struct {
	ID          string
	ExternalNo  string // 外部收款号（幂等键）
	Amount      Money
	Allocated   Money // 历史累计已分配（不因撤销减少）
	Reversed    Money // 累计已撤销冲回，满足 Reversed <= Allocated
	Unallocated Money // 显式保留的未分配余款：Amount - Allocated
	Mode        AllocationMode
	CreatedAt   time.Time
}

// Reversible 该收款尚可撤销的金额（已分配且尚未撤销的部分）。
// 未分配余款不属于任何发票，不能通过撤销冲回，应使用退款等其他流程。
func (p Payment) Reversible() Money { return p.Allocated - p.Reversed }

// Allocation 一条收款对一张发票的分配。
type Allocation struct {
	ID        string
	PaymentID string
	InvoiceID string
	Amount    Money
	Reversed  Money // 其中已被撤销冲回的金额；有效占用 = Amount - Reversed
	Seq       int
	CreatedAt time.Time
}

// Effective 尚未被撤销的有效分配额。
func (a Allocation) Effective() Money { return a.Amount - a.Reversed }

// Reversal 针对原收款的一次撤销（全额或部分）。
type Reversal struct {
	ID         string
	ReversalNo string // 撤销幂等键
	PaymentID  string
	Amount     Money
	CreatedAt  time.Time
	Items      []ReversalItem
}

// ReversalItem 撤销对某条原分配的逐项冲回。
type ReversalItem struct {
	ID           string
	ReversalID   string
	AllocationID string
	InvoiceID    string
	Amount       Money
	Seq          int
	CreatedAt    time.Time
}

// LedgerEntry 是发票分配/冲回历史中的一行（不可变台账）。
// 正数表示收款分配（占用余额），负数表示撤销冲回（释放余额）。
type LedgerEntry struct {
	At               time.Time
	Kind             string // "allocation" 或 "reversal"
	PaymentID        string
	ExternalNo       string
	ReversalID       string // Kind == "reversal" 时非空
	ReversalNo       string
	AllocationID     string
	InvoiceID        string
	Amount           Money // 带方向：分配为正，冲回为负
	InvoicePaidAfter Money // 该笔入账后发票的累计已付金额
}
