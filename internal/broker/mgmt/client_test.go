package mgmt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	method, uri, body, auth string
}

type recorder struct {
	mu   sync.Mutex
	reqs []recorded
}

func (r *recorder) add(req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, recorded{req.Method, req.RequestURI, string(b), req.Header.Get("Authorization")})
}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.reqs...)
}

func newServer(t *testing.T, h http.HandlerFunc) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return New(base, "lockmq-ro", []byte("s3cr3t-pw"), nil, nil, time.Second), rec
}

func okJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
}

func TestEscaping(t *testing.T) {
	c, rec := newServer(t, okJSON(`{}`))
	var out map[string]any
	if err := c.Get(context.Background(), Path{Kind: "queue", Segs: []string{"/", "a/b"}}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), Path{Kind: "queue", Segs: []string{"v h", "50%#x"}}, nil, &out); err != nil {
		t.Fatal(err)
	}
	reqs := rec.all()
	if got := reqs[0].uri; got != "/api/queues/%2F/a%2Fb" {
		t.Errorf("uri = %q, want /api/queues/%%2F/a%%2Fb", got)
	}
	if got := reqs[1].uri; got != "/api/queues/v%20h/50%25%23x" {
		t.Errorf("uri = %q", got)
	}
}

func TestBasePathPrefix(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rec.add(r); io.WriteString(w, `{}`) }))
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/rabbitmq/")
	c := New(base, "u", []byte("p"), nil, nil, time.Second)
	var out any
	if err := c.Get(context.Background(), Path{Kind: "overview"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if got := rec.all()[0].uri; got != "/rabbitmq/api/overview" {
		t.Errorf("uri = %q", got)
	}
}

func TestUnknownKindRefusedWithoutIO(t *testing.T) {
	c, rec := newServer(t, okJSON(`{}`))
	var out any
	for _, p := range []Path{
		{Kind: "definitions"},
		{Kind: "users"},
		{Kind: "../definitions"},
		{Kind: "queue", Segs: []string{"/"}},            // missing name
		{Kind: "overview", Segs: []string{"extra"}},     // too many segments
		{Kind: "queues", Segs: []string{""}},            // empty vhost
		{Kind: "permissions", Segs: []string{"/", "x"}}, // not allowed
	} {
		if err := c.Get(context.Background(), p, nil, &out); err == nil {
			t.Errorf("%+v accepted", p)
		}
	}
	if n := len(rec.all()); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

func TestAllowedHasNoForbiddenEndpoint(t *testing.T) {
	for kind, tmpl := range Allowed {
		for _, bad := range []string{"definitions", "users", "permissions", "/get", "purge", "contents"} {
			if strings.Contains(tmpl, bad) {
				t.Errorf("%s → %s holds %q", kind, tmpl, bad)
			}
		}
	}
}

func TestOnlyGETAndOnePOST(t *testing.T) {
	c, rec := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			io.WriteString(w, `[{"payload":"hi","payload_encoding":"string","redelivered":true,"properties":{"headers":{"x-delivery-count":2}}}]`)
			return
		}
		io.WriteString(w, `{}`)
	})
	ctx := context.Background()
	var out any
	for kind := range Allowed {
		segs := []string{}
		if strings.Contains(Allowed[kind], "{v}") {
			segs = append(segs, "/")
		}
		if strings.Contains(Allowed[kind], "{n}") {
			segs = append(segs, "orders")
		}
		if err := c.Get(ctx, Path{Kind: kind, Segs: segs}, nil, &out); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	msgs, err := c.PeekRequeue(ctx, "/", "orders.failed", 3, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || !msgs[0].Redelivered || msgs[0].Payload != "hi" {
		t.Errorf("msgs = %+v", msgs)
	}
	posts := 0
	for _, r := range rec.all() {
		switch r.method {
		case http.MethodGet:
		case http.MethodPost:
			posts++
			if r.uri != "/api/queues/%2F/orders.failed/get" {
				t.Errorf("POST to %q", r.uri)
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(r.body), &body); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"count": 3.0, "ackmode": "ack_requeue_true", "encoding": "auto", "truncate": 2048.0}
			if len(body) != len(want) {
				t.Errorf("body = %v", body)
			}
			for k, v := range want {
				if body[k] != v {
					t.Errorf("body[%s] = %v, want %v", k, body[k], v)
				}
			}
		default:
			t.Errorf("method %s sent", r.method)
		}
	}
	if posts != 1 {
		t.Errorf("%d POSTs", posts)
	}
}

