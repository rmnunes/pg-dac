package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/rmnunes/pg-dac/internal/dbconn"
	"github.com/rmnunes/pg-dac/internal/deploy"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/drift"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/execute"
	"github.com/rmnunes/pg-dac/internal/introspect"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/project"
	"github.com/rmnunes/pg-dac/internal/publish"
	"github.com/rmnunes/pg-dac/internal/source"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
)

// newServeCmd runs pg-dac as a local daemon: it keeps ONE embedded scratch
// Postgres warm for its whole lifetime (so GUI calls are sub-second instead of
// re-paying the embedded-PG cold start) and exposes the comparison verbs over a
// loopback HTTP/SSE API. The first stdout line is the bound address (the
// handshake the GUI supervisor reads); a bearer token from $PGDAC_TOKEN, when
// set, is required on every request — and must be set to bind anything but a
// loopback address, since the daemon connects to whatever it is told and runs
// DDL there.
func newServeCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:    "serve",
		Short:  "Run pg-dac as a local daemon (warm scratch Postgres + HTTP/SSE API) for the GUI",
		Hidden: true, // a tooling/integration entry point, not a day-to-day verb
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd, addr, os.Getenv("PGDAC_TOKEN"))
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:0", "address to bind (default: loopback, OS-chosen port)")
	return cmd
}

// maxRequestBody bounds a request body; a compare request is a few hundred
// bytes of specs and options plus the selection, far below this.
const maxRequestBody = 1 << 20

// bindAllowed refuses to expose the daemon beyond the local machine without a
// token: it accepts connection strings and runs DDL on behalf of its callers.
func bindAllowed(addr, token string) error {
	if token != "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", addr, err)
	}
	if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("refusing to bind %s without PGDAC_TOKEN: the daemon connects to the databases it is told to and runs DDL there", addr)
}

func runServe(cmd *cobra.Command, addr, token string) error {
	if err := bindAllowed(addr, token); err != nil {
		return err
	}
	// One warm scratch Postgres for the whole process lifetime.
	ctx, cleanup := devdb.WithShared(context.Background())
	defer cleanup()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", addr, err)
	}

	d := &daemon{ctx: ctx, token: token, version: appVersion(), log: cmd.ErrOrStderr()}
	var once sync.Once
	stop := make(chan struct{})
	shutdown := func() { once.Do(func() { close(stop) }) }

	mux := http.NewServeMux()
	mux.HandleFunc("/health", d.handle(http.MethodGet, d.health))
	mux.HandleFunc("/plan", d.handle(http.MethodPost, d.plan))
	mux.HandleFunc("/replan", d.handle(http.MethodPost, d.replan))
	mux.HandleFunc("/cancel", d.handle(http.MethodPost, d.cancelOp))
	mux.HandleFunc("/script", d.handle(http.MethodPost, d.script))
	mux.HandleFunc("/publish", d.handle(http.MethodPost, d.publish))
	mux.HandleFunc("/drift", d.handle(http.MethodPost, d.drift))
	mux.HandleFunc("/shutdown", d.handle(http.MethodPost, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		shutdown()
	}))

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	// Handshake: first stdout line is the address the supervisor connects to.
	fmt.Fprintln(cmd.OutOrStdout(), ln.Addr().String())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
		case <-stop:
		}
		_ = srv.Shutdown(context.Background())
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

type daemon struct {
	ctx     context.Context
	token   string
	version string
	log     interface{ Write([]byte) (int, error) }

	// ops tracks in-flight cancelable operations by client-supplied opId so a
	// /cancel request can abort a running compare (which propagates through the
	// pgx-based introspection and scratch build, freeing the warm Postgres).
	mu  sync.Mutex
	ops map[string]context.CancelFunc

	// last is what the latest successful /plan computed from, so /replan can
	// re-run the engine for a changed selection or option without reading the
	// sources again (SSDT re-scripts a selection change the same way).
	lastMu sync.Mutex
	last   *lastCompare
}

// compareKey identifies what a comparison was computed from: the two specs
// and everything that shapes how they are read.
type compareKey struct {
	from, to, schemas, requireKey string
	targetMajor                   int
}

