package goinvoicecollection

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// 逐项撤销：AllocationID 留空表示冲回明确保留的未分配余款，
// 可与普通分配冲回条目混用。
func TestItemizedRefundUnallocatedRemainder(t *testing.T) {
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
	alloc1 := pay.Allocations[0]

	// 一条冲分配 40，一条冲未分配余款 30。
	r, err := svc.CreateRefund(ctx, CreateRefundInput{
		ExternalNo: "R-MIX", PaymentID: pay.Payment.ID,
		Instructions: []RefundInstruction{
			{AllocationID: "", Amount: MustParseMoney("30.00")},
			{AllocationID: alloc1.ID, Amount: MustParseMoney("40.00")},
		},
	})
	if err != nil {
		t.Fatalf("mixed itemized refund: %v", err)
	}
	if r.Refund.Amount.String() != "70.00" || len(r.Items) != 2 {
		t.Fatalf("refund = %+v items=%d", r.Refund, len(r.Items))
	}

	// 明细顺序：先发票分配（沿用原 sequence），余款条目在最后且无发票。
	if r.Items[0].AllocationID != alloc1.ID || r.Items[0].InvoiceID != inv1.ID ||
		r.Items[0].Amount.String() != "40.00" {
		t.Fatalf("item0 = %+v", r.Items[0])
	}
	if r.Items[1].AllocationID != "" || r.Items[1].InvoiceID != "" ||
		r.Items[1].Amount.String() != "30.00" {
		t.Fatalf("item1 (unallocated) = %+v", r.Items[1])
	}

	// 发票只受分配冲回影响：inv1 100 -> 60（partial）；inv2 仍 50 已付。
	b1, _ := svc.InvoiceBalance(ctx, inv1.ID)
	if b1.PaidAmount.String() != "60.00" || b1.Status != InvoicePartial {
		t.Fatalf("inv1 = %+v", b1)
	}
	b2, _ := svc.InvoiceBalance(ctx, inv2.ID)
	if b2.PaidAmount.String() != "50.00" || b2.Status != InvoicePaid {
		t.Fatalf("inv2 = %+v", b2)
	}

	got, _ := svc.GetPayment(ctx, pay.Payment.ID)
	if got.AllocatedAmount.String() != "110.00" ||
		got.UnallocatedAmount.String() != "20.00" ||
		got.RefundedAmount.String() != "70.00" {
		t.Fatalf("payment parts = %+v", got)
	}

	// 剩余未分配余款只有 20，再冲 21 必须冲突。
	_, err = svc.CreateRefund(ctx, CreateRefundInput{
		ExternalNo: "R-REST", PaymentID: pay.Payment.ID,
		Instructions: []RefundInstruction{{AllocationID: "", Amount: MustParseMoney("21.00")}},
	})
	if !IsKind(err, KindConflict) {
		t.Fatalf("over unallocated expected conflict, got %v", err)
	}

	// 一次撤销至多一条余款条目。
	_, err = svc.CreateRefund(ctx, CreateRefundInput{
		PaymentID: pay.Payment.ID,
		Instructions: []RefundInstruction{
			{AllocationID: "", Amount: MustParseMoney("5.00")},
			{AllocationID: "", Amount: MustParseMoney("5.00")},
		},
	})
	if !IsKind(err, KindValidation) {
		t.Fatalf("duplicate unallocated entry expected validation error, got %v", err)
	}

	// 同号同内容重放返回首次记录（留空条目参与指纹）。
	replay, err := svc.CreateRefund(ctx, CreateRefundInput{
		ExternalNo: "R-MIX", PaymentID: pay.Payment.ID,
		Instructions: []RefundInstruction{
			{AllocationID: alloc1.ID, Amount: MustParseMoney("40.00")}, // 顺序不同
			{AllocationID: "", Amount: MustParseMoney("30.00")},
		},
	})
	if err != nil || !replay.Replayed || replay.Refund.ID != r.Refund.ID {
		t.Fatalf("replay = %+v err=%v", replay, err)
	}
	got, _ = svc.GetPayment(ctx, pay.Payment.ID)
	if got.RefundedAmount.String() != "70.00" {
		t.Fatalf("refunded after replay = %s, want 70.00", got.RefundedAmount)
	}

	// 同号改金额仍冲突。
	_, err = svc.CreateRefund(ctx, CreateRefundInput{
		ExternalNo: "R-MIX", PaymentID: pay.Payment.ID,
		Instructions: []RefundInstruction{
			{AllocationID: alloc1.ID, Amount: MustParseMoney("40.00")},
			{AllocationID: "", Amount: MustParseMoney("29.00")},
		},
	})
	if !IsKind(err, KindConflict) {
		t.Fatalf("changed content expected conflict, got %v", err)
	}
}

