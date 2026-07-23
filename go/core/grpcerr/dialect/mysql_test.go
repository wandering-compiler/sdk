package dialect

import (
	"errors"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// MySQL fixtures recorded 2026-05-09 against MySQL 8.0 in
// docker (wc-schemas-mysql80). Message strings are the
// engine-emitted text verbatim — re-recording requires
// running INSERTs that violate UNIQUE / FK / CHECK / NOT_NULL
// against a real MySQL 8.0 instance and capturing the
// MySQLError.Message.

func TestParseMySQL_Unique_8_0(t *testing.T) {
	err := &mysql.MySQLError{
		Number:   1062,
		SQLState: [5]byte{'2', '3', '0', '0', '0'},
		Message:  "Duplicate entry 'alice@example.com' for key 'probe_users.probe_users_email_unique'",
	}
	ce, ok := ParseMySQL(err)
	if !ok || ce.Kind != KindUnique {
		t.Fatalf("got %+v ok=%v", ce, ok)
	}
	if ce.Name != "probe_users_email_unique" {
		t.Errorf("Name = %q", ce.Name)
	}
}

func TestParseMySQL_Unique_5_7_NoPrefix(t *testing.T) {
	// Pre-8.0 the message omits the "<table>." prefix:
	// `for key 'probe_users_email_unique'`. Pattern handles
	// both shapes via the optional non-capturing group.
	err := &mysql.MySQLError{
		Number:  1062,
		Message: "Duplicate entry 'alice@example.com' for key 'probe_users_email_unique'",
	}
	ce, ok := ParseMySQL(err)
	if !ok || ce.Name != "probe_users_email_unique" {
		t.Fatalf("5.7 shape lost: %+v ok=%v", ce, ok)
	}
}

func TestParseMySQL_FK_NoReferencedRow(t *testing.T) {
	err := &mysql.MySQLError{
		Number:  1452,
		Message: "Cannot add or update a child row: a foreign key constraint fails (`probe`.`probe_orders`, CONSTRAINT `probe_orders_user_fk` FOREIGN KEY (`user_id`) REFERENCES `probe_users` (`id`))",
	}
	ce, ok := ParseMySQL(err)
	if !ok || ce.Kind != KindFK {
		t.Fatalf("got %+v ok=%v", ce, ok)
	}
	if ce.Name != "probe_orders_user_fk" {
		t.Errorf("Name = %q", ce.Name)
	}
	if ce.Table != "probe_orders" {
		t.Errorf("Table = %q", ce.Table)
	}
}

func TestParseMySQL_FK_RowIsReferenced(t *testing.T) {
	err := &mysql.MySQLError{
		Number:  1451,
		Message: "Cannot delete or update a parent row: a foreign key constraint fails (`probe`.`probe_orders`, CONSTRAINT `probe_orders_user_fk` FOREIGN KEY (`user_id`) REFERENCES `probe_users` (`id`))",
	}
	ce, ok := ParseMySQL(err)
	if !ok || ce.Name != "probe_orders_user_fk" {
		t.Fatalf("got %+v ok=%v", ce, ok)
	}
}

func TestParseMySQL_Check_8_0(t *testing.T) {
	err := &mysql.MySQLError{
		Number:  3819,
		Message: "Check constraint 'probe_users_age_check' is violated.",
	}
	ce, ok := ParseMySQL(err)
	if !ok || ce.Kind != KindCheck {
		t.Fatalf("got %+v ok=%v", ce, ok)
	}
	if ce.Name != "probe_users_age_check" {
		t.Errorf("Name = %q", ce.Name)
	}
}

func TestParseMySQL_NotNull(t *testing.T) {
	err := &mysql.MySQLError{
		Number:  1048,
		Message: "Column 'email' cannot be null",
	}
	ce, ok := ParseMySQL(err)
	if !ok || ce.Kind != KindNotNull {
		t.Fatalf("got %+v ok=%v", ce, ok)
	}
	if len(ce.Columns) != 1 || ce.Columns[0] != "email" {
		t.Errorf("Columns = %v", ce.Columns)
	}
}

func TestParseMySQL_UnknownNumber(t *testing.T) {
	err := &mysql.MySQLError{Number: 9999, Message: "garbled"}
	if _, ok := ParseMySQL(err); ok {
		t.Error("unknown number should fall through ok=false")
	}
}

func TestParseMySQL_NotMySQLError(t *testing.T) {
	if _, ok := ParseMySQL(errors.New("not a mysql err")); ok {
		t.Error("plain error should fall through")
	}
}
