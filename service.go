package goinvoicecollection

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Service 在 Store 之上实现发票、收款、撤销与台账查询的全部业务规则。
type Service struct {
	store *Store
	now   func() time.Time
}

// NewService 创建绑定到指定 Store 的服务。
func NewService(store *Store) *Service {
	return &Service{store: store, now: time.Now}
}

// CreateInvoiceInput 是登记应收发票的入参。
type CreateInvoiceInput struct {
	// ID 可空，为空时系统生成。Number 必填且唯一。
	ID     string
	Number string
	Total  Money
	DueAt  time.Time
}

// PaymentAllocationInput 指定把多少金额分配给哪张发票（手工分配）。
type PaymentAllocationInput struct {
	InvoiceID string
	Amount    Money
}

// RegisterPaymentInput 登记收款的入参。
//
// Mode = AllocManual 时使用 Allocations（可为空，表示整笔留作未分配余款）；
// Mode = AllocAuto 时系统忽略 Allocations，按发票到期日从早到晚自动分配，
// 分配不完的金额显式保留为 Payment.Unallocated。
type RegisterPaymentInput struct {
	// ID 可空，为空时系统生成。ExternalNo 必填，作为幂等键。
	ID          string
	ExternalNo  string
	Amount      Money
	Mode        AllocationMode
	Allocations []PaymentAllocationInput
}

// ReversalItemInput 指定对原收款的某条分配冲回多少。
// AllocationID 与 InvoiceID 二选一即可（同一收款对每张发票只有一条分配）。
type ReversalItemInput struct {
	AllocationID string
	InvoiceID    string
	Amount       Money
}

// ReversePaymentInput 撤销原收款的入参。
//
// 三种等价用法：
//   - 不给 Items 且 Amount 为 0：全额撤销，逐项冲回当时所有尚未撤销的分配；
//   - 不给 Items 但 Amount > 0：按原分配顺序冲回该金额（FIFO）；
//   - 显式给出 Items：逐项指定冲回金额（多次部分撤销的典型用法）。
type ReversePaymentInput struct {
	// ID 可空，为空时系统生成。ReversalNo 必填，作为撤销幂等键。
	ID         string
	ReversalNo string
	PaymentID  string
	Amount     Money
	Items      []ReversalItemInput
}

// RegisterResult 是登记收款的返回。
type RegisterResult struct {
	Payment     *Payment
	Allocations []Allocation
	// Replayed 为 true 表示该外部收款号曾以相同内容登记过，本次返回首次记录。
	Replayed bool
}

// CreateInvoice 登记一张应收发票。
func (s *Service) CreateInvoice(ctx context.Context, in CreateInvoiceInput) (*Invoice, error) {
	if strings.TrimSpace(in.Number) == "" {
		return nil, fmt.Errorf("%w: invoice number is required", ErrInvalidInput)
	}
	if in.Total <= 0 {
		return nil, fmt.Errorf("%w: invoice total must be positive", ErrInvalidInput)
	}
	if in.DueAt.IsZero() {
		return nil, fmt.Errorf("%w: invoice due time is required", ErrInvalidInput)
	}
	id := in.ID
	if id == "" {
		id = newID("inv")
	}
	now := s.now().UTC()

	tx, err := s.store.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.rollback(ctx)

	_, err = tx.ExecContext(ctx,
		`INSERT INTO invoices (id, number, total, paid, due_at, created_at)
		 VALUES (?, ?, ?, 0, ?, ?)`,
		id, in.Number, in.Total.Cents(), in.DueAt.UTC().Format(time.RFC3339Nano),
		now.Format(time.RFC3339Nano))
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := tx.commit(ctx); err != nil {
		return nil, err
	}
	return &Invoice{
		ID: id, Number: in.Number, Total: in.Total,
		DueAt: in.DueAt.UTC(), CreatedAt: now,
	}, nil
}