// 撤销后发票回到 unpaid，新收款可以再次把同一余额结清：
// 验证“已经撤销的部分不能再次使用”是针对撤销额度，而非锁死发票。
func TestRefundReopensInvoiceForNewPayment(t *testing.T) {
	svc := newInMemoryService(t)
	ctx := context.Background()
	inv := mustInvoice(t, svc, "INV-1", "C1", "100.00", testDate(10))
	pay := register(t, svc, RegisterPaymentInput{
		ExternalNo: "P-1", CustomerID: "C1", Amount: MustParseMoney("100.00"),
		Mode:         AllocationManual,
		Instructions: []AllocationInstruction{{InvoiceID: inv.ID, Amount: MustParseMoney("100.00")}},
	})

	if _, err := svc.CreateRefund(ctx, CreateRefundInput{
		ExternalNo: "R-1", PaymentID: pay.Payment.ID, Amount: MustParseMoney("100.00"),
	}); err != nil {
		t.Fatalf("full refund: %v", err)
	}
	b, _ := svc.InvoiceBalance(ctx, inv.ID)
	if b.Status != InvoiceUnpaid || !b.PaidAmount.IsZero() || b.OpenAmount.String() != "100.00" {
		t.Fatalf("invoice after full refund = %+v", b)
	}

	// 原收款已无可撤额度。
	if _, err := svc.CreateRefund(ctx, CreateRefundInput{
		PaymentID: pay.Payment.ID, Amount: MustParseMoney("0.01"),
	}); !IsKind(err, KindConflict) {
		t.Fatalf("re-refund expected conflict, got %v", err)
	}

	// 新收款可以重新结清该发票。
	pay2 := register(t, svc, RegisterPaymentInput{
		ExternalNo: "P-2", CustomerID: "C1", Amount: MustParseMoney("100.00"),
		Mode:         AllocationManual,
		Instructions: []AllocationInstruction{{InvoiceID: inv.ID, Amount: MustParseMoney("100.00")}},
	})
	b, _ = svc.InvoiceBalance(ctx, inv.ID)
	if b.Status != InvoicePaid || b.PaidAmount.String() != "100.00" {
		t.Fatalf("invoice after re-payment = %+v", b)
	}
	if len(pay2.Allocations) != 1 || pay2.Allocations[0].RemainingAmount.String() != "100.00" {
		t.Fatalf("new allocation = %+v", pay2.Allocations)
	}
}