func (r compareReq) key() compareKey {
	return compareKey{from: r.From, to: r.To, schemas: strings.Join(r.Schemas, "\x00"), requireKey: r.RequireKey, targetMajor: r.TargetMajor}
}

// lastCompare holds the resolved sides of the latest comparison.
type lastCompare struct {
	key      compareKey
	from, to []byte // canonical models, marshalled: each re-plan works on a fresh copy
	prelude  []sqlgen.Statement
	dplan    *deploy.Plan
	warnings []sideWarning // target/source/rename warnings from that read
}

// track derives a cancelable child of the daemon context for one request and,
// when opID is non-empty, registers its cancel func so /cancel can find it. The
// returned func cancels and unregisters; call it with defer.
func (d *daemon) track(opID string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(d.ctx)
	if opID != "" {
		d.mu.Lock()
		if d.ops == nil {
			d.ops = make(map[string]context.CancelFunc)
		}
		d.ops[opID] = cancel
		d.mu.Unlock()
	}
	return ctx, func() {
		cancel()
		if opID != "" {
			d.mu.Lock()
			delete(d.ops, opID)
			d.mu.Unlock()
		}
	}
}

// cancelOp aborts the in-flight operation with the given opId, if any.
func (d *daemon) cancelOp(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OpID string `json:"opId"`
	}
	if err := decodeInto(w, r, &body); err != nil {
		badRequest(w, err)
		return
	}
	d.mu.Lock()
	cancel := d.ops[body.OpID]
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	writeJSON(w, http.StatusOK, map[string]any{"canceled": cancel != nil})
}

// handle wraps a handler with the bearer-token check and the method check.
func (d *daemon) handle(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.token != "" && r.Header.Get("Authorization") != "Bearer "+d.token {
			fail(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if r.Method != method {
			fail(w, http.StatusMethodNotAllowed, "method not allowed; use "+method)
			return
		}
		h(w, r)
	}
}

func (d *daemon) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"version":       d.version,
		"formatVersion": engine.PlanFormatVersion,
	})
}

// compareReq is the request body for /plan, /replan, /script, /publish and
// /drift.
type compareReq struct {
	From                   string   `json:"from"`
	To                     string   `json:"to"`
	OpID                   string   `json:"opId"`
	Schemas                []string `json:"schemas"`
	TargetMajor            int      `json:"targetMajor"`
	Online                 *bool    `json:"online"`
	AllowHazards           bool     `json:"allowHazards"`
	DropObjectsNotInSource bool     `json:"dropObjectsNotInSource"`
	IncludeIDs             []string `json:"includeIds"`
	ExcludeIDs             []string `json:"excludeIds"`
	RequireKey             string   `json:"requireKey"`
	SkipDeployScripts      bool     `json:"skipDeployScripts"`
	AllowDrift             bool     `json:"allowDrift"`
	RecordBaseline         *bool    `json:"recordBaseline"` // default true
	// Comparison semantics; nil / "" defers to the desired-state project's
	// pgdac.yaml compare: block.
	IgnorePrivileges   *bool             `json:"ignorePrivileges"`
	ColumnOrder        string            `json:"columnOrder"`
	IgnoreComments     *bool             `json:"ignoreComments"`
	CompareWhitespace  *bool             `json:"compareWhitespace"`
	ExcludeObjectTypes []string          `json:"excludeObjectTypes"`
	Variables          map[string]string `json:"variables"`
	// Publish execution.
	SingleTransaction  bool `json:"singleTransaction"`
	LockTimeoutMs      int  `json:"lockTimeoutMs"`
	StatementTimeoutMs int  `json:"statementTimeoutMs"`
}

// sideWarning is a warning tagged with the side of the comparison it concerns:
// "target" (the engine's --from, the database that gets updated), "source"
// (--to, the desired state) or "plan" (the generated migration, the refactorlog
// included).
type sideWarning struct {
	Side    string `json:"side"`
	Message string `json:"message"`
}

func tagWarnings(side string, ws []string) []sideWarning {
	out := make([]sideWarning, 0, len(ws))
	for _, w := range ws {
		out = append(out, sideWarning{Side: side, Message: w})
	}
	return out
}

