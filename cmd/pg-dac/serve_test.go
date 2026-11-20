package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/deploy"
	"github.com/rmnunes/pg-dac/internal/model"
)

// TestBindAllowed: the daemon runs DDL wherever it is pointed, so only a
// loopback bind may go without a bearer token.
func TestBindAllowed(t *testing.T) {
	cases := []struct {
		addr, token string
		ok          bool
	}{
		{"127.0.0.1:0", "", true},
		{"localhost:9000", "", true},
		{"[::1]:9000", "", true},
		{"0.0.0.0:9000", "", false},
		{"10.0.0.5:9000", "", false},
		{"0.0.0.0:9000", "secret", true},
		{"nonsense", "", false},
	}
	for _, c := range cases {
		if err := bindAllowed(c.addr, c.token); (err == nil) != c.ok {
			t.Errorf("bindAllowed(%q, token=%v) = %v, want ok=%v", c.addr, c.token != "", err, c.ok)
		}
	}
}

// TestHandle_AuthAndMethod: every endpoint checks the token first and the
// method second, and answers with the one JSON error shape.
func TestHandle_AuthAndMethod(t *testing.T) {
	d := &daemon{token: "secret"}
	h := d.handle(http.MethodPost, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	call := func(method, auth string) (int, string) {
		req := httptest.NewRequest(method, "/x", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := call(http.MethodPost, ""); code != http.StatusUnauthorized || !strings.Contains(body, `"error":"unauthorized"`) {
		t.Errorf("no token: %d %s", code, body)
	}
	if code, body := call(http.MethodPost, "Bearer wrong"); code != http.StatusUnauthorized || !strings.Contains(body, `"error"`) {
		t.Errorf("wrong token: %d %s", code, body)
	}
	if code, body := call(http.MethodGet, "Bearer secret"); code != http.StatusMethodNotAllowed || !strings.Contains(body, "POST") {
		t.Errorf("wrong method: %d %s", code, body)
	}
	if code, _ := call(http.MethodPost, "Bearer secret"); code != http.StatusNoContent {
		t.Errorf("authorized POST: %d", code)
	}
}

// TestDecode_BoundsTheBody: a request body beyond the limit is rejected
// instead of being read into memory.
func TestDecode_BoundsTheBody(t *testing.T) {
	huge := `{"from":"` + strings.Repeat("x", maxRequestBody) + `"}`
	rec := httptest.NewRecorder()
	if _, err := decode(rec, httptest.NewRequest(http.MethodPost, "/plan", strings.NewReader(huge))); err == nil {
		t.Error("an oversized body must be refused")
	}
	rec = httptest.NewRecorder()
	req, err := decode(rec, httptest.NewRequest(http.MethodPost, "/plan", strings.NewReader(`{"from":"a","to":"b","excludeIds":["x"]}`)))
	if err != nil || req.From != "a" || req.To != "b" || len(req.ExcludeIDs) != 1 {
		t.Errorf("ordinary body: %+v err=%v", req, err)
	}
}

// TestCancel_RejectsAMalformedBody: /cancel reports a bad body like every
// other endpoint instead of answering {"canceled": false}.
func TestCancel_RejectsAMalformedBody(t *testing.T) {
	d := &daemon{}
	rec := httptest.NewRecorder()
	d.cancelOp(rec, httptest.NewRequest(http.MethodPost, "/cancel", strings.NewReader("{")))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("malformed body: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	d.cancelOp(rec, httptest.NewRequest(http.MethodPost, "/cancel", strings.NewReader(`{"opId":"none"}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"canceled":false`) {
		t.Errorf("unknown op: %d %s", rec.Code, rec.Body.String())
	}
}

func table(schema, name string) *model.Table {
	return &model.Table{Schema: schema, Name: name, Columns: []*model.Column{{Name: "a", Type: "integer"}}}
}

type replanBody struct {
	Plan struct {
		StatementCount int `json:"statementCount"`
		Changes        []struct {
			ID string `json:"id"`
		} `json:"changes"`
	} `json:"plan"`
	Warnings []sideWarning `json:"warnings"`
	Error    string        `json:"error"`
}

func post(t *testing.T, h http.HandlerFunc, req compareReq) (int, replanBody) {
	t.Helper()
	b, _ := json.Marshal(req)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/replan", bytes.NewReader(b)))
	var body replanBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %s", rec.Body.String())
	}
	return rec.Code, body
}

// TestReplan: a re-plan runs the engine on the sides the last /plan
// remembered, with the request's selection, and carries that read's warnings;
// without a matching comparison — none yet, other sources, other schemas or
// target version — it answers 409 so the GUI compares again.
func TestReplan(t *testing.T) {
	from := &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Tables: []*model.Table{table("app", "t")}}}}
	to := &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Tables: []*model.Table{table("app", "added"), table("app", "t")}}}}
	d := &daemon{log: io.Discard}
	req := compareReq{From: "postgres://target.example.internal/db", To: "/desired", Schemas: []string{"app"}}

	if code, body := post(t, d.replan, req); code != http.StatusConflict || body.Error == "" {
		t.Fatalf("before any compare: %d %+v", code, body)
	}

	d.remember(req, &compared{from: from, to: to, dplan: &deploy.Plan{}, warnings: []sideWarning{{Side: "target", Message: "unmanaged thing"}}})

	code, body := post(t, d.replan, req)
	if code != http.StatusOK || body.Plan.StatementCount != 1 || len(body.Plan.Changes) != 1 {
		t.Fatalf("full plan: %d %+v", code, body)
	}
	if len(body.Warnings) != 1 || body.Warnings[0].Message != "unmanaged thing" {
		t.Errorf("the read's warnings must come back with every re-plan: %+v", body.Warnings)
	}

	excluded := req
	excluded.ExcludeIDs = []string{model.ObjRef{Kind: model.KindTable, Schema: "app", Name: "added"}.Key()}
	if code, body := post(t, d.replan, excluded); code != http.StatusOK || body.Plan.StatementCount != 0 {
		t.Errorf("excluding the only change: %d %+v", code, body)
	}

	for name, other := range map[string]func(*compareReq){
		"other target":  func(r *compareReq) { r.From = "postgres://elsewhere/db" },
		"other source":  func(r *compareReq) { r.To = "/other" },
		"other schemas": func(r *compareReq) { r.Schemas = nil },
		"other major":   func(r *compareReq) { r.TargetMajor = 15 },
		"other key":     func(r *compareReq) { r.RequireKey = "abc" },
	} {
		r := req
		other(&r)
		if code, _ := post(t, d.replan, r); code != http.StatusConflict {
			t.Errorf("%s: want 409, got %d", name, code)
		}
	}
}
