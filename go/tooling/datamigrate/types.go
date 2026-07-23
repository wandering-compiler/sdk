// Package datamigrate is the format + validation layer for KV
// data migrations (Phase E — D-iter3-15). The compiler emits
// these as YAML bodies on the migration's `up_sql` field for
// KV dialects (Redis, S3) when a field add / remove / rename
// touches existing entries; the apply tool parses + dispatches
// each operation against the typed client (go-redis HSET/HDEL/
// RENAME loop, AWS SDK GetObject/PutObject loop).
//
// **Wire shape (YAML).**
//
//	# wc:expected_pre_fingerprint: <hex>
//	version: 1                 # YAML schema version
//	encoding: json             # v1 only "json"; v2 reserves "protobuf"
//	parallel: 8                # default workers
//	estimated_records: 50000   # author hint → risk-header
//	operations:
//	  - op: ADD_FIELD_DEFAULT
//	    keyspace: users:*
//	    field: full_name
//	    value: ""
//	  - op: REMOVE_FIELD
//	    keyspace: sessions:*
//	    field: deprecated_token
//	  - op: RENAME_FIELD
//	    keyspace: events:*
//	    from: old_name
//	    to: new_name
//	# wc:content_signature: <hex>
//
// Header + signature lines are wc-platform comment markers
// (the `decorate` package handles them per-dialect; for KV
// dialects the prefix is `#`, which is also a valid YAML
// comment prefix — header / footer survive YAML round-trips).
//
// **v1 scope.** ADD_FIELD_DEFAULT, REMOVE_FIELD, RENAME_FIELD —
// replay-safe by construction (re-running mid-migration is
// always cheap + correct). TRANSFORM_FIELD escape hatch is
// reserved for v2 via embedded sandboxed Starlark (the
// `script:` field on Operation is parsed but rejected with a
// "deferred" error). encoding=protobuf reserved for v2 via
// inline FileDescriptorSet + dynamicpb (the `encoding` field
// only accepts "json" today).
package datamigrate

// Migration is the top-level YAML doc shape the compiler emits
// + apply tool consumes.
type Migration struct {
	// Version of THIS YAML schema — bumps when format changes
	// in a backwards-incompatible way. v1 is the only valid
	// value today; future versions add fields without breaking
	// the v1 shape.
	Version int `yaml:"version"`

	// Encoding declares how each KV value is serialised.
	// v1 only accepts "json"; v2 reserves "protobuf" via
	// inline FileDescriptorSet (see ProtoDescriptor field).
	// Apply tool refuses unknown values.
	Encoding string `yaml:"encoding"`

	// ProtoDescriptor (v2, reserved) is base64-encoded
	// FileDescriptorSet bytes the apply tool feeds into
	// dynamicpb.NewMessageType for reflection-based
	// unmarshal/marshal. Empty in v1; non-empty rejected
	// with "encoding=protobuf deferred to v2".
	ProtoDescriptor string `yaml:"proto_descriptor,omitempty"`

	// ProtoMessage (v2, reserved) is the fully-qualified
	// message name within ProtoDescriptor. Empty in v1.
	ProtoMessage string `yaml:"proto_message,omitempty"`

	// Parallel is the project-side default worker count for
	// per-keyspace iteration. CLI `--parallel` on the apply
	// tool overrides (force). Empty (0) = fall back to
	// DefaultParallel.
	Parallel int `yaml:"parallel,omitempty"`

	// EstimatedRecords is the author's hint for risk-header
	// generation at compile time. Apply tool uses it for
	// progress messages; mismatch with actual key count is
	// not an error, just a less-accurate ETA.
	EstimatedRecords int `yaml:"estimated_records,omitempty"`

	// Operations is the ordered list of ops the apply tool
	// runs against each affected keyspace. Order matters —
	// rename-then-default on same field would mean different
	// data than default-then-rename.
	Operations []Operation `yaml:"operations"`
}

// Operation is one data-migration step. The Op field tags the
// kind; only fields relevant to that kind carry meaning. YAML
// unmarshal accepts unknown fields (forward-compat); validation
// rejects when required fields are missing per kind.
type Operation struct {
	// Op is the discriminator. v1 kinds:
	//   "ADD_FIELD_DEFAULT" — set Field to Value when missing
	//   "REMOVE_FIELD"      — strip Field from each entry
	//   "RENAME_FIELD"      — copy From→To, delete From
	//
	// v2 reserved:
	//   "TRANSFORM_FIELD"   — run Script per entry (deferred)
	Op string `yaml:"op"`

	// Keyspace is the wildcard pattern over which the op
	// runs. Format mirrors the dialect's iteration primitive:
	//   Redis: SCAN MATCH pattern (`users:*`)
	//   S3:    ListObjectsV2 prefix (`users/`)
	//
	// Apply tool consumers normalise to their native form.
	Keyspace string `yaml:"keyspace"`

	// Field is the field name affected by ADD_FIELD_DEFAULT,
	// REMOVE_FIELD, TRANSFORM_FIELD. Empty for RENAME_FIELD
	// (which uses From/To instead).
	Field string `yaml:"field,omitempty"`

	// Value is the default value for ADD_FIELD_DEFAULT.
	// Encoding-dependent representation:
	//   json:     scalar / object marshalled by yaml.v3
	//             (apply tool re-marshals to JSON before
	//             writing back)
	//   protobuf: any-typed scalar consumed by
	//             dynamicpb.Message.Set (v2)
	Value any `yaml:"value,omitempty"`

	// From / To are RENAME_FIELD's source / target field
	// names. Apply tool copies entry[From]→entry[To] and
	// deletes entry[From].
	From string `yaml:"from,omitempty"`
	To   string `yaml:"to,omitempty"`

	// Script (v2, reserved) is the TRANSFORM_FIELD body. v1
	// rejects non-empty Script with "TRANSFORM_FIELD deferred
	// to v2". The signed migration body covers this field —
	// signature verification implicitly authenticates the
	// script.
	Script string `yaml:"script,omitempty"`

	// ScriptLang (v2, reserved) is the embedded VM dialect.
	// v2 lands "starlark" first; "tengo" / "lua" deferred.
	ScriptLang string `yaml:"script_lang,omitempty"`
}

// Op constants — string values mirror the YAML literals.
const (
	OpAddFieldDefault = "ADD_FIELD_DEFAULT"
	OpRemoveField     = "REMOVE_FIELD"
	OpRenameField     = "RENAME_FIELD"
	OpTransformField  = "TRANSFORM_FIELD" // v2, reserved
)

// EncodingJSON / EncodingProtobuf are the supported encoding
// values. Only EncodingJSON is implemented in v1.
const (
	EncodingJSON     = "json"
	EncodingProtobuf = "protobuf" // v2, reserved
)

// DefaultParallel is the worker count when neither YAML
// `parallel` nor `--parallel` CLI flag is set. Conservative
// to avoid overwhelming production-tier KV stores; operators
// crank up via the flag when target capacity allows.
const DefaultParallel = 4

// CurrentVersion is the v1 YAML schema version. The compiler
// always emits this; the apply tool refuses higher versions
// (run a newer w17migrate) and silently accepts equal-or-lower
// (forward-compat read).
const CurrentVersion = 1
