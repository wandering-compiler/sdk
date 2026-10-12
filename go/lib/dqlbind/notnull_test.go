package dqlbind

import (
	"database/sql"
	"testing"

	"github.com/lib/pq"
	"google.golang.org/protobuf/types/descriptorpb"
	_ "modernc.org/sqlite"
)

// An empty repeated value binds as the empty array, not as NULL: lib/pq
// sends a nil slice as NULL, which a NOT NULL array column refuses.
func TestArr_EmptyBindsTheEmptyArrayNotNull(t *testing.T) {
	v, err := pq.Array(Arr[int64](nil)).Value()
	if err != nil {
		t.Fatal(err)
	}
	if v == nil {
		t.Fatal("an empty repeated value binds NULL")
	}
	if s, _ := v.(string); s != "{}" {
		t.Errorf("bound %v, want the empty array {}", v)
	}
	if got := Arr([]string{"a", "b"}); len(got) != 2 || got[0] != "a" {
		t.Errorf("Arr changed a non-empty slice: %v", got)
	}
}

// An empty payload binds as the empty blob: a NOT NULL bytes column takes
// it, where a nil slice would be NULL and refused.
func TestBytes_EmptyIsStoredInANotNullColumn(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE fixtures (name TEXT, body BLOB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO fixtures VALUES (?, ?)`, "raw nil", []byte(nil)); err == nil {
		t.Fatal("decoy: a nil slice was accepted by the NOT NULL column; the test cannot tell the helper from no helper")
	}
	if _, err := db.Exec(`INSERT INTO fixtures VALUES (?, ?)`, "empty", Bytes(nil)); err != nil {
		t.Fatalf("Bytes(nil) refused by a NOT NULL column: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT length(body) FROM fixtures WHERE name = 'empty'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("stored length = %d (%v), want an empty blob", n, err)
	}
}

// An enum on a submessage that is absent binds NULL, like an unset enum on
// a present one; only a set enum binds its number.
func TestEnumFieldOrNull_AbsentParentAndUnsetFieldAreNull(t *testing.T) {
	var absent *descriptorpb.FieldDescriptorProto
	if got := EnumFieldOrNull(absent, "type"); got != nil {
		t.Errorf("absent parent = %#v, want nil", got)
	}
	if got := EnumFieldOrNull(&descriptorpb.FieldDescriptorProto{}, "type"); got != nil {
		t.Errorf("unset enum = %#v, want nil", got)
	}
	set := &descriptorpb.FieldDescriptorProto{Type: descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum()}
	if got := EnumFieldOrNull(set, "type"); got != int32(descriptorpb.FieldDescriptorProto_TYPE_BOOL) {
		t.Errorf("set enum = %#v, want %d", got, descriptorpb.FieldDescriptorProto_TYPE_BOOL)
	}
}
