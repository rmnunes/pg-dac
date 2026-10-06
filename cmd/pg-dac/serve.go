package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/rmnunes/pg-dac/internal/artifact"
	"github.com/rmnunes/pg-dac/internal/deploy"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/drift"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/execute"
	"github.com/rmnunes/pg-dac/internal/introspect"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/refactor"
	"github.com/rmnunes/pg-dac/internal/source"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
)

// newServeCmd runs pg-dac as a local daemon: it keeps ONE embedded scratch
// Postgres warm for its whole lifetime (so GUI calls are sub-second instead of
// re-paying the embedded-PG cold start) and exposes the comparison verbs over a
// loopback HTTP/SSE API. The first stdout line is the bound address (the
// handshake the GUI supervisor reads); a bearer token from $PGDAC_TOKEN, when
// set, is required on every request.
func newServeCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:    "serve",
		Short:  "Run pg-dac as a local daemon (warm scratch Postgres + HTTP/SSE API) for the GUI",
		Hidden: true, // a tooling/integration entry point, not a day-to-day verb
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd, addr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:0", "address to bind (default: loopback, OS-chosen port)")
	return cmd
}

func runServe(cmd *cobra.Command, addr string) error {
	// One warm scratch Postgres for the whole process lifetime.
	ctx, cleanup := devdb.WithShared(context.Background())
	defer cleanup()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", addr, err)
	}

	trusted, err := artifact.LoadTrustedKeys()
	if err != nil {
		return fmt.Errorf("read trusted keys: %w", err)
	}
	d := &daemon{ctx: ctx, token: os.Getenv("PGDAC_TOKEN"), version: appVersion(), trustedKeys: trusted}
	var once sync.Once
	stop := make(chan struct{})
	shutdown := func() { once.Do(func() { close(stop) }) }

	mux := http.NewServeMux()
	mux.HandleFunc("/health", d.auth(d.health))
	mux.HandleFunc("/plan", d.auth(d.plan))
	mux.HandleFunc("/cancel", d.auth(d.cancelOp))
	mux.HandleFunc("/script", d.auth(d.script))
	mux.HandleFunc("/publish", d.auth(d.publish))
	mux.HandleFunc("/drift", d.auth(d.drift))
	mux.HandleFunc("/shutdown", d.auth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		shutdown()
	}))

	srv := &http.Server{Handler: mux}

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
	ctx         context.Context
	token       string
	version     string
	trustedKeys []string

	// ops tracks in-flight cancelable operations by client-supplied opId so a
	// /cancel request can abort a running compare (which propagates through the
	// pgx-based introspection and scratch build, freeing the warm Postgres).
	mu  sync.Mutex
	ops map[string]context.CancelFunc
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
	_ = json.NewDecoder(r.Body).Decode(&body)
	d.mu.Lock()
	cancel := d.ops[body.OpID]
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	writeJSON(w, http.StatusOK, map[string]any{"canceled": cancel != nil})
}

func (d *daemon) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.token != "" && r.Header.Get("Authorization") != "Bearer "+d.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
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

// compareReq is the request body for /plan, /script, and /publish.
type compareReq struct {
	From                   string   `json:"from"`
	To                     string   `json:"to"`
	OpID                   string   `json:"opId"`
	Schemas                []string `json:"schemas"`
	TargetMajor            int      `json:"targetMajor"`
	Online                 *bool    `json:"online"`
	AllowHazards           bool     `json:"allowHazards"`
	DropObjectsNotInSource bool     `json:"dropObjectsNotInSource"`
	IncludeIDs             []string `json:"includeIDs"`
	ExcludeIDs             []string `json:"excludeIDs"`
	RequireKey             string   `json:"requireKey"`
	SkipDeployScripts      bool     `json:"skipDeployScripts"`
	AllowDrift             bool     `json:"allowDrift"`
	RecordBaseline         *bool    `json:"recordBaseline"` // default true
	// Comparison semantics; nil / "" defers to the desired-state project's
	// pgdac.yaml compare: block.
	IgnorePrivileges  *bool  `json:"ignorePrivileges"`
	ColumnOrder       string `json:"columnOrder"`
	IgnoreComments    *bool  `json:"ignoreComments"`
	CompareWhitespace *bool  `json:"compareWhitespace"`
}

// sideWarning is a warning tagged with the side of the comparison it concerns:
// "target" (the engine's --from, the database that gets updated), "source"
// (--to, the desired state) or "plan" (the generated migration).
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
	opts.IncludeIDs = toSet(r.IncludeIDs)
	opts.ExcludeIDs = toSet(r.ExcludeIDs)
	return opts
}

