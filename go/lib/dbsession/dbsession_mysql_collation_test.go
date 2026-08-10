package dbsession

import (
	"strings"
	"testing"
)

// T1-4 pass #12, B-F2. The MySQL collation pin lived in the DEV DSN only
// (`composeDSN`), while the session contract every process shares — the one an
// operator-supplied DSN goes through — carried sql_mode and parseTime and not
// this. go-sql-driver's default handshake collation is `utf8mb4_general_ci`,
// so on an operator DSN a string comparison that is case-SENSITIVE in dev
// answered case-INSENSITIVELY in production: `'Foo' = 'foo'` → 1 there, 0 here,
// live-proven on mysql 8.4.10.
//
// The flagship scenario the report led with — `ORDER BY CAST(x AS CHAR)` — does
// NOT diverge: the MySQL emitter appends `COLLATE utf8mb4_0900_as_cs` to every
// character-result cast, and DDL `COLLATE` anchors column comparisons. What
// survives is the comparison no column anchors: literal↔literal and
// param↔literal.
//
// The fix is a LIST ENTRY, because the session/config axis already has one
// declaration and a second would be the very defect this pass kept finding.
func TestMySQLDSNParams_PinsCollation(t *testing.T) {
	var found string
	for _, p := range MySQLDSNParams() {
		if strings.HasPrefix(p, "collation=") {
			found = p
		}
	}
	if found == "" {
		t.Fatalf("the MySQL session contract must pin a collation; got %v", MySQLDSNParams())
	}
	if !strings.Contains(found, "_as_cs") {
		t.Errorf("the pin must be accent- and case-SENSITIVE (the dev DSN's own choice); got %q", found)
	}
}

func TestApplyMySQLDSNParams_AddsCollationToABareDSN(t *testing.T) {
	got := ApplyMySQLDSNParams("user:pw@tcp(db:3306)/app")
	if !strings.Contains(got, "collation=") {
		t.Errorf("an operator DSN must come out carrying the collation pin; got %q", got)
	}
}

// The non-override contract, the same one sql_mode and parseTime carry: these
// are safety pins for an ACCIDENTAL setting, not a way to overrule a chosen
// one. An operator who names a collation keeps it.
func TestApplyMySQLDSNParams_KeepsAnOperatorCollation(t *testing.T) {
	got := ApplyMySQLDSNParams("user:pw@tcp(db:3306)/app?collation=utf8mb4_bin")
	if strings.Count(got, "collation=") != 1 {
		t.Errorf("an operator-chosen collation must survive untouched; got %q", got)
	}
	if !strings.Contains(got, "collation=utf8mb4_bin") {
		t.Errorf("the operator's value was replaced: %q", got)
	}
}