// RegisterPayment 登记收款并一次性整体写入全部发票分配。
func (s *Service) RegisterPayment(ctx context.Context, in RegisterPaymentInput) (*RegisterResult, error) {
	if strings.TrimSpace(in.ExternalNo) == "" {
		return nil, fmt.Errorf("%w: external payment number is required", ErrInvalidInput)
	}
	if in.Amount <= 0 {
		return nil, fmt.Errorf("%w: payment amount must be positive", ErrInvalidInput)
	}
	if in.Mode != AllocManual && in.Mode != AllocAuto {
		return nil, fmt.Errorf("%w: allocation mode must be manual or auto", ErrInvalidInput)
	}

	tx, err := s.store.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.rollback(ctx)

	// 幂等：同号先查，命中则核对内容指纹，相同则重放，不同则冲突。
	if existing, err := loadPaymentByExternalNo(ctx, tx, in.ExternalNo); err == nil {
		fp := paymentFingerprint(in.Amount, in.Mode, in.Allocations)
		if existing.fingerprint != fp {
			return nil, fmt.Errorf("%w: payment %q already registered with different amount or allocation",
				ErrConflict, in.ExternalNo)
		}
		p, allocs, err := loadPaymentDetail(ctx, tx, existing.id)
		if err != nil {
			return nil, err
		}
		return &RegisterResult{Payment: p, Allocations: allocs, Replayed: true}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	// 计算本次分配计划。
	plan, err := s.buildAllocationPlan(ctx, tx, in)
	if err != nil {
		return nil, err
	}

	id := in.ID
	if id == "" {
		id = newID("pay")
	}
	now := s.now().UTC()
	fp := paymentFingerprint(in.Amount, in.Mode, in.Allocations)

	// 先写收款（allocated=0），再逐行写分配；触发器负责累计与余额推进，
	// 任何一行超额都会 ABORT 整个事务，保证"整体写入"——不存在半成功状态。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO payments (id, external_no, amount, allocated, reversed, unallocated, mode, fingerprint, created_at)
		 VALUES (?, ?, ?, 0, 0, ?, ?, ?, ?)`,
		id, in.ExternalNo, in.Amount.Cents(), in.Amount.Cents(), string(in.Mode), fp,
		now.Format(time.RFC3339Nano)); err != nil {
		return nil, mapDBError(err)
	}

	for i, item := range plan {
		aid := newID("alc")
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO allocations (id, payment_id, invoice_id, amount, reversed, seq, created_at)
			 VALUES (?, ?, ?, ?, 0, ?, ?)`,
			aid, id, item.InvoiceID, item.Amount.Cents(), i+1,
			now.Format(time.RFC3339Nano)); err != nil {
			return nil, mapDBError(err)
		}
	}

	if err := tx.commit(ctx); err != nil {
		return nil, err
	}

	p, allocs, err := loadPaymentDetail(ctx, s.store, id)
	if err != nil {
		return nil, err
	}
	return &RegisterResult{Payment: p, Allocations: allocs, Replayed: false}, nil
}

// buildAllocationPlan 在事务内校验/生成分配计划。
func (s *Service) buildAllocationPlan(ctx context.Context, e execer, in RegisterPaymentInput) ([]PaymentAllocationInput, error) {
	if in.Mode == AllocManual {
		return buildManualPlan(ctx, e, in.Amount, in.Allocations)
	}
	return buildAutoPlan(ctx, e, in.Amount)
}

