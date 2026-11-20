package tree

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/version"
)

const maxWrite = 256 << 10

// postCommitGrace allows an interrupted server request to settle before a fresh
// manifest is used to decide whether to retry. It is not a uniqueness guarantee.
const postCommitGrace = 2 * time.Minute
const postClockSkew = 5 * time.Minute

var errPostGrace = errors.New("tree: waiting for post commit grace period")
var errPostUnconfirmed = errors.New("tree: could not confirm whether this post was created")

// pendingWrite is persisted before sending, including the exact wire bytes.
// Sending is deliberately durable: a restart must assume the server committed.
type pendingWrite struct {
	AmbiguousAt time.Time     `json:"ambiguous_at,omitempty"`
	SentAt      time.Time     `json:"sent_at,omitempty"`
	Path        string        `json:"path"`
	Revision    string        `json:"revision,omitempty"`
	Body        []byte        `json:"body"`
	LocalHash   string        `json:"local_hash"`
	PostKey     string        `json:"post_key,omitempty"`
	Started     time.Time     `json:"started"`
	Sending     bool          `json:"sending"`
	Receipt     *writeReceipt `json:"receipt,omitempty"`
	Rejection   string        `json:"rejection,omitempty"`
}

type writeReceipt struct {
	Path     string `json:"path"`
	Revision string `json:"revision"`
}

type writeResponse struct {
	Status  int
	Code    string
	Receipt *writeReceipt
}

func writePath(name string) error {
	if err := safePath(name, false); err != nil {
		return err
	}
	if strings.ContainsAny(name, "%?#") || ignoredLocal(name) || pluginManifest(name) {
		return refuse("tree: reserved or encoded write path")
	}
	return nil
}

func ignoredLocal(name string) bool {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		// Layouts 3 and 4 render each plugin's manifest at skills/…/
		// .claude-plugin/plugin.json: the server's file, not an editor's, in a
		// directory no manifest lets a person write. Nothing else in that
		// directory is.
		if part == ".claude-plugin" && parts[0] == "skills" && (i == len(parts)-1 || i == len(parts)-2 && parts[i+1] == "plugin.json") {
			continue
		}
		if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "#") || strings.HasSuffix(part, "~") ||
			strings.HasSuffix(part, ".tmp") || strings.HasSuffix(part, ".swp") || strings.HasSuffix(part, ".swo") ||
			strings.HasSuffix(part, ".rejected.md") || strings.HasSuffix(part, ".conflict.md") {
			return true
		}
	}
	return false
}

func put(ctx context.Context, source Source, pending pendingWrite) (writeResponse, error) {
	if err := writePath(pending.Path); err != nil {
		return writeResponse{}, err
	}
	if len(pending.Body) > maxWrite {
		return writeResponse{Status: 413, Code: "body_too_large"}, nil
	}
	if strings.ContainsAny(pending.Revision, "\"\r\n") {
		return writeResponse{}, refuse("tree: invalid saved revision")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		strings.TrimRight(source.BaseURL, "/")+"/overlay/self/tree/"+pending.Path, bytes.NewReader(pending.Body))
	if err != nil {
		return writeResponse{}, errors.New("tree: invalid gateway URL")
	}
	// Do not reuse a connection after an ambiguous write. Cancellation closes
	// the active request; Close prevents it rejoining the transport pool.
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+source.Bearer)
	req.Header.Set("X-Node-Capver", strconv.Itoa(version.NodeCapver))
	req.Header.Set("Content-Type", "text/markdown; charset=utf-8")
	if pending.Revision != "" {
		req.Header.Set("If-Match", strconv.Quote(pending.Revision))
	}
	client := http.Client{}
	if source.HTTP != nil {
		client = *source.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return writeResponse{}, &transportError{"tree: write request failed"}
	}
	defer response.Body.Close()
	result := writeResponse{Status: response.StatusCode}
	if response.StatusCode == http.StatusNoContent {
		receipt, err := parseReceipt(pending.Path, response.Header.Get("Location"), response.Header.Get("ETag"))
		if err != nil {
			return result, err
		}
		result.Receipt = &receipt
		return result, nil
	}
	// Only stable known codes may leave this parser. A proxy error can contain
	// credentials or submitted text; never surface its body as a diagnostic.
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil {
		return result, &transportError{"tree: write response read failed"}
	}
	var payload struct {
		Error string `json:"error"`
	}
	if len(body) <= 4096 && json.Unmarshal(body, &payload) == nil && knownWriteCode(payload.Error) {
		result.Code = payload.Error
	} else {
		result.Code = "http_" + strconv.Itoa(result.Status)
	}
	return result, nil
}

func knownWriteCode(code string) bool {
	switch code {
	case "frontmatter_readonly", "readonly_target", "unexpected_revision", "invalid_revision", "invalid_refs", "stale", "exists", "not_found", "unknown_project", "unknown_board", "forbidden", "unauthorized", "board_closed", "body_too_large", "invalid_frontmatter", "invalid_path", "invalid_utf8":
		return true
	}
	return false
}