func TestPeekCountBounds(t *testing.T) {
	c, rec := newServer(t, okJSON(`[]`))
	for _, n := range []int{0, -1, 21} {
		if _, err := c.PeekRequeue(context.Background(), "/", "q", n, 100); err == nil {
			t.Errorf("count %d accepted", n)
		}
	}
	if _, err := c.PeekRequeue(context.Background(), "", "q", 1, 100); err == nil {
		t.Error("empty vhost accepted")
	}
	if len(rec.all()) != 0 {
		t.Error("requests sent")
	}
}

func TestPagination(t *testing.T) {
	c, rec := newServer(t, okJSON(`{"filtered_count":250,"item_count":2,"items":[{"name":"a"},{"name":"b"}],"page":2,"page_count":3,"page_size":100,"total_count":250}`))
	var items []map[string]any
	total, err := c.GetPage(context.Background(), Path{Kind: "queues", Segs: []string{"orders"}}, 2, 100, []string{"name", "messages"}, &items)
	if err != nil {
		t.Fatal(err)
	}
	if total != 250 || len(items) != 2 || items[1]["name"] != "b" {
		t.Errorf("total %d items %v", total, items)
	}
	u, _ := url.Parse(rec.all()[0].uri)
	q := u.Query()
	if u.Path != "/api/queues/orders" || q.Get("page") != "2" || q.Get("page_size") != "100" || q.Get("columns") != "name,messages" {
		t.Errorf("request = %s", rec.all()[0].uri)
	}
	if _, err := c.GetPage(context.Background(), Path{Kind: "queues", Segs: []string{"orders"}}, 0, 100, nil, &items); err == nil {
		t.Error("page 0 accepted")
	}
	if _, err := c.GetPage(context.Background(), Path{Kind: "queues", Segs: []string{"orders"}}, 1, 501, nil, &items); err == nil {
		t.Error("page size 501 accepted")
	}
}

func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	c := New(base, "u", []byte("p"), nil, nil, 100*time.Millisecond)
	start := time.Now()
	var out any
	err := c.Get(context.Background(), Path{Kind: "overview"}, nil, &out)
	if err == nil {
		t.Fatal("no error")
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("took %s", d)
	}
	if err.Error() != "mgmt: timeout" {
		t.Errorf("error = %q", err)
	}
}

func TestErrorHasNoSecret(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"not_authorised","reason":"bad login lockmq-ro s3cr3t-pw `+auth+`"}`)
	})
	var out any
	err := c.Get(context.Background(), Path{Kind: "whoami"}, nil, &out)
	if err == nil {
		t.Fatal("no error")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 401 {
		t.Errorf("error = %#v", err)
	}
	for _, leak := range []string{"lockmq-ro", "s3cr3t", "Basic", "not_authorised"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q holds %q", err, leak)
		}
	}
	if err.Error() != "mgmt: HTTP 401" {
		t.Errorf("error = %q", err)
	}
}

func TestDialErrorHasNoSecret(t *testing.T) {
	base, _ := url.Parse("http://127.0.0.1:1")
	c := New(base, "lockmq-ro", []byte("s3cr3t-pw"), nil, nil, time.Second)
	var out any
	err := c.Get(context.Background(), Path{Kind: "overview"}, nil, &out)
	if err == nil {
		t.Fatal("no error")
	}
	for _, leak := range []string{"lockmq-ro", "s3cr3t", "127.0.0.1"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q holds %q", err, leak)
		}
	}
}

func TestBasicAuthSent(t *testing.T) {
	c, rec := newServer(t, okJSON(`{}`))
	var out any
	if err := c.Get(context.Background(), Path{Kind: "whoami"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if got := rec.all()[0].auth; got != "Basic bG9ja21xLXJvOnMzY3IzdC1wdw==" {
		t.Errorf("auth = %q", got)
	}
}

func TestNoRedirectFollowed(t *testing.T) {
	c, rec := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/overview" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		io.WriteString(w, `{}`)
	})
	var out any
	if err := c.Get(context.Background(), Path{Kind: "overview"}, nil, &out); err == nil {
		t.Error("redirect followed without error")
	}
	if n := len(rec.all()); n != 1 {
		t.Errorf("%d requests", n)
	}
}

func TestResponseTooLarge(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `["`+strings.Repeat("x", MaxResponseBytes)+`"]`)
	})
	var out any
	if err := c.Get(context.Background(), Path{Kind: "overview"}, nil, &out); err == nil || err.Error() != "mgmt: response too large" {
		t.Errorf("error = %v", err)
	}
}