func buildManualPlan(ctx context.Context, e execer, total Money, reqs []PaymentAllocationInput) ([]PaymentAllocationInput, error) {
	plan := make([]PaymentAllocationInput, 0, len(reqs))
	seen := make(map[string]struct{}, len(reqs))
	var sum Money
	for _, r := range reqs {
		if strings.TrimSpace(r.InvoiceID) == "" {
			return nil, fmt.Errorf("%w: allocation invoice id is required", ErrInvalidInput)
		}
		if r.Amount <= 0 {
			return nil, fmt.Errorf("%w: allocation amount must be positive", ErrInvalidInput)
		}
		if _, dup := seen[r.InvoiceID]; dup {
			return nil, fmt.Errorf("%w: duplicate allocation for invoice %q", ErrInvalidInput, r.InvoiceID)
		}
		seen[r.InvoiceID] = struct{}{}
		sum += r.Amount
		if sum > total {
			return nil, fmt.Errorf("%w: allocations total %s exceeds payment %s",
				ErrExceedsPayment, sum, total)
		}
		plan = append(plan, r)
	}
	// 逐张发票核对未付余额（行锁在 BEGIN IMMEDIATE 下由全库写锁覆盖）。
	for _, r := range plan {
		inv, err := loadInvoice(ctx, e, r.InvoiceID)
		if err != nil {
			return nil, err
		}
		if r.Amount > inv.Balance() {
			return nil, fmt.Errorf("%w: invoice %q balance %s, tried to allocate %s",
				ErrExceedsInvoice, r.InvoiceID, inv.Balance(), r.Amount)
		}
	}
	return plan, nil
}

