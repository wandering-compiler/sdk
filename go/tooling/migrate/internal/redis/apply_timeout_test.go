package redis_test

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
// Every deadline below interrupts a different point of a run over a
// keyspace large enough to be cut mid-scan.
func TestApply_YAMLApplyTimeoutLeavesAResumableState_Live(t *testing.T) {
	const keys = 2000
	idempotent := "version: 1\nencoding: json\noperations:\n" +
		"  - op: ADD_FIELD_DEFAULT\n    keyspace: users:*\n    field: a\n    value: true\n" +
		"  - op: ADD_FIELD_DEFAULT\n    keyspace: users:*\n    field: b\n    value: true\n"
	transform := "version: 1\nencoding: json\noperations:\n" +
		"  - op: TRANSFORM_FIELD\n    keyspace: users:*\n    script_lang: starlark\n    script: |\n" +
		"      def transform(value):\n          if value == b\"v\":\n              return b\"once\"\n          return b\"twice\"\n"

	for _, d := range []time.Duration{time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond, 80 * time.Millisecond} {
		t.Run(fmt.Sprintf("idempotent ops, deadline %v", d), func(t *testing.T) {
			a, mr := liveApplier(t)
			for i := 0; i < keys; i++ {
				_ = mr.Set(fmt.Sprintf("users:%d", i), `{"n":1}`)
			}
			m := &applyfetchpb.Migration{Id: "20260601T000001Z", UpSql: idempotent}
			short, cancel := context.WithTimeout(context.Background(), d)
			_ = a.Apply(short, m)
			cancel()
			if err := a.Apply(liveCtx(t), m); err != nil {
				t.Fatalf("the run after a timeout did not finish: %v", err)
			}
			for i := 0; i < keys; i++ {
				v, _ := mr.Get(fmt.Sprintf("users:%d", i))
				if !strings.Contains(v, `"a":true`) || !strings.Contains(v, `"b":true`) {
					t.Fatalf("users:%d = %s after the resumed run", i, v)
				}
			}
			if mr.HGet("w17:migrations", m.GetId()) == "" {
				t.Error("the resumed run did not record the migration")
			}
		})
		t.Run(fmt.Sprintf("transform, deadline %v", d), func(t *testing.T) {
			a, mr := liveApplier(t)
			for i := 0; i < keys; i++ {
				_ = mr.Set(fmt.Sprintf("users:%d", i), "v")
			}
			m := &applyfetchpb.Migration{Id: "20260601T000002Z", UpSql: transform}
			short, cancel := context.WithTimeout(context.Background(), d)
			firstErr := a.Apply(short, m)
			cancel()
			err := a.Apply(liveCtx(t), m)
			if firstErr == nil {
				return // finished inside the deadline; nothing was interrupted
			}
			if err == nil {
				// Only an op that never started may run again: every key
				// transformed exactly once.
				for i := 0; i < keys; i++ {
					if v, _ := mr.Get(fmt.Sprintf("users:%d", i)); v != "once" {
						t.Fatalf("users:%d = %q after the resumed run, want one transform", i, v)
					}
				}
				return
			}
			if !strings.Contains(err.Error(), "TRANSFORM_FIELD") || !strings.Contains(err.Error(), "HDEL") {
				t.Fatalf("the refused resume does not name the op and the remedy: %v", err)
			}
		})
	}
}
