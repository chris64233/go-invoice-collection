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

func newTestService(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	store, err := OpenMemory(ctx, "test_"+t.Name())
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewService(store)
}

func mustInvoice(t *testing.T, svc *Service, id, number, total string, due time.Time) *Invoice {
	t.Helper()
	inv, err := svc.CreateInvoice(context.Background(), CreateInvoiceInput{
		ID: id, Number: number, Total: MustParseMoney(total), DueAt: due,
	})
	if err != nil {
		t.Fatalf("create invoice %s: %v", number, err)
	}
	return inv
}

func TestParseMoney(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"100", 10000},
		{"100.5", 10050},
		{"0.01", 1},
		{"12.30", 1230},
		{"-12.30", -1230},
		{"  7.77  ", 777},
	}
	for _, c := range cases {
		got, err := ParseMoney(c.in)
		if err != nil {
			t.Errorf("ParseMoney(%q) error %v", c.in, err)
			continue
		}
		if got.Cents() != c.want {
			t.Errorf("ParseMoney(%q) = %d, want %d", c.in, got.Cents(), c.want)
		}
	}
	bad := []string{"", "abc", "1.234", "1.", ".", "--1", "1e3", "1,00"}
	for _, b := range bad {
		if _, err := ParseMoney(b); err == nil {
			t.Errorf("ParseMoney(%q) expected error", b)
		}
	}
	if got := Money(150).String(); got != "1.50" {
		t.Errorf("Money.String = %q, want 1.50", got)
	}
	if got := Money(-5).String(); got != "-0.05" {
		t.Errorf("Money.String = %q, want -0.05", got)
	}
}

func TestManualAllocation(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mustInvoice(t, svc, "inv1", "INV-1", "100.00", base.AddDate(0, 0, 10))
	mustInvoice(t, svc, "inv2", "INV-2", "50.00", base.AddDate(0, 0, 20))

	// 收款 120：分给 inv1 100（结清）、inv2 10（部分），余 10 显式保留。
	res, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
		ID: "pay1", ExternalNo: "EXT-1", Amount: MustParseMoney("120.00"),
		Mode: AllocManual,
		Allocations: []PaymentAllocationInput{
			{InvoiceID: "inv1", Amount: MustParseMoney("100")},
			{InvoiceID: "inv2", Amount: MustParseMoney("10")},
		},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if res.Replayed {
		t.Fatal("first register should not be a replay")
	}
	if res.Payment.Unallocated.Cents() != 1000 {
		t.Errorf("unallocated = %s, want 10.00", res.Payment.Unallocated)
	}
	if len(res.Allocations) != 2 {
		t.Fatalf("allocations = %d, want 2", len(res.Allocations))
	}

	inv1, _ := svc.GetInvoice(ctx, "inv1")
	inv2, _ := svc.GetInvoice(ctx, "inv2")
	if inv1.Status() != InvoicePaid || inv1.Paid.Cents() != 10000 {
		t.Errorf("inv1 = paid %s status %s, want paid 100", inv1.Paid, inv1.Status())
	}
	if inv2.Status() != InvoicePartial || inv2.Balance().Cents() != 4000 {
		t.Errorf("inv2 balance = %s, want 40", inv2.Balance())
	}
}

