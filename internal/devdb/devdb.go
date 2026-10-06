// Package devdb manages a throwaway ("scratch") PostgreSQL instance that pg-dac
// uses for two purposes:
//
//   - Normalization: authored .sql or a .pgdac artifact is applied to a scratch
//     database and re-introspected, so it canonicalizes identically to a model
//     read from any live database (the "normalize via a real DB" strategy).
//   - Plan validation: a generated migration is replayed against a scratch copy
//     of the source schema to prove it executes and converges before the user
//     trusts it.
//
// The instance is an auto-managed embedded PostgreSQL (no external server or
// Docker required); the binaries are downloaded once and cached by the
// embedded-postgres library.
//
// Starting a PostgreSQL server takes a few seconds, so a single CLI invocation
// should start at most one. Use WithShared to put a lazily-started, shared server
// on a context; Acquire then hands out cheap, isolated scratch DATABASES on that
// one server (creating a server only if there is no shared one).
package devdb

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
)

const dbUser, dbPass = "postgres", "postgres"

// Instance is a running scratch PostgreSQL server. Call Stop when done.
type Instance struct {
	pg      *embeddedpostgres.EmbeddedPostgres
	port    uint32
	baseDir string
	counter int64
}

// Config configures a scratch instance.
type Config struct {
	Version      embeddedpostgres.PostgresVersion
	StartTimeout time.Duration
}

// DefaultConfig returns the recommended scratch configuration.
func DefaultConfig() Config {
	return Config{Version: embeddedpostgres.V16, StartTimeout: 60 * time.Second}
}

// Start launches a scratch PostgreSQL instance on a free local port.
func Start(ctx context.Context, cfg Config) (*Instance, error) {
	if cfg.Version == "" {
		cfg.Version = embeddedpostgres.V16
	}
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = 60 * time.Second
	}
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("allocate port: %w", err)
	}
	baseDir, err := os.MkdirTemp("", "pgdac-devdb-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}

	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(cfg.Version).
		Port(port).
		Username(dbUser).
		Password(dbPass).
		Database("postgres").
		RuntimePath(filepath.Join(baseDir, "runtime")).
		DataPath(filepath.Join(baseDir, "data")).
		BinariesPath(binariesCache(cfg.Version)).
		StartTimeout(cfg.StartTimeout).
		Logger(nil))

	// Serialize the (first-time) binary extraction across processes (per version):
	// parallel test binaries — or two concurrent pg-dac invocations — would
	// otherwise race to populate the shared binaries cache and fail to rename
	// files on Windows. Held only for the duration of Start.
	unlock := acquireStartLock(cfg.Version)
	err = pg.Start()
	unlock()
	if err != nil {
		_ = os.RemoveAll(baseDir)
		return nil, fmt.Errorf("start scratch postgres: %w", err)
	}
	return &Instance{pg: pg, port: port, baseDir: baseDir}, nil
}