func mergeWarnings(groups ...[]sideWarning) []sideWarning {
	out := []sideWarning{}
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// engineOptions derives the engine options; the desired-state source's
// dropPolicy and compare: block are the defaults, explicit request values win.
func (r compareReq) engineOptions(dplan *deploy.Plan) engine.Options {
	opts := engine.DefaultOptions()
	opts.TargetMajor = r.TargetMajor
	if r.Online != nil {
		opts.Online = *r.Online
	}
	opts.AllowHazards = r.AllowHazards
	deploy.ApplyDropPolicy(&opts, dplan.DropPolicy)
	deploy.ApplyCompareSettings(&opts, dplan.Compare)
	if r.DropObjectsNotInSource {
		opts.DropObjectsNotInSource = true
	}
	if r.IgnorePrivileges != nil {
		opts.IgnorePrivileges = *r.IgnorePrivileges
	}
	if r.ColumnOrder != "" {
		opts.ColumnOrder = r.ColumnOrder
	}
	if r.IgnoreComments != nil {
		opts.IgnoreComments = *r.IgnoreComments
	}
	if r.CompareWhitespace != nil {
		opts.CompareWhitespace = *r.CompareWhitespace
	}
	if len(r.ExcludeObjectTypes) > 0 {
		opts.ExcludeObjectTypes = r.ExcludeObjectTypes
	}
	opts.IncludeIDs = toSet(r.IncludeIDs)
	opts.ExcludeIDs = toSet(r.ExcludeIDs)
	return opts
}

// sourceOptions derives the source resolution options (schemas, target major,
// signer trust) of a request.
func (r compareReq) sourceOptions() (source.Options, error) {
	return source.OptionsFor(r.Schemas, r.TargetMajor, r.RequireKey)
}

// execOptions derives the lock-safety and batching options of a publish.
func (r compareReq) execOptions() execute.Options {
	opts := execute.DefaultOptions()
	opts.SingleTransaction = r.SingleTransaction
	if r.LockTimeoutMs > 0 {
		opts.LockTimeout = time.Duration(r.LockTimeoutMs) * time.Millisecond
	}
	if r.StatementTimeoutMs > 0 {
		opts.StatementTimeout = time.Duration(r.StatementTimeoutMs) * time.Millisecond
	}
	return opts
}

func (r compareReq) recordBaseline() bool { return r.RecordBaseline == nil || *r.RecordBaseline }

// deployPlan reads the desired-state source's deployment plan, tolerating
// sources that have none.
func deployPlan(spec string) *deploy.Plan {
	p, err := deploy.ForSource(spec)
	if err != nil || p == nil {
		return &deploy.Plan{}
	}
	return p
}

// sse prepares a Server-Sent Events response and returns the function that
// emits one event, or an error when the connection cannot stream.
func sse(w http.ResponseWriter) (func(event string, v any), error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming unsupported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	return func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}, nil
}

// compared is a comparison of two resolved sides.
type compared struct {
	from, to *model.Database
	res      *engine.Result
	dplan    *deploy.Plan
	prelude  []sqlgen.Statement
	warnings []sideWarning // target, source and refactorlog warnings from the read
}

// compare resolves both sides of a request and runs the engine; phase, when
// set, narrates the slow steps (reading each side, then diffing).
func (d *daemon) compare(ctx context.Context, req compareReq, phase func(step, message string, extra map[string]any)) (*compared, error) {
	if phase == nil {
		phase = func(string, string, map[string]any) {}
	}
	srcOpts, err := req.sourceOptions()
	if err != nil {
		return nil, err
	}
	dplan := deployPlan(req.To)
	phase("read-from", "Reading Target…", map[string]any{"source": req.From})
	from, fw, err := source.Resolve(ctx, req.From, srcOpts)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	phase("read-to", "Reading Source…", map[string]any{"source": req.To, "fromCount": objectCount(from)})
	to, tw, err := source.Resolve(ctx, req.To, srcOpts)
	if err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	phase("diff", "Diffing and generating plan…", map[string]any{"toCount": objectCount(to)})
	from, prelude, rwarns, err := pendingRenames(ctx, req.From, from, dplan)
	if err != nil {
		return nil, err
	}
	res, err := engine.Compare(from, to, req.engineOptions(dplan))
	if err != nil {
		return nil, err
	}
	res.Prelude = prelude
	return &compared{from: from, to: to, res: res, dplan: dplan, prelude: prelude,
		warnings: mergeWarnings(tagWarnings("target", fw), tagWarnings("source", tw), tagWarnings("plan", rwarns))}, nil
}

