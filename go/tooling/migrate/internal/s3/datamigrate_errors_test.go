package s3_test

import (
	"net/http"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// Object-key helpers mirroring datamigrate.go's well-known prefixes so a
// fault can target exactly one side-channel object (forward cursor,
// rollback cursor, or the bookkeeping tracker) without tripping the
// others.
func fwdCursorKey(id string) string { return "wc-data-migrations/" + id + ".cursor.json" }
func rbCursorKey(id string) string  { return "wc-data-migrations/" + id + ".rollback.cursor.json" }
func trackerKey(id string) string   { return "wc-migrations/" + id + ".json" }

const yamlRemoveActive = `version: 1
encoding: json
operations:
  - op: REMOVE_FIELD
    keyspace: users/
    field: active
`

// yamlTransformID is a valid identity TRANSFORM_FIELD — buildTransformVMs
// compiles it, so the apply/rollback flow reaches the cursor + guard
// logic rather than bailing at compile time.
const yamlTransformID = `version: 1
encoding: json
operations:
  - op: TRANSFORM_FIELD
    keyspace: users/
    script_lang: starlark
    script: |
      def transform(value):
          return value
`

// yamlTransformBad has no transform() function, so buildTransformVMs
// refuses it up front.
const yamlTransformBad = `version: 1
encoding: json
operations:
  - op: TRANSFORM_FIELD
    keyspace: users/
    script_lang: starlark
    script: |
      x = 1
`

// yamlProtoBad declares protobuf encoding with an undecodable
// descriptor, so buildCodec refuses it before any S3 I/O.
const yamlProtoBad = `version: 1
encoding: protobuf
proto_descriptor: "!!!not-base64!!!"
proto_message: pkg.User
operations:
  - op: REMOVE_FIELD
    keyspace: users/
    field: active
`

// TestApply_YAMLErrorWraps pins the forward applyYAMLDataMigration
// failure arms left open by the happy-path suite: codec / transform
// compile refusals, the cursor-read fault, the per-object GetObject and
// apply-op faults, and the TRANSFORM start-marker write fault. Each
// surfaces as a stage-tagged wrapped error rather than a panic or a
// silent skip.
func TestApply_YAMLErrorWraps(t *testing.T) {
	cases := []struct {
		name, id, body, frag string
		setup                func(*fakeS3)
	}{
		{name: "codec build refused", id: "e-codec", body: yamlProtoBad, frag: "applyYAMLDataMigration"},
		{name: "transform build refused", id: "e-tf", body: yamlTransformBad, frag: "TRANSFORM_FIELD"},
		{
			name: "cursor read fault", id: "e-cur", body: yamlAdd, frag: "cursor read",
			setup: func(f *fakeS3) { f.failGet[fwdCursorKey("e-cur")] = http.StatusInternalServerError },
		},
		{
			name: "getobject fault", id: "e-get", body: yamlAdd, frag: "GetObject",
			setup: func(f *fakeS3) {
				f.objects["users/1"] = []byte(`{"id":1}`)
				f.failGet["users/1"] = http.StatusInternalServerError
			},
		},
		{
			name: "apply-op fault", id: "e-apply", body: yamlAdd, frag: "decode JSON",
			setup: func(f *fakeS3) { f.objects["users/1"] = []byte(`not json`) },
		},
		{
			name: "start-marker write fault", id: "e-sm", body: yamlTransformID, frag: "start-marker",
			setup: func(f *fakeS3) { f.failPut[fwdCursorKey("e-sm")] = http.StatusInternalServerError },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3()
			if tc.setup != nil {
				tc.setup(f)
			}
			a := newApplier(t, f)
			err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: tc.id, UpSql: tc.body})
			if err == nil || !strings.Contains(err.Error(), tc.frag) {
				t.Fatalf("Apply err = %v, want fragment %q", err, tc.frag)
			}
		})
	}
}

// TestRollback_YAMLErrorWraps pins the inverse rollbackYAMLDataMigration
// failure arms — the whole rollback error surface that the single happy
// rollback test left dark: transform compile refusal, cursor-read fault,
// the non-idempotent TRANSFORM resume refusal, the op fault, and the
// three terminal write/delete faults (cursor write, bookkeeping erase,
// cursor delete).
func TestRollback_YAMLErrorWraps(t *testing.T) {
	withActive := func(f *fakeS3) { f.objects["users/1"] = []byte(`{"id":1,"active":true}`) }
	cases := []struct {
		name, id, down, frag string
		setup                func(*fakeS3)
	}{
		{name: "transform build refused", id: "r-tf", down: yamlTransformBad, frag: "TRANSFORM_FIELD"},
		{
			name: "cursor read fault", id: "r-cur", down: yamlRemoveActive, frag: "cursor read",
			setup: func(f *fakeS3) { f.failGet[rbCursorKey("r-cur")] = http.StatusInternalServerError },
		},
		{
			name: "transform resume refused", id: "r-resume", down: yamlTransformID, frag: "interrupted mid-op",
			setup: func(f *fakeS3) {
				f.objects[rbCursorKey("r-resume")] = []byte(`{"completed_ops":[],"started_ops":[0]}`)
			},
		},
		{
			name: "op fault", id: "r-op", down: yamlRemoveActive, frag: "ListObjectsV2",
			setup: func(f *fakeS3) { f.failList = true },
		},
		{
			name: "cursor write fault", id: "r-cw", down: yamlRemoveActive, frag: "cursor write",
			setup: func(f *fakeS3) {
				withActive(f)
				f.failPut[rbCursorKey("r-cw")] = http.StatusInternalServerError
			},
		},
		{
			name: "bookkeeping fault", id: "r-bk", down: yamlRemoveActive, frag: "bookkeeping",
			setup: func(f *fakeS3) {
				withActive(f)
				f.failDelete[trackerKey("r-bk")] = http.StatusInternalServerError
			},
		},
		{
			name: "cursor delete fault", id: "r-cd", down: yamlRemoveActive, frag: "cursor delete",
			setup: func(f *fakeS3) {
				withActive(f)
				f.failDelete[rbCursorKey("r-cd")] = http.StatusInternalServerError
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3()
			if tc.setup != nil {
				tc.setup(f)
			}
			a := newApplier(t, f)
			err := a.Rollback(ctx(t), &applyfetchpb.Migration{Id: tc.id, UpSql: yamlAdd, DownSql: tc.down})
			if err == nil || !strings.Contains(err.Error(), tc.frag) {
				t.Fatalf("Rollback err = %v, want fragment %q", err, tc.frag)
			}
		})
	}
}

// TestRollback_YAMLResumesFromCursor pins the rollback resumability skip:
// a rollback cursor marking op 0 complete makes the rollback skip it, so
// the object keeps its field (no inverse re-applied) yet the run still
// finishes cleanly.
func TestRollback_YAMLResumesFromCursor(t *testing.T) {
	f := newFakeS3()
	f.objects["users/1"] = []byte(`{"id":1,"active":true}`)
	f.objects[rbCursorKey("r-skip")] = []byte(`{"completed_ops":[0]}`)
	a := newApplier(t, f)
	if err := a.Rollback(ctx(t), &applyfetchpb.Migration{Id: "r-skip", UpSql: yamlAdd, DownSql: yamlRemoveActive}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !strings.Contains(string(f.objects["users/1"]), "active") {
		t.Errorf("op 0 should have been skipped (active must remain): %s", f.objects["users/1"])
	}
}
