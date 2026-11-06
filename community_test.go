package opensdk

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAsset = "8a1f2c3d-4e5f-4a6b-9c7d-0e1f2a3b4c5d"

// seenRequest is one request the mock got, with its body.
type seenRequest struct {
	method, uri, auth string
	header            http.Header
	body              []byte
}

type recorder struct {
	mu   sync.Mutex
	reqs []seenRequest
}

func (rec *recorder) add(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	rec.mu.Lock()
	rec.reqs = append(rec.reqs, seenRequest{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Clone(), b})
	rec.mu.Unlock()
	return b
}

func (rec *recorder) lines() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]string, len(rec.reqs))
	for i, r := range rec.reqs {
		out[i] = r.method + " " + r.uri
	}
	return out
}

func equalLines(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("requests %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("requests %q, want %q", got, want)
		}
	}
}

func TestUploadImagePutsToTheSlot(t *testing.T) {
	rec := &recorder{}
	var m *mock
	m = newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		b := rec.add(r)
		if r.URL.Path == APIPrefix+"/community/images" {
			var body map[string]any
			_ = json.Unmarshal(b, &body)
			if body["content_type"] != "image/png" || body["size_bytes"] != float64(3) {
				t.Errorf("slot body %v", body)
			}
			writeJSON(w, 201, map[string]any{
				"key": "img/k1", "upload_url": m.srv.URL + "/upload/img-1?sig=x", "method": "PUT",
				"headers": map[string]string{"Content-Type": "image/png", "X-Amz-Meta-A": "1"}, "expires_at": "",
			})
			return
		}
		w.WriteHeader(200)
	})
	key, err := m.client().UploadImage(context.Background(), "cn.u-1", []byte{1, 2, 3}, "image/png")
	if err != nil || key != "img/k1" {
		t.Fatalf("key %q, err %v", key, err)
	}
	equalLines(t, rec.lines(), []string{"POST /open-apis/v1/community/images", "PUT /upload/img-1?sig=x"})
	put := rec.reqs[1]
	if rec.reqs[0].auth != "Bearer cn.u-1" || put.auth != "" {
		t.Fatalf("auth %q / %q", rec.reqs[0].auth, put.auth)
	}
	if !bytes.Equal(put.body, []byte{1, 2, 3}) || put.header.Get("X-Amz-Meta-A") != "1" || put.header.Get("Content-Type") != "image/png" {
		t.Fatalf("put %v %v", put.body, put.header)
	}
}

func TestUploadRefusedIsAnError(t *testing.T) {
	var m *mock
	m = newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		if r.Method == http.MethodPost {
			writeJSON(w, 201, map[string]any{"key": "k", "upload_url": m.srv.URL + "/upload/x", "method": "PUT", "headers": map[string]string{}})
			return
		}
		w.WriteHeader(403)
	})
	_, err := m.client().UploadImage(context.Background(), "u", []byte{1}, "image/jpeg")
	if e, ok := AsError(err); !ok || e.Status != 403 || e.Code != CodeUploadFailed {
		t.Fatalf("err %v", err)
	}
}

