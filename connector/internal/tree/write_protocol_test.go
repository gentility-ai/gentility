package tree

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWriteProtocol(t *testing.T) {
	for _, revision := range []string{"", "42"} {
		t.Run("revision="+revision, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "PUT" || r.URL.Path != "/overlay/self/tree/acme/notes/test.md" {
					t.Errorf("request %s %s", r.Method, r.URL.Path)
				}
				expected := ""
				if revision != "" {
					expected = `"42"`
				}
				if r.Header.Get("If-Match") != expected {
					t.Errorf("If-Match=%q", r.Header.Get("If-Match"))
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != "hello" {
					t.Errorf("body=%q", body)
				}
				w.Header().Set("Location", "/overlay/self/tree/acme/notes/test.md")
				w.Header().Set("ETag", `"43"`)
				w.WriteHeader(204)
			}))
			defer server.Close()
			result, err := put(context.Background(), Source{BaseURL: server.URL}, pendingWrite{Path: "acme/notes/test.md", Revision: revision, Body: []byte("hello")})
			if err != nil || result.Receipt == nil || result.Receipt.Revision != "43" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestWriteProtocolRefusesPathsBeforeNetwork(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	defer server.Close()
	for _, name := range []string{"/acme/notes/x.md", "acme/../x.md", "acme/notes/%2e.md", "acme/notes/x.md?secret", "acme/notes/x.rejected.md", ".sync-status", "acme/notes/.DS_Store"} {
		if _, err := put(context.Background(), Source{BaseURL: server.URL}, pendingWrite{Path: name}); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if calls != 0 {
		t.Fatalf("sent %d unsafe requests", calls)
	}
}

func TestWriteReceiptConfinement(t *testing.T) {
	for _, location := range []string{"https://elsewhere.invalid/x", "/overlay/self/tree/other/notes/x.md", "/overlay/self/tree/acme/notes/../x.md", "/overlay/self/tree/acme/notes/%2f.md", "/overlay/self/tree/acme/notes/renamed.md"} {
		if _, err := parseReceipt("acme/notes/x.md", location, `"1"`); err == nil {
			t.Errorf("accepted %q", location)
		}
	}
	result, err := parseReceipt("acme/boards/general/draft.md", "/overlay/self/tree/acme/boards/general/canonical.md", `"post-uuid"`)
	if err != nil || result.Path != "acme/boards/general/canonical.md" {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestWriteErrorsAreSanitized(t *testing.T) {
	for _, status := range []int{401, 403, 404, 412, 413, 422, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":"secret submitted content"}`)
			}))
			defer server.Close()
			result, err := put(context.Background(), Source{BaseURL: server.URL}, pendingWrite{Path: "acme/notes/x.md"})
			if err != nil || result.Status != status || result.Code != fmt.Sprintf("http_%d", status) {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}

func TestPostRecoveryKeyAndScan(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Second)
	key, err := newPostKey(start)
	if err != nil || len(key) != 36 || key[14] != '7' {
		t.Fatalf("key=%q err=%v", key, err)
	}
	body, err := postWithKey([]byte("# Title\n\nrefs: {\"task_number\":152}\n\nBody"), key)
	if err != nil || !strings.Contains(string(body), `"task_number":152`) || !strings.HasSuffix(string(body), "\n\nBody") {
		t.Fatalf("body=%s err=%v", body, err)
	}
	var refs map[string]any
	header := strings.Split(string(body), "\n")[2]
	if err := json.Unmarshal([]byte(strings.TrimPrefix(header, "refs: ")), &refs); err != nil || refs["tree_write"] != key {
		t.Fatalf("refs=%v err=%v", refs, err)
	}
	rendered := []byte("# Title\n\nid: post-id\nposted_at: " + start.Add(time.Second).Format(time.RFC3339Nano) + "\n" + header + "\nretired_at: \n\nBody")
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gets++; w.Write(rendered) }))
	defer server.Close()
	manifest := Manifest{Files: []Entry{
		{Path: "acme/boards/other/20261101-0000-ignore.md", Kind: "post", SHA256: digest(rendered)},
		{Path: "acme/boards/general/" + start.Format("20060102-1504") + "-title.md", Kind: "post", SHA256: digest(rendered), Revision: "post-id"},
	}}
	pending := pendingWrite{Path: "acme/boards/general/draft.md", PostKey: key, Started: start}
	receipt, err := recoverPost(context.Background(), Source{BaseURL: server.URL}, manifest, pending)
	if err != nil || receipt == nil || receipt.Revision != "post-id" || gets != 1 {
		t.Fatalf("receipt=%+v gets=%d err=%v", receipt, gets, err)
	}
	pending.AmbiguousAt = start.Add(-25 * time.Hour)
	if _, err := recoverPost(context.Background(), Source{BaseURL: server.URL}, manifest, pending); err == nil {
		t.Fatal("expired scan accepted")
	}
	if gets != 1 {
		t.Fatal("expired scan fetched data")
	}
}