func TestManualAllocationValidation(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mustInvoice(t, svc, "inv1", "INV-1", "100.00", base)

	// 累计分配超过收款额。
	_, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
		ExternalNo: "EXT-X", Amount: MustParseMoney("50"), Mode: AllocManual,
		Allocations: []PaymentAllocationInput{{InvoiceID: "inv1", Amount: MustParseMoney("60")}},
	})
	if !errors.Is(err, ErrExceedsPayment) {
		t.Fatalf("want ErrExceedsPayment, got %v", err)
	}

	// 分配超过发票余额。
	_, err = svc.RegisterPayment(ctx, RegisterPaymentInput{
		ExternalNo: "EXT-Y", Amount: MustParseMoney("200"), Mode: AllocManual,
		Allocations: []PaymentAllocationInput{{InvoiceID: "inv1", Amount: MustParseMoney("150")}},
	})
	if !errors.Is(err, ErrExceedsInvoice) {
		t.Fatalf("want ErrExceedsInvoice, got %v", err)
	}

	// 整体写入：失败后发票余额与收款均不得变化。
	inv1, _ := svc.GetInvoice(ctx, "inv1")
	if inv1.Paid.Cents() != 0 {
		t.Errorf("inv1 paid = %s, want 0 after failed tx", inv1.Paid)
	}
	if _, _, gerr := svc.GetPaymentByExternalNo(ctx, "EXT-Y"); !errors.Is(gerr, ErrNotFound) {
		t.Errorf("failed payment must not be persisted, got %v", gerr)
	}

	// 重复发票条目。
	_, err = svc.RegisterPayment(ctx, RegisterPaymentInput{
		ExternalNo: "EXT-Z", Amount: MustParseMoney("50"), Mode: AllocManual,
		Allocations: []PaymentAllocationInput{
			{InvoiceID: "inv1", Amount: MustParseMoney("10")},
			{InvoiceID: "inv1", Amount: MustParseMoney("10")},
		},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for duplicate, got %v", err)
	}
}

func TestAutoAllocationOrderAndLeftover(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	day := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	// 到期时间：inv2 最早，inv1 次之，inv3 最晚（同日时按 ID 稳定排序）。
	mustInvoice(t, svc, "inv1", "INV-1", "100.00", day.AddDate(0, 0, 2))
	mustInvoice(t, svc, "inv2", "INV-2", "60.00", day.AddDate(0, 0, 1))
	mustInvoice(t, svc, "inv3", "INV-3", "30.00", day.AddDate(0, 0, 3))

	// 收款 200：按到期日顺序填满 inv2(60)、inv1(100)、inv3(30)=190，余 10 保留。
	res, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
		ID: "payA", ExternalNo: "EXT-A", Amount: MustParseMoney("200"), Mode: AllocAuto,
	})
	if err != nil {
		t.Fatalf("auto register: %v", err)
	}
	got := map[string]int64{}
	for _, a := range res.Allocations {
		got[a.InvoiceID] = a.Amount.Cents()
	}
	want := map[string]int64{"inv2": 6000, "inv1": 10000, "inv3": 3000}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("invoice %s allocated %d, want %d", k, got[k], v)
		}
	}
	if res.Payment.Unallocated.Cents() != 1000 {
		t.Errorf("unallocated = %d, want 1000", res.Payment.Unallocated.Cents())
	}
	// 顺序必须按到期日。
	if res.Allocations[0].InvoiceID != "inv2" ||
		res.Allocations[1].InvoiceID != "inv1" ||
		res.Allocations[2].InvoiceID != "inv3" {
		t.Errorf("auto order = %v, want inv2,inv1,inv3", res.Allocations)
	}

	// 收款小于全部余额：只结清最早的一张，其余不动。
	mustInvoice(t, svc, "inv4", "INV-4", "100.00", day.AddDate(0, 0, 5))
	res2, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
		ExternalNo: "EXT-B", Amount: MustParseMoney("20"), Mode: AllocAuto,
	})
	if err != nil {
		t.Fatalf("auto register 2: %v", err)
	}
	// inv1/2/3 已结清；下一个最早且有余额的是 inv4，分给它 20。
	if len(res2.Allocations) != 1 || res2.Allocations[0].InvoiceID != "inv4" ||
		res2.Allocations[0].Amount.Cents() != 2000 {
		t.Fatalf("auto partial = %+v, want 20 to inv4", res2.Allocations)
	}
	if res2.Payment.Unallocated.Cents() != 0 {
		t.Errorf("fully consumed payment unallocated = %d, want 0", res2.Payment.Unallocated.Cents())
	}
}