func buildAutoPlan(ctx context.Context, e execer, total Money) ([]PaymentAllocationInput, error) {
	// 到期早的先分；到期相同按发票 ID 排序，保证结果确定。只考虑尚有余额的发票。
	rows, err := e.QueryContext(ctx,
		`SELECT id, total, paid FROM invoices
		  WHERE paid < total
		  ORDER BY due_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	plan := make([]PaymentAllocationInput, 0)
	remaining := total
	for rows.Next() {
		if remaining <= 0 {
			break
		}
		var id string
		var totalC, paidC int64
		if err := rows.Scan(&id, &totalC, &paidC); err != nil {
			return nil, err
		}
		balance := Money(totalC) - Money(paidC)
		take := balance
		if take > remaining {
			take = remaining
		}
		plan = append(plan, PaymentAllocationInput{InvoiceID: id, Amount: take})
		remaining -= take
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return plan, nil
}

// ReversePayment 引用原收款逐项冲回当时的分配。
func (s *Service) ReversePayment(ctx context.Context, in ReversePaymentInput) (*Reversal, error) {
	if strings.TrimSpace(in.ReversalNo) == "" {
		return nil, fmt.Errorf("%w: reversal number is required", ErrInvalidInput)
	}
	if strings.TrimSpace(in.PaymentID) == "" {
		return nil, fmt.Errorf("%w: original payment id is required", ErrInvalidInput)
	}
	if in.Amount < 0 {
		return nil, fmt.Errorf("%w: reversal amount cannot be negative", ErrInvalidInput)
	}

	tx, err := s.store.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.rollback(ctx)

	// 撤销幂等：同号先查。指纹只基于请求内容（原收款 + 金额/逐项条目），
	// 不依赖当前可撤销余额，因此整笔撤销后用同号重放仍能命中首次记录。
	fp := reversalRequestFingerprint(in)
	if existing, err := loadReversalByNo(ctx, tx, in.ReversalNo); err == nil {
		if existing.fingerprint != fp {
			return nil, fmt.Errorf("%w: reversal %q already exists with different content",
				ErrConflict, in.ReversalNo)
		}
		return loadReversalDetail(ctx, tx, existing.id)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	// 首次提交：解析冲回计划并校验可撤销余额、已撤销部分不可重复使用。
	plan, err := s.resolveReversalPlan(ctx, tx, in)
	if err != nil {
		return nil, err
	}

	id := in.ID
	if id == "" {
		id = newID("rev")
	}
	now := s.now().UTC()
	var total Money
	for _, p := range plan {
		total += p.Amount
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO reversals (id, reversal_no, payment_id, amount, fingerprint, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, in.ReversalNo, in.PaymentID, total.Cents(), fp, now.Format(time.RFC3339Nano)); err != nil {
		return nil, mapDBError(err)
	}
	for i, item := range plan {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO reversal_items (id, reversal_id, allocation_id, amount, seq, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			newID("rit"), id, item.AllocationID, item.Amount.Cents(), i+1,
			now.Format(time.RFC3339Nano)); err != nil {
			return nil, mapDBError(err)
		}
	}

	if err := tx.commit(ctx); err != nil {
		return nil, err
	}
	return loadReversalDetail(ctx, s.store, id)
}

type reversalPlanItem struct {
	AllocationID string
	InvoiceID    string
	Amount       Money
}

// resolveReversalPlan 把三种入参形式统一解析为针对原分配的逐项冲回计划，
// 并在应用层保证：累计不超过原收款的可撤销余额、已撤销部分不能重复使用。
func (s *Service) resolveReversalPlan(ctx context.Context, e execer, in ReversePaymentInput) ([]reversalPlanItem, error) {
	var exists int
	if err := e.QueryRowContext(ctx,
		`SELECT 1 FROM payments WHERE id = ?`, in.PaymentID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: payment %q", ErrNotFound, in.PaymentID)
		}
		return nil, err
	}
	allocs, err := loadAllocations(ctx, e, in.PaymentID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Allocation, len(allocs))
	byInvoice := make(map[string]Allocation, len(allocs))
	var reversibleTotal Money
	for _, a := range allocs {
		byID[a.ID] = a
		byInvoice[a.InvoiceID] = a
		reversibleTotal += a.Effective()
	}
	if reversibleTotal <= 0 {
		return nil, fmt.Errorf("%w: payment %q has no reversible allocation left",
			ErrAlreadyReversed, in.PaymentID)
	}

	switch {
	case len(in.Items) > 0:
		plan := make([]reversalPlanItem, 0, len(in.Items))
		seen := make(map[string]struct{}, len(in.Items))
		var sum Money
		for _, it := range in.Items {
			if it.Amount <= 0 {
				return nil, fmt.Errorf("%w: reversal item amount must be positive", ErrInvalidInput)
			}
			a, err := findReversalAllocation(byID, byInvoice, it)
			if err != nil {
				return nil, err
			}
			if _, dup := seen[a.ID]; dup {
				return nil, fmt.Errorf("%w: duplicate reversal item for allocation %q",
					ErrInvalidInput, a.ID)
			}
			seen[a.ID] = struct{}{}
			// 先判断该条分配是否在重复使用已撤销部分，再判断累计超额。
			if it.Amount > a.Effective() {
				return nil, fmt.Errorf("%w: allocation %q already reversed %s of %s",
					ErrAlreadyReversed, a.ID, a.Reversed, a.Amount)
			}
			sum += it.Amount
			if sum > reversibleTotal {
				return nil, fmt.Errorf("%w: reversals total %s exceeds reversible %s",
					ErrReversalExceeds, sum, reversibleTotal)
			}
			plan = append(plan, reversalPlanItem{
				AllocationID: a.ID, InvoiceID: a.InvoiceID, Amount: it.Amount,
			})
		}
		if len(plan) == 0 {
			return nil, fmt.Errorf("%w: reversal items are empty", ErrInvalidInput)
		}
		return plan, nil

	case in.Amount > 0:
		if in.Amount > reversibleTotal {
			return nil, fmt.Errorf("%w: requested %s exceeds reversible %s",
				ErrReversalExceeds, in.Amount, reversibleTotal)
		}
		// 按原分配顺序（seq 从早到晚）逐项冲回，单笔分配可能只冲回一部分。
		sort.Slice(allocs, func(i, j int) bool { return allocs[i].Seq < allocs[j].Seq })
		plan := make([]reversalPlanItem, 0)
		remaining := in.Amount
		for _, a := range allocs {
			if remaining <= 0 {
				break
			}
			avail := a.Effective()
			if avail <= 0 {
				continue
			}
			take := avail
			if take > remaining {
				take = remaining
			}
			plan = append(plan, reversalPlanItem{
				AllocationID: a.ID, InvoiceID: a.InvoiceID, Amount: take,
			})
			remaining -= take
		}
		return plan, nil

	default:
		// 全额撤销：逐项冲回所有尚未撤销的分配。
		plan := make([]reversalPlanItem, 0, len(allocs))
		sort.Slice(allocs, func(i, j int) bool { return allocs[i].Seq < allocs[j].Seq })
		for _, a := range allocs {
			if a.Effective() > 0 {
				plan = append(plan, reversalPlanItem{
					AllocationID: a.ID, InvoiceID: a.InvoiceID, Amount: a.Effective(),
				})
			}
		}
		return plan, nil
	}
}

func findReversalAllocation(byID, byInvoice map[string]Allocation, it ReversalItemInput) (Allocation, error) {
	switch {
	case it.AllocationID != "":
		a, ok := byID[it.AllocationID]
		if !ok {
			return Allocation{}, fmt.Errorf("%w: allocation %q does not belong to this payment",
				ErrInvalidInput, it.AllocationID)
		}
		return a, nil
	case it.InvoiceID != "":
		a, ok := byInvoice[it.InvoiceID]
		if !ok {
			return Allocation{}, fmt.Errorf("%w: invoice %q has no allocation under this payment",
				ErrInvalidInput, it.InvoiceID)
		}
		return a, nil
	default:
		return Allocation{}, fmt.Errorf("%w: reversal item must specify allocation id or invoice id",
			ErrInvalidInput)
	}
}

// GetInvoice 查询发票（不存在返回 ErrNotFound）。
func (s *Service) GetInvoice(ctx context.Context, id string) (*Invoice, error) {
	return loadInvoice(ctx, s.store.db, id)
}

// GetInvoiceByNumber 按发票号查询。
func (s *Service) GetInvoiceByNumber(ctx context.Context, number string) (*Invoice, error) {
	return loadInvoiceBy(ctx, s.store.db, "number", number)
}

// GetPayment 查询收款及其全部分配。
func (s *Service) GetPayment(ctx context.Context, id string) (*Payment, []Allocation, error) {
	return loadPaymentDetail(ctx, s.store.db, id)
}

// GetPaymentByExternalNo 按外部收款号查询收款及其分配。
func (s *Service) GetPaymentByExternalNo(ctx context.Context, no string) (*Payment, []Allocation, error) {
	row, err := loadPaymentByExternalNo(ctx, s.store.db, no)
	if err != nil {
		return nil, nil, err
	}
	return loadPaymentDetail(ctx, s.store.db, row.id)
}

// GetReversal 查询撤销单及其逐项冲回明细。
func (s *Service) GetReversal(ctx context.Context, id string) (*Reversal, error) {
	return loadReversalDetail(ctx, s.store.db, id)
}

// GetReversalByNo 按撤销幂等号查询。
func (s *Service) GetReversalByNo(ctx context.Context, no string) (*Reversal, error) {
	row, err := loadReversalByNo(ctx, s.store.db, no)
	if err != nil {
		return nil, err
	}
	return loadReversalDetail(ctx, s.store.db, row.id)
}

// InvoiceHistory 返回某张发票不可变的分配/冲回台账，按发生时间排序。
// 每行的 InvoicePaidAfter 是按同一顺序重放后的累计已付金额，末行即当前已付。
func (s *Service) InvoiceHistory(ctx context.Context, invoiceID string) ([]LedgerEntry, error) {
	if _, err := loadInvoice(ctx, s.store.db, invoiceID); err != nil {
		return nil, err
	}
	const q = `
		SELECT kind, at, payment_id, external_no, reversal_id, reversal_no,
		       allocation_id, signed, seq FROM (
			SELECT 'allocation' AS kind, a.created_at AS at, p.id AS payment_id,
			       p.external_no AS external_no, '' AS reversal_id, '' AS reversal_no,
			       a.id AS allocation_id, a.amount AS signed, a.seq AS seq
			  FROM allocations a JOIN payments p ON p.id = a.payment_id
			 WHERE a.invoice_id = ?
			UNION ALL
			SELECT 'reversal' AS kind, ri.created_at AS at, p.id AS payment_id,
			       p.external_no AS external_no, r.id AS reversal_id, r.reversal_no AS reversal_no,
			       a.id AS allocation_id, -ri.amount AS signed, ri.seq AS seq
			  FROM reversal_items ri
			       JOIN allocations a ON a.id = ri.allocation_id
			       JOIN reversals r ON r.id = ri.reversal_id
			       JOIN payments p ON p.id = r.payment_id
			 WHERE a.invoice_id = ?
		) ORDER BY at ASC, kind ASC, seq ASC`
	rows, err := s.store.db.QueryContext(ctx, q, invoiceID, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]LedgerEntry, 0)
	var running Money
	var seq int
	for rows.Next() {
		var e LedgerEntry
		var kind, at string
		var signed int64
		if err := rows.Scan(&kind, &at, &e.PaymentID, &e.ExternalNo, &e.ReversalID,
			&e.ReversalNo, &e.AllocationID, &signed, &seq); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, err
		}
		e.Kind, e.At, e.Amount = kind, t, Money(signed)
		e.InvoiceID = invoiceID
		running += Money(signed)
		e.InvoicePaidAfter = running
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ---------------- 指纹（幂等冲突判定） ----------------

// paymentFingerprint 规范化摘要：同金额、同分配方式、同发票-金额集合才算相同。
// 手工条目的先后顺序不影响指纹（按发票 ID 规范化排序）。
func paymentFingerprint(amount Money, mode AllocationMode, reqs []PaymentAllocationInput) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|", mode, amount.Cents())
	if mode == AllocManual {
		pairs := make([]string, len(reqs))
		for i, r := range reqs {
			pairs[i] = fmt.Sprintf("%s:%d", r.InvoiceID, r.Amount.Cents())
		}
		sort.Strings(pairs)
		h.Write([]byte(strings.Join(pairs, ",")))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// reversalRequestFingerprint 规范化撤销请求摘要：原收款 + 金额（FIFO 形式）
// 或逐项的分配/发票-金额集合。只取决于请求本身，使其在撤销完成后的重放
// 仍然命中首次记录。
func reversalRequestFingerprint(in ReversePaymentInput) string {
	h := sha256.New()
	h.Write([]byte(in.PaymentID + "|"))
	if len(in.Items) > 0 {
		pairs := make([]string, len(in.Items))
		for i, it := range in.Items {
			key := it.AllocationID
			if key == "" {
				key = "inv:" + it.InvoiceID
			}
			pairs[i] = fmt.Sprintf("%s:%d", key, it.Amount.Cents())
		}
		sort.Strings(pairs)
		h.Write([]byte("items|" + strings.Join(pairs, ",")))
	} else {
		// Amount == 0 表示全额撤销；>0 表示按原顺序 FIFO 冲回该金额。
		fmt.Fprintf(h, "amount|%d", in.Amount.Cents())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ---------------- 行加载辅助 ----------------

type paymentRow struct {
	id          string
	fingerprint string
}

func loadPaymentByExternalNo(ctx context.Context, q queryer, no string) (paymentRow, error) {
	var r paymentRow
	err := q.QueryRowContext(ctx,
		`SELECT id, fingerprint FROM payments WHERE external_no = ?`, no).
		Scan(&r.id, &r.fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return paymentRow{}, ErrNotFound
	}
	return r, err
}

func loadReversalByNo(ctx context.Context, q queryer, no string) (paymentRow, error) {
	var r paymentRow
	err := q.QueryRowContext(ctx,
		`SELECT id, fingerprint FROM reversals WHERE reversal_no = ?`, no).
		Scan(&r.id, &r.fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return paymentRow{}, ErrNotFound
	}
	return r, err
}

const invoiceColumns = `id, number, total, paid, due_at, created_at`

func scanInvoice(sc interface{ Scan(...any) error }) (*Invoice, error) {
	var inv Invoice
	var dueAt, createdAt string
	var totalC, paidC int64
	if err := sc.Scan(&inv.ID, &inv.Number, &totalC, &paidC, &dueAt, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	inv.Total, inv.Paid = Money(totalC), Money(paidC)
	if t, err := time.Parse(time.RFC3339Nano, dueAt); err == nil {
		inv.DueAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		inv.CreatedAt = t
	}
	return &inv, nil
}

func loadInvoice(ctx context.Context, q queryer, id string) (*Invoice, error) {
	return loadInvoiceBy(ctx, q, "id", id)
}

func loadInvoiceBy(ctx context.Context, q queryer, col, val string) (*Invoice, error) {
	// col 是内部常量，不存在注入面。
	row := q.QueryRowContext(ctx,
		`SELECT `+invoiceColumns+` FROM invoices WHERE `+col+` = ?`, val)
	return scanInvoice(row)
}

const paymentColumns = `id, external_no, amount, allocated, reversed, unallocated, mode, created_at`

func scanPayment(sc interface{ Scan(...any) error }) (*Payment, error) {
	var p Payment
	var mode, createdAt string
	var amountC, allocatedC, reversedC, unallocatedC int64
	if err := sc.Scan(&p.ID, &p.ExternalNo, &amountC, &allocatedC, &reversedC,
		&unallocatedC, &mode, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.Amount = Money(amountC)
	p.Allocated = Money(allocatedC)
	p.Reversed = Money(reversedC)
	p.Unallocated = Money(unallocatedC)
	p.Mode = AllocationMode(mode)
	if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		p.CreatedAt = t
	}
	return &p, nil
}

func loadPaymentDetail(ctx context.Context, q queryer, id string) (*Payment, []Allocation, error) {
	p, err := scanPayment(q.QueryRowContext(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE id = ?`, id))
	if err != nil {
		return nil, nil, err
	}
	allocs, err := loadAllocations(ctx, q, id)
	if err != nil {
		return nil, nil, err
	}
	return p, allocs, nil
}