func parseReceipt(original, location, etag string) (writeReceipt, error) {
	const prefix = "/overlay/self/tree/"
	if !strings.HasPrefix(location, prefix) {
		return writeReceipt{}, errors.New("tree: invalid write Location")
	}
	name := strings.TrimPrefix(location, prefix)
	if err := writePath(name); err != nil {
		return writeReceipt{}, err
	}
	// Writes never cross scope or directory; only a newly-created post changes
	// its basename. Location is a local receipt, never a redirect to follow.
	if parentPath(name) != parentPath(original) || (name != original && !postPath(original)) {
		return writeReceipt{}, refuse("tree: write Location changed scope")
	}
	if len(etag) < 3 || etag[0] != '"' || etag[len(etag)-1] != '"' || strings.ContainsAny(etag[1:len(etag)-1], "\"\r\n") {
		return writeReceipt{}, errors.New("tree: invalid write ETag")
	}
	return writeReceipt{Path: name, Revision: etag[1 : len(etag)-1]}, nil
}

func postPath(name string) bool {
	parts := strings.Split(name, "/")
	return len(parts) >= 4 && parts[len(parts)-3] == "boards"
}

func newPostKey(now time.Time) (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	millis := uint64(now.UnixMilli())
	for i := 5; i >= 0; i-- {
		id[i] = byte(millis)
		millis >>= 8
	}
	id[6] = (id[6] & 15) | 0x70
	id[8] = (id[8] & 63) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), nil
}

// postWithKey mirrors the server's optional header block. It does not hide
// forbidden server-owned headers by converting them into ordinary body text.
func postWithKey(body []byte, key string) ([]byte, error) {
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	text = strings.TrimLeft(strings.TrimPrefix(text, "\ufeff"), "\n")
	if !strings.HasPrefix(text, "# ") {
		return nil, errors.New("tree: post needs a title heading")
	}
	heading, rest, _ := strings.Cut(text, "\n")
	refs := map[string]json.RawMessage{}
	if strings.HasPrefix(rest, "\n") {
		rest = strings.TrimPrefix(rest, "\n")
		block, tail, _ := strings.Cut(rest, "\n\n")
		if strings.HasPrefix(block, "refs: ") {
			if strings.Contains(block, "\n") || json.Unmarshal([]byte(strings.TrimPrefix(block, "refs: ")), &refs) != nil || refs == nil {
				return nil, errors.New("tree: invalid post refs")
			}
			rest = tail
		} else {
			for _, field := range []string{"id: ", "posted_by: ", "posted_at: ", "retired_at: ", "retired_reason: "} {
				if strings.HasPrefix(block, field) {
					return nil, errors.New("tree: post header is read-only")
				}
			}
		}
	}
	if _, exists := refs["tree_write"]; exists {
		return nil, errors.New("tree: tree_write is reserved for create recovery")
	}
	value, _ := json.Marshal(key)
	refs["tree_write"] = value
	encoded, err := json.Marshal(refs)
	if err != nil {
		return nil, err
	}
	return []byte(heading + "\n\nrefs: " + string(encoded) + "\n\n" + rest), nil
}

// recoverPost searches only a bounded snapshot of this board. Absence is not
// proof of non-commit: the server does not enforce uniqueness on tree_write.
func recoverPost(ctx context.Context, source Source, manifest Manifest, pending pendingWrite) (*writeReceipt, error) {
	ambiguousAt := pending.AmbiguousAt
	if ambiguousAt.IsZero() {
		ambiguousAt = pending.SentAt
	}
	if !ambiguousAt.IsZero() && time.Since(ambiguousAt) > 24*time.Hour {
		return nil, errPostUnconfirmed
	}
	if time.Since(pending.SentAt) < postCommitGrace {
		return nil, errPostGrace
	}
	start := pending.Started.Add(-postClockSkew)
	count := 0
	reachesStart := false
	for _, entry := range manifest.Files {
		if entry.Kind != "post" || parentPath(entry.Path) != parentPath(pending.Path) {
			continue
		}
		count++
		if count > 500 {
			return nil, errors.New("tree: post recovery scan exceeds board bound")
		}
		// Canonical names carry a minute-resolution timestamp; include that minute
		// and inspect posted_at before matching the key.
		if len(path.Base(entry.Path)) >= 13 {
			stamp, err := time.Parse("20060102-1504", path.Base(entry.Path)[:13])
			if err == nil && stamp.Add(time.Minute).Before(start) {
				reachesStart = true
				continue
			}
		}
		body, _, _, err := get(ctx, source, layoutRoute("blob/"+entry.SHA256, manifest.layout()), "", maxBlob)
		if err != nil {
			return nil, err
		}
		if digest(body) != entry.SHA256 {
			return nil, errors.New("tree: recovery blob SHA-256 mismatch")
		}
		_, headers, ok := strings.Cut(string(body), "\n\n")
		if !ok {
			return nil, errors.New("tree: invalid rendered post headers")
		}
		headers, _, _ = strings.Cut(headers, "\n\n")
		var posted time.Time
		var refs map[string]json.RawMessage
		for _, line := range strings.Split(headers, "\n") {
			if strings.HasPrefix(line, "posted_at: ") {
				posted, _ = time.Parse(time.RFC3339Nano, strings.TrimPrefix(line, "posted_at: "))
			}
			if strings.HasPrefix(line, "refs: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "refs: ")), &refs); err != nil {
					return nil, errors.New("tree: invalid rendered post refs")
				}
			}
		}
		if posted.IsZero() {
			return nil, errors.New("tree: invalid rendered post timestamp")
		}
		if posted.Before(start) {
			reachesStart = true
		}
		var key string
		if value, ok := refs["tree_write"]; ok {
			_ = json.Unmarshal(value, &key)
		}
		if key == pending.PostKey {
			return &writeReceipt{Path: entry.Path, Revision: entry.Revision}, nil
		}
	}
	if count == 500 && !reachesStart {
		return nil, errPostUnconfirmed
	}
	return nil, nil
}