// sourceOptions derives source resolution options (schemas, target major,
// signer trust) for a request.
func (d *daemon) sourceOptions(r compareReq) source.Options {
	opts := source.Options{Schemas: r.Schemas, TargetMajor: r.TargetMajor, RequireKey: r.RequireKey}
	if r.RequireKey == "" {
		opts.TrustedKeys = d.trustedKeys
	}
	return opts
}

// deployPlan reads the desired-state source's deployment plan, tolerating
// sources that have none.
func deployPlan(spec string) *deploy.Plan {
	p, err := deploy.ForSource(spec)
	if err != nil || p == nil {
		return &deploy.Plan{}
	}
	return p
}

// plan streams the comparison over Server-Sent Events so the GUI can show what
// it is doing (reading each side, then diffing/planning) and cancel mid-flight:
// "phase" events narrate progress, then a single "result" (the PlanDoc),
// "canceled", or "error". Resolving a side is the slow part (introspecting a live
// DB, or applying a project to the scratch Postgres and re-introspecting), so a
// phase is emitted before each.
func (d *daemon) plan(w http.ResponseWriter, r *http.Request) {
	req, err := decode(r)
	if err != nil {
		badRequest(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	ctx, done := d.track(req.OpID)
	defer done()

	// canceled reports the canceled event when an error is actually a cancellation,
	// returning true so the caller stops.
	canceled := func() bool {
		if ctx.Err() != nil {
			send("canceled", map[string]any{})
			return true
		}
		return false
	}

	srcOpts := d.sourceOptions(req)
	dplan := deployPlan(req.To)
	send("phase", map[string]any{"step": "read-from", "message": "Reading Target…", "source": req.From})
	from, fw, err := source.Resolve(ctx, req.From, srcOpts)
	if err != nil {
		if canceled() {
			return
		}
		send("error", map[string]any{"message": "from: " + err.Error()})
		return
	}

	send("phase", map[string]any{"step": "read-to", "message": "Reading Source…", "source": req.To, "fromCount": objectCount(from)})
	to, tw, err := source.Resolve(ctx, req.To, srcOpts)
	if err != nil {
		if canceled() {
			return
		}
		send("error", map[string]any{"message": "to: " + err.Error()})
		return
	}

	send("phase", map[string]any{"step": "diff", "message": "Diffing and generating plan…", "toCount": objectCount(to)})
	from, prelude, rwarns, err := pendingRenames(ctx, req.From, from, dplan)
	if err != nil {
		send("error", map[string]any{"message": err.Error()})
		return
	}
	res, err := engine.Compare(from, to, req.engineOptions(dplan))
	if err != nil {
		if canceled() {
			return
		}
		send("error", map[string]any{"message": err.Error()})
		return
	}
	res.Prelude = prelude
	send("result", map[string]any{"plan": res.PlanDoc(),
		"warnings": mergeWarnings(tagWarnings("target", fw), tagWarnings("source", tw), tagWarnings("plan", rwarns), tagWarnings("plan", res.Warnings))})
}

// pendingRenames applies the desired state's pending refactorlog renames to
// the target model (reading the applied ids when the target is a database) and
// returns the statements that would perform and record them.
func pendingRenames(ctx context.Context, fromSpec string, from *model.Database, dplan *deploy.Plan) (*model.Database, []sqlgen.Statement, []string, error) {
	if dplan == nil || dplan.Refactor == nil || len(dplan.Refactor.Operations) == 0 {
		return from, nil, nil, nil
	}
	applied := map[string]bool{}
	if source.IsConnString(fromSpec) {
		conn, err := pgx.Connect(ctx, fromSpec)
		if err != nil {
			return nil, nil, nil, err
		}
		defer conn.Close(ctx)
		if applied, err = refactor.ReadApplied(ctx, conn); err != nil {
			return nil, nil, nil, err
		}
	}
	renames, warns := refactor.Pending(from, dplan.Refactor, applied)
	if len(renames) == 0 {
		return from, nil, warns, nil
	}
	return refactor.ApplyToModel(from, renames), refactor.Statements(renames), warns, nil
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
	req, err := decode(r)
	if err != nil {
		badRequest(w, err)
		return
	}
	srcOpts := d.sourceOptions(req)
	dplan := deployPlan(req.To)
	from, fw, err := source.Resolve(d.ctx, req.From, srcOpts)
	if err != nil {
		badRequest(w, fmt.Errorf("from: %w", err))
		return
	}
	to, tw, err := source.Resolve(d.ctx, req.To, srcOpts)
	if err != nil {
		badRequest(w, fmt.Errorf("to: %w", err))
		return
	}
	from, prelude, rwarns, err := pendingRenames(d.ctx, req.From, from, dplan)
	if err != nil {
		badRequest(w, err)
		return
	}
	res, err := engine.Compare(from, to, req.engineOptions(dplan))
	if err != nil {
		badRequest(w, err)
		return
	}
	res.Prelude = prelude
	writeJSON(w, http.StatusOK, map[string]any{"sql": res.Script(req.AllowHazards),
		"warnings": mergeWarnings(tagWarnings("target", fw), tagWarnings("source", tw), tagWarnings("plan", rwarns), tagWarnings("plan", res.Warnings))})
}

// publish streams a live apply over Server-Sent Events: a "start" event, one
// "progress" event per statement, then "done" or "error".
func (d *daemon) publish(w http.ResponseWriter, r *http.Request) {
	req, err := decode(r)
	if err != nil {
		badRequest(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if !source.IsConnString(req.From) {
		badRequest(w, fmt.Errorf("publish target (from) must be a database connection string"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	conn, err := pgx.Connect(d.ctx, req.From)
	if err != nil {
		send("error", map[string]any{"message": err.Error()})
		return
	}
	defer conn.Close(d.ctx)

	// Resolve the desired state first: nothing touches the target until the
	// source is known to build.
	dplan := deployPlan(req.To)
	toModel, warns, err := source.Resolve(d.ctx, req.To, d.sourceOptions(req))
	if err != nil {
		send("error", map[string]any{"message": "resolve source: " + err.Error()})
		return
	}
	fromModel, _, err := introspect.IntrospectWithWarnings(d.ctx, conn, introspect.Options{IncludeSchemas: req.Schemas})
	if err != nil {
		send("error", map[string]any{"message": "introspect target: " + err.Error()})
		return
	}
	drifted, hadBaseline := false, false
	if len(req.Schemas) == 0 {
		rep, err := drift.Check(d.ctx, conn, fromModel)
		if err != nil {
			send("error", map[string]any{"message": err.Error()})
			return
		}
		drifted, hadBaseline = rep.Drifted, rep.Baseline != nil
		if rep.Drifted && !req.AllowDrift {
			send("error", map[string]any{"drift": true, "message": fmt.Sprintf(
				"the target has drifted from its last baseline (#%d from %s): %d change(s) were made outside pg-dac; set allowDrift to publish anyway",
				rep.Baseline.ID, rep.Baseline.Source, len(rep.Changes.Plan.Ops))})
			return
		}
	}
	// Refactorlog renames run first, are recorded, and the target is re-read.
	if dplan.Refactor != nil && len(dplan.Refactor.Operations) > 0 {
		applied, err := refactor.ReadApplied(d.ctx, conn)
		if err != nil {
			send("error", map[string]any{"message": err.Error()})
			return
		}
		renames, _ := refactor.Pending(fromModel, dplan.Refactor, applied)
		if len(renames) > 0 {
			for _, r := range renames {
				send("progress", map[string]any{"index": 0, "total": 0, "sql": "-- refactorlog: " + r.Op.Describe()})
			}
			if err := execute.ApplyStatements(d.ctx, conn, refactor.Statements(renames)); err != nil {
				send("error", map[string]any{"message": "refactorlog: " + err.Error()})
				return
			}
			if fromModel, _, err = introspect.IntrospectWithWarnings(d.ctx, conn, introspect.Options{IncludeSchemas: req.Schemas}); err != nil {
				send("error", map[string]any{"message": "introspect target: " + err.Error()})
				return
			}
		}
	}
	// The raw introspection stays the drift/baseline form; the comparison runs
	// on its canonical form, like every other source.
	canonFrom, canonWarns := source.Canonical(d.ctx, fromModel, d.sourceOptions(req))
	opts := req.engineOptions(dplan)
	res, err := engine.Compare(canonFrom, toModel, opts)
	if err != nil {
		send("error", map[string]any{"message": err.Error()})
		return
	}
	pre, post := dplan.Pre, dplan.Post
	if req.SkipDeployScripts {
		pre, post = nil, nil
	}
	send("start", map[string]any{"statementCount": len(res.Statements), "dataLoss": res.HasDataLoss(),
		"deployScripts": len(pre) + len(post),
		"warnings":      mergeWarnings(tagWarnings("source", warns), tagWarnings("target", canonWarns), tagWarnings("plan", res.Warnings))})
	if res.Empty() && len(pre)+len(post) == 0 {
		baselineRecorded := false
		if (req.RecordBaseline == nil || *req.RecordBaseline) && len(req.Schemas) == 0 && (drifted || !hadBaseline) {
			if _, err := drift.Record(d.ctx, conn, fromModel, req.To, "pg-dac "+d.version); err == nil {
				baselineRecorded = true
			}
		}
		send("done", map[string]any{"applied": 0, "baselineRecorded": baselineRecorded})
		return
	}
	total := len(res.Statements)
	announce := func(phase string) func(string) {
		return func(name string) {
			send("progress", map[string]any{"index": 0, "total": total, "sql": "-- " + phase + "-deploy script: " + name})
		}
	}
	// Missing roles are the one blocker a pre-deploy script may remove: run the
	// scripts and re-check before refusing (the schema is untouched so far).
	if res.HasBlocking() && !opts.AllowHazards && res.OnlyMissingRoles() && len(pre) > 0 {
		if err := deploy.Run(d.ctx, conn, pre, announce("pre")); err != nil {
			send("error", map[string]any{"applied": 0, "total": total, "message": err.Error()})
			return
		}
		pre = nil
		roles, err := introspect.ReadRoles(d.ctx, conn)
		if err != nil {
			send("error", map[string]any{"message": err.Error()})
			return
		}
		canonFrom.Roles = roles
		if res, err = engine.Compare(canonFrom, toModel, opts); err != nil {
			send("error", map[string]any{"message": err.Error()})
			return
		}
		total = len(res.Statements)
	}
	if res.HasBlocking() && !opts.AllowHazards {
		if missing := res.MissingRoles(); len(missing) > 0 {
			send("error", map[string]any{"message": fmt.Sprintf("refusing to publish: the source grants privileges to role(s) %v that do not exist in the target; create them (a pre-deploy script is the place) or remove the grants", missing)})
			return
		}
		send("error", map[string]any{"message": "refusing to publish: data-loss or unsupported changes present; set allowHazards to proceed"})
		return
	}
	if err := deploy.Run(d.ctx, conn, pre, announce("pre")); err != nil {
		send("error", map[string]any{"applied": 0, "total": total, "message": err.Error()})
		return
	}
	applied, err := execute.ApplyStream(d.ctx, conn, res.Statements, execute.DefaultOptions(),
		func(index, total int, s sqlgen.Statement) {
			send("progress", map[string]any{"index": index, "total": total, "sql": s.SQL})
		})
	if err != nil {
		send("error", map[string]any{"applied": applied, "total": total, "message": err.Error()})
		return
	}
	if err := deploy.Run(d.ctx, conn, post, announce("post")); err != nil {
		send("error", map[string]any{"applied": applied, "total": total, "message": err.Error()})
		return
	}
	baselineRecorded := false
	if (req.RecordBaseline == nil || *req.RecordBaseline) && len(req.Schemas) == 0 {
		if after, err := introspect.Introspect(d.ctx, conn, introspect.Options{}); err == nil {
			if _, err := drift.Record(d.ctx, conn, after, req.To, "pg-dac "+d.version); err == nil {
				baselineRecorded = true
			}
		}
	}
	send("done", map[string]any{"applied": applied, "deployScripts": len(pre) + len(post), "baselineRecorded": baselineRecorded})
}

// drift reports how a database differs from the baseline its last publish
// recorded: {drifted, currentSha256, baseline{...}, changes: PlanDoc}.
func (d *daemon) drift(w http.ResponseWriter, r *http.Request) {
	req, err := decode(r)
	if err != nil {
		badRequest(w, err)
		return
	}
	if !source.IsConnString(req.From) {
		badRequest(w, fmt.Errorf("drift target (from) must be a database connection string"))
		return
	}
	conn, err := pgx.Connect(d.ctx, req.From)
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
		out["changes"] = rep.Changes.PlanDoc()
	}
	writeJSON(w, http.StatusOK, out)
}

// --- helpers ---

func decode(r *http.Request) (compareReq, error) {
	var req compareReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, fmt.Errorf("invalid request body: %w", err)
	}
	return req, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func badRequest(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
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
