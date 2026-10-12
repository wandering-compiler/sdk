package s3_test

import (
	"fmt"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// A TRANSFORM_FIELD runs at most once per key in a run, even when the
// store's listing hands the same key back more than once.
func TestApply_YAMLTransformAppliesOncePerKeyDespiteListRepeats(t *testing.T) {
	f := newFakeS3()
	f.repeatList = true
	for i := 0; i < 5; i++ {
		f.objects[fmt.Sprintf("users/%d", i)] = []byte("v")
	}
	a := newApplier(t, f)
	// One worker: the repeated key then reaches the transform after the
	// first pass wrote, so a missing guard shows on every run.
	body := "version: 1\nencoding: json\nparallel: 1\noperations:\n" +
		"  - op: TRANSFORM_FIELD\n    keyspace: users/\n    script_lang: starlark\n    script: |\n" +
		"      def transform(value):\n          if value == b\"v\":\n              return b\"once\"\n          return b\"twice\"\n"
	if err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-repeat-1", UpSql: body}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := 0; i < 5; i++ {
		if v := string(f.objects[fmt.Sprintf("users/%d", i)]); v != "once" {
			t.Errorf("users/%d = %q, want one transform (a repeated listing key was transformed again)", i, v)
		}
	}
}
