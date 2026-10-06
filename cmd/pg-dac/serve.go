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

	"github.com/rodrigonunes/pg-dac/internal/devdb"
	"github.com/rodrigonunes/pg-dac/internal/engine"
	"github.com/rodrigonunes/pg-dac/internal/execute"
	"github.com/rodrigonunes/pg-dac/internal/introspect"
	"github.com/rodrigonunes/pg-dac/internal/model"
	"github.com/rodrigonunes/pg-dac/internal/source"
	"github.com/rodrigonunes/pg-dac/internal/sqlgen"
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

	d := &daemon{ctx: ctx, token: os.Getenv("PGDAC_TOKEN"), version: appVersion()}
	var once sync.Once
	stop := make(chan struct{})
	shutdown := func() { once.Do(func() { close(stop) }) }

	mux := http.NewServeMux()
	mux.HandleFunc("/health", d.auth(d.health))
	mux.HandleFunc("/plan", d.auth(d.plan))
	mux.HandleFunc("/cancel", d.auth(d.cancelOp))
	mux.HandleFunc("/script", d.auth(d.script))
	mux.HandleFunc("/publish", d.auth(d.publish))
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
	ctx     context.Context
	token   string
	version string

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
}

func (r compareReq) engineOptions() engine.Options {
	opts := engine.DefaultOptions()
	opts.TargetMajor = r.TargetMajor
	if r.Online != nil {
		opts.Online = *r.Online
	}
	opts.AllowHazards = r.AllowHazards
	opts.DropObjectsNotInSource = r.DropObjectsNotInSource
	opts.IncludeIDs = toSet(r.IncludeIDs)
	opts.ExcludeIDs = toSet(r.ExcludeIDs)
	return opts
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

	send("phase", map[string]any{"step": "read-from", "message": "Reading Target…", "source": req.From})
	from, fw, err := source.Resolve(ctx, req.From, source.Options{Schemas: req.Schemas})
	if err != nil {
		if canceled() {
			return
		}
		send("error", map[string]any{"message": "from: " + err.Error()})
		return
	}

	send("phase", map[string]any{"step": "read-to", "message": "Reading Source…", "source": req.To, "fromCount": objectCount(from)})
	to, tw, err := source.Resolve(ctx, req.To, source.Options{Schemas: req.Schemas})
	if err != nil {
		if canceled() {
			return
		}
		send("error", map[string]any{"message": "to: " + err.Error()})
		return
	}

	send("phase", map[string]any{"step": "diff", "message": "Diffing and generating plan…", "toCount": objectCount(to)})
	res, err := engine.Compare(from, to, req.engineOptions())
	if err != nil {
		if canceled() {
			return
		}
		send("error", map[string]any{"message": err.Error()})
		return
	}
	send("result", map[string]any{"plan": res.PlanDoc(), "warnings": append(fw, tw...)})
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
	from, fw, err := source.Resolve(d.ctx, req.From, source.Options{Schemas: req.Schemas})
	if err != nil {
		badRequest(w, fmt.Errorf("from: %w", err))
		return
	}
	to, tw, err := source.Resolve(d.ctx, req.To, source.Options{Schemas: req.Schemas})
	if err != nil {
		badRequest(w, fmt.Errorf("to: %w", err))
		return
	}
	res, err := engine.Compare(from, to, req.engineOptions())
	if err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sql": res.Script(req.AllowHazards), "warnings": append(fw, tw...)})
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

	fromModel, _, err := introspect.IntrospectWithWarnings(d.ctx, conn, introspect.Options{IncludeSchemas: req.Schemas})
	if err != nil {
		send("error", map[string]any{"message": "introspect target: " + err.Error()})
		return
	}
	toModel, warns, err := source.Resolve(d.ctx, req.To, source.Options{Schemas: req.Schemas})
	if err != nil {
		send("error", map[string]any{"message": "resolve source: " + err.Error()})
		return
	}
	opts := req.engineOptions()
	res, err := engine.Compare(fromModel, toModel, opts)
	if err != nil {
		send("error", map[string]any{"message": err.Error()})
		return
	}
	send("start", map[string]any{"statementCount": len(res.Statements), "dataLoss": res.HasDataLoss(), "warnings": warns})
	if res.Empty() {
		send("done", map[string]any{"applied": 0})
		return
	}
	if res.HasDataLoss() && !opts.AllowHazards {
		send("error", map[string]any{"message": "refusing to publish: possible data loss; set allowHazards to proceed"})
		return
	}

	applied, err := execute.ApplyStream(d.ctx, conn, res.Statements, execute.DefaultOptions(),
		func(index, total int, s sqlgen.Statement) {
			send("progress", map[string]any{"index": index, "total": total, "sql": s.SQL})
		})
	if err != nil {
		send("error", map[string]any{"applied": applied, "total": len(res.Statements), "message": err.Error()})
		return
	}
	send("done", map[string]any{"applied": applied})
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
