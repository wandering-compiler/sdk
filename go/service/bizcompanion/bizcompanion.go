// Package bizcompanion is the business companion: the Go half of a
// business tier written in another language (Python, TypeScript). A Go
// business tier serves two things in-process that such a tier cannot
// reproduce exactly — the project's egress clients (package
// lib/egress) and the domain's upload file stores (package lib/kvfs) — and
// the companion serves the same Go code to it over gRPC:
//
//   - every egress client's own `<Name>Api` service, unchanged, configured
//     from its W17_CLIENT_<NAME>_DSN through the secret chain, exactly as a Go
//     tier registers it ([egress.RegisterFromEnv]);
//   - w17.common.files.W17Files (lib/kvfs/kvfsgrpc) over the stores of the
//     domain's UPLOADED_FILE / UPLOADED_IMAGE connections, each opened from
//     the same root variable the gateway and a Go tier read.
//
// Everything it serves is read from the business bundle's manifest
// (w17-business.json, its `companion` section) and its descriptor set, so
// nothing is generated per project: the split bundle's `main` is [Main], and
// a composed binary that folds the companion calls [Load] and [Register] on
// its internal listener.
//
// The caller's principal, trace context and labels arrive as the call's
// incoming metadata — what an egress handler receives in a Go tier, where
// the in-process connection applies the same forward. The SDKs never send a
// transaction id to the companion: a file or an HTTP call is not part of a
// storage transaction.
package bizcompanion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/wandering-compiler/sdk/go/lib/egress"
	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	"github.com/wandering-compiler/sdk/go/lib/kvfs/kvfsgrpc"
	kvfsstore "github.com/wandering-compiler/sdk/go/lib/kvfs/store"
	filespb "github.com/wandering-compiler/sdk/go/pb/common/files"
)

// The manifest formats this package reads. Format 1 has no companion
// section; format 2 is the first with one.
const (
	ManifestFormatMin = 1
	ManifestFormatMax = 2
)

// FilesService is the only file service a manifest may name.
const FilesService = "w17.common.files.W17Files"

// Config is what the companion serves: the manifest's identity, its
// `companion` section and the descriptor set's services.
type Config struct {
	Format    int
	Domain    string
	Bundle    string
	EnvPrefix string
	// DescriptorSet is the descriptor set's path, resolved against the
	// manifest's directory.
	DescriptorSet string
	// Companion is the manifest's `companion` section; nil when the domain
	// needs none (a format-1 manifest).
	Companion *Companion
	// Files is the descriptor set, as a registry.
	Files *protoregistry.Files
}

// Companion is the manifest's `companion` section.
type Companion struct {
	AddressEnv     string        `json:"address_env"`
	DefaultAddress string        `json:"default_address"`
	Egress         []EgressEntry `json:"egress"`
	FileStorage    *FileStorage  `json:"files"`
}

// EgressEntry is one egress client the companion serves.
type EgressEntry struct {
	Client  string `json:"client"`
	Service string `json:"service"`
	DSNEnv  string `json:"dsn_env"`

	// descriptor is Service, resolved in the descriptor set by Load.
	descriptor protoreflect.ServiceDescriptor
}

// FileStorage is the `companion.files` section.
type FileStorage struct {
	Service     string           `json:"service"`
	Connections []FileConnection `json:"connections"`
	Placements  []FilePlacement  `json:"placements"`
}

// FileConnection is one upload connection of the domain.
type FileConnection struct {
	Name string `json:"name"`
	// RootEnv is the variable naming the store: a directory (a path or
	// file:///…) or a bucket (s3://…). The gateway reads the same one.
	RootEnv string `json:"root_env"`
	// DefaultRoot is the value the bundles' .env.defaults write for it;
	// the companion itself requires the variable, as a Go tier does.
	DefaultRoot string `json:"default_root"`
	// Writable is true when the business tier writes the connection
	// (`upload.writer: BUSINESS`).
	Writable bool `json:"writable"`
}

// FilePlacement is one `writer: BUSINESS` table field.
type FilePlacement struct {
	Field           string `json:"field"`
	Name            string `json:"name"`
	Connection      string `json:"connection"`
	BucketPath      string `json:"bucket_path"`
	ExpectedObjects uint64 `json:"expected_objects"`
	MaxPerBucket    uint32 `json:"max_per_bucket"`
	MaxSizeBytes    uint64 `json:"max_size_bytes"`
}

