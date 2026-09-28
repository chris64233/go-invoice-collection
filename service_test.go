package goinvoicecollection

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testDate(day int) time.Time {
	return time.Date(2026, time.January, day, 0, 0, 0, 0, time.UTC)
}

func newInMemoryService(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	store, err := OpenInMemory(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewService(store)
}

func newFileStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	store, err := OpenFile(context.Background(), path)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	_ = store.Close()
	return path
}

func openStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	store, err := OpenFile(context.Background(), path)
	if err != nil {
		t.Fatalf("open file %s: %v", path, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func mustInvoice(t *testing.T, svc *Service, number, customer string, amount string, due time.Time) Invoice {
	t.Helper()
	inv, err := svc.CreateInvoice(context.Background(), NewInvoice{
		Number:     number,
		CustomerID: customer,
		IssueDate:  testDate(1),
		DueDate:    due,
		Amount:     MustParseMoney(amount),
	})
	if err != nil {
		t.Fatalf("create invoice %s: %v", number, err)
	}
	return inv
}

func register(t *testing.T, svc *Service, in RegisterPaymentInput) RegisterPaymentResult {
	t.Helper()
	res, err := svc.RegisterPayment(context.Background(), in)
	if err != nil {
		t.Fatalf("register %s: %v", in.ExternalNo, err)
	}
	return res
}

// ---------------------------------------------------------------------
// 手工分配
// ---------------------------------------------------------------------

func TestManualAllocation(t *testing.T) {
	svc := newInMemoryService(t)
	ctx := context.Background()
	inv1 := mustInvoice(t, svc, "INV-1", "C1", "100.00", testDate(10))
	inv2 := mustInvoice(t, svc, "INV-2", "C1", "50.00", testDate(20))

	res := register(t, svc, RegisterPaymentInput{
		ExternalNo: "P-1", CustomerID: "C1", Amount: MustParseMoney("120.00"),
		Mode: AllocationManual,
		Instructions: []AllocationInstruction{
			{InvoiceID: inv1.ID, Amount: MustParseMoney("100.00")},
			{InvoiceID: inv2.ID, Amount: MustParseMoney("20.00")},
		},
	})
	if len(res.Allocations) != 2 {
		t.Fatalf("allocations = %d", len(res.Allocations))
	}
	pay := res.Payment
	if pay.AllocatedAmount.String() != "120.00" || !pay.UnallocatedAmount.IsZero() {
		t.Fatalf("payment parts: allocated=%s unallocated=%s", pay.AllocatedAmount, pay.UnallocatedAmount)
	}

	b1, _ := svc.InvoiceBalance(ctx, inv1.ID)
	if b1.Status != InvoicePaid || b1.OpenAmount.String() != "0.00" || b1.PaidAmount.String() != "100.00" {
		t.Fatalf("inv1 balance: %+v", b1)
	}
	b2, _ := svc.InvoiceBalance(ctx, inv2.ID)
	if b2.Status != InvoicePartial || b2.OpenAmount.String() != "30.00" || b2.PaidAmount.String() != "20.00" {
		t.Fatalf("inv2 balance: %+v", b2)
	}

	// 部分付款，余款明确保留。
	res2 := register(t, svc, RegisterPaymentInput{
		ExternalNo: "P-2", CustomerID: "C1", Amount: MustParseMoney("10.00"),
		Mode: AllocationManual,
		Instructions: []AllocationInstruction{
			{InvoiceID: inv2.ID, Amount: MustParseMoney("5.00")},
		},
	})
	if res2.Payment.AllocatedAmount.String() != "5.00" || res2.Payment.UnallocatedAmount.String() != "5.00" {
		t.Fatalf("unallocated not retained: %+v", res2.Payment)
	}
	b2, _ = svc.InvoiceBalance(ctx, inv2.ID)
	if b2.PaidAmount.String() != "25.00" || b2.Status != InvoicePartial {
		t.Fatalf("inv2 after second pay: %+v", b2)
	}
}

func TestManualAllocationValidation(t *testing.T) {
	svc := newInMemoryService(t)
	inv := mustInvoice(t, svc, "INV-1", "C1", "50.00", testDate(10))
	ctx := context.Background()

	// 超过发票未付余额。
	_, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
		ExternalNo: "P-X", CustomerID: "C1", Amount: MustParseMoney("60.00"),
		Mode:         AllocationManual,
		Instructions: []AllocationInstruction{{InvoiceID: inv.ID, Amount: MustParseMoney("60.00")}},
	})
	if !IsKind(err, KindConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}

	// 分配累计超过收款额。
	_, err = svc.RegisterPayment(ctx, RegisterPaymentInput{
		ExternalNo: "P-Y", CustomerID: "C1", Amount: MustParseMoney("10.00"),
		Mode: AllocationManual,
		Instructions: []AllocationInstruction{
			{InvoiceID: inv.ID, Amount: MustParseMoney("6.00")},
			{InvoiceID: inv.ID, Amount: MustParseMoney("5.00")}, // 同时也是重复发票
		},
	})
	if !IsKind(err, KindValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}

	// 发票不存在。
	_, err = svc.RegisterPayment(ctx, RegisterPaymentInput{
		ExternalNo: "P-Z", CustomerID: "C1", Amount: MustParseMoney("10.00"),
		Mode:         AllocationManual,
		Instructions: []AllocationInstruction{{InvoiceID: "nope", Amount: MustParseMoney("10.00")}},
	})
	if !IsKind(err, KindNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

// ---------------------------------------------------------------------
// 自动分配（按到期日从早到晚）
// ---------------------------------------------------------------------

func TestAutoAllocationByDueDate(t *testing.T) {
	svc := newInMemoryService(t)
	ctx := context.Background()
	invLate := mustInvoice(t, svc, "INV-LATE", "C1", "100.00", testDate(20))
	invEarly := mustInvoice(t, svc, "INV-EARLY", "C1", "100.00", testDate(5))
	invMid := mustInvoice(t, svc, "INV-MID", "C1", "100.00", testDate(10))

	res := register(t, svc, RegisterPaymentInput{
		ExternalNo: "P-AUTO", CustomerID: "C1",
		Amount: MustParseMoney("250.00"), Mode: AllocationAuto,
	})
	if len(res.Allocations) != 3 {
		t.Fatalf("allocations = %d, want 3", len(res.Allocations))
	}
	wantOrder := []string{invEarly.ID, invMid.ID, invLate.ID}
	wantAmounts := []string{"100.00", "100.00", "50.00"}
	for i, a := range res.Allocations {
		if a.InvoiceID != wantOrder[i] {
			t.Fatalf("alloc %d invoice = %s, want %s", i, a.InvoiceID, wantOrder[i])
		}
		if a.Amount.String() != wantAmounts[i] {
			t.Fatalf("alloc %d amount = %s, want %s", i, a.Amount, wantAmounts[i])
		}
	}
	if res.Payment.UnallocatedAmount.String() != "0.00" {
		t.Fatalf("unallocated = %s", res.Payment.UnallocatedAmount)
	}

	early, _ := svc.InvoiceBalance(ctx, invEarly.ID)
	mid, _ := svc.InvoiceBalance(ctx, invMid.ID)
	late, _ := svc.InvoiceBalance(ctx, invLate.ID)
	if early.Status != InvoicePaid || mid.Status != InvoicePaid {
		t.Fatalf("early/mid should be paid")
	}
	if late.Status != InvoicePartial || late.OpenAmount.String() != "50.00" {
		t.Fatalf("late = %+v", late)
	}
}

func TestAutoAllocationRetainsRemainder(t *testing.T) {
	svc := newInMemoryService(t)
	mustInvoice(t, svc, "INV-1", "C1", "100.00", testDate(5))
	mustInvoice(t, svc, "INV-2", "C2", "100.00", testDate(1)) // 别的客户，不参与

	res := register(t, svc, RegisterPaymentInput{
		ExternalNo: "P-BIG", CustomerID: "C1",
		Amount: MustParseMoney("300.00"), Mode: AllocationAuto,
	})
	if res.Payment.AllocatedAmount.String() != "100.00" ||
		res.Payment.UnallocatedAmount.String() != "200.00" {
		t.Fatalf("parts = %+v", res.Payment)
	}
}

// ---------------------------------------------------------------------
// 幂等
// ---------------------------------------------------------------------

func TestPaymentIdempotency(t *testing.T) {
	svc := newInMemoryService(t)
	ctx := context.Background()
	inv1 := mustInvoice(t, svc, "INV-1", "C1", "100.00", testDate(10))
	inv2 := mustInvoice(t, svc, "INV-2", "C1", "100.00", testDate(20))

	in := RegisterPaymentInput{
		ExternalNo: "P-IDEM", CustomerID: "C1", Amount: MustParseMoney("100.00"),
		Mode: AllocationManual,
		Instructions: []AllocationInstruction{
			{InvoiceID: inv1.ID, Amount: MustParseMoney("60.00")},
			{InvoiceID: inv2.ID, Amount: MustParseMoney("40.00")},
		},
	}
	first := register(t, svc, in)
	if first.Replayed {
		t.Fatal("first call should not be replay")
	}

	// 同号同内容（指令顺序不同、内容一致）→ 返回首次记录。
	in.Instructions[0], in.Instructions[1] = in.Instructions[1], in.Instructions[0]
	second := register(t, svc, in)
	if !second.Replayed {
		t.Fatal("identical replay should set Replayed")
	}
	if second.Payment.ID != first.Payment.ID {
		t.Fatalf("replay created new payment: %s vs %s", second.Payment.ID, first.Payment.ID)
	}

	// 同号金额改变 → 冲突。
	changed := in
	changed.Amount = MustParseMoney("101.00")
	changed.Instructions = []AllocationInstruction{
		{InvoiceID: inv1.ID, Amount: MustParseMoney("61.00")},
		{InvoiceID: inv2.ID, Amount: MustParseMoney("40.00")},
	}
	if _, err := svc.RegisterPayment(ctx, changed); !IsKind(err, KindConflict) {
		t.Fatalf("amount change: expected conflict, got %v", err)
	}

	// 同号分配方式改变 → 冲突。
	changedMode := RegisterPaymentInput{
		ExternalNo: "P-IDEM", CustomerID: "C1", Amount: MustParseMoney("100.00"),
		Mode: AllocationAuto,
	}
	if _, err := svc.RegisterPayment(ctx, changedMode); !IsKind(err, KindConflict) {
		t.Fatalf("mode change: expected conflict, got %v", err)
	}

	// 发票余额只能被分配一次。
	b1, _ := svc.InvoiceBalance(ctx, inv1.ID)
	b2, _ := svc.InvoiceBalance(ctx, inv2.ID)
	if b1.PaidAmount.String() != "60.00" || b2.PaidAmount.String() != "40.00" {
		t.Fatalf("double allocation: %s / %s", b1.PaidAmount, b2.PaidAmount)
	}
}

// ---------------------------------------------------------------------
// 撤销
// ---------------------------------------------------------------------

func TestFIFORefundFullAndPartial(t *testing.T) {
	svc := newInMemoryService(t)
	ctx := context.Background()
	inv1 := mustInvoice(t, svc, "INV-1", "C1", "100.00", testDate(10))
	inv2 := mustInvoice(t, svc, "INV-2", "C1", "50.00", testDate(20))
	pay := register(t, svc, RegisterPaymentInput{
		ExternalNo: "P-1", CustomerID: "C1", Amount: MustParseMoney("150.00"),
		Mode: AllocationManual,
		Instructions: []AllocationInstruction{
			{InvoiceID: inv1.ID, Amount: MustParseMoney("100.00")},
			{InvoiceID: inv2.ID, Amount: MustParseMoney("50.00")},
		},
	})

	// 第一次部分撤销 30：FIFO 先冲第一条分配。
	r1, err := svc.CreateRefund(ctx, CreateRefundInput{
		ExternalNo: "R-1", PaymentID: pay.Payment.ID, Amount: MustParseMoney("30.00"),
	})
	if err != nil {
		t.Fatalf("refund 1: %v", err)
	}
	if r1.Replayed || r1.Refund.Amount.String() != "30.00" {
		t.Fatalf("refund1 = %+v", r1)
	}
	if len(r1.Items) != 1 || r1.Items[0].InvoiceID != inv1.ID || r1.Items[0].Amount.String() != "30.00" {
		t.Fatalf("refund1 items = %+v", r1.Items)
	}
	b1, _ := svc.InvoiceBalance(ctx, inv1.ID)
	if b1.Status != InvoicePartial || b1.PaidAmount.String() != "70.00" || b1.OpenAmount.String() != "30.00" {
		t.Fatalf("inv1 after partial refund: %+v", b1)
	}
	allocs, _ := svc.ListAllocations(ctx, pay.Payment.ID)
	if allocs[0].RemainingAmount.String() != "70.00" || allocs[1].RemainingAmount.String() != "50.00" {
		t.Fatalf("allocation remaining: %+v", allocs)
	}

	// 第二次部分撤销 90：冲第一条剩余 70，再冲第二条 20。
	r2, err := svc.CreateRefund(ctx, CreateRefundInput{
		PaymentID: pay.Payment.ID, Amount: MustParseMoney("90.00"),
	})
	if err != nil {
		t.Fatalf("refund 2: %v", err)
	}
	if len(r2.Items) != 2 {
		t.Fatalf("refund2 items = %d: %+v", len(r2.Items), r2.Items)
	}
	if r2.Items[0].Amount.String() != "70.00" || r2.Items[1].Amount.String() != "20.00" {
		t.Fatalf("refund2 split = %+v", r2.Items)
	}

	// 最后撤销剩余 30，全部回到未付。
	if _, err := svc.CreateRefund(ctx, CreateRefundInput{
		PaymentID: pay.Payment.ID, Amount: MustParseMoney("30.00"),
	}); err != nil {
		t.Fatalf("refund 3: %v", err)
	}
	b1, _ = svc.InvoiceBalance(ctx, inv1.ID)
	b2, _ := svc.InvoiceBalance(ctx, inv2.ID)
	if b1.Status != InvoiceUnpaid || !b1.PaidAmount.IsZero() || b2.Status != InvoiceUnpaid || !b2.PaidAmount.IsZero() {
		t.Fatalf("after full refunds: %+v / %+v", b1, b2)
	}
	got, _ := svc.GetPayment(ctx, pay.Payment.ID)
	if got.RefundedAmount.String() != "150.00" || !got.AllocatedAmount.IsZero() {
		t.Fatalf("payment after full refund: %+v", got)
	}

	// 已全部撤销，不能再次使用。
	_, err = svc.CreateRefund(ctx, CreateRefundInput{
		PaymentID: pay.Payment.ID, Amount: MustParseMoney("0.01"),
	})
	if !IsKind(err, KindConflict) {
		t.Fatalf("over-refund expected conflict, got %v", err)
	}
}

func TestItemizedRefund(t *testing.T) {
	svc := newInMemoryService(t)
	ctx := context.Background()
	inv1 := mustInvoice(t, svc, "INV-1", "C1", "100.00", testDate(10))
	inv2 := mustInvoice(t, svc, "INV-2", "C1", "50.00", testDate(20))
	pay := register(t, svc, RegisterPaymentInput{
		ExternalNo: "P-1", CustomerID: "C1", Amount: MustParseMoney("200.00"),
		Mode: AllocationManual,
		Instructions: []AllocationInstruction{
			{InvoiceID: inv1.ID, Amount: MustParseMoney("100.00")},
			{InvoiceID: inv2.ID, Amount: MustParseMoney("50.00")},
		},
	})
	if pay.Payment.UnallocatedAmount.String() != "50.00" {
		t.Fatalf("unallocated = %s", pay.Payment.UnallocatedAmount)
	}
	alloc2 := pay.Allocations[1]

	// 逐项只冲第二条分配 40。
	r, err := svc.CreateRefund(ctx, CreateRefundInput{
		ExternalNo: "R-ITEM", PaymentID: pay.Payment.ID,
		Instructions: []RefundInstruction{{AllocationID: alloc2.ID, Amount: MustParseMoney("40.00")}},
	})
	if err != nil {
		t.Fatalf("itemized refund: %v", err)
	}
	if r.Refund.Amount.String() != "40.00" || len(r.Items) != 1 {
		t.Fatalf("itemized refund = %+v", r)
	}
	b2, _ := svc.InvoiceBalance(ctx, inv2.ID)
	if b2.PaidAmount.String() != "10.00" {
		t.Fatalf("inv2 paid = %s", b2.PaidAmount)
	}

	// 同一条分配剩余只有 10，再冲 11 必须冲突。
	_, err = svc.CreateRefund(ctx, CreateRefundInput{
		PaymentID:    pay.Payment.ID,
		Instructions: []RefundInstruction{{AllocationID: alloc2.ID, Amount: MustParseMoney("11.00")}},
	})
	if !IsKind(err, KindConflict) {
		t.Fatalf("over allocation refund expected conflict, got %v", err)
	}

	// 撤销幂等：同号同内容重放；同号改金额冲突。
	replay, err := svc.CreateRefund(ctx, CreateRefundInput{
		ExternalNo: "R-ITEM", PaymentID: pay.Payment.ID,
		Instructions: []RefundInstruction{{AllocationID: alloc2.ID, Amount: MustParseMoney("40.00")}},
	})
	if err != nil || !replay.Replayed || replay.Refund.ID != r.Refund.ID {
		t.Fatalf("refund replay: %+v err=%v", replay, err)
	}
	_, err = svc.CreateRefund(ctx, CreateRefundInput{
		ExternalNo: "R-ITEM", PaymentID: pay.Payment.ID,
		Instructions: []RefundInstruction{{AllocationID: alloc2.ID, Amount: MustParseMoney("1.00")}},
	})
	if !IsKind(err, KindConflict) {
		t.Fatalf("changed refund expected conflict, got %v", err)
	}
}

func TestFIFORefundTouchesUnallocated(t *testing.T) {
	svc := newInMemoryService(t)
	ctx := context.Background()
	inv := mustInvoice(t, svc, "INV-1", "C1", "20.00", testDate(10))
	pay := register(t, svc, RegisterPaymentInput{
		ExternalNo: "P-1", CustomerID: "C1", Amount: MustParseMoney("100.00"),
		Mode:         AllocationManual,
		Instructions: []AllocationInstruction{{InvoiceID: inv.ID, Amount: MustParseMoney("20.00")}},
	})

	// 撤销 50：20 冲分配（发票回未付），30 冲未分配余款。
	r, err := svc.CreateRefund(ctx, CreateRefundInput{
		PaymentID: pay.Payment.ID, Amount: MustParseMoney("50.00"),
	})
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	var onInvoice, onRemainder Money
	for _, it := range r.Items {
		if it.InvoiceID == inv.ID {
			onInvoice = onInvoice.Add(it.Amount)
		} else {
			onRemainder = onRemainder.Add(it.Amount)
		}
	}
	if onInvoice.String() != "20.00" || onRemainder.String() != "30.00" {
		t.Fatalf("split = invoice %s remainder %s", onInvoice, onRemainder)
	}
	b, _ := svc.InvoiceBalance(ctx, inv.ID)
	if b.Status != InvoiceUnpaid || !b.PaidAmount.IsZero() {
		t.Fatalf("invoice = %+v", b)
	}
	got, _ := svc.GetPayment(ctx, pay.Payment.ID)
	if got.RefundedAmount.String() != "50.00" || got.UnallocatedAmount.String() != "50.00" || got.AllocatedAmount.String() != "0.00" {
		t.Fatalf("payment = %+v", got)
	}

	// 再撤 60 超过剩余可撤 50 → 冲突。
	_, err = svc.CreateRefund(ctx, CreateRefundInput{
		PaymentID: pay.Payment.ID, Amount: MustParseMoney("60.00"),
	})
	if !IsKind(err, KindConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

// ---------------------------------------------------------------------
// 分配历史 / 发票流水
// ---------------------------------------------------------------------

func TestInvoiceLedger(t *testing.T) {
	svc := newInMemoryService(t)
	ctx := context.Background()
	inv := mustInvoice(t, svc, "INV-1", "C1", "100.00", testDate(10))
	pay := register(t, svc, RegisterPaymentInput{
		ExternalNo: "P-1", CustomerID: "C1", Amount: MustParseMoney("100.00"),
		Mode:         AllocationManual,
		Instructions: []AllocationInstruction{{InvoiceID: inv.ID, Amount: MustParseMoney("100.00")}},
	})
	if _, err := svc.CreateRefund(ctx, CreateRefundInput{
		ExternalNo: "R-1", PaymentID: pay.Payment.ID, Amount: MustParseMoney("30.00"),
	}); err != nil {
		t.Fatalf("refund: %v", err)
	}

	ledger, err := svc.ListInvoiceLedger(ctx, inv.ID)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if len(ledger) != 2 {
		t.Fatalf("ledger entries = %d: %+v", len(ledger), ledger)
	}
	if ledger[0].Kind != "allocation" || ledger[0].Amount.String() != "100.00" || ledger[0].BalanceAfter.String() != "100.00" {
		t.Fatalf("entry0 = %+v", ledger[0])
	}
	if ledger[1].Kind != "refund" || ledger[1].Amount.String() != "-30.00" || ledger[1].BalanceAfter.String() != "70.00" {
		t.Fatalf("entry1 = %+v", ledger[1])
	}
	if ledger[1].PaymentID != pay.Payment.ID || ledger[1].PaymentNo != "P-1" {
		t.Fatalf("refund entry should trace original payment: %+v", ledger[1])
	}

	refunds, _ := svc.ListRefunds(ctx, pay.Payment.ID)
	if len(refunds) != 1 || refunds[0].ExternalNo != "R-1" {
		t.Fatalf("refunds = %+v", refunds)
	}
}

// ---------------------------------------------------------------------
// 并发：多个独立 *sql.DB（模拟多进程），不能重复结清 / 账本守恒
// ---------------------------------------------------------------------

func TestConcurrentManualPaymentsNeverOverpay(t *testing.T) {
	path := newFileStore(t)
	ctx := context.Background()

	// 用一个连接做初始化。
	bootstrap := NewService(openStoreAt(t, path))
	inv := mustInvoice(t, bootstrap, "INV-1", "C1", "100.00", testDate(10))

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	var success, conflict int
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			svc := NewService(openStoreAt(t, path)) // 每个 goroutine 独立连接池
			<-start
			_, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
				ExternalNo: fmt.Sprintf("P-%02d", i), CustomerID: "C1",
				Amount: MustParseMoney("10.00"), Mode: AllocationManual,
				Instructions: []AllocationInstruction{{InvoiceID: inv.ID, Amount: MustParseMoney("10.00")}},
			})
			mu.Lock()
			switch {
			case err == nil:
				success++
			case IsKind(err, KindConflict):
				conflict++
			default:
				t.Errorf("unexpected error: %v", err)
			}
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()

	if success != 10 || conflict != 10 {
		t.Fatalf("success=%d conflict=%d, want 10/10", success, conflict)
	}
	b, err := bootstrap.InvoiceBalance(ctx, inv.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if b.PaidAmount.String() != "100.00" || b.Status != InvoicePaid {
		t.Fatalf("final balance = %+v", b)
	}

	// 分配历史之和必须等于已付金额。
	var total Money
	rows, err := bootstrap.ListInvoices(ctx, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("invoices = %v err=%v", rows, err)
	}
	// 直接校验每张发票的分配总额（通过流水）。
	ledger, err := bootstrap.ListInvoiceLedger(ctx, inv.ID)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	for _, e := range ledger {
		total = total.Add(e.Amount)
	}
	if total.String() != "100.00" {
		t.Fatalf("ledger sum = %s", total)
	}
}

func TestConcurrentRefundsAndPayments(t *testing.T) {
	path := newFileStore(t)
	ctx := context.Background()
	bootstrap := NewService(openStoreAt(t, path))
	inv := mustInvoice(t, bootstrap, "INV-1", "C1", "100.00", testDate(10))
	initial := register(t, bootstrap, RegisterPaymentInput{
		ExternalNo: "P-INIT", CustomerID: "C1", Amount: MustParseMoney("100.00"),
		Mode:         AllocationManual,
		Instructions: []AllocationInstruction{{InvoiceID: inv.ID, Amount: MustParseMoney("100.00")}},
	})

	// 10 个撤销（每个 10，累计恰好 100）+ 10 个新收款（每个 10，只在有余额时成功），
	// 全部通过独立连接并发执行。
	const n = 10
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	errorsSeen := 0
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) { // 撤销
			defer wg.Done()
			svc := NewService(openStoreAt(t, path))
			<-start
			_, err := svc.CreateRefund(ctx, CreateRefundInput{
				ExternalNo: fmt.Sprintf("R-%02d", i),
				PaymentID:  initial.Payment.ID,
				Amount:     MustParseMoney("10.00"),
			})
			if err != nil {
				mu.Lock()
				errorsSeen++
				mu.Unlock()
				t.Errorf("refund %d: %v", i, err)
			}
		}(i)
		go func(i int) { // 新收款
			defer wg.Done()
			svc := NewService(openStoreAt(t, path))
			<-start
			_, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
				ExternalNo: fmt.Sprintf("P-NEW-%02d", i), CustomerID: "C1",
				Amount: MustParseMoney("10.00"), Mode: AllocationManual,
				Instructions: []AllocationInstruction{{InvoiceID: inv.ID, Amount: MustParseMoney("10.00")}},
			})
			if err != nil && !IsKind(err, KindConflict) {
				mu.Lock()
				errorsSeen++
				mu.Unlock()
				t.Errorf("payment %d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if errorsSeen != 0 {
		t.Fatalf("unexpected errors: %d", errorsSeen)
	}

	// 最终账本守恒：paid = 100 - 100(撤销) + 新收款成功额。
	final, err := bootstrap.InvoiceBalance(ctx, inv.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	ledger, err := bootstrap.ListInvoiceLedger(ctx, inv.ID)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	var sum Money
	var refunds, newAllocs Money
	for _, e := range ledger {
		sum = sum.Add(e.Amount)
		switch e.Kind {
		case "refund":
			refunds = refunds.Add(e.Amount.Abs())
		case "allocation":
			if e.PaymentNo == "P-INIT" {
				// 初始收款
			} else {
				newAllocs = newAllocs.Add(e.Amount)
			}
		}
	}
	if refunds.String() != "100.00" {
		t.Fatalf("total refunded = %s, want 100.00", refunds)
	}
	if sum != final.PaidAmount {
		t.Fatalf("ledger sum %s != paid %s", sum, final.PaidAmount)
	}
	if final.PaidAmount != newAllocs {
		t.Fatalf("paid %s != new allocations %s", final.PaidAmount, newAllocs)
	}
	if final.PaidAmount.IsZero() {
		if final.Status != InvoiceUnpaid {
			t.Fatalf("zero paid must be unpaid, got %s", final.Status)
		}
	} else if final.PaidAmount == final.Amount {
		if final.Status != InvoicePaid {
			t.Fatalf("full paid must be paid, got %s", final.Status)
		}
	} else if final.Status != InvoicePartial {
		t.Fatalf("partial payment must be partial, got %s", final.Status)
	}
	if final.PaidAmount.IsNegative() || final.PaidAmount > final.Amount {
		t.Fatalf("paid out of range: %+v", final)
	}

	// 原收款累计撤销不得超过 100。
	got, _ := bootstrap.GetPayment(ctx, initial.Payment.ID)
	if got.RefundedAmount.String() != "100.00" {
		t.Fatalf("initial payment refunded = %s", got.RefundedAmount)
	}
}

func TestConcurrentSameExternalNo(t *testing.T) {
	path := newFileStore(t)
	ctx := context.Background()
	bootstrap := NewService(openStoreAt(t, path))
	inv := mustInvoice(t, bootstrap, "INV-1", "C1", "100.00", testDate(10))

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make(chan string, n)
	var mu sync.Mutex
	unexpected := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc := NewService(openStoreAt(t, path))
			<-start
			res, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
				ExternalNo: "P-SAME", CustomerID: "C1", Amount: MustParseMoney("100.00"),
				Mode:         AllocationManual,
				Instructions: []AllocationInstruction{{InvoiceID: inv.ID, Amount: MustParseMoney("100.00")}},
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				unexpected++
				t.Errorf("identical concurrent request should replay, got: %v", err)
				return
			}
			if !res.Replayed && res.Payment.ID == "" {
				t.Errorf("empty payment id")
			}
			ids <- res.Payment.ID
		}()
	}
	close(start)
	wg.Wait()
	close(ids)
	if unexpected != 0 {
		t.Fatalf("unexpected errors: %d", unexpected)
	}
	unique := map[string]struct{}{}
	for id := range ids {
		unique[id] = struct{}{}
	}
	if len(unique) != 1 {
		t.Fatalf("expected exactly one payment record, got %d", len(unique))
	}
	b, _ := bootstrap.InvoiceBalance(ctx, inv.ID)
	if b.PaidAmount.String() != "100.00" {
		t.Fatalf("paid = %s, want 100.00", b.PaidAmount)
	}
}

