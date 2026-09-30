package goinvoicecollection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Service 是收款分配/撤销的应用服务，方法均为并发安全。
// 正确性依赖数据库事务而非进程内锁，因此多个 Service 实例、
// 多个进程同时操作同一个数据库文件也能保持账本一致。
type Service struct {
	store *Store
	now   func() time.Time
}

// NewService 基于 Store 创建服务。
func NewService(store *Store) *Service {
	return &Service{store: store, now: nowUTC}
}

// =====================================================================
// 发票
// =====================================================================

// CreateInvoice 登记一张应收发票。发票号重复返回 KindConflict。
func (s *Service) CreateInvoice(ctx context.Context, in NewInvoice) (Invoice, error) {
	if strings.TrimSpace(in.Number) == "" {
		return Invoice{}, kindError(KindValidation, "invoice number is required")
	}
	if strings.TrimSpace(in.CustomerID) == "" {
		return Invoice{}, kindError(KindValidation, "customer id is required")
	}
	if !in.Amount.IsPositive() {
		return Invoice{}, kindError(KindValidation, "invoice amount must be positive")
	}
	if in.DueDate.IsZero() {
		return Invoice{}, kindError(KindValidation, "due date is required")
	}
	id, err := randomID("inv")
	if err != nil {
		return Invoice{}, err
	}
	now := s.now()
	inv := Invoice{
		ID:         id,
		Number:     strings.TrimSpace(in.Number),
		CustomerID: strings.TrimSpace(in.CustomerID),
		IssueDate:  normalizeDate(in.IssueDate, now),
		DueDate:    in.DueDate.UTC(),
		Amount:     in.Amount,
		PaidAmount: 0,
		Status:     InvoiceUnpaid,
		Version:    1,
		CreatedAt:  now,
	}
	err = s.store.runTx(ctx, "create_invoice", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO invoices
  (id, number, customer_id, issue_date, due_date, amount, paid_amount, status, version, created_at)
VALUES (?, ?, ?, ?, ?, ?, 0, 'unpaid', 1, ?)`,
			inv.ID, inv.Number, inv.CustomerID,
			encodeDate(inv.IssueDate), encodeDate(inv.DueDate),
			int64(inv.Amount), encodeTimestamp(inv.CreatedAt))
		if err != nil {
			if isUniqueViolation(err) {
				return kindError(KindConflict, "invoice number %q already exists", inv.Number)
			}
			return wrapError(KindStorage, "insert invoice", err)
		}
		return nil
	})
	if err != nil {
		return Invoice{}, err
	}
	return inv, nil
}

// =====================================================================
// 收款登记
// =====================================================================

// RegisterPayment 登记一笔收款并完成分配。
//
//   - Mode=manual：必须通过 Instructions 显式指定每张发票的分配金额；
//   - Mode=auto：系统按发票到期日从早到晚（同日按发票号）自动分配；
//   - 未分配完的余款记入 Payment.UnallocatedAmount 明确保留；
//   - 全部分配与收款主记录在单个立即写事务中整体提交；
//   - ExternalNo 为幂等键：同号同内容返回首次记录（Replayed=true），
//     同号不同金额或不同分配方式返回 KindConflict。
func (s *Service) RegisterPayment(ctx context.Context, in RegisterPaymentInput) (RegisterPaymentResult, error) {
	if err := validateRegisterInput(in); err != nil {
		return RegisterPaymentResult{}, err
	}
	fingerprint := paymentFingerprint(in)

	var result RegisterPaymentResult
	err := s.store.runTx(ctx, "register_payment", func(tx *sql.Tx) error {
		// 幂等检查在事务内进行：BEGIN IMMEDIATE 已取写锁，
		// 两个并发同号请求只会有一个插入成功。
		extNo := strings.TrimSpace(in.ExternalNo)
		existing, err := getPaymentByExternalNoTx(ctx, tx, extNo)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if existing.fingerprint != fingerprint {
				return kindError(KindConflict,
					"payment external no %q already exists with different amount or allocation", extNo)
			}
			return loadPaymentResult(ctx, tx, existing, &result)
		}

		plan, err := s.buildAllocationPlan(ctx, tx, in)
		if err != nil {
			return err
		}
		if err := applyPaymentPlan(ctx, tx, in, fingerprint, plan, s.now()); err != nil {
			return err
		}
		payment, allocations, err := reloadPaymentResult(ctx, tx, extNo)
		if err != nil {
			return err
		}
		result = RegisterPaymentResult{Payment: payment, Allocations: allocations, Replayed: false}
		return nil
	})
	if err != nil {
		return RegisterPaymentResult{}, err
	}
	return result, nil
}

func validateRegisterInput(in RegisterPaymentInput) error {
	if strings.TrimSpace(in.ExternalNo) == "" {
		return kindError(KindValidation, "external payment no is required")
	}
	if !in.Amount.IsPositive() {
		return kindError(KindValidation, "payment amount must be positive")
	}
	switch in.Mode {
	case AllocationManual:
		if len(in.Instructions) == 0 {
			return kindError(KindValidation, "manual allocation requires at least one instruction")
		}
		var sum Money
		seen := map[string]struct{}{}
		for i, ins := range in.Instructions {
			if strings.TrimSpace(ins.InvoiceID) == "" {
				return kindError(KindValidation, "instruction %d: invoice id is required", i+1)
			}
			if !ins.Amount.IsPositive() {
				return kindError(KindValidation, "instruction %d: amount must be positive", i+1)
			}
			if _, dup := seen[ins.InvoiceID]; dup {
				return kindError(KindValidation, "instruction %d: invoice %q listed more than once", i+1, ins.InvoiceID)
			}
			seen[ins.InvoiceID] = struct{}{}
			sum = sum.Add(ins.Amount)
			if sum > in.Amount {
				return kindError(KindValidation,
					"allocated total %s exceeds payment amount %s", sum, in.Amount)
			}
		}
	case AllocationAuto:
		if len(in.Instructions) > 0 {
			return kindError(KindValidation, "auto allocation must not carry instructions")
		}
		if strings.TrimSpace(in.CustomerID) == "" {
			return kindError(KindValidation, "customer id is required for auto allocation")
		}
	default:
		return kindError(KindValidation, "unknown allocation mode %q", in.Mode)
	}
	return nil
}

// allocLine 是事务内计算出的一条待写入分配。
type allocLine struct {
	invoiceID string
	amount    Money
	sequence  int
}

// allocPlan 是一次收款的完整分配计划。
type allocPlan struct {
	lines       []allocLine
	allocated   Money
	unallocated Money
}

func (s *Service) buildAllocationPlan(ctx context.Context, tx *sql.Tx, in RegisterPaymentInput) (allocPlan, error) {
	switch in.Mode {
	case AllocationManual:
		return s.buildManualPlan(ctx, tx, in)
	case AllocationAuto:
		return s.buildAutoPlan(ctx, tx, in)
	default:
		return allocPlan{}, kindError(KindValidation, "unknown allocation mode %q", in.Mode)
	}
}

func (s *Service) buildManualPlan(ctx context.Context, tx *sql.Tx, in RegisterPaymentInput) (allocPlan, error) {
	plan := allocPlan{}
	ids := make([]string, len(in.Instructions))
	for i, ins := range in.Instructions {
		ids[i] = ins.InvoiceID
	}
	invoices, err := lockInvoicesByIDs(ctx, tx, ids)
	if err != nil {
		return allocPlan{}, err
	}
	byID := make(map[string]invoiceRow, len(invoices))
	for _, iv := range invoices {
		byID[iv.id] = iv
	}
	for i, ins := range in.Instructions {
		iv, ok := byID[ins.InvoiceID]
		if !ok {
			return allocPlan{}, kindError(KindNotFound, "invoice %q not found", ins.InvoiceID)
		}
		if in.CustomerID != "" && iv.customerID != strings.TrimSpace(in.CustomerID) {
			return allocPlan{}, kindError(KindValidation,
				"invoice %q belongs to another customer", iv.number)
		}
		open := Money(iv.amount - iv.paidAmount)
		if ins.Amount > open {
			return allocPlan{}, kindError(KindConflict,
				"allocation %s to invoice %q exceeds open amount %s", ins.Amount, iv.number, open)
		}
		plan.lines = append(plan.lines, allocLine{
			invoiceID: iv.id, amount: ins.Amount, sequence: i + 1,
		})
		plan.allocated = plan.allocated.Add(ins.Amount)
	}
	plan.unallocated = in.Amount.Sub(plan.allocated)
	return plan, nil
}

func (s *Service) buildAutoPlan(ctx context.Context, tx *sql.Tx, in RegisterPaymentInput) (allocPlan, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT id, number, customer_id, issue_date, due_date, amount, paid_amount, status, version, created_at
FROM invoices
WHERE customer_id = ?
ORDER BY due_date ASC, number ASC, id ASC`,
		strings.TrimSpace(in.CustomerID))
	if err != nil {
		return allocPlan{}, wrapError(KindStorage, "query invoices for auto allocation", err)
	}
	defer rows.Close()

	plan := allocPlan{}
	remaining := in.Amount
	seq := 0
	for rows.Next() {
		if remaining.IsZero() {
			break
		}
		iv, err := scanInvoice(rows)
		if err != nil {
			return allocPlan{}, err
		}
		open := iv.Amount.Sub(iv.PaidAmount)
		if !open.IsPositive() {
			continue
		}
		take := moneyMin(remaining, open)
		seq++
		plan.lines = append(plan.lines, allocLine{
			invoiceID: iv.ID, amount: take, sequence: seq,
		})
		plan.allocated = plan.allocated.Add(take)
		remaining = remaining.Sub(take)
	}
	if err := rows.Err(); err != nil {
		return allocPlan{}, wrapError(KindStorage, "iterate invoices", err)
	}
	plan.unallocated = remaining
	return plan, nil
}