// manifest is the part of w17-business.json the companion reads; every
// other key is the business tier's.
type manifest struct {
	Format        *int            `json:"format"`
	Domain        string          `json:"domain"`
	Bundle        string          `json:"bundle"`
	EnvPrefix     string          `json:"env_prefix"`
	DescriptorSet string          `json:"descriptor_set"`
	Companion     json.RawMessage `json:"companion"`
}

// Load reads the manifest at path and the descriptor set it names, and
// checks the `companion` section against it: every egress service exists in
// the set, the file service is [FilesService], every placement names one of
// the connections. A format it does not read is refused before anything
// else, as the SDKs refuse it.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator's manifest
	if err != nil {
		return nil, fmt.Errorf("companion manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("companion manifest: %s is not a manifest: %w", path, err)
	}
	if m.Format == nil || *m.Format < ManifestFormatMin || *m.Format > ManifestFormatMax {
		got := "none"
		if m.Format != nil {
			got = fmt.Sprint(*m.Format)
		}
		return nil, fmt.Errorf("companion manifest: format %s is not one this companion reads (it reads %d to %d) — use the companion of the project's sdk_version",
			got, ManifestFormatMin, ManifestFormatMax)
	}
	if m.Domain == "" {
		return nil, errors.New("companion manifest: domain is required")
	}
	cfg := &Config{
		Format:        *m.Format,
		Domain:        m.Domain,
		Bundle:        m.Bundle,
		EnvPrefix:     m.EnvPrefix,
		DescriptorSet: m.DescriptorSet,
	}
	if cfg.Bundle == "" {
		cfg.Bundle = m.Domain + "-business"
	}
	if cfg.EnvPrefix == "" {
		return nil, errors.New("companion manifest: env_prefix is required")
	}
	if cfg.DescriptorSet == "" {
		cfg.DescriptorSet = "descriptors.binpb"
	}
	if !filepath.IsAbs(cfg.DescriptorSet) {
		cfg.DescriptorSet = filepath.Join(filepath.Dir(path), cfg.DescriptorSet)
	}
	if len(m.Companion) > 0 && string(m.Companion) != "null" {
		if cfg.Format < 2 {
			return nil, fmt.Errorf("companion manifest: format %d has no companion section; a manifest with one is format 2", cfg.Format)
		}
		var c Companion
		if err := json.Unmarshal(m.Companion, &c); err != nil {
			return nil, fmt.Errorf("companion manifest: companion: %w", err)
		}
		cfg.Companion = &c
	}
	if cfg.Files, err = loadDescriptors(cfg.DescriptorSet); err != nil {
		return nil, err
	}
	if cfg.Companion != nil {
		if err := cfg.Companion.check(cfg.Files); err != nil {
			return nil, fmt.Errorf("companion manifest: %w", err)
		}
	}
	return cfg, nil
}