func TestUploadVideoCreatesPutsAndCompletes(t *testing.T) {
	rec := &recorder{}
	var m *mock
	m = newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		b := rec.add(r)
		switch r.URL.Path {
		case APIPrefix + "/community/videos":
			var body map[string]any
			_ = json.Unmarshal(b, &body)
			if body["filename"] != "a.mp4" || body["content_type"] != "video/mp4" || body["size_bytes"] != float64(4) {
				t.Errorf("slot body %v", body)
			}
			writeJSON(w, 201, map[string]any{"asset_id": testAsset, "status": "uploading", "upload": map[string]any{
				"url": m.srv.URL + "/upload/v-1", "method": "PUT", "headers": map[string]string{"X-H": "v"}, "expires_at": ""}})
		case APIPrefix + "/community/videos/" + testAsset + "/complete":
			writeJSON(w, 200, map[string]any{"asset_id": testAsset, "status": "queued"})
		default:
			if r.Header.Get("X-H") != "v" || string(b) != "\x00\x01\x02\x03" {
				t.Errorf("put %q %v", b, r.Header)
			}
			w.WriteHeader(200)
		}
	})
	v, err := m.client().UploadVideo(context.Background(), "u", bytes.NewReader([]byte{0, 1, 2, 3}), 4, "a.mp4", "video/mp4")
	if err != nil || v["status"] != "queued" || v["asset_id"] != testAsset {
		t.Fatalf("video %v, err %v", v, err)
	}
	equalLines(t, rec.lines(), []string{
		"POST /open-apis/v1/community/videos", "PUT /upload/v-1",
		"POST /open-apis/v1/community/videos/" + testAsset + "/complete",
	})
	if rec.reqs[2].auth != "Bearer u" {
		t.Fatalf("complete auth %q", rec.reqs[2].auth)
	}
}

func TestWaitVideoReady(t *testing.T) {
	states := []string{"queued", "processing", "ready"}
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		if r.URL.Path != APIPrefix+"/community/videos/"+testAsset {
			t.Errorf("path %s", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"asset_id": testAsset, "status": states[n-1], "duration_s": 12})
	})
	v, err := m.client().WaitVideoReady(context.Background(), "u", testAsset, WithPollInterval(time.Millisecond, 2*time.Millisecond))
	if err != nil || v["status"] != "ready" || m.apiCalls.Load() != 3 {
		t.Fatalf("video %v, err %v, calls %d", v, err, m.apiCalls.Load())
	}
}

func TestWaitVideoFailedAndTimeout(t *testing.T) {
	status := "failed"
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		writeJSON(w, 200, map[string]any{"asset_id": testAsset, "status": status, "error": "unsupported_codec"})
	})
	c := m.client()
	_, err := c.WaitVideoReady(context.Background(), "u", testAsset, WithPollInterval(time.Millisecond, time.Millisecond))
	if e, ok := AsError(err); !ok || e.Reason != ReasonVideoFailed || e.Details["error"] != "unsupported_codec" {
		t.Fatalf("err %v", err)
	}
	status = "processing"
	_, err = c.WaitVideoReady(context.Background(), "u", testAsset,
		WithWaitTimeout(30*time.Millisecond), WithPollInterval(5*time.Millisecond, 5*time.Millisecond))
	if e, ok := AsError(err); !ok || e.Code != CodeTimeout || e.Reason != ReasonVideoNotReady {
		t.Fatalf("err %v", err)
	}
}

func TestCreatePostAddsAUUIDAndReusesItOnRetries(t *testing.T) {
	rec := &recorder{}
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		rec.add(r)
		if n == 1 {
			writeErr(w, 503, "SERVICE_UNAVAILABLE", "", "")
			return
		}
		writeJSON(w, 201, map[string]any{"post_id": "cn:post:1"})
	})
	post := Params{"body": "你好 #学中文"}
	p, err := m.client().CreatePost(context.Background(), "u", post)
	if err != nil || p["post_id"] != "cn:post:1" {
		t.Fatalf("post %v, err %v", p, err)
	}
	if len(rec.reqs) != 2 {
		t.Fatalf("%d calls", len(rec.reqs))
	}
	var first, second map[string]any
	_ = json.Unmarshal(rec.reqs[0].body, &first)
	_ = json.Unmarshal(rec.reqs[1].body, &second)
	id, _ := first["uuid"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("uuid %q", id)
	}
	if second["uuid"] != id || first["body"] != "你好 #学中文" {
		t.Fatalf("bodies %v / %v", first, second)
	}
	if _, ok := post["uuid"]; ok {
		t.Fatal("the caller's map was changed")
	}

	type myPost struct {
		UUID  string `json:"uuid"`
		Title string `json:"title"`
	}
	rec.reqs = nil
	if _, err := m.client().CreatePost(context.Background(), "u", myPost{UUID: "mine-1", Title: "T"}); err != nil {
		t.Fatal(err)
	}
	if got := string(rec.reqs[0].body); got != `{"title":"T","uuid":"mine-1"}` {
		t.Fatalf("body %s", got)
	}
}