// applyPaymentPlan 在当前事务中条件更新发票余额并写入收款与分配。
func applyPaymentPlan(ctx context.Context, tx *sql.Tx, in RegisterPaymentInput, fingerprint string, plan allocPlan, now time.Time) error {
	paymentID, err := randomID("pay")
	if err != nil {
		return err
	}
	createdAt := encodeTimestamp(now)

	// 1) 条件更新每张发票：只有 paid + 本次 <= amount 才会生效。
	//    这是防并发重复结清的最终防线——即使上面读到的余额在计划
	//    构造后被另一事务改变，条件不满足时影响行数为 0，整个事务失败。
	for _, line := range plan.lines {
		res, err := tx.ExecContext(ctx, `
UPDATE invoices
SET paid_amount = paid_amount + ?,
    status = CASE
      WHEN paid_amount + ? = amount THEN 'paid'
      ELSE 'partial'
    END,
    version = version + 1
WHERE id = ? AND paid_amount + ? <= amount`,
			int64(line.amount), int64(line.amount), line.invoiceID, int64(line.amount))
		if err != nil {
			return wrapError(KindStorage, "update invoice balance", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return wrapError(KindStorage, "rows affected", err)
		}
		if n != 1 {
			return kindError(KindConflict,
				"invoice %s open amount changed concurrently; retry allocation", line.invoiceID)
		}
	}

	// 2) 写入收款主记录（external_no 唯一，并发同号至多一条成功）。
	_, err = tx.ExecContext(ctx, `
INSERT INTO payments
  (id, external_no, customer_id, amount, allocation_mode,
   allocated_amount, refunded_amount, unallocated_amount, fingerprint, created_at, version)
VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, 1)`,
		paymentID, strings.TrimSpace(in.ExternalNo), strings.TrimSpace(in.CustomerID),
		int64(in.Amount), string(in.Mode),
		int64(plan.allocated), int64(plan.unallocated), fingerprint, createdAt)
	if err != nil {
		if isUniqueViolation(err) {
			return kindError(KindConflict, "payment external no %q already exists", in.ExternalNo)
		}
		return wrapError(KindStorage, "insert payment", err)
	}

	// 3) 写入分配明细，整体提交。
	for _, line := range plan.lines {
		allocID, err := randomID("al")
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO allocations
  (id, payment_id, invoice_id, amount, remaining_amount, sequence, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
			allocID, paymentID, line.invoiceID,
			int64(line.amount), int64(line.amount), line.sequence, createdAt)
		if err != nil {
			return wrapError(KindStorage, "insert allocation", err)
		}
	}
	return nil
}

// paymentFingerprint 生成请求内容指纹，用于幂等冲突判定。
// 手工模式对发票-金额对按发票 ID 排序后归一化，使顺序不同但内容相同
// 的请求仍视为同一分配方式。
func paymentFingerprint(in RegisterPaymentInput) string {
	h := sha256.New()
	fmt.Fprintf(h, "v1|mode=%s|amount=%d|customer=%s", in.Mode, int64(in.Amount), strings.TrimSpace(in.CustomerID))
	if in.Mode == AllocationManual {
		type pair struct {
			id  string
			amt int64
		}
		pairs := make([]pair, len(in.Instructions))
		for i, ins := range in.Instructions {
			pairs[i] = pair{id: strings.TrimSpace(ins.InvoiceID), amt: int64(ins.Amount)}
		}
		sort.Slice(pairs, func(i, j int) bool { return pairs[i].id < pairs[j].id })
		for _, p := range pairs {
			fmt.Fprintf(h, "|%s=%d", p.id, p.amt)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// =====================================================================
// 撤销（refund / reversal）
// =====================================================================

// CreateRefund 对一笔已有收款执行原路撤销，可全额或部分撤销。
//
//   - Instructions 为空：按 FIFO 冲回——优先冲回最早建立、仍有余额
//     的分配，若收款含未分配余款且冲回额超过分配总额，其余冲回余款；
//   - Instructions 非空：逐项指定冲回哪条原分配（AllocationID），
//     每条不超过其剩余可冲回金额；AllocationID 留空的条目冲回该收款
//     明确保留的未分配余款（至多一条，且不超过余款余额）；
//   - 同一收款多次撤销的累计金额不得超过收款额；
//   - 被冲回的发票按实际冲回金额恢复为部分未付或未付；
//   - ExternalNo 非空时作为撤销幂等键，同号不同内容返回 KindConflict。
func (s *Service) CreateRefund(ctx context.Context, in CreateRefundInput) (CreateRefundResult, error) {
	if strings.TrimSpace(in.PaymentID) == "" {
		return CreateRefundResult{}, kindError(KindValidation, "payment id is required")
	}
	if in.Amount.IsNegative() {
		return CreateRefundResult{}, kindError(KindValidation, "refund amount must not be negative")
	}
	itemized := len(in.Instructions) > 0
	if !itemized && in.Amount.IsZero() {
		return CreateRefundResult{}, kindError(KindValidation,
			"FIFO refund requires a positive amount (use RefundableAmount for full refund)")
	}
	for i, ins := range in.Instructions {
		if !ins.Amount.IsPositive() {
			return CreateRefundResult{}, kindError(KindValidation,
				"instruction %d: amount must be positive", i+1)
		}
	}

	var result CreateRefundResult
	err := s.store.runTx(ctx, "create_refund", func(tx *sql.Tx) error {
		payment, err := getPaymentByIDTx(ctx, tx, in.PaymentID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return kindError(KindNotFound, "payment %q not found", in.PaymentID)
			}
			return err
		}

		// 撤销幂等（外部撤销号）。
		extNo := strings.TrimSpace(in.ExternalNo)
		if extNo != "" {
			existing, err := getRefundByExternalNoTx(ctx, tx, extNo)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				if existing.fingerprint != requestedRefundFingerprint(in, payment.id) {
					return kindError(KindConflict,
						"refund external no %q already exists with different content", extNo)
				}
				return loadRefundResult(ctx, tx, existing.id, true, &result)
			}
		}

		var lines []refundLine
		switch {
		case itemized:
			lines, err = buildItemizedRefund(ctx, tx, payment, in)
		default:
			lines, err = buildFIFORefund(ctx, tx, payment, in.Amount)
		}
		if err != nil {
			return err
		}

		total := sumRefundLines(lines)
		if total.IsZero() {
			return kindError(KindConflict, "payment %q has no refundable amount", payment.id)
		}
		if Money(payment.refundedAmount)+total > Money(payment.amount) {
			return kindError(KindConflict,
				"cumulative refund %s would exceed payment amount %s",
				Money(payment.refundedAmount)+total, Money(payment.amount))
		}

		fp := plannedRefundFingerprint(payment.id, itemized, total, lines)
		refundID, err := applyRefundPlan(ctx, tx, payment, lines, total, itemized, fp, in.Reason, extNo, s.now())
		if err != nil {
			return err
		}
		return loadRefundResult(ctx, tx, refundID, false, &result)
	})
	if err != nil {
		return CreateRefundResult{}, err
	}
	return result, nil
}

// refundLine 是事务内计算出的一条冲回。
// allocationID 为空表示冲回收款的未分配余款。
type refundLine struct {
	allocationID string
	invoiceID    string
	amount       Money
	sequence     int
}

func buildItemizedRefund(ctx context.Context, tx *sql.Tx, payment paymentRow, in CreateRefundInput) ([]refundLine, error) {
	var total Money
	seen := map[string]struct{}{}
	var unallocatedWant Money
	emptyEntries := 0
	for i, ins := range in.Instructions {
		allocID := strings.TrimSpace(ins.AllocationID)
		if allocID == "" {
			// 留空 AllocationID 的条目表示冲回该收款明确保留的未分配余款。
			emptyEntries++
			if emptyEntries > 1 {
				return nil, kindError(KindValidation,
					"instruction %d: only one unallocated-remainder entry is allowed per refund", i+1)
			}
			unallocatedWant = unallocatedWant.Add(ins.Amount)
			total = total.Add(ins.Amount)
			continue
		}
		if _, dup := seen[allocID]; dup {
			return nil, kindError(KindValidation,
				"instruction %d: allocation %q listed more than once", i+1, allocID)
		}
		seen[allocID] = struct{}{}
		total = total.Add(ins.Amount)
	}
	if !in.Amount.IsZero() && in.Amount != total {
		return nil, kindError(KindValidation,
			"refund amount %s does not match instruction total %s", in.Amount, total)
	}
	if Money(payment.refundedAmount)+total > Money(payment.amount) {
		return nil, kindError(KindConflict,
			"refund total %s plus already refunded %s exceeds payment amount %s",
			total, Money(payment.refundedAmount), Money(payment.amount))
	}
	if unallocatedWant > Money(payment.unallocatedAmount) {
		return nil, kindError(KindConflict,
			"unallocated refund %s exceeds remaining unallocated amount %s of payment %s",
			unallocatedWant, Money(payment.unallocatedAmount), payment.id)
	}

	rows, err := tx.QueryContext(ctx, `
SELECT id, payment_id, invoice_id, amount, remaining_amount, sequence
FROM allocations
WHERE payment_id = ?
ORDER BY sequence ASC, id ASC`, payment.id)
	if err != nil {
		return nil, wrapError(KindStorage, "query allocations for refund", err)
	}
	defer rows.Close()
	allocs := map[string]allocationRemaining{}
	for rows.Next() {
		var a struct {
			id, paymentID, invoiceID string
			amount, remaining        int64
			sequence                 int
		}
		if err := rows.Scan(&a.id, &a.paymentID, &a.invoiceID, &a.amount, &a.remaining, &a.sequence); err != nil {
			return nil, wrapError(KindStorage, "scan allocation", err)
		}
		allocs[a.id] = allocationRemaining{
			id: a.id, invoiceID: a.invoiceID, amount: Money(a.amount),
			remaining: Money(a.remaining), sequence: a.sequence,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, wrapError(KindStorage, "iterate allocations", err)
	}

	lines := make([]refundLine, 0, len(in.Instructions))
	for i, ins := range in.Instructions {
		allocID := strings.TrimSpace(ins.AllocationID)
		if allocID == "" {
			continue // 余款条目排序后追加在末尾
		}
		a, ok := allocs[allocID]
		if !ok {
			return nil, kindError(KindNotFound,
				"allocation %q does not belong to payment %q", allocID, payment.id)
		}
		if ins.Amount > a.remaining {
			return nil, kindError(KindConflict,
				"refund %s to allocation %s exceeds its remaining refundable amount %s",
				ins.Amount, a.id, a.remaining)
		}
		lines = append(lines, refundLine{
			allocationID: a.id,
			invoiceID:    a.invoiceID,
			amount:       ins.Amount,
			sequence:     i + 1,
		})
	}
	// 保持按原分配顺序输出，稳定可读。
	sort.SliceStable(lines, func(i, j int) bool {
		return allocs[lines[i].allocationID].sequence < allocs[lines[j].allocationID].sequence
	})
	for i := range lines {
		lines[i].sequence = i + 1
	}
	if unallocatedWant.IsPositive() {
		lines = append(lines, refundLine{
			allocationID: "", invoiceID: "", amount: unallocatedWant,
			sequence: len(lines) + 1,
		})
	}
	return lines, nil
}

type allocationRemaining struct {
	id        string
	invoiceID string
	amount    Money
	remaining Money
	sequence  int
}

func buildFIFORefund(ctx context.Context, tx *sql.Tx, payment paymentRow, want Money) ([]refundLine, error) {
	if Money(payment.refundedAmount)+want > Money(payment.amount) {
		return nil, kindError(KindConflict,
			"refund %s plus already refunded %s exceeds payment amount %s",
			want, Money(payment.refundedAmount), Money(payment.amount))
	}

	rows, err := tx.QueryContext(ctx, `
SELECT id, payment_id, invoice_id, amount, remaining_amount, sequence
FROM allocations
WHERE payment_id = ? AND remaining_amount > 0
ORDER BY sequence ASC, id ASC`, payment.id)
	if err != nil {
		return nil, wrapError(KindStorage, "query allocations for FIFO refund", err)
	}
	defer rows.Close()

	var lines []refundLine
	remaining := want
	seq := 0
	for rows.Next() {
		if remaining.IsZero() {
			break
		}
		var id, pid, invoiceID string
		var amount, rem int64
		var sequence int
		if err := rows.Scan(&id, &pid, &invoiceID, &amount, &rem, &sequence); err != nil {
			return nil, wrapError(KindStorage, "scan allocation", err)
		}
		take := moneyMin(remaining, Money(rem))
		if !take.IsPositive() {
			continue
		}
		seq++
		lines = append(lines, refundLine{
			allocationID: id, invoiceID: invoiceID, amount: take, sequence: seq,
		})
		remaining = remaining.Sub(take)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapError(KindStorage, "iterate allocations", err)
	}

	// 分配部分冲完后仍有缺口：冲回明确保留的未分配余款。
	if remaining.IsPositive() {
		if remaining > Money(payment.unallocatedAmount) {
			return nil, kindError(KindConflict,
				"refund %s exceeds refundable amount of payment %s", want, payment.id)
		}
		seq++
		lines = append(lines, refundLine{allocationID: "", invoiceID: "", amount: remaining, sequence: seq})
		remaining = 0
	}
	return lines, nil
}

func sumRefundLines(lines []refundLine) Money {
	var total Money
	for _, l := range lines {
		total = total.Add(l.amount)
	}
	return total
}

// applyRefundPlan 在当前事务中执行冲回：条件扣减发票已付额、
// 扣减分配剩余额、累加收款已撤销额（或扣减未分配余款）并写入撤销记录。
// 返回新建撤销记录的 ID。
func applyRefundPlan(ctx context.Context, tx *sql.Tx, payment paymentRow, lines []refundLine, total Money, itemized bool, fingerprint, reason, extNo string, now time.Time) (string, error) {
	refundID, err := randomID("rf")
	if err != nil {
		return "", err
	}

	allocatedRefund := Money(0)
	unallocatedRefund := Money(0)
	for _, line := range lines {
		if line.invoiceID == "" {
			unallocatedRefund = unallocatedRefund.Add(line.amount)
			continue
		}
		allocatedRefund = allocatedRefund.Add(line.amount)

		// 条件更新发票：paid_amount - 冲回额 >= 0。
		res, err := tx.ExecContext(ctx, `
UPDATE invoices
SET paid_amount = paid_amount - ?,
    status = CASE
      WHEN paid_amount - ? = 0 THEN 'unpaid'
      ELSE 'partial'
    END,
    version = version + 1
WHERE id = ? AND paid_amount - ? >= 0`,
			int64(line.amount), int64(line.amount), line.invoiceID, int64(line.amount))
		if err != nil {
			return "", wrapError(KindStorage, "reverse invoice balance", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return "", wrapError(KindStorage, "rows affected", err)
		}
		if n != 1 {
			return "", kindError(KindConflict,
				"invoice %s balance changed concurrently; retry refund", line.invoiceID)
		}

		// 条件更新分配剩余额，保证同一分配不会被重复冲回。
		res, err = tx.ExecContext(ctx, `
UPDATE allocations
SET remaining_amount = remaining_amount - ?
WHERE id = ? AND remaining_amount - ? >= 0`,
			int64(line.amount), line.allocationID, int64(line.amount))
		if err != nil {
			return "", wrapError(KindStorage, "reverse allocation", err)
		}
		n, err = res.RowsAffected()
		if err != nil {
			return "", wrapError(KindStorage, "rows affected", err)
		}
		if n != 1 {
			return "", kindError(KindConflict,
				"allocation %s was refunded concurrently; retry", line.allocationID)
		}
	}

	// 更新收款：已撤销累加，未分配余款相应减少。
	res, err := tx.ExecContext(ctx, `
UPDATE payments
SET refunded_amount = refunded_amount + ?,
    unallocated_amount = unallocated_amount - ?,
    allocated_amount = allocated_amount - ?,
    version = version + 1
WHERE id = ?
  AND refunded_amount + ? <= amount
  AND unallocated_amount - ? >= 0
  AND allocated_amount - ? >= 0`,
		int64(total), int64(unallocatedRefund), int64(allocatedRefund), payment.id,
		int64(total), int64(unallocatedRefund), int64(allocatedRefund))
	if err != nil {
		return "", wrapError(KindStorage, "update payment refund totals", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", wrapError(KindStorage, "rows affected", err)
	}
	if n != 1 {
		return "", kindError(KindConflict,
			"payment %s refund totals changed concurrently; retry", payment.id)
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO refunds (id, external_no, payment_id, amount, reason, itemized, fingerprint, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		refundID, nullableString(extNo), payment.id, int64(total), reason,
		boolToInt64(itemized), fingerprint, encodeTimestamp(now))
	if err != nil {
		if isUniqueViolation(err) {
			return "", kindError(KindConflict, "refund external no %q already exists", extNo)
		}
		return "", wrapError(KindStorage, "insert refund", err)
	}

	for _, line := range lines {
		itemID, err := randomID("ri")
		if err != nil {
			return "", err
		}
		var allocID, invoiceID sql.NullString
		if line.allocationID != "" {
			allocID = sql.NullString{String: line.allocationID, Valid: true}
			invoiceID = sql.NullString{String: line.invoiceID, Valid: true}
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO refund_items (id, refund_id, allocation_id, invoice_id, amount, sequence)
VALUES (?, ?, ?, ?, ?, ?)`,
			itemID, refundID, allocID, invoiceID, int64(line.amount), line.sequence)
		if err != nil {
			return "", wrapError(KindStorage, "insert refund item", err)
		}
	}
	return refundID, nil
}

func effectiveRefundAmount(in CreateRefundInput) Money {
	if !in.Amount.IsZero() {
		return in.Amount
	}
	var sum Money
	for _, ins := range in.Instructions {
		sum = sum.Add(ins.Amount)
	}
	return sum
}

// fpPair 是指纹计算用的 (ID, 金额) 对。
type fpPair struct {
	id  string
	amt int64
}

// refundFingerprint 归一化撤销请求内容，用于外部撤销号的幂等判定。
// 逐项模式只记录 (分配ID, 冲回金额) 有序对，与指令书写顺序无关；
// FIFO 模式只记录总额（具体冲回哪些分配由当时账本唯一决定）。
func refundFingerprint(paymentID string, itemized bool, amount Money, pairs []fpPair) string {
	h := sha256.New()
	fmt.Fprintf(h, "v1|payment=%s|itemized=%t|amount=%d", paymentID, itemized, int64(amount))
	sorted := append([]fpPair(nil), pairs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].id != sorted[j].id {
			return sorted[i].id < sorted[j].id
		}
		return sorted[i].amt < sorted[j].amt
	})
	for _, p := range sorted {
		fmt.Fprintf(h, "|%s=%d", p.id, p.amt)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// requestedRefundFingerprint 由调用方输入构造指纹。
func requestedRefundFingerprint(in CreateRefundInput, paymentID string) string {
	itemized := len(in.Instructions) > 0
	pairs := make([]fpPair, 0, len(in.Instructions))
	for _, ins := range in.Instructions {
		pairs = append(pairs, fpPair{id: strings.TrimSpace(ins.AllocationID), amt: int64(ins.Amount)})
	}
	return refundFingerprint(paymentID, itemized, effectiveRefundAmount(in), pairs)
}

// plannedRefundFingerprint 由事务内解析出的冲回计划构造指纹，
// 与 requestedRefundFingerprint 对同一逻辑请求必然一致。
func plannedRefundFingerprint(paymentID string, itemized bool, total Money, lines []refundLine) string {
	pairs := make([]fpPair, 0, len(lines))
	if itemized {
		for _, l := range lines {
			pairs = append(pairs, fpPair{id: l.allocationID, amt: int64(l.amount)})
		}
	}
	return refundFingerprint(paymentID, itemized, total, pairs)
}

// =====================================================================
// 查询
// =====================================================================

// GetInvoice 返回发票当前状态。
func (s *Service) GetInvoice(ctx context.Context, id string) (Invoice, error) {
	var inv Invoice
	err := s.store.db.QueryRowContext(ctx, invoiceSelect+` WHERE id = ?`, id).
		Scan(invoiceScanDest(&inv)...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Invoice{}, kindError(KindNotFound, "invoice %q not found", id)
		}
		return Invoice{}, wrapError(KindStorage, "get invoice", err)
	}
	return inv, nil
}

// GetInvoiceByNumber 按发票号查询。
func (s *Service) GetInvoiceByNumber(ctx context.Context, number string) (Invoice, error) {
	var inv Invoice
	err := s.store.db.QueryRowContext(ctx, invoiceSelect+` WHERE number = ?`, strings.TrimSpace(number)).
		Scan(invoiceScanDest(&inv)...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Invoice{}, kindError(KindNotFound, "invoice %q not found", number)
		}
		return Invoice{}, wrapError(KindStorage, "get invoice by number", err)
	}
	return inv, nil
}

// InvoiceBalance 返回发票余额快照。
func (s *Service) InvoiceBalance(ctx context.Context, id string) (InvoiceBalance, error) {
	inv, err := s.GetInvoice(ctx, id)
	if err != nil {
		return InvoiceBalance{}, err
	}
	return InvoiceBalance{
		InvoiceID:  inv.ID,
		Number:     inv.Number,
		CustomerID: inv.CustomerID,
		Amount:     inv.Amount,
		PaidAmount: inv.PaidAmount,
		OpenAmount: inv.OpenAmount(),
		Status:     inv.Status,
		Version:    inv.Version,
	}, nil
}

// ListInvoices 列出发票，可按客户过滤（空字符串=全部），按到期日升序。
func (s *Service) ListInvoices(ctx context.Context, customerID string) ([]Invoice, error) {
	q := invoiceSelect
	args := []any{}
	if customerID != "" {
		q += ` WHERE customer_id = ?`
		args = append(args, strings.TrimSpace(customerID))
	}
	q += ` ORDER BY due_date ASC, number ASC, id ASC`
	rows, err := s.store.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, wrapError(KindStorage, "list invoices", err)
	}
	defer rows.Close()
	var out []Invoice
	for rows.Next() {
		var inv Invoice
		if err := rows.Scan(invoiceScanDest(&inv)...); err != nil {
			return nil, wrapError(KindStorage, "scan invoice", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// GetPayment 按内部 ID 查询收款。
func (s *Service) GetPayment(ctx context.Context, id string) (Payment, error) {
	row, err := getPaymentByID(ctx, s.store.db, id)
	if err != nil {
		return Payment{}, err
	}
	return row.toModel(), nil
}

// GetPaymentByExternalNo 按外部收款号查询。
func (s *Service) GetPaymentByExternalNo(ctx context.Context, no string) (Payment, error) {
	row, err := getPaymentByExternalNo(ctx, s.store.db, strings.TrimSpace(no))
	if err != nil {
		return Payment{}, err
	}
	return row.toModel(), nil
}

// ListAllocations 返回某笔收款的分配历史（按顺序）。
// RemainingAmount 反映截至查询时每条分配尚未冲回的金额。
func (s *Service) ListAllocations(ctx context.Context, paymentID string) ([]Allocation, error) {
	rows, err := s.store.db.QueryContext(ctx, `
SELECT a.id, a.payment_id, a.invoice_id, i.number, a.amount, a.remaining_amount, a.sequence, a.created_at
FROM allocations a
JOIN invoices i ON i.id = a.invoice_id
WHERE a.payment_id = ?
ORDER BY a.sequence ASC, a.id ASC`, paymentID)
	if err != nil {
		return nil, wrapError(KindStorage, "list allocations", err)
	}
	defer rows.Close()
	var out []Allocation
	for rows.Next() {
		var a Allocation
		var createdAt string
		if err := rows.Scan(&a.ID, &a.PaymentID, &a.InvoiceID, &a.InvoiceNumber,
			(*int64)(&a.Amount), (*int64)(&a.RemainingAmount), &a.Sequence, &createdAt); err != nil {
			return nil, wrapError(KindStorage, "scan allocation", err)
		}
		a.CreatedAt = parseTimestamp(createdAt)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListRefunds 列出某笔收款的全部撤销（按时间升序）。
func (s *Service) ListRefunds(ctx context.Context, paymentID string) ([]Refund, error) {
	rows, err := s.store.db.QueryContext(ctx, `
SELECT id, external_no, payment_id, amount, reason, itemized, created_at
FROM refunds WHERE payment_id = ? ORDER BY created_at ASC, id ASC`, paymentID)
	if err != nil {
		return nil, wrapError(KindStorage, "list refunds", err)
	}
	defer rows.Close()
	var out []Refund
	for rows.Next() {
		rf, err := scanRefund(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rf)
	}
	return out, rows.Err()
}

// ListRefundItems 返回某次撤销的逐项冲回明细。
func (s *Service) ListRefundItems(ctx context.Context, refundID string) ([]RefundItem, error) {
	rows, err := s.store.db.QueryContext(ctx, `
SELECT id, refund_id, allocation_id, invoice_id, amount, sequence
FROM refund_items WHERE refund_id = ? ORDER BY sequence ASC, id ASC`, refundID)
	if err != nil {
		return nil, wrapError(KindStorage, "list refund items", err)
	}
	defer rows.Close()
	var out []RefundItem
	for rows.Next() {
		var it RefundItem
		var allocID, invoiceID sql.NullString
		if err := rows.Scan(&it.ID, &it.RefundID, &allocID, &invoiceID,
			(*int64)(&it.Amount), &it.Sequence); err != nil {
			return nil, wrapError(KindStorage, "scan refund item", err)
		}
		it.AllocationID = allocID.String
		it.InvoiceID = invoiceID.String
		out = append(out, it)
	}
	return out, rows.Err()
}

// ListInvoiceLedger 返回某张发票的账本流水（分配为正、冲回为负），
// BalanceAfter 为该发票在每条流水后的累计已付金额。
func (s *Service) ListInvoiceLedger(ctx context.Context, invoiceID string) ([]LedgerEntry, error) {
	return s.queryLedger(ctx, invoiceID)
}

func (s *Service) queryLedger(ctx context.Context, invoiceID string) ([]LedgerEntry, error) {
	rows, err := s.store.db.QueryContext(ctx, ledgerSQL, invoiceID, invoiceID)
	if err != nil {
		return nil, wrapError(KindStorage, "query invoice ledger", err)
	}
	defer rows.Close()
	var out []LedgerEntry
	var balance int64
	for rows.Next() {
		var (
			kind, at, refID, paymentID, paymentNo string
			refundID                              sql.NullString
			amount                                int64
		)
		if err := rows.Scan(&kind, &at, &refID, &paymentID, &paymentNo, &refundID, &amount); err != nil {
			return nil, wrapError(KindStorage, "scan ledger entry", err)
		}
		balance += amount
		out = append(out, LedgerEntry{
			At:           parseTimestamp(at),
			InvoiceID:    invoiceID,
			PaymentID:    paymentID,
			PaymentNo:    paymentNo,
			RefundID:     refundID.String,
			Kind:         kind,
			Amount:       Money(amount),
			BalanceAfter: Money(balance),
		})
	}
	return out, rows.Err()
}

const ledgerSQL = `
SELECT kind, at, ref_id, payment_id, payment_no, refund_id, amount FROM (
    SELECT 'allocation' AS kind,
           a.created_at AS at,
           a.id AS ref_id,
           a.payment_id AS payment_id,
           p.external_no AS payment_no,
           NULL AS refund_id,
           a.amount AS amount,
           a.created_at AS sort_at, a.id AS sort_id
    FROM allocations a
    JOIN payments p ON p.id = a.payment_id
    WHERE a.invoice_id = ?
    UNION ALL
    SELECT 'refund' AS kind,
           rf.created_at AS at,
           ri.id AS ref_id,
           p.id AS payment_id,
           p.external_no AS payment_no,
           rf.id AS refund_id,
           -ri.amount AS amount,
           rf.created_at AS sort_at, ri.id AS sort_id
    FROM refund_items ri
    JOIN refunds rf ON rf.id = ri.refund_id
    JOIN payments p ON p.id = rf.payment_id
    WHERE ri.invoice_id = ?
)
ORDER BY sort_at ASC, sort_id ASC`

const invoiceSelect = `
SELECT id, number, customer_id, issue_date, due_date, amount, paid_amount, status, version, created_at
FROM invoices`

type rowScanner interface {
	Scan(dest ...any) error
}

type invoiceRow struct {
	id, number, customerID, issueDate, dueDate, status, createdAt string
	amount, paidAmount                                            int64
	version                                                       int64
}

func scanInvoice(r rowScanner) (Invoice, error) {
	var row invoiceRow
	if err := r.Scan(&row.id, &row.number, &row.customerID, &row.issueDate, &row.dueDate,
		&row.amount, &row.paidAmount, &row.status, &row.version, &row.createdAt); err != nil {
		return Invoice{}, wrapError(KindStorage, "scan invoice", err)
	}
	return Invoice{
		ID:         row.id,
		Number:     row.number,
		CustomerID: row.customerID,
		IssueDate:  parseDate(row.issueDate),
		DueDate:    parseDate(row.dueDate),
		Amount:     Money(row.amount),
		PaidAmount: Money(row.paidAmount),
		Status:     InvoiceStatus(row.status),
		Version:    row.version,
		CreatedAt:  parseTimestamp(row.createdAt),
	}, nil
}

func invoiceScanDest(inv *Invoice) []any {
	issue := dateField{t: &inv.IssueDate}
	due := dateField{t: &inv.DueDate}
	status := statusField{s: &inv.Status}
	created := timestampField{t: &inv.CreatedAt}
	return []any{
		&inv.ID, &inv.Number, &inv.CustomerID,
		&issue, &due,
		(*int64)(&inv.Amount), (*int64)(&inv.PaidAmount),
		&status, &inv.Version, &created,
	}
}

// 以下小类型把 TEXT 列直接扫描进 time.Time / InvoiceStatus。
type dateField struct{ t *time.Time }

func (d dateField) Scan(src any) error { *d.t = parseDate(stringFrom(src)); return nil }

type timestampField struct{ t *time.Time }

func (d timestampField) Scan(src any) error { *d.t = parseTimestamp(stringFrom(src)); return nil }

type statusField struct{ s *InvoiceStatus }

func (d statusField) Scan(src any) error { *d.s = InvoiceStatus(stringFrom(src)); return nil }

func stringFrom(src any) string {
	switch v := src.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprintf("%v", src)
	}
}

// lockInvoicesByIDs 在事务中按 ID 集合读取发票（写事务本身已持锁）。
func lockInvoicesByIDs(ctx context.Context, tx *sql.Tx, ids []string) ([]invoiceRow, error) {
	q := `SELECT id, number, customer_id, issue_date, due_date, amount, paid_amount, status, version, created_at
FROM invoices WHERE id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, wrapError(KindStorage, "query invoices by ids", err)
	}
	defer rows.Close()
	var out []invoiceRow
	for rows.Next() {
		var r invoiceRow
		if err := rows.Scan(&r.id, &r.number, &r.customerID, &r.issueDate, &r.dueDate,
			&r.amount, &r.paidAmount, &r.status, &r.version, &r.createdAt); err != nil {
			return nil, wrapError(KindStorage, "scan invoice", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// paymentRow 是 payments 表的内部行表示。
type paymentRow struct {
	id, externalNo, customerID, mode, createdAt                string
	amount, allocatedAmount, refundedAmount, unallocatedAmount int64
	version                                                    int64
	fingerprint                                                string
}

const paymentColumns = `id, external_no, customer_id, amount, allocation_mode,
	allocated_amount, refunded_amount, unallocated_amount, fingerprint, created_at, version`

func scanPaymentRow(r rowScanner) (paymentRow, error) {
	var p paymentRow
	err := r.Scan(&p.id, &p.externalNo, &p.customerID, &p.amount, &p.mode,
		&p.allocatedAmount, &p.refundedAmount, &p.unallocatedAmount,
		&p.fingerprint, &p.createdAt, &p.version)
	if err != nil {
		return paymentRow{}, wrapError(KindStorage, "scan payment", err)
	}
	return p, nil
}

func (p paymentRow) toModel() Payment {
	return Payment{
		ID:                p.id,
		ExternalNo:        p.externalNo,
		CustomerID:        p.customerID,
		Amount:            Money(p.amount),
		AllocationMode:    AllocationMode(p.mode),
		AllocatedAmount:   Money(p.allocatedAmount),
		RefundedAmount:    Money(p.refundedAmount),
		UnallocatedAmount: Money(p.unallocatedAmount),
		CreatedAt:         parseTimestamp(p.createdAt),
		Version:           p.version,
	}
}

func getPaymentByExternalNoTx(ctx context.Context, tx *sql.Tx, no string) (paymentRow, error) {
	return scanPaymentRow(tx.QueryRowContext(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE external_no = ?`, no))
}

func getPaymentByIDTx(ctx context.Context, tx *sql.Tx, id string) (paymentRow, error) {
	return scanPaymentRow(tx.QueryRowContext(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE id = ?`, id))
}

func getPaymentByID(ctx context.Context, db *sql.DB, id string) (paymentRow, error) {
	p, err := scanPaymentRow(db.QueryRowContext(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE id = ?`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return paymentRow{}, kindError(KindNotFound, "payment %q not found", id)
		}
		return paymentRow{}, err
	}
	return p, nil
}

func getPaymentByExternalNo(ctx context.Context, db *sql.DB, no string) (paymentRow, error) {
	p, err := scanPaymentRow(db.QueryRowContext(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE external_no = ?`, no))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return paymentRow{}, kindError(KindNotFound, "payment %q not found", no)
		}
		return paymentRow{}, err
	}
	return p, nil
}

func scanRefund(r rowScanner) (Refund, error) {
	var row struct {
		id, externalNo, paymentID, reason, createdAt string
		amount                                       int64
		itemized                                     int64
	}
	var ext sql.NullString
	if err := r.Scan(&row.id, &ext, &row.paymentID, &row.amount, &row.reason,
		&row.itemized, &row.createdAt); err != nil {
		return Refund{}, wrapError(KindStorage, "scan refund", err)
	}
	return Refund{
		ID:         row.id,
		ExternalNo: ext.String,
		PaymentID:  row.paymentID,
		Amount:     Money(row.amount),
		Reason:     row.reason,
		Itemized:   row.itemized != 0,
		CreatedAt:  parseTimestamp(row.createdAt),
	}, nil
}

func getRefundByExternalNoTx(ctx context.Context, tx *sql.Tx, no string) (refundRowFull, error) {
	var r refundRowFull
	var ext sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT id, external_no, payment_id, amount, reason, itemized, fingerprint, created_at
FROM refunds WHERE external_no = ?`, no).
		Scan(&r.id, &ext, &r.paymentID, &r.amount, &r.reason, &r.itemized, &r.fingerprint, &r.createdAt)
	if err != nil {
		return refundRowFull{}, err
	}
	r.externalNo = ext.String
	return r, nil
}

type refundRowFull struct {
	id, externalNo, paymentID, reason, fingerprint, createdAt string
	amount                                                    int64
	itemized                                                  int64
}

func loadPaymentResult(ctx context.Context, tx *sql.Tx, p paymentRow, out *RegisterPaymentResult) error {
	allocs, err := listAllocationsTx(ctx, tx, p.id)
	if err != nil {
		return err
	}
	*out = RegisterPaymentResult{Payment: p.toModel(), Allocations: allocs, Replayed: true}
	return nil
}

func reloadPaymentResult(ctx context.Context, tx *sql.Tx, externalNo string) (Payment, []Allocation, error) {
	p, err := getPaymentByExternalNoTx(ctx, tx, externalNo)
	if err != nil {
		return Payment{}, nil, err
	}
	allocs, err := listAllocationsTx(ctx, tx, p.id)
	if err != nil {
		return Payment{}, nil, err
	}
	return p.toModel(), allocs, nil
}

func listAllocationsTx(ctx context.Context, tx *sql.Tx, paymentID string) ([]Allocation, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT a.id, a.payment_id, a.invoice_id, i.number, a.amount, a.remaining_amount, a.sequence, a.created_at
FROM allocations a
JOIN invoices i ON i.id = a.invoice_id
WHERE a.payment_id = ?
ORDER BY a.sequence ASC, a.id ASC`, paymentID)
	if err != nil {
		return nil, wrapError(KindStorage, "list allocations tx", err)
	}
	defer rows.Close()
	var out []Allocation
	for rows.Next() {
		var a Allocation
		var createdAt string
		if err := rows.Scan(&a.ID, &a.PaymentID, &a.InvoiceID, &a.InvoiceNumber,
			(*int64)(&a.Amount), (*int64)(&a.RemainingAmount), &a.Sequence, &createdAt); err != nil {
			return nil, wrapError(KindStorage, "scan allocation", err)
		}
		a.CreatedAt = parseTimestamp(createdAt)
		out = append(out, a)
	}
	return out, rows.Err()
}

func loadRefundResult(ctx context.Context, tx *sql.Tx, refundID string, replayed bool, out *CreateRefundResult) error {
	var r refundRowFull
	var ext sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT id, external_no, payment_id, amount, reason, itemized, fingerprint, created_at
FROM refunds WHERE id = ?`, refundID).
		Scan(&r.id, &ext, &r.paymentID, &r.amount, &r.reason, &r.itemized, &r.fingerprint, &r.createdAt)
	if err != nil {
		return wrapError(KindStorage, "load refund", err)
	}
	items, err := listRefundItemsTx(ctx, tx, refundID)
	if err != nil {
		return err
	}
	*out = CreateRefundResult{
		Refund: Refund{
			ID:         r.id,
			ExternalNo: ext.String,
			PaymentID:  r.paymentID,
			Amount:     Money(r.amount),
			Reason:     r.reason,
			Itemized:   r.itemized != 0,
			CreatedAt:  parseTimestamp(r.createdAt),
		},
		Items:    items,
		Replayed: replayed,
	}
	return nil
}

func listRefundItemsTx(ctx context.Context, tx *sql.Tx, refundID string) ([]RefundItem, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT id, refund_id, allocation_id, invoice_id, amount, sequence
FROM refund_items WHERE refund_id = ? ORDER BY sequence ASC, id ASC`, refundID)
	if err != nil {
		return nil, wrapError(KindStorage, "list refund items tx", err)
	}
	defer rows.Close()
	var out []RefundItem
	for rows.Next() {
		var it RefundItem
		var allocID, invoiceID sql.NullString
		if err := rows.Scan(&it.ID, &it.RefundID, &allocID, &invoiceID,
			(*int64)(&it.Amount), &it.Sequence); err != nil {
			return nil, wrapError(KindStorage, "scan refund item", err)
		}
		it.AllocationID = allocID.String
		it.InvoiceID = invoiceID.String
		out = append(out, it)
	}
	return out, rows.Err()
}

func normalizeDate(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback.UTC().Truncate(24 * time.Hour)
	}
	return t.UTC()
}

func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
