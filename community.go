package opensdk

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ── posting in a person's name (D105), and as the institution (M6.21 K2) ────

// UploadImage uploads an image for a post: it asks for an upload slot, PUTs the bytes there and returns
// the key to put in images[].key. Up to 10 MB; image/jpeg, image/png, image/webp or image/gif.
func (c *Client) UploadImage(ctx context.Context, userAccessToken string, data []byte, contentType string) (string, error) {
	return c.uploadImage(ctx, WithUserAccessToken(userAccessToken), data, contentType)
}

// UploadImageAsOrg is UploadImage for a post of the institution (its tenant token; the institution granted
// community:post).
func (c *Client) UploadImageAsOrg(ctx context.Context, tenantKey string, data []byte, contentType string) (string, error) {
	return c.uploadImage(ctx, WithTenantKey(tenantKey), data, contentType)
}

func (c *Client) uploadImage(ctx context.Context, who CallOption, data []byte, contentType string) (string, error) {
	slot, err := c.CreateCommunityImageUpload(ctx, Params{"content_type": contentType, "size_bytes": len(data)}, who)
	if err != nil {
		return "", err
	}
	uploadURL, _ := slot["upload_url"].(string)
	if err := c.putUpload(ctx, uploadURL, slot["headers"], bytes.NewReader(data), int64(len(data))); err != nil {
		return "", err
	}
	key, _ := slot["key"].(string)
	return key, nil
}

// UploadVideo uploads a video for a post (up to 4 GB, 1 hour): it asks for an upload slot, PUTs size
// bytes of r there (streamed; for a file, size is its Stat().Size()) and starts transcoding. It returns
// the video's status (asset_id); wait for ready with WaitVideoReady before posting.
func (c *Client) UploadVideo(ctx context.Context, userAccessToken string, r io.Reader, size int64, filename, contentType string) (Object, error) {
	return c.uploadVideo(ctx, WithUserAccessToken(userAccessToken), r, size, filename, contentType)
}

// UploadVideoAsOrg is UploadVideo for a post of the institution.
func (c *Client) UploadVideoAsOrg(ctx context.Context, tenantKey string, r io.Reader, size int64, filename, contentType string) (Object, error) {
	return c.uploadVideo(ctx, WithTenantKey(tenantKey), r, size, filename, contentType)
}

func (c *Client) uploadVideo(ctx context.Context, who CallOption, r io.Reader, size int64, filename, contentType string) (Object, error) {
	slot, err := c.CreateCommunityVideoUpload(ctx,
		Params{"filename": filename, "content_type": contentType, "size_bytes": size}, who)
	if err != nil {
		return nil, err
	}
	upload, _ := slot["upload"].(map[string]any)
	uploadURL, _ := upload["url"].(string)
	if err := c.putUpload(ctx, uploadURL, upload["headers"], r, size); err != nil {
		return nil, err
	}
	assetID, _ := slot["asset_id"].(string)
	// Calling it again has no effect, so it is retried like a GET.
	return c.call(ctx, &callSpec{
		id: "completeCommunityVideoUpload", method: http.MethodPost, path: "/community/videos/{asset_id}/complete",
		pathParams: map[string]string{"asset_id": assetID}, tokenTypes: []TokenType{TokenUser, TokenTenant}, idempotent: true,
	}, []CallOption{who})
}

// WaitOption sets how WaitVideoReady polls.
type WaitOption func(*waitOptions)

type waitOptions struct {
	timeout, interval, maxInterval time.Duration
}

// WithWaitTimeout gives up after d (an *Error with code TIMEOUT). Default 30 minutes; a deadline of ctx
// also stops the wait.
func WithWaitTimeout(d time.Duration) WaitOption {
	return func(o *waitOptions) { o.timeout = d }
}

// WithPollInterval sets the first wait between two looks (default 2s); it doubles up to longest
// (default 15s).
func WithPollInterval(first, longest time.Duration) WaitOption {
	return func(o *waitOptions) { o.interval, o.maxInterval = first, longest }
}

// WaitVideoReady waits until a video has been transcoded and returns it (status ready). A failed
// transcode returns an *Error with reason video_failed (upload another file); running out of time
// returns one with code TIMEOUT (reason video_not_ready). You also get the community.video_ready event.
func (c *Client) WaitVideoReady(ctx context.Context, userAccessToken, assetID string, opts ...WaitOption) (Object, error) {
	return c.waitVideoReady(ctx, WithUserAccessToken(userAccessToken), assetID, opts)
}

// WaitVideoReadyAsOrg is WaitVideoReady for a video uploaded for the institution.
func (c *Client) WaitVideoReadyAsOrg(ctx context.Context, tenantKey, assetID string, opts ...WaitOption) (Object, error) {
	return c.waitVideoReady(ctx, WithTenantKey(tenantKey), assetID, opts)
}

