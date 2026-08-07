package datamigrate

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Marshal serialises a Migration to YAML bytes ready to land in
// the migration's `up_sql` / `down_sql` body. Output is a
// single YAML document, indent=2, fields ordered per the struct
// declaration (yaml.v3 defaults).
//
// Header / signature comment lines (`# wc:expected_pre_fingerprint:
// ...` / `# wc:content_signature: ...`) are NOT added here —
// they're the `decorate` package's responsibility, layered over
// the YAML body identically to how Phase D wraps SQL bodies. A
// raw Marshal output is the plain YAML doc; the registry-side
// decorate pass adds header + footer.
//
// Validates the migration before emitting. Refuses to marshal
// invalid shapes — better to surface compile-time errors than
// to ship a body the apply tool will reject.
func Marshal(m *Migration) ([]byte, error) {
	if err := Validate(m); err != nil {
		return nil, fmt.Errorf("datamigrate.Marshal: %w", err)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("datamigrate.Marshal: yaml encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("datamigrate.Marshal: yaml close: %w", err)
	}
	return buf.Bytes(), nil
}

// Unmarshal parses YAML body bytes back into a Migration. The
// input may carry header / signature comment lines (the
// `decorate` package's wrap shape); yaml.v3 strips comments
// during decode so both raw and decorated bodies parse the same.
//
// Validates the migration after parsing. Apply-tool callers
// MUST treat any error as a refusal to apply.
func Unmarshal(body []byte) (*Migration, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("datamigrate.Unmarshal: empty body")
	}
	var m Migration
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(false) // forward-compat: ignore unknown fields v2+ might add
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("datamigrate.Unmarshal: yaml decode: %w", err)
	}
	if err := Validate(&m); err != nil {
		return nil, fmt.Errorf("datamigrate.Unmarshal: %w", err)
	}
	return &m, nil
}

// LooksLikeYAML is the fast classifier the apply tool uses to
// distinguish a YAML data migration body from a SQL / redis-cli
// / nats-cli style migration body. A body is YAML when, after
// stripping `#` comment lines + blank lines, the first
// non-comment line starts with `version:` followed by an int.
//
// Cheap content-shape sniff — avoids the full Unmarshal cost
// for non-data bodies on the apply hot path. Falls through to
// the dialect's normal command dispatch when classifier says
// no.
func LooksLikeYAML(body []byte) bool {
	for _, raw := range bytes.Split(body, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || bytes.HasPrefix(line, []byte("#")) {
			continue
		}
		return bytes.HasPrefix(line, []byte("version:"))
	}
	return false
}