func TestIdempotencyReplayAndConflict(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	mustInvoice(t, svc, "inv1", "INV-1", "100.00", base)

	in := RegisterPaymentInput{
		ID: "p1", ExternalNo: "DUP-1", Amount: MustParseMoney("100"), Mode: AllocManual,
		Allocations: []PaymentAllocationInput{{InvoiceID: "inv1", Amount: MustParseMoney("100")}},
	}
	first, err := svc.RegisterPayment(ctx, in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := svc.RegisterPayment(ctx, in)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed || second.Payment.ID != first.Payment.ID {
		t.Fatalf("replay must return first record %s, got replayed=%v id=%s",
			first.Payment.ID, second.Replayed, second.Payment.ID)
	}
	// 重放不得重复占用余额：inv1 仍只有 100。
	inv1, _ := svc.GetInvoice(ctx, "inv1")
	if inv1.Paid.Cents() != 10000 {
		t.Errorf("inv1 paid after replay = %s, want 100", inv1.Paid)
	}

	// 同号改金额 -> 冲突。
	_, err = svc.RegisterPayment(ctx, RegisterPaymentInput{
		ID: "p2", ExternalNo: "DUP-1", Amount: MustParseMoney("90"), Mode: AllocManual,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("amount change want ErrConflict, got %v", err)
	}
	// 同号同金额改分配方式 -> 冲突。
	_, err = svc.RegisterPayment(ctx, RegisterPaymentInput{
		ID: "p3", ExternalNo: "DUP-1", Amount: MustParseMoney("100"), Mode: AllocAuto,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("mode change want ErrConflict, got %v", err)
	}
	// 同号同金额改手工分配目标 -> 冲突。
	mustInvoice(t, svc, "inv2", "INV-2", "100.00", base.AddDate(0, 0, 1))
	_, err = svc.RegisterPayment(ctx, RegisterPaymentInput{
		ID: "p4", ExternalNo: "DUP-1", Amount: MustParseMoney("100"), Mode: AllocManual,
		Allocations: []PaymentAllocationInput{{InvoiceID: "inv2", Amount: MustParseMoney("100")}},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("allocation change want ErrConflict, got %v", err)
	}
}

func setupForReversal(t *testing.T, svc *Service) *Payment {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	mustInvoice(t, svc, "i1", "R-1", "100.00", base)
	mustInvoice(t, svc, "i2", "R-2", "50.00", base.AddDate(0, 0, 1))
	res, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
		ID: "rp1", ExternalNo: "REV-SRC", Amount: MustParseMoney("200"), Mode: AllocManual,
		Allocations: []PaymentAllocationInput{
			{InvoiceID: "i1", Amount: MustParseMoney("100")},
			{InvoiceID: "i2", Amount: MustParseMoney("50")},
		},
	})
	if err != nil {
		t.Fatalf("source payment: %v", err)
	}
	return res.Payment
}

func TestFullReversal(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	setupForReversal(t, svc)

	rev, err := svc.ReversePayment(ctx, ReversePaymentInput{
		ID: "rv1", ReversalNo: "RV-1", PaymentID: "rp1",
	})
	if err != nil {
		t.Fatalf("full reversal: %v", err)
	}
	if rev.Amount.Cents() != 15000 {
		t.Errorf("reversal amount = %s, want 150", rev.Amount)
	}
	if len(rev.Items) != 2 {
		t.Fatalf("reversal items = %d, want 2", len(rev.Items))
	}
	i1, _ := svc.GetInvoice(ctx, "i1")
	i2, _ := svc.GetInvoice(ctx, "i2")
	if i1.Status() != InvoiceUnpaid || i1.Paid.Cents() != 0 {
		t.Errorf("i1 = %s/%s, want unpaid 0", i1.Paid, i1.Status())
	}
	if i2.Status() != InvoiceUnpaid || i2.Paid.Cents() != 0 {
		t.Errorf("i2 = %s/%s, want unpaid 0", i2.Paid, i2.Status())
	}

	// 撤销幂等：同号重放返回首次记录，不重复冲回。
	again, err := svc.ReversePayment(ctx, ReversePaymentInput{
		ReversalNo: "RV-1", PaymentID: "rp1",
	})
	if err != nil {
		t.Fatalf("replay reversal: %v", err)
	}
	if again.ID != "rv1" {
		t.Errorf("replay returned %s, want rv1", again.ID)
	}
	i1, _ = svc.GetInvoice(ctx, "i1")
	if i1.Paid.Cents() != 0 {
		t.Errorf("paid changed on replay: %s", i1.Paid)
	}

	// 已全部撤销，再次撤销应失败，且已撤销部分不能再次使用。
	_, err = svc.ReversePayment(ctx, ReversePaymentInput{
		ReversalNo: "RV-2", PaymentID: "rp1",
	})
	if !errors.Is(err, ErrAlreadyReversed) {
		t.Fatalf("re-reverse want ErrAlreadyReversed, got %v", err)
	}
}

func TestPartialReversalsCumulative(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	setupForReversal(t, svc) // i1<-100, i2<-50

	// 第一次部分撤销：显式冲回 i1 的 40。
	r1, err := svc.ReversePayment(ctx, ReversePaymentInput{
		ID: "r1", ReversalNo: "PR-1", PaymentID: "rp1",
		Items: []ReversalItemInput{{InvoiceID: "i1", Amount: MustParseMoney("40")}},
	})
	if err != nil {
		t.Fatalf("partial 1: %v", err)
	}
	if r1.Amount.Cents() != 4000 {
		t.Errorf("r1 amount = %s, want 40", r1.Amount)
	}
	i1, _ := svc.GetInvoice(ctx, "i1")
	if i1.Paid.Cents() != 6000 || i1.Status() != InvoicePartial {
		t.Errorf("i1 after r1 = %s/%s, want 60 partial", i1.Paid, i1.Status())
	}

	// 第二次部分撤销：FIFO 冲回 80 —— i1 剩 60 先冲完，再冲 i2 的 20。
	r2, err := svc.ReversePayment(ctx, ReversePaymentInput{
		ID: "r2", ReversalNo: "PR-2", PaymentID: "rp1", Amount: MustParseMoney("80"),
	})
	if err != nil {
		t.Fatalf("partial 2: %v", err)
	}
	if len(r2.Items) != 2 || r2.Items[0].Amount.Cents() != 6000 || r2.Items[1].Amount.Cents() != 2000 {
		t.Fatalf("r2 items = %+v, want 60 then 20", r2.Items)
	}
	p, allocs := mustPayment(t, svc, "rp1")
	if p.Reversed.Cents() != 12000 {
		t.Errorf("payment reversed = %s, want 120", p.Reversed)
	}
	if p.Reversible().Cents() != 3000 {
		t.Errorf("reversible = %s, want 30", p.Reversible())
	}
	for _, a := range allocs {
		if a.InvoiceID == "i1" && a.Effective().Cents() != 0 {
			t.Errorf("i1 effective = %s, want 0", a.Effective())
		}
	}

	// 第三次：超过剩余可撤销（30）必须失败。
	_, err = svc.ReversePayment(ctx, ReversePaymentInput{
		ReversalNo: "PR-3", PaymentID: "rp1", Amount: MustParseMoney("31"),
	})
	if !errors.Is(err, ErrReversalExceeds) {
		t.Fatalf("over-reverse want ErrReversalExceeds, got %v", err)
	}

	// 冲回 i1 已经冲过的部分（同一分配再冲 70 > 剩余 0）必须失败。
	_, err = svc.ReversePayment(ctx, ReversePaymentInput{
		ReversalNo: "PR-4", PaymentID: "rp1",
		Items: []ReversalItemInput{{InvoiceID: "i1", Amount: MustParseMoney("70")}},
	})
	if !errors.Is(err, ErrAlreadyReversed) {
		t.Fatalf("reuse reversed part want ErrAlreadyReversed, got %v", err)
	}

	// 恰好撤销剩余 30（i2 的有效余额）成功。
	r3, err := svc.ReversePayment(ctx, ReversePaymentInput{
		ID: "r3", ReversalNo: "PR-5", PaymentID: "rp1", Amount: MustParseMoney("30"),
	})
	if err != nil {
		t.Fatalf("final reversal: %v", err)
	}
	if r3.Amount.Cents() != 3000 {
		t.Errorf("r3 = %s, want 30", r3.Amount)
	}
	i1, _ = svc.GetInvoice(ctx, "i1")
	i2, _ := svc.GetInvoice(ctx, "i2")
	if i1.Paid.Cents() != 0 || i2.Paid.Cents() != 0 {
		t.Errorf("invoices after full unwind: i1=%s i2=%s, want 0/0", i1.Paid, i2.Paid)
	}
	p, _ = mustPayment(t, svc, "rp1")
	if p.Reversed.Cents() != p.Allocated.Cents() {
		t.Errorf("reversed %s != allocated %s", p.Reversed, p.Allocated)
	}
}

func TestInvoiceHistory(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	setupForReversal(t, svc) // i1 +100

	if _, err := svc.ReversePayment(ctx, ReversePaymentInput{
		ReversalNo: "H-1", PaymentID: "rp1",
		Items: []ReversalItemInput{{InvoiceID: "i1", Amount: MustParseMoney("30")}},
	}); err != nil {
		t.Fatalf("reversal: %v", err)
	}
	// 再向 i1 收 20。
	if _, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
		ExternalNo: "H-P2", Amount: MustParseMoney("20"), Mode: AllocManual,
		Allocations: []PaymentAllocationInput{{InvoiceID: "i1", Amount: MustParseMoney("20")}},
	}); err != nil {
		t.Fatalf("second payment: %v", err)
	}

	hist, err := svc.InvoiceHistory(ctx, "i1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("history len = %d, want 3: %+v", len(hist), hist)
	}
	want := []struct {
		kind  string
		amt   int64
		after int64
	}{
		{"allocation", 10000, 10000},
		{"reversal", -3000, 7000},
		{"allocation", 2000, 9000},
	}
	for i, w := range want {
		if hist[i].Kind != w.kind || hist[i].Amount.Cents() != w.amt ||
			hist[i].InvoicePaidAfter.Cents() != w.after {
			t.Errorf("hist[%d] = kind=%s amt=%d after=%d, want %s %d %d",
				i, hist[i].Kind, hist[i].Amount.Cents(), hist[i].InvoicePaidAfter.Cents(),
				w.kind, w.amt, w.after)
		}
	}
	inv, _ := svc.GetInvoice(ctx, "i1")
	if inv.Paid != hist[len(hist)-1].InvoicePaidAfter {
		t.Errorf("ledger tail %s != invoice paid %s", hist[len(hist)-1].InvoicePaidAfter, inv.Paid)
	}
}