// 同号并发但内容不同：恰好一个成功，其余全部冲突。
func TestConcurrentSameExternalNoConflictingContent(t *testing.T) {
	path := newFileStore(t)
	ctx := context.Background()
	bootstrap := NewService(openStoreAt(t, path))
	inv := mustInvoice(t, bootstrap, "INV-1", "C1", "100.00", testDate(10))

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	success, conflict := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			svc := NewService(openStoreAt(t, path))
			<-start
			// 金额各不相同 → 同号内容互斥。
			amt := MustParseMoney(fmt.Sprintf("%d.00", 50+i))
			_, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
				ExternalNo: "P-CONFLICT", CustomerID: "C1", Amount: amt,
				Mode:         AllocationManual,
				Instructions: []AllocationInstruction{{InvoiceID: inv.ID, Amount: amt}},
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case IsKind(err, KindConflict):
				conflict++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if success != 1 || conflict != n-1 {
		t.Fatalf("success=%d conflict=%d, want 1/%d", success, conflict, n-1)
	}
}

// 确保不会有未预期的错误类型泄漏。
func TestErrorClassification(t *testing.T) {
	if !errors.Is(kindError(KindNotFound, "x"), &Error{Kind: KindNotFound}) {
		t.Fatal("errors.Is by kind failed")
	}
	if errors.Is(kindError(KindNotFound, "x"), &Error{Kind: KindConflict}) {
		t.Fatal("wrong kind matched")
	}
}