func TestPostRecoveryGraceAndFullWindow(t *testing.T) {
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	body := []byte("# Other\n\nid: other\nposted_at: " + start.Add(time.Minute).Format(time.RFC3339Nano) + "\nrefs: {}\n\nOther")
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gets++; w.Write(body) }))
	defer server.Close()
	source := Source{BaseURL: server.URL}
	pending := pendingWrite{Path: "acme/boards/general/draft.md", PostKey: "our-key", Started: start, SentAt: time.Now()}
	if _, err := recoverPost(context.Background(), source, Manifest{}, pending); err != errPostGrace {
		t.Fatalf("grace: %v", err)
	}
	if gets != 0 {
		t.Fatal("fetched during grace")
	}
	pending.SentAt = time.Now().Add(-postCommitGrace - time.Second)
	manifest := Manifest{}
	for i := 0; i < 500; i++ {
		manifest.Files = append(manifest.Files, Entry{Path: fmt.Sprintf("acme/boards/general/%s-post-%d.md", start.Add(time.Minute).Format("20060102-1504"), i), Kind: "post", SHA256: digest(body)})
	}
	if _, err := recoverPost(context.Background(), source, manifest, pending); err != errPostUnconfirmed {
		t.Fatalf("full window: %v", err)
	}
	manifest.Files = manifest.Files[:499]
	if _, err := recoverPost(context.Background(), source, manifest, pending); err != nil {
		t.Fatalf("complete history: %v", err)
	}
	manifest.Files = append(manifest.Files, Entry{Path: "acme/boards/general/" + start.Add(-time.Hour).Format("20060102-1504") + "-older.md", Kind: "post", SHA256: digest(body)})
	if _, err := recoverPost(context.Background(), source, manifest, pending); err != nil {
		t.Fatalf("window reaches before attempt: %v", err)
	}
}

func TestWritePoliciesFailClosed(t *testing.T) {
	for _, raw := range []string{`true`, `null`, `{"create":"yes"}`, `{"edit":null}`, `{"unknown":true}`} {
		policy, ok := decodeWritePolicy(json.RawMessage(raw))
		if ok || policy.Create || policy.Edit {
			t.Errorf("accepted %s", raw)
		}
	}
	manifest := Manifest{Directories: []Directory{{Path: "acme/notes", Write: json.RawMessage(`{"create":true,"edit":true}`)}}}
	if !manifest.policy("acme/notes/a/b/c.md").Create || manifest.policy("acme/notes/a/b/c/d/e.md").Create {
		t.Fatal("note inheritance depth")
	}
	if fileMode(Entry{Path: "acme/notes/x.md", Kind: "note"}, manifest, "person") != 0600 {
		t.Fatal("note not writable")
	}
	if fileMode(Entry{Path: "acme/notes/INDEX.md"}, manifest, "person") != 0400 {
		t.Fatal("index writable")
	}
	if fileMode(Entry{Path: "acme/notes/x.md", Kind: "post"}, manifest, "person") != 0400 {
		t.Fatal("post editable")
	}
	if fileMode(Entry{Path: "acme/notes/x.md", Kind: "note"}, manifest, "node") != 0440 {
		t.Fatal("daemon writable")
	}
}

func TestPostRecoveryToleratesClockSkew(t *testing.T) {
	start := time.Now().UTC()
	key, err := newPostKey(start)
	if err != nil {
		t.Fatal(err)
	}
	for _, skew := range []time.Duration{-2 * time.Minute, -30 * time.Second, 30 * time.Second, 2 * time.Minute} {
		posted := start.Add(skew)
		body := []byte("# Title\n\nposted_at: " + posted.Format(time.RFC3339Nano) + "\nrefs: {\"tree_write\":\"" + key + "\"}\n\nBody")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
		defer server.Close()
		entry := Entry{Path: "acme/boards/general/" + posted.Format("20060102-1504") + "-title.md", Kind: "post", SHA256: digest(body), Revision: "post-id"}
		pending := pendingWrite{Path: "acme/boards/general/draft.md", PostKey: key, Started: start}
		receipt, err := recoverPost(context.Background(), Source{BaseURL: server.URL}, Manifest{Files: []Entry{entry}}, pending)
		if err != nil || receipt == nil || receipt.Revision != "post-id" {
			t.Fatalf("receipt=%+v err=%v", receipt, err)
		}
	}

}

func TestPostAmbiguityCapStartsAtSend(t *testing.T) {
	pending := pendingWrite{Path: "acme/boards/general/draft.md", PostKey: "key", Started: time.Now().Add(-48 * time.Hour), SentAt: time.Now().Add(-3 * time.Minute), AmbiguousAt: time.Now().Add(-3 * time.Minute)}
	if _, err := recoverPost(context.Background(), Source{}, Manifest{}, pending); err != nil {
		t.Fatal(err)
	}
	pending.AmbiguousAt = time.Now().Add(-25 * time.Hour)
	if _, err := recoverPost(context.Background(), Source{}, Manifest{}, pending); err != errPostUnconfirmed {
		t.Fatalf("got %v", err)
	}
}
