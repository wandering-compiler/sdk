package datamigrate

import "testing"

// A stored JSON `null` holds no fields: every op is a no-op on it, exactly as
// on an empty value — and none of them may panic, since they run in a worker
// goroutine with no recovery.
func TestJSONApplyOp_NullValueIsANoOp(t *testing.T) {
	ops := []Operation{
		{Op: OpAddFieldDefault, Keyspace: "k:*", Field: "f", Value: 1},
		{Op: OpRemoveField, Keyspace: "k:*", Field: "f"},
		{Op: OpRenameField, Keyspace: "k:*", From: "a", To: "b"},
	}
	for _, raw := range []string{"null", " null\n"} {
		for _, op := range ops {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("%s on %q panicked: %v", op.Op, raw, r)
					}
				}()
				out, changed, err := JSONApplyOp([]byte(raw), op)
				if err != nil || changed || out != nil {
					t.Errorf("%s on %q = (%q, %v, %v), want a no-op", op.Op, raw, out, changed, err)
				}
			}()
		}
	}
}
