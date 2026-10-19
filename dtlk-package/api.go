package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxRetries   = 5
	partTimeout  = 10 * time.Minute
	readIdleTime = 60 * time.Second
)

type apiClient struct {
	base string
	hc   *http.Client
}

func newAPIClient() (*apiClient, error) {
	base, err := serverURL()
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	return newAPIClientAt(base), nil
}

// newAPIClientAt talks to base (a normalized server URL) regardless of the config.
func newAPIClientAt(base string) *apiClient {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 2 * time.Minute
	return &apiClient{base: base, hc: &http.Client{Transport: tr}}
}

// apiError is a non-2xx response; Detail is the server's message.
type apiError struct {
	Status     int
	Detail     string
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	msg := e.Detail
	if msg == "" {
		msg = fmt.Sprintf("server returned %d", e.Status)
	}
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %s)", e.RetryAfter.Round(time.Second))
	}
	return msg
}

func readAPIError(resp *http.Response) error {
	e := &apiError{Status: resp.StatusCode}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var parsed struct {
		Detail any `json:"detail"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		if s, ok := parsed.Detail.(string); ok {
			e.Detail = s
		}
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		e.RetryAfter = time.Duration(s) * time.Second
	}
	return e
}

// retryable reports whether a failed request is worth repeating.
func retryable(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusRequestTimeout || (ae.Status >= 500 && ae.Status != http.StatusInsufficientStorage)
	}
	return true // network error
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func backoff(attempt int) time.Duration {
	return min(time.Duration(1<<attempt)*time.Second, 15*time.Second)
}

func (c *apiClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "dtlk/"+version)
	return req, nil
}

func (c *apiClient) send(req *http.Request) (*http.Response, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctxErr := req.Context().Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return nil, err
		}
		return nil, fmt.Errorf("cannot reach server (%s): %w", c.base, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, readAPIError(resp)
	}
	return resp, nil
}

func (c *apiClient) doJSON(req *http.Request, out any) error {
	resp, err := c.send(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("failed to decode server response: %w", err)
	}
	return nil
}

func (c *apiClient) postJSON(ctx context.Context, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.doJSON(req, out)
}

type createUploadReq struct {
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	EstimatedSize int64  `json:"estimated_size"`
	Hours         int    `json:"hours"`
	Tag           string `json:"tag,omitempty"`
}

type createUploadResp struct {
	UploadID  string `json:"upload_id"`
	ChunkSize int64  `json:"chunk_size"`
	MaxSize   int64  `json:"max_size"`
}

type completeResp struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	Size      int64     `json:"size"`
}

type fileInfo struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Size      int64     `json:"size"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c *apiClient) createUpload(ctx context.Context, r createUploadReq) (*createUploadResp, error) {
	var out createUploadResp
	if err := c.postJSON(ctx, "/api/uploads", r, &out); err != nil {
		return nil, err
	}
	if out.UploadID == "" || out.ChunkSize <= 0 {
		return nil, errors.New("invalid response from server")
	}
	return &out, nil
}

func (c *apiClient) putPart(ctx context.Context, id string, n int, data []byte) error {
	var err error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err = c.putPartOnce(ctx, id, n, data); err == nil || ctx.Err() != nil || !retryable(err) {
			return err
		}
		if serr := sleepCtx(ctx, backoff(attempt)); serr != nil {
			return serr
		}
	}
	return err
}

func (c *apiClient) putPartOnce(ctx context.Context, id string, n int, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, partTimeout)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodPut, fmt.Sprintf("/api/uploads/%s/parts/%d", url.PathEscape(id), n), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	return c.doJSON(req, nil)
}

func (c *apiClient) complete(ctx context.Context, id string) (*completeResp, error) {
	var out completeResp
	if err := c.postJSON(ctx, "/api/uploads/"+url.PathEscape(id)+"/complete", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// abort tells the server to drop an unfinished upload; best effort, even after Ctrl+C.
func (c *apiClient) abort(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodDelete, "/api/uploads/"+url.PathEscape(id), nil)
	if err != nil {
		return
	}
	_ = c.doJSON(req, nil)
}

func (c *apiClient) fileInfo(ctx context.Context, code string) (*fileInfo, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/files/"+code, nil)
	if err != nil {
		return nil, err
	}
	var out fileInfo
	if err := c.doJSON(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// resumableReader streams /api/files/{code}/download and transparently
// reconnects with a Range header when the connection drops or stalls.
type resumableReader struct {
	ctx    context.Context
	c      *apiClient
	code   string
	size   int64
	off    int64
	fails  int
	body   io.ReadCloser
	cancel context.CancelFunc
	idle   *time.Timer
	onRead func(int)
	warn   func(string)
}

func (c *apiClient) openDownload(ctx context.Context, code string, size int64, onRead func(int), warn func(string)) io.ReadCloser {
	return &resumableReader{ctx: ctx, c: c, code: code, size: size, onRead: onRead, warn: warn}
}

func (r *resumableReader) open() error {
	ctx, cancel := context.WithCancel(r.ctx)
	req, err := r.c.newRequest(ctx, http.MethodGet, "/api/files/"+r.code+"/download", nil)
	if err != nil {
		cancel()
		return err
	}
	if r.off > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", r.off))
	}
	idle := time.AfterFunc(readIdleTime, cancel)
	resp, err := r.c.send(req)
	if err != nil {
		idle.Stop()
		cancel()
		return err
	}
	if r.off > 0 && (resp.StatusCode != http.StatusPartialContent ||
		!strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", r.off))) {
		resp.Body.Close()
		idle.Stop()
		cancel()
		return errors.New("server does not support resuming downloads")
	}
	r.body, r.cancel, r.idle = resp.Body, cancel, idle
	return nil
}

func (r *resumableReader) closeBody() {
	if r.body != nil {
		r.idle.Stop()
		r.body.Close()
		r.cancel()
		r.body = nil
	}
}

func (r *resumableReader) retry(err error) error {
	if r.ctx.Err() != nil {
		return r.ctx.Err()
	}
	r.fails++
	if !retryable(err) || r.fails > maxRetries {
		return err
	}
	if r.warn != nil {
		r.warn(fmt.Sprintf("Connection lost, resuming (%d/%d): %v", r.fails, maxRetries, err))
	}
	return sleepCtx(r.ctx, backoff(r.fails))
}

func (r *resumableReader) Read(p []byte) (int, error) {
	for {
		if r.off >= r.size {
			r.closeBody()
			return 0, io.EOF
		}
		if r.body == nil {
			if err := r.open(); err != nil {
				if err := r.retry(err); err != nil {
					return 0, err
				}
				continue
			}
		}
		n, err := r.body.Read(p)
		if n > 0 {
			r.off += int64(n)
			r.fails = 0
			r.idle.Reset(readIdleTime)
			if r.onRead != nil {
				r.onRead(n)
			}
			return n, nil
		}
		if err == nil {
			continue
		}
		r.closeBody()
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		if err := r.retry(err); err != nil {
			return 0, err
		}
	}
}

func (r *resumableReader) Close() error {
	r.closeBody()
	return nil
}