// loadDescriptors reads a FileDescriptorSet (with its imports). The
// options of the w17 annotation protos are decoded as the linked extension
// types (this package links sdk/go/pb/w17 through lib/egress), so an egress
// client loaded here is the one a generated Go package would register.
func loadDescriptors(path string) (*protoregistry.Files, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the bundle's own descriptor set
	if err != nil {
		return nil, fmt.Errorf("companion descriptor set: %w", err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("companion descriptor set %s: %w", path, err)
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return nil, fmt.Errorf("companion descriptor set %s: %w", path, err)
	}
	return files, nil
}

// check refuses what the SDKs refuse in the same section (py manifest.py,
// ts manifest.ts): the three must agree on which manifests boot.
func (c *Companion) check(files *protoregistry.Files) error {
	if c.AddressEnv == "" {
		return errors.New("address_env is required")
	}
	clients := make(map[string]bool, len(c.Egress))
	services := make(map[string]bool, len(c.Egress))
	for i := range c.Egress {
		e := &c.Egress[i]
		if e.Client == "" || e.Service == "" || e.DSNEnv == "" {
			return fmt.Errorf("egress[%d]: client, service and dsn_env are required", i)
		}
		if clients[e.Client] {
			return fmt.Errorf("egress: client %s is listed twice", e.Client)
		}
		clients[e.Client] = true
		// One gRPC server registers a service once (a second registration
		// is a fatal error there), and a tier could not tell them apart.
		if services[e.Service] {
			return fmt.Errorf("egress: service %s is listed twice", e.Service)
		}
		services[e.Service] = true
		d, err := files.FindDescriptorByName(protoreflect.FullName(e.Service))
		if err != nil {
			return fmt.Errorf("egress client %s: service %s is not in the descriptor set", e.Client, e.Service)
		}
		svc, ok := d.(protoreflect.ServiceDescriptor)
		if !ok {
			return fmt.Errorf("egress client %s: %s is not a service", e.Client, e.Service)
		}
		e.descriptor = svc
	}
	fs := c.FileStorage
	if fs == nil {
		return nil
	}
	if fs.Service != FilesService {
		return fmt.Errorf("files.service is %q; the companion serves %s", fs.Service, FilesService)
	}
	conns := make(map[string]bool, len(fs.Connections))
	for _, cn := range fs.Connections {
		if cn.Name == "" || cn.RootEnv == "" {
			return errors.New("files.connections[]: name and root_env are required")
		}
		if conns[cn.Name] {
			return fmt.Errorf("files.connections: %s is listed twice", cn.Name)
		}
		conns[cn.Name] = true
	}
	fields := make(map[string]bool, len(fs.Placements))
	for _, p := range fs.Placements {
		if p.Field == "" || p.Connection == "" {
			return errors.New("files.placements[]: field and connection are required")
		}
		if fields[p.Field] {
			return fmt.Errorf("files.placements: %s is listed twice", p.Field)
		}
		fields[p.Field] = true
		if !conns[p.Connection] {
			return fmt.Errorf("files.placements: %s is on connection %s, which is not in files.connections", p.Field, p.Connection)
		}
	}
	return nil
}

// Register serves cfg's companion on reg: every egress client
// ([egress.RegisterFromEnv], its DSN read through lookup — the secret chain,
// secret.NewResolver().Lookup) and, when the domain has upload connections,
// W17Files over a store per connection, opened from the connection's root
// variable in the process environment (kvfs/store.Open; a writable
// connection's root is created, a read-only one must exist).
//
// It fails, naming the variable and never its value, on a DSN that does not
// parse and on a root that is unset or cannot be opened — a Go tier refuses
// the same at boot. An unset DSN is not an error: the client is served
// refusing every call with FAILED_PRECONDITION naming the variable. No
// upstream is contacted here.
//
// The returned closer releases what Register opened; call it after the
// server has stopped.
func Register(ctx context.Context, reg grpc.ServiceRegistrar, cfg *Config, lookup func(string) (string, bool)) (closer func(context.Context) error, err error) {
	if cfg == nil || cfg.Companion == nil {
		return nil, errors.New("companion: the manifest has no companion section — this domain's business tier needs no companion")
	}
	c := cfg.Companion
	for _, e := range c.Egress {
		svc := e.descriptor
		if svc == nil {
			return nil, fmt.Errorf("companion: egress client %s was not resolved — build the Config with Load", e.Client)
		}
		if err := egress.RegisterFromEnv(reg, svc, e.DSNEnv, lookup); err != nil {
			return nil, fmt.Errorf("companion: egress client %s: %w", e.Client, err)
		}
	}
	if c.FileStorage != nil {
		conns := make(map[string]kvfsgrpc.Conn, len(c.FileStorage.Connections))
		for _, cn := range c.FileStorage.Connections {
			root := os.Getenv(cn.RootEnv)
			if root == "" {
				return nil, fmt.Errorf("companion: upload connection %q: %s is required", cn.Name, cn.RootEnv)
			}
			d, err := kvfsstore.Open(ctx, root, cn.Writable)
			if err != nil {
				return nil, fmt.Errorf("companion: upload connection %q (%s): %w", cn.Name, cn.RootEnv, err)
			}
			conns[cn.Name] = kvfsgrpc.Conn{Driver: d, Writable: cn.Writable}
		}
		placements := make(map[string]kvfsgrpc.Placement, len(c.FileStorage.Placements))
		for _, p := range c.FileStorage.Placements {
			placements[p.Field] = kvfsgrpc.Placement{Connection: p.Connection, Placement: kvfs.Placement{
				BucketPath:      p.BucketPath,
				ExpectedObjects: p.ExpectedObjects,
				MaxPerBucket:    p.MaxPerBucket,
				MaxSizeBytes:    p.MaxSizeBytes,
			}}
		}
		filespb.RegisterW17FilesServer(reg, kvfsgrpc.NewServer(conns, placements))
	}
	// Neither an egress caller nor a kvfs driver holds anything that needs
	// closing; the closer is the seam for one that will.
	return func(context.Context) error { return nil }, nil
}