// TestConcurrentPaymentsNoDoubleSettle 并发收款竞争同一张发票余额，
// 总分配金额绝不能超过发票未付余额（并发正确性由数据库写事务保证）。
func TestConcurrentPaymentsNoDoubleSettle(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mustInvoice(t, svc, "hot", "HOT", "100.00", base)

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 每笔都试图手工分配 100 给同一张只有 100 余额的发票。
			_, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
				ExternalNo: fmt.Sprintf("C-%02d", i),
				Amount:     MustParseMoney("100"),
				Mode:       AllocManual,
				Allocations: []PaymentAllocationInput{
					{InvoiceID: "hot", Amount: MustParseMoney("100")},
				},
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	var succeeded, overErrs int
	for err := range errs {
		switch {
		case errors.Is(err, ErrExceedsInvoice):
			overErrs++
		default:
			t.Fatalf("unexpected concurrent error: %v", err)
		}
	}
	succeeded = n - overErrs
	if succeeded != 1 {
		t.Errorf("succeeded = %d, want exactly 1", succeeded)
	}
	inv, _ := svc.GetInvoice(ctx, "hot")
	if inv.Paid.Cents() != 10000 {
		t.Errorf("hot invoice paid = %s, want exactly 100.00", inv.Paid)
	}
	if inv.Paid > inv.Total {
		t.Fatal("IMPOSSIBLE: paid exceeds total — double settlement")
	}
}