func TestPostingAsTheInstitution(t *testing.T) {
	rec := &recorder{}
	var m *mock
	m = newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		rec.add(r)
		switch r.URL.Path {
		case APIPrefix + "/community/images":
			writeJSON(w, 201, map[string]any{
				"key": "img/org", "upload_url": m.srv.URL + "/upload/org-1", "method": "PUT",
				"headers": map[string]string{}, "expires_at": "",
			})
		case APIPrefix + "/community/posts":
			writeJSON(w, 201, map[string]any{"post_id": "cn:post:2"})
		default:
			w.WriteHeader(200)
		}
	})
	c := m.client()
	key, err := c.UploadImageAsOrg(context.Background(), "cn.org-1", []byte{9}, "image/png")
	if err != nil || key != "img/org" {
		t.Fatalf("key %q, err %v", key, err)
	}
	if _, err := c.CreatePostAsOrg(context.Background(), "cn.org-1", Params{"body": "招生啦"}); err != nil {
		t.Fatal(err)
	}
	equalLines(t, rec.lines(), []string{
		"POST /open-apis/v1/community/images", "PUT /upload/org-1", "POST /open-apis/v1/community/posts",
	})
	if !strings.HasPrefix(rec.reqs[0].auth, "Bearer t-cn.org-1-") || rec.reqs[1].auth != "" || rec.reqs[2].auth != rec.reqs[0].auth {
		t.Fatalf("auth %q / %q / %q", rec.reqs[0].auth, rec.reqs[1].auth, rec.reqs[2].auth)
	}
	var sent map[string]any
	_ = json.Unmarshal(rec.reqs[2].body, &sent)
	if id, _ := sent["uuid"].(string); len(id) != 36 || sent["body"] != "招生啦" {
		t.Fatalf("body %v", sent)
	}
}

func TestGetAllInsights(t *testing.T) {
	rec := &recorder{}
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		rec.add(r)
		tab := r.URL.Query().Get("tab")
		if tab == "" {
			tab = "enrollment"
		}
		writeJSON(w, 200, map[string]any{
			"tab": tab, "tabs": []string{"enrollment", "teaching", "income"}, "range": r.URL.Query().Get("range"),
		})
	})
	pages, err := m.client().GetAllInsights(context.Background(), "cn.org-1", Params{"range": "7d", "tab": "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 3 || pages["income"]["range"] != "7d" || pages["teaching"]["tab"] != "teaching" {
		t.Fatalf("pages %v", pages)
	}
	equalLines(t, rec.lines(), []string{
		"GET /open-apis/v1/insights?range=7d",
		"GET /open-apis/v1/insights?range=7d&tab=teaching",
		"GET /open-apis/v1/insights?range=7d&tab=income",
	})
	if !strings.HasPrefix(rec.reqs[0].auth, "Bearer t-cn.org-1-") {
		t.Fatalf("auth %q", rec.reqs[0].auth)
	}
}

func TestCreatePostConsentRequired(t *testing.T) {
	m := newMock(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		writeJSON(w, 409, map[string]any{"error": map[string]any{"code": "CONFLICT", "message": "consent",
			"details": map[string]any{"reason": "consent_required", "consent_url": "https://www.ichinese.cn/consent/x"}}})
	})
	_, err := m.client().CreatePost(context.Background(), "u", Params{"body": "x"})
	e, ok := AsError(err)
	if !ok || e.Status != 409 || e.Reason != ReasonConsentRequired || e.ConsentURL() != "https://www.ichinese.cn/consent/x" {
		t.Fatalf("err %v", err)
	}
	if m.apiCalls.Load() != 1 {
		t.Fatalf("%d calls", m.apiCalls.Load())
	}
}