func loadAllocations(ctx context.Context, q queryer, paymentID string) ([]Allocation, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, payment_id, invoice_id, amount, reversed, seq, created_at
		   FROM allocations WHERE payment_id = ? ORDER BY seq ASC`, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Allocation, 0)
	for rows.Next() {
		var a Allocation
		var createdAt string
		var amountC, reversedC int64
		if err := rows.Scan(&a.ID, &a.PaymentID, &a.InvoiceID, &amountC,
			&reversedC, &a.Seq, &createdAt); err != nil {
			return nil, err
		}
		a.Amount, a.Reversed = Money(amountC), Money(reversedC)
		if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			a.CreatedAt = t
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 收款不存在（没有任何分配行，包括收款行本身不存在）时由调用方区分；
	// 这里仅返回空切片，支付存在性在调用点检查。
	return out, nil
}

func loadReversalDetail(ctx context.Context, q queryer, id string) (*Reversal, error) {
	var r Reversal
	var createdAt string
	var amountC int64
	err := q.QueryRowContext(ctx,
		`SELECT id, reversal_no, payment_id, amount, created_at
		   FROM reversals WHERE id = ?`, id).
		Scan(&r.ID, &r.ReversalNo, &r.PaymentID, &amountC, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Amount = Money(amountC)
	if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		r.CreatedAt = t
	}

	rows, err := q.QueryContext(ctx,
		`SELECT ri.id, ri.reversal_id, ri.allocation_id, a.invoice_id, ri.amount,
		        ri.seq, ri.created_at
		   FROM reversal_items ri JOIN allocations a ON a.id = ri.allocation_id
		  WHERE ri.reversal_id = ? ORDER BY ri.seq ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var it ReversalItem
		var at string
		var c int64
		if err := rows.Scan(&it.ID, &it.ReversalID, &it.AllocationID, &it.InvoiceID,
			&c, &it.Seq, &at); err != nil {
			return nil, err
		}
		it.Amount = Money(c)
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			it.CreatedAt = t
		}
		r.Items = append(r.Items, it)
	}
	return &r, rows.Err()
}

// ---------------- 杂项 ----------------

func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}
