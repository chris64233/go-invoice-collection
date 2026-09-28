package goinvoicecollection

import _ "embed"

// schemaSQL 是建库脚本，随二进制内嵌。
//
//go:embed schema.sql
var schemaSQL string