// TestConcurrentReversalAndPayment 撤销与新收款同时作用于同一张发票，
// 最终发票余额必须与台账（所有分配减所有冲回）一致。
func TestConcurrentReversalAndPayment(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mustInvoice(t, svc, "mix", "MIX", "1000.00", base)

	// 先放两笔来源收款，各分配 300（共 600），留足可撤销空间。
	for i, no := range []string{"SRC-A", "SRC-B"} {
		_, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
			ID: fmt.Sprintf("src%d", i), ExternalNo: no, Amount: MustParseMoney("300"),
			Mode:        AllocManual,
			Allocations: []PaymentAllocationInput{{InvoiceID: "mix", Amount: MustParseMoney("300")}},
		})
		if err != nil {
			t.Fatalf("seed %s: %v", no, err)
		}
	}

	const workers = 10
	var wg sync.WaitGroup
	// 一半 worker 撤销来源收款各 20（FIFO 落在 src0 上），一半 worker 新收款 20。
	for w := 0; w < workers; w++ {
		wg.Add(1)
		w := w
		go func() {
			defer wg.Done()
			if w%2 == 0 {
				_, err := svc.ReversePayment(ctx, ReversePaymentInput{
					ReversalNo: fmt.Sprintf("CR-%02d", w), PaymentID: "src0",
					Amount: MustParseMoney("20"),
				})
				if err != nil {
					t.Errorf("reverse worker %d: %v", w, err)
				}
			} else {
				_, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
					ExternalNo: fmt.Sprintf("CP-%02d", w), Amount: MustParseMoney("20"),
					Mode:        AllocManual,
					Allocations: []PaymentAllocationInput{{InvoiceID: "mix", Amount: MustParseMoney("20")}},
				})
				if err != nil {
					t.Errorf("pay worker %d: %v", w, err)
				}
			}
		}()
	}
	wg.Wait()

	// 台账口径：所有带符号条目之和必须等于发票当前已付金额。
	hist, err := svc.InvoiceHistory(ctx, "mix")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var ledgerSum int64
	for _, e := range hist {
		ledgerSum += e.Amount.Cents()
	}
	inv, _ := svc.GetInvoice(ctx, "mix")
	if ledgerSum != inv.Paid.Cents() {
		t.Errorf("ledger sum %d != invoice paid %d", ledgerSum, inv.Paid.Cents())
	}
	if inv.Paid < 0 || inv.Paid > inv.Total {
		t.Errorf("paid %s out of [0,%s]", inv.Paid, inv.Total)
	}
	// 预期：初始 600，5 个撤销各 -20，5 个收款各 +20 -> 仍为 600。
	if inv.Paid.Cents() != 60000 {
		t.Errorf("final paid = %s, want 600.00", inv.Paid)
	}
}