// plan streams the comparison over Server-Sent Events so the GUI can show what
// it is doing (reading each side, then diffing/planning) and cancel mid-flight:
// "phase" events narrate progress, then a single "result" (the PlanDoc),
// "canceled", or "error".
func (d *daemon) plan(w http.ResponseWriter, r *http.Request) {
	req, err := decode(w, r)
	if err != nil {
		badRequest(w, err)
		return
	}
	send, err := sse(w)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, done := d.track(req.OpID)
	defer done()

	c, err := d.compare(ctx, req, func(step, message string, extra map[string]any) {
		ev := map[string]any{"step": step, "message": message}
		maps.Copy(ev, extra)
		send("phase", ev)
	})
	if err != nil {
		if ctx.Err() != nil {
			send("canceled", map[string]any{})
			return
		}
		send("error", map[string]any{"message": err.Error()})
		return
	}
	doc, err := c.res.PlanDoc()
	if err != nil {
		send("error", map[string]any{"message": err.Error()})
		return
	}
	d.remember(req, c)
	send("result", map[string]any{"plan": doc, "warnings": mergeWarnings(c.warnings, tagWarnings("plan", c.res.Warnings))})
}

// remember keeps the resolved sides of a comparison for /replan.
func (d *daemon) remember(req compareReq, c *compared) {
	fb, err := c.from.Marshal()
	if err == nil {
		var tb []byte
		if tb, err = c.to.Marshal(); err == nil {
			d.lastMu.Lock()
			d.last = &lastCompare{key: req.key(), from: fb, to: tb, prelude: c.prelude, dplan: c.dplan, warnings: c.warnings}
			d.lastMu.Unlock()
			return
		}
	}
	fmt.Fprintf(d.log, "pg-dac serve: the comparison cannot be kept for re-planning: %v\n", err)
}

// replan re-runs the engine on the sides of the latest /plan with the
// request's current selection and options, without reading the sources again.
// Sub-second, so the GUI can keep the plan in step with every include/exclude
// toggle: an excluded object that still has to be recreated for a dependency
// shows up as such, and the statement count is the one a publish would run.
// 409 when the sources (or how they are read) differ from the last comparison,
// or none ran yet.
func (d *daemon) replan(w http.ResponseWriter, r *http.Request) {
	req, err := decode(w, r)
	if err != nil {
		badRequest(w, err)
		return
	}
	d.lastMu.Lock()
	last := d.last
	d.lastMu.Unlock()
	if last == nil || last.key != req.key() {
		fail(w, http.StatusConflict, "no comparison of these sources to re-plan; run Compare first")
		return
	}
	from, err1 := model.Unmarshal(last.from)
	to, err2 := model.Unmarshal(last.to)
	if err1 != nil || err2 != nil {
		fail(w, http.StatusConflict, "stored comparison is unreadable; run Compare again")
		return
	}
	res, err := engine.Compare(from, to, req.engineOptions(last.dplan))
	if err != nil {
		badRequest(w, err)
		return
	}
	res.Prelude = last.prelude
	doc, err := res.PlanDoc()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan": doc,
		"warnings": mergeWarnings(last.warnings, tagWarnings("plan", res.Warnings))})
}

// objectCount is a rough size of a model, shown in the compare progress dialog.
func objectCount(m *model.Database) int {
	n := len(m.Extensions)
	for _, s := range m.Schemas {
		n += len(s.Tables) + len(s.Views) + len(s.MatViews) +
			len(s.Sequences) + len(s.Enums) + len(s.Collations) + len(s.Functions)
	}
	return n
}