// acquireStartLock takes a best-effort cross-process lock (an exclusively-created
// lock file) and returns a release function. It steals locks older than two
// minutes (a crashed holder) and, if it cannot acquire within the deadline,
// proceeds anyway so a stuck lock never deadlocks a start.
func acquireStartLock(v embeddedpostgres.PostgresVersion) func() {
	lockPath := binariesCache(v) + ".start.lock"
	_ = os.MkdirAll(filepath.Dir(lockPath), 0o755)
	deadline := time.Now().Add(90 * time.Second)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { _ = os.Remove(lockPath) }
		}
		if fi, e := os.Stat(lockPath); e == nil && time.Since(fi.ModTime()) > 2*time.Minute {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return func() {}
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// ConnString returns a connection string to the instance's default database.
func (i *Instance) ConnString() string { return i.connStringFor("postgres") }

func (i *Instance) connStringFor(db string) string {
	return fmt.Sprintf("postgres://%s:%s@localhost:%d/%s?sslmode=disable", dbUser, dbPass, i.port, db)
}

// Stop shuts the instance down and removes its temp directory.
func (i *Instance) Stop() error {
	err := i.pg.Stop()
	_ = os.RemoveAll(i.baseDir)
	return err
}

// newScratchDB creates a fresh, isolated database on the instance and returns a
// Scratch whose Release drops it.
func (i *Instance) newScratchDB(ctx context.Context) (*Scratch, error) {
	n := atomic.AddInt64(&i.counter, 1)
	name := fmt.Sprintf("pgdac_scratch_%d", n)

	admin, err := pgx.Connect(ctx, i.ConnString())
	if err != nil {
		return nil, fmt.Errorf("connect to scratch server: %w", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return nil, fmt.Errorf("create scratch database: %w", err)
	}

	connStr := i.connStringFor(name)
	release := func() error {
		c, err := pgx.Connect(context.Background(), i.ConnString())
		if err != nil {
			return err
		}
		defer c.Close(context.Background())
		_, err = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		return err
	}
	return &Scratch{ConnString: connStr, release: release}, nil
}

// Scratch is an isolated, empty scratch database. Call Release when done.
type Scratch struct {
	ConnString string
	release    func() error
}

// Release drops the scratch database (and, for a dedicated server, stops it).
func (s *Scratch) Release() error {
	if s.release == nil {
		return nil
	}
	return s.release()
}

// VersionFor maps a PostgreSQL major version to the matching embedded-postgres
// version, so a scratch database used for normalization/validation matches the
// schema's target major (mismatched majors can introduce false diffs and reject
// newer syntax). Unknown/0 falls back to a stable default.
func VersionFor(major int) embeddedpostgres.PostgresVersion {
	switch major {
	case 18:
		return embeddedpostgres.V18
	case 17:
		return embeddedpostgres.V17
	case 16:
		return embeddedpostgres.V16
	case 15:
		return embeddedpostgres.V15
	case 14:
		return embeddedpostgres.V14
	case 13:
		return embeddedpostgres.V13
	case 12:
		return embeddedpostgres.V12
	case 11:
		return embeddedpostgres.V11
	default:
		return embeddedpostgres.V16
	}
}

// --- shared-server plumbing ---

type ctxKey struct{}

// holder lazily starts one shared server per requested PostgreSQL version, so a
// single process can serve scratch databases of different majors while still
// amortizing startup within each version.
type holder struct {
	mu    sync.Mutex
	insts map[embeddedpostgres.PostgresVersion]*Instance
}

func (h *holder) get(ctx context.Context, v embeddedpostgres.PostgresVersion) (*Instance, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if inst, ok := h.insts[v]; ok {
		return inst, nil
	}
	inst, err := Start(ctx, Config{Version: v})
	if err != nil {
		return nil, err
	}
	if h.insts == nil {
		h.insts = map[embeddedpostgres.PostgresVersion]*Instance{}
	}
	h.insts[v] = inst
	return inst, nil
}

func (h *holder) stopAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, inst := range h.insts {
		_ = inst.Stop()
	}
}

// WithShared returns a context carrying lazily-started shared scratch servers
// (one per PostgreSQL version requested via Acquire) and a cleanup function that
// stops them all.
func WithShared(ctx context.Context) (context.Context, func()) {
	h := &holder{}
	return context.WithValue(ctx, ctxKey{}, h), h.stopAll
}

// Acquire returns an isolated, empty scratch database on a server matching the
// requested PostgreSQL major version. If the context carries shared servers
// (via WithShared) it reuses/starts the per-version warm server; otherwise it
// starts a dedicated server whose lifetime is tied to the returned Scratch.
func Acquire(ctx context.Context, major int) (*Scratch, error) {
	v := VersionFor(major)
	if h, ok := ctx.Value(ctxKey{}).(*holder); ok {
		inst, err := h.get(ctx, v)
		if err != nil {
			return nil, err
		}
		return inst.newScratchDB(ctx)
	}

	inst, err := Start(ctx, Config{Version: v})
	if err != nil {
		return nil, err
	}
	sc, err := inst.newScratchDB(ctx)
	if err != nil {
		_ = inst.Stop()
		return nil, err
	}
	dropDB := sc.release
	sc.release = func() error {
		err := dropDB()
		_ = inst.Stop()
		return err
	}
	return sc, nil
}

func freePort() (uint32, error) {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return uint32(l.Addr().(*net.TCPAddr).Port), nil
}

// binariesCache returns a PER-VERSION directory for the downloaded/extracted
// PostgreSQL binaries. Each major must have its own dir, or different versions
// clobber each other's binaries on extraction (and collide with a running
// instance's locked files).
func binariesCache(v embeddedpostgres.PostgresVersion) string {
	ver := strings.ReplaceAll(string(v), ".", "_")
	if home, err := os.UserCacheDir(); err == nil {
		return filepath.Join(home, "pg-dac", "embedded-postgres", ver)
	}
	return filepath.Join(os.TempDir(), "pg-dac-embedded-postgres", ver)
}