// TestCrossProcessConcurrency 用两个独立 Store 句柄打开同一个内存库，
// 模拟两个进程：进程间没有任何共享的进程内锁，只靠数据库事务保证正确。
func TestCrossProcessConcurrency(t *testing.T) {
	ctx := context.Background()
	const dbName = "cross_process_db"
	storeA, err := OpenMemory(ctx, dbName)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	storeB, err := OpenMemory(ctx, dbName)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	t.Cleanup(func() { _ = storeA.Close(); _ = storeB.Close() })

	svcA := NewService(storeA)
	svcB := NewService(storeB)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	mustInvoice(t, svcA, "x", "X", "100.00", base)

	start := make(chan struct{})
	var wg sync.WaitGroup
	register := func(svc *Service, no string, errOut *error) {
		defer wg.Done()
		<-start
		_, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
			ExternalNo: no, Amount: MustParseMoney("100"), Mode: AllocManual,
			Allocations: []PaymentAllocationInput{{InvoiceID: "x", Amount: MustParseMoney("100")}},
		})
		*errOut = err
	}
	var errA, errB error
	wg.Add(2)
	go register(svcA, "XA", &errA)
	go register(svcB, "XB", &errB)
	close(start)
	wg.Wait()

	failures := 0
	for _, e := range []error{errA, errB} {
		if e != nil {
			if !errors.Is(e, ErrExceedsInvoice) {
				t.Fatalf("unexpected cross-process error: %v", e)
			}
			failures++
		}
	}
	if failures != 1 {
		t.Errorf("one store should fail with ErrExceedsInvoice, failures=%d", failures)
	}
	inv, _ := svcA.GetInvoice(ctx, "x")
	if inv.Paid.Cents() != 10000 {
		t.Errorf("cross-process paid = %s, want 100", inv.Paid)
	}
}