func (d *daemon) script(w http.ResponseWriter, r *http.Request) {
	req, err := decode(w, r)
	if err != nil {
		badRequest(w, err)
		return
	}
	ctx, done := d.track(req.OpID)
	defer done()
	c, err := d.compare(ctx, req, nil)
	if err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sql": c.res.Script(req.AllowHazards),
		"warnings": mergeWarnings(c.warnings, tagWarnings("plan", c.res.Warnings))})
}

// publish streams a live apply over Server-Sent Events: a "start" event, one
// "progress" event per statement, then "done" or "error".
func (d *daemon) publish(w http.ResponseWriter, r *http.Request) {
	req, err := decode(w, r)
	if err != nil {
		badRequest(w, err)
		return
	}
	isDB := source.IsConnString(req.From)
	isProject := !isDB && project.Exists(strings.TrimPrefix(req.From, "file://"))
	if !isDB && !isProject {
		badRequest(w, fmt.Errorf("publish target (from) must be a database connection string or a project directory"))
		return
	}
	send, err := sse(w)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if isProject {
		d.publishProject(req, send)
		return
	}

	srcOpts, err := req.sourceOptions()
	if err != nil {
		send("error", map[string]any{"message": err.Error()})
		return
	}
	conn, err := dbconn.Connect(d.ctx, req.From)
	if err != nil {
		send("error", map[string]any{"message": err.Error()})
		return
	}
	defer conn.Close(d.ctx)

	dplan := deployPlan(req.To)
	preq := publish.Request{
		Source:            req.To,
		SourceOptions:     srcOpts,
		Plan:              dplan,
		Engine:            req.engineOptions(dplan),
		Variables:         req.Variables,
		SkipDeployScripts: req.SkipDeployScripts,
		AllowDrift:        req.AllowDrift,
		RecordBaseline:    req.recordBaseline(),
		Exec:              req.execOptions(),
		Generator:         "pg-dac " + d.version,
	}
	// Warnings gather until "start", which carries them; the daemon's sides
	// are target / source / plan.
	var warnings []sideWarning
	total := 0
	progress := func(sql string) {
		send("progress", map[string]any{"index": 0, "total": total, "sql": sql})
	}
	obs := publish.Observer{
		Warnings: func(side string, msgs []string) {
			if side == "refactorlog" {
				side = "plan"
			}
			warnings = append(warnings, tagWarnings(side, msgs)...)
		},
		Drift:  func(msg string) { warnings = append(warnings, sideWarning{Side: "target", Message: msg}) },
		Rename: func(desc string) { progress("-- refactorlog: " + desc) },
		Planned: func(res *engine.Result, pre, post []deploy.Script) {
			total = len(res.Statements)
			send("start", map[string]any{"statementCount": total, "dataLoss": res.HasDataLoss(),
				"deployScripts": len(pre) + len(post), "warnings": mergeWarnings(warnings)})
		},
		Script: func(phase, name string) { progress("-- " + phase + "-deploy script: " + name) },
		Statement: func(index, total int, s sqlgen.Statement) {
			send("progress", map[string]any{"index": index, "total": total, "sql": s.SQL})
		},
	}
	res, err := publish.Run(d.ctx, conn, preq, obs)
	if err != nil {
		applied := 0
		if res != nil {
			applied = res.Applied
		}
		ev := map[string]any{"applied": applied, "total": total, "message": err.Error()}
		var blocked *publish.BlockedError
		switch {
		case publish.IsDrift(err):
			ev["drift"] = true
			ev["message"] = err.Error() + "; set allowDrift to publish anyway"
		case errors.As(err, &blocked) && len(blocked.MissingRoles) > 0:
			ev["message"] = "refusing to publish: " + blocked.Error()
		case errors.As(err, &blocked):
			ev["message"] = "refusing to publish: " + blocked.Error() + "; set allowHazards to proceed"
		}
		send("error", ev)
		return
	}
	send("done", map[string]any{"applied": res.Applied, "deployScripts": res.DeployScripts(), "baselineRecorded": res.Baseline != nil})
}