func (c *Client) waitVideoReady(ctx context.Context, who CallOption, assetID string, opts []WaitOption) (Object, error) {
	o := waitOptions{timeout: 30 * time.Minute, interval: 2 * time.Second, maxInterval: 15 * time.Second}
	for _, f := range opts {
		f(&o)
	}
	deadline := c.now().Add(o.timeout)
	interval := o.interval
	for {
		video, err := c.GetCommunityVideo(ctx, assetID, who)
		if err != nil {
			return nil, err
		}
		status, _ := video["status"].(string)
		switch status {
		case "ready":
			return video, nil
		case "failed":
			msg := fmt.Sprintf("video %s could not be transcoded", assetID)
			if reason, _ := video["error"].(string); reason != "" {
				msg += " (" + reason + ")"
			}
			return nil, newError(http.StatusConflict, "CONFLICT", msg,
				map[string]any{"reason": ReasonVideoFailed, "asset_id": assetID, "error": video["error"]})
		}
		left := deadline.Sub(c.now())
		if left <= 0 {
			return nil, newError(0, CodeTimeout, fmt.Sprintf("video %s is not ready yet (%s)", assetID, status),
				map[string]any{"reason": ReasonVideoNotReady, "asset_id": assetID, "status": status})
		}
		if err := sleep(ctx, min(interval, left)); err != nil {
			return nil, err
		}
		interval = min(interval*2, o.maxInterval)
	}
}

// CreatePost posts in the person's name. post is a Params (or any struct that encodes to the contract's
// CreateCommunityPost). Without a uuid the SDK makes one, so its own retries never post twice; give your
// own uuid to make your retries safe too (one post per uuid within an hour). Errors keep the server's
// reason: consent_required (send the person to (*Error).ConsentURL), video_not_ready, video_failed,
// post_rate_limited, app_user_daily_limit, daily_quota_exceeded, minor_not_allowed, app_post_stopped, …
func (c *Client) CreatePost(ctx context.Context, userAccessToken string, post any) (Object, error) {
	return c.createPost(ctx, WithUserAccessToken(userAccessToken), post)
}

// CreatePostAsOrg posts as the institution, with its community account (its name and logo; the post says it
// came from this app). The institution must have installed the app and granted community:post; before its
// homepage was first published there is no account yet (409 no_org_account). The same limits, review and
// uuid handling as CreatePost.
func (c *Client) CreatePostAsOrg(ctx context.Context, tenantKey string, post any) (Object, error) {
	return c.createPost(ctx, WithTenantKey(tenantKey), post)
}

func (c *Client) createPost(ctx context.Context, who CallOption, post any) (Object, error) {
	body := Params{}
	if p, ok := post.(map[string]any); ok {
		for k, v := range p {
			body[k] = v
		}
	} else if post != nil {
		b, err := json.Marshal(post)
		if err != nil {
			return nil, fmt.Errorf("opensdk: CreatePost: encoding the post: %w", err)
		}
		if err := json.Unmarshal(b, &body); err != nil {
			return nil, fmt.Errorf("opensdk: CreatePost: the post is not a JSON object: %w", err)
		}
	}
	if id, _ := body["uuid"].(string); id == "" {
		body["uuid"] = newUUID()
	}
	return c.call(ctx, &callSpec{
		id: "createCommunityPost", method: http.MethodPost, path: "/community/posts", body: body,
		tokenTypes: []TokenType{TokenUser, TokenTenant}, idempotent: true,
	}, []CallOption{who})
}

// putUpload PUTs an upload to the slot the platform gave (object storage, not the open API: no token,
// its own headers). Not limited by WithTimeout, a large video takes as long as it takes; cancel with ctx.
func (c *Client) putUpload(ctx context.Context, uploadURL string, headers any, body io.Reader, size int64) error {
	if uploadURL == "" {
		return newError(0, CodeInvalidPayload, "the upload slot has no address", nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, body)
	if err != nil {
		return newError(0, CodeInvalidPayload, "PUT upload: "+err.Error(), nil)
	}
	req.ContentLength = size
	if h, ok := headers.(map[string]any); ok {
		for k, v := range h {
			if s, ok := v.(string); ok {
				req.Header.Set(k, s)
			}
		}
	}
	res, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return newError(0, CodeNetwork, "PUT upload: "+err.Error(), nil)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 500))
		return newError(res.StatusCode, CodeUploadFailed, fmt.Sprintf("PUT upload: HTTP %d", res.StatusCode),
			map[string]any{"reason": "upload_failed", "body": string(data)})
	}
	_, _ = io.Copy(io.Discard, res.Body)
	return nil
}

// newUUID is a random UUID (version 4).
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
