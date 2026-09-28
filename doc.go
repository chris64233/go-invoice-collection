// Package goinvoicecollection 实现收款在多张应收发票之间的分配与原路撤销。
//
// 核心能力：
//
//   - 登记发票（CreateInvoice）与登记收款（Service.RegisterPayment）。收款可手工
//     指定每张发票分配多少（AllocManual），也可由系统按发票到期时间从早到晚
//     自动分配（AllocAuto）；到期相同按发票 ID 稳定排序。
//   - 一次收款涉及的所有分配在一个数据库事务中整体写入：累计金额不得超过收款额，
//     也不得超过任何发票的未付余额；任一分录超额，整笔收款回滚，不会半成功。
//   - 外部收款号（ExternalNo）是幂等键：同号、同金额、同一分配方式（手工模式还包括
//     相同的发票-金额集合）返回首次记录；同号内容改变返回 ErrConflict。
//   - 金额用 Money（int64 分，固定两位小数）精确表示，不使用浮点数；分配不完的
//     余款显式保存在 Payment.Unallocated，绝不静默丢弃。
//   - Service.ReversePayment 必须引用原收款，逐项冲回当时的分配（reversal_items），
//     发票按实际冲回金额重新变为部分未付或未付。支持全额撤销、按金额 FIFO 撤销和
//     显式逐项撤销；多次部分撤销累计不得超过原收款，已撤销部分不能再次使用。
//   - Service.InvoiceHistory 返回不可变的分配/冲回台账；GetInvoice/GetPayment/
//     GetReversal 提供余额与历史查询。
//
// 并发正确性不依赖进程内锁：存储为 SQLite，所有写操作以 BEGIN IMMEDIATE 开启
// 全库写事务串行执行，并由 schema 中的 CHECK 约束与触发器在数据库层强制
// “分配不超收款额/发票余额、撤销不超已分配且未撤销部分”。因此多个 Store，
// 甚至多个进程打开同一个数据库文件，也能得到与最终账本一致的余额与状态。
//
// 典型用法见 README 与 service_test.go。
package goinvoicecollection