// 并发自动分配：多个独立连接同时自动收款，发票总已付额绝不能超过应收额。
func TestConcurrentAutoAllocationsNeverOverpay(t *testing.T) {
	path := newFileStore(t)
	ctx := context.Background()
	bootstrap := NewService(openStoreAt(t, path))
	// 两张发票，每张 100，总应收 200。
	mustInvoice(t, bootstrap, "INV-1", "C1", "100.00", testDate(10))
	mustInvoice(t, bootstrap, "INV-2", "C1", "100.00", testDate(20))

	const n = 20
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
			// 每笔 30：最多分配 200；并发下成功数取决于串行顺序，
			// 但任何结果都不得超付或丢失分配。
			_, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
				ExternalNo: fmt.Sprintf("PA-%02d", i), CustomerID: "C1",
				Amount: MustParseMoney("30.00"), Mode: AllocationAuto,
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
	t.Logf("auto allocations: success=%d conflict=%d", success, conflict)

	invs, err := bootstrap.ListInvoices(ctx, "C1")
	if err != nil || len(invs) != 2 {
		t.Fatalf("invoices = %v err=%v", invs, err)
	}
	var paidTotal Money
	for _, inv := range invs {
		if inv.PaidAmount > inv.Amount || inv.PaidAmount.IsNegative() {
			t.Fatalf("invoice %s out of range: paid=%s amount=%s", inv.Number, inv.PaidAmount, inv.Amount)
		}
		switch {
		case inv.PaidAmount.IsZero() && inv.Status != InvoiceUnpaid:
			t.Fatalf("invoice %s zero paid but status %s", inv.Number, inv.Status)
		case inv.PaidAmount == inv.Amount && inv.Status != InvoicePaid:
			t.Fatalf("invoice %s fully paid but status %s", inv.Number, inv.Status)
		case inv.PaidAmount > 0 && inv.PaidAmount < inv.Amount && inv.Status != InvoicePartial:
			t.Fatalf("invoice %s partial paid but status %s", inv.Number, inv.Status)
		}
		paidTotal = paidTotal.Add(inv.PaidAmount)
	}
	if paidTotal.String() != "200.00" {
		t.Fatalf("total paid = %s, want exactly 200.00 (no overpay, no lost allocation)", paidTotal)
	}

	// 每张发票的流水之和必须等于其 paid_amount（账本守恒）。
	for _, inv := range invs {
		ledger, err := bootstrap.ListInvoiceLedger(ctx, inv.ID)
		if err != nil {
			t.Fatalf("ledger %s: %v", inv.Number, err)
		}
		var sum Money
		for _, e := range ledger {
			sum = sum.Add(e.Amount)
		}
		if sum != inv.PaidAmount {
			t.Fatalf("invoice %s ledger sum %s != paid %s", inv.Number, sum, inv.PaidAmount)
		}
	}
}

// 并发逐项撤销同一条分配：冲回总额不得超过该分配的原始金额，
// 已经冲回的部分不能被重复使用。
func TestConcurrentItemizedRefundsSameAllocation(t *testing.T) {
	path := newFileStore(t)
	ctx := context.Background()
	bootstrap := NewService(openStoreAt(t, path))
	inv := mustInvoice(t, bootstrap, "INV-1", "C1", "100.00", testDate(10))
	pay := register(t, bootstrap, RegisterPaymentInput{
		ExternalNo: "P-1", CustomerID: "C1", Amount: MustParseMoney("100.00"),
		Mode:         AllocationManual,
		Instructions: []AllocationInstruction{{InvoiceID: inv.ID, Amount: MustParseMoney("100.00")}},
	})
	allocID := pay.Allocations[0].ID

	const n = 10
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
			_, err := svc.CreateRefund(ctx, CreateRefundInput{
				ExternalNo: fmt.Sprintf("RI-%02d", i), PaymentID: pay.Payment.ID,
				Instructions: []RefundInstruction{
					{AllocationID: allocID, Amount: MustParseMoney("15.00")},
				},
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
	// 每笔冲 15，分配只有 100：恰好 6 笔成功（90），第 7 笔起全部冲突。
	if success != 6 || conflict != 4 {
		t.Fatalf("success=%d conflict=%d, want 6/4", success, conflict)
	}

	got, _ := bootstrap.GetPayment(ctx, pay.Payment.ID)
	if got.RefundedAmount.String() != "90.00" || got.AllocatedAmount.String() != "10.00" {
		t.Fatalf("payment = %+v", got)
	}
	allocs, _ := bootstrap.ListAllocations(ctx, pay.Payment.ID)
	if allocs[0].RemainingAmount.String() != "10.00" {
		t.Fatalf("remaining = %s, want 10.00", allocs[0].RemainingAmount)
	}
	b, _ := bootstrap.InvoiceBalance(ctx, inv.ID)
	if b.PaidAmount.String() != "10.00" || b.Status != InvoicePartial {
		t.Fatalf("invoice = %+v", b)
	}
	// 冲回流水绝对值之和必须等于 100 - paid。
	ledger, _ := bootstrap.ListInvoiceLedger(ctx, inv.ID)
	var refunded Money
	for _, e := range ledger {
		if e.Kind == "refund" {
			refunded = refunded.Add(e.Amount.Abs())
		}
	}
	if refunded.String() != "90.00" {
		t.Fatalf("ledger refunds = %s, want 90.00", refunded)
	}
}
