package goinvoicecollection

import "errors"

// 领域错误：服务方法返回这些（可能被包装），调用方用 errors.Is 判断。
var (
	// ErrNotFound 引用的发票、收款或撤销不存在。
	ErrNotFound = errors.New("not found")
	// ErrConflict 外部收款号/撤销号已存在，但金额或分配方式与首次记录不一致。
	ErrConflict = errors.New("idempotency conflict")
	// ErrInvalidInput 入参非法（金额为负、缺字段、分配条目重复等）。
	ErrInvalidInput = errors.New("invalid input")
	// ErrExceedsPayment 分配/撤销金额超过收款可用（未分配或已分配未撤销）金额。
	ErrExceedsPayment = errors.New("amount exceeds payment available amount")
	// ErrExceedsInvoice 分配金额超过发票未付余额。
	ErrExceedsInvoice = errors.New("amount exceeds invoice unpaid balance")
	// ErrReversalExceeds 撤销金额超过原收款尚可撤销的金额。
	ErrReversalExceeds = errors.New("reversal exceeds original payment")
	// ErrAlreadyReversed 试图冲回已经撤销过的分配部分。
	ErrAlreadyReversed = errors.New("allocation already reversed")
)