// drift reports how a database differs from the baseline its last publish
// recorded: {drifted, currentSha256, baseline{...}, changes: PlanDoc}.
func (d *daemon) drift(w http.ResponseWriter, r *http.Request) {
	req, err := decode(w, r)
	if err != nil {
		badRequest(w, err)
		return
	}
	if !source.IsConnString(req.From) {
		badRequest(w, fmt.Errorf("drift target (from) must be a database connection string"))
		return
	}
	conn, err := dbconn.Connect(d.ctx, req.From)
	if err != nil {
		badRequest(w, err)
		return
	}
	defer conn.Close(d.ctx)
	current, err := introspect.Introspect(d.ctx, conn, introspect.Options{IncludeSchemas: req.Schemas})
	if err != nil {
		badRequest(w, err)
		return
	}
	rep, err := drift.Check(d.ctx, conn, current)
	if err != nil {
		badRequest(w, err)
		return
	}
	out := map[string]any{"drifted": rep.Drifted, "currentSha256": rep.CurrentSHA256}
	if rep.Baseline != nil {
		out["baseline"] = map[string]any{"id": rep.Baseline.ID, "recordedAt": rep.Baseline.RecordedAt, "source": rep.Baseline.Source, "sha256": rep.Baseline.SHA256}
	}
	if rep.Changes != nil {
		doc, err := rep.Changes.PlanDoc()
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		out["changes"] = doc
	}
	writeJSON(w, http.StatusOK, out)
}

// --- helpers ---

// decodeInto reads a bounded JSON request body into v.
func decodeInto(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

func decode(w http.ResponseWriter, r *http.Request) (compareReq, error) {
	var req compareReq
	err := decodeInto(w, r, &req)
	return req, err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail answers with the one error shape every endpoint uses: {"error": msg}.
func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func badRequest(w http.ResponseWriter, err error) {
	fail(w, http.StatusBadRequest, err.Error())
}

func toSet(ss []string) map[string]bool {
	if len(ss) == 0 {
		return nil
	}
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// publishProject streams an update of a declarative project (the target) from
// the desired state: SSDT's "Update project from database". "start" carries
// the number of file changes, one "progress" per file, then "done".
func (d *daemon) publishProject(req compareReq, send func(string, any)) {
	dir := strings.TrimPrefix(req.From, "file://")
	man, err := project.Load(dir)
	if err != nil {
		send("error", map[string]any{"message": "target project: " + err.Error()})
		return
	}
	srcOpts, err := req.sourceOptions()
	if err != nil {
		send("error", map[string]any{"message": err.Error()})
		return
	}
	dplan := deployPlan(req.To)
	deploy.WithProjectDefaults(dplan, man)
	pr, err := publish.Project(d.ctx, publish.ProjectRequest{
		Dir: dir, Manifest: man, Source: req.To, SourceOptions: srcOpts, Engine: req.engineOptions(dplan),
	})
	if err != nil {
		send("error", map[string]any{"message": err.Error()})
		return
	}
	plan := pr.Plan
	var refused []string
	for _, r := range plan.Refused {
		refused = append(refused, r.Object+": "+r.Reason)
	}
	send("start", map[string]any{"statementCount": len(plan.Changes), "dataLoss": false, "deployScripts": 0, "files": true,
		"warnings": mergeWarnings(tagWarnings("target", pr.FromWarnings), tagWarnings("source", pr.ToWarnings), tagWarnings("plan", pr.Migration.Warnings), tagWarnings("plan", refused))})
	if len(plan.Refused) > 0 && !req.AllowHazards {
		send("error", map[string]any{"message": fmt.Sprintf("refusing to update the project: %d object(s) are defined imperatively and cannot be rewritten in place (see warnings); set allowHazards to update the others", len(plan.Refused))})
		return
	}
	if plan.Empty() {
		send("done", map[string]any{"applied": 0, "files": true})
		return
	}
	for i, c := range plan.Changes {
		send("progress", map[string]any{"index": i + 1, "total": len(plan.Changes), "sql": "-- " + c.Action + " " + c.Path})
	}
	if err := plan.Apply(); err != nil {
		send("error", map[string]any{"applied": 0, "total": len(plan.Changes), "message": err.Error()})
		return
	}
	send("done", map[string]any{"applied": len(plan.Changes), "files": true, "rolesAdded": plan.RolesAdded})
}