func TestReversalConflict(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	setupForReversal(t, svc)
	in := ReversePaymentInput{
		ID: "c1", ReversalNo: "RC-1", PaymentID: "rp1", Amount: MustParseMoney("10"),
	}
	if _, err := svc.ReversePayment(ctx, in); err != nil {
		t.Fatalf("first: %v", err)
	}
	// 同号但金额改变 -> 冲突。
	_, err := svc.ReversePayment(ctx, ReversePaymentInput{
		ReversalNo: "RC-1", PaymentID: "rp1", Amount: MustParseMoney("20"),
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("changed reversal want ErrConflict, got %v", err)
	}
}

func TestReversingUnknownPayment(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	_, err := svc.ReversePayment(ctx, ReversePaymentInput{
		ReversalNo: "NOPE", PaymentID: "ghost", Amount: MustParseMoney("1"),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestFileStorePersistence 用文件库验证：数据落盘后重新打开仍在，且并发收款
// 与跨句柄（模拟多进程）竞争同一张发票时不会重复结清。
func TestFileStorePersistenceAndConcurrency(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")

	store, err := OpenFile(ctx, path, 10000)
	if err != nil {
		t.Fatalf("open file store: %v", err)
	}
	svc := NewService(store)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	mustInvoice(t, svc, "f1", "F-1", "100.00", base)

	if _, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
		ID: "fp1", ExternalNo: "FILE-1", Amount: MustParseMoney("40"), Mode: AllocManual,
		Allocations: []PaymentAllocationInput{{InvoiceID: "f1", Amount: MustParseMoney("40")}},
	}); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 重新打开，数据必须仍在（幂等号、余额都持久化）。
	reopened, err := OpenFile(ctx, path, 10000)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	svc = NewService(reopened)
	inv, _ := svc.GetInvoice(ctx, "f1")
	if inv.Paid.Cents() != 4000 {
		t.Fatalf("persisted paid = %s, want 40", inv.Paid)
	}

	// 两个独立句柄（不同 *sql.DB，模拟两个进程）并发竞争剩余的 60 余额。
	other, err := OpenFile(ctx, path, 10000)
	if err != nil {
		t.Fatalf("second handle: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	svcA, svcB := NewService(reopened), NewService(other)

	start := make(chan struct{})
	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	race := func(svc *Service, no string) {
		defer wg.Done()
		<-start
		_, err := svc.RegisterPayment(ctx, RegisterPaymentInput{
			ExternalNo: no, Amount: MustParseMoney("60"), Mode: AllocManual,
			Allocations: []PaymentAllocationInput{{InvoiceID: "f1", Amount: MustParseMoney("60")}},
		})
		if err != nil {
			errs <- err
		}
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		if i%2 == 0 {
			go race(svcA, fmt.Sprintf("FA-%02d", i))
		} else {
			go race(svcB, fmt.Sprintf("FB-%02d", i))
		}
	}
	close(start)
	wg.Wait()
	close(errs)

	var overErrs int
	for e := range errs {
		if !errors.Is(e, ErrExceedsInvoice) {
			t.Fatalf("unexpected error: %v", e)
		}
		overErrs++
	}
	if overErrs != n-1 {
		t.Errorf("over-limit failures = %d, want %d", overErrs, n-1)
	}
	inv, _ = svcA.GetInvoice(ctx, "f1")
	if inv.Paid.Cents() != 10000 || inv.Status() != InvoicePaid {
		t.Errorf("final invoice = paid %s status %s, want 100 paid", inv.Paid, inv.Status())
	}
}

func mustPayment(t *testing.T, svc *Service, id string) (*Payment, []Allocation) {
	t.Helper()
	p, a, err := svc.GetPayment(context.Background(), id)
	if err != nil {
		t.Fatalf("get payment %s: %v", id, err)
	}
	return p, a
}
