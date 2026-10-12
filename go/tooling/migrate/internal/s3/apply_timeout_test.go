package s3_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// Running into the apply timeout — wherever in the run it lands — leaves a
// state the next run either finishes, or refuses with the remedy spelled out.
// The fake lists one key per page, so even a short keyspace is cut mid-scan.
func TestApply_YAMLApplyTimeoutLeavesAResumableState(t *testing.T) {
	const keys = 150
	idempotent := "version: 1\nencoding: json\noperations:\n" +
		"  - op: ADD_FIELD_DEFAULT\n    keyspace: users/\n    field: a\n    value: true\n" +
		"  - op: ADD_FIELD_DEFAULT\n    keyspace: users/\n    field: b\n    value: true\n"
	transform := "version: 1\nencoding: json\noperations:\n" +
		"  - op: TRANSFORM_FIELD\n    keyspace: users/\n    script_lang: starlark\n    script: |\n" +
		"      def transform(value):\n          if value == b\"v\":\n              return b\"once\"\n          return b\"twice\"\n"

	for _, d := range []time.Duration{2 * time.Millisecond, 10 * time.Millisecond, 40 * time.Millisecond, 150 * time.Millisecond} {
		t.Run(fmt.Sprintf("idempotent ops, deadline %v", d), func(t *testing.T) {
			f := newFakeS3()
			for i := 0; i < keys; i++ {
				f.objects[fmt.Sprintf("users/%d", i)] = []byte(`{"n":1}`)
			}
			a := newApplier(t, f)
			m := &applyfetchpb.Migration{Id: "ts-timeout-1", UpSql: idempotent}
			short, cancel := context.WithTimeout(context.Background(), d)
			_ = a.Apply(short, m)
			cancel()
			if err := a.Apply(ctx(t), m); err != nil {
				t.Fatalf("the run after a timeout did not finish: %v", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for i := 0; i < keys; i++ {
				v := string(f.objects[fmt.Sprintf("users/%d", i)])
				if !strings.Contains(v, `"a":true`) || !strings.Contains(v, `"b":true`) {
					t.Fatalf("users/%d = %s after the resumed run", i, v)
				}
			}
			if _, ok := f.objects["w17-migrations/ts-timeout-1.json"]; !ok {
				t.Error("the resumed run did not record the migration")
			}
		})
		t.Run(fmt.Sprintf("transform, deadline %v", d), func(t *testing.T) {
			f := newFakeS3()
			for i := 0; i < keys; i++ {
				f.objects[fmt.Sprintf("users/%d", i)] = []byte("v")
			}
			a := newApplier(t, f)
			m := &applyfetchpb.Migration{Id: "ts-timeout-2", UpSql: transform}
			short, cancel := context.WithTimeout(context.Background(), d)
			firstErr := a.Apply(short, m)
			cancel()
			err := a.Apply(ctx(t), m)
			if firstErr == nil {
				return // finished inside the deadline; nothing was interrupted
			}
			if err == nil {
				f.mu.Lock()
				defer f.mu.Unlock()
				for i := 0; i < keys; i++ {
					if v := string(f.objects[fmt.Sprintf("users/%d", i)]); v != "once" {
						t.Fatalf("users/%d = %q after the resumed run, want one transform", i, v)
					}
				}
				return
			}
			if !strings.Contains(err.Error(), "TRANSFORM_FIELD") {
				t.Fatalf("the refused resume does not name the op: %v", err)
			}
		})
	}
}
