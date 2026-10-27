// Package client is the CLI's client for the public runtime API. Studio uses the TypeScript
// gateway client in gateway-client/; both speak only the documented HTTP API.
package client

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

// Error is a non-2xx API response.
type Error struct {
	Status  int
	Code    string
	Field   string
	Message string
}

func (e *Error) Error() string { return e.Message }

// ErrUnavailable is a transport failure: the runtime could not be reached.
var ErrUnavailable = errors.New("Runtime is unavailable")

type Client struct {
	URL       string
	token     string
	namespace string
	http      *http.Client
}

func New(base, token, namespace string) *Client {
	return &Client{
		URL:       strings.TrimSuffix(base, "/"),
		token:     token,
		namespace: namespace,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects are refused")
		}},
	}
}

// Response performs a request; body is nil, []byte (sent as given) or a JSON value.
func (c *Client) Response(path, method string, body any, headers map[string]string) (*http.Response, error) {
	var reader io.Reader
	if raw, ok := body.([]byte); ok {
		reader = bytes.NewReader(raw)
	} else if body != nil {
		reader = bytes.NewReader(jsonv.MustMarshal(body))
	}
	req, err := http.NewRequest(method, c.URL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.namespace != "" {
		req.Header.Set("X-Media-Namespace", c.namespace)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		defer res.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		e := &Error{Status: res.StatusCode, Message: fmt.Sprintf("Runtime returned %d", res.StatusCode)}
		if parsed, err := jsonv.Parse(data); err == nil {
			if o, ok := parsed.(*jsonv.Object); ok {
				detail := o.Obj("error")
				if m := detail.Str("message"); m != "" {
					e.Message = m
				}
				e.Code, e.Field = detail.Str("code"), detail.Str("field")
			}
		}
		return nil, e
	}
	return res, nil
}

func (c *Client) JSON(path, method string, body any) (any, error) {
	return c.decode(c.Response(path, method, body, nil))
}

func (c *Client) decode(res *http.Response, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, ErrUnavailable
	}
	return jsonv.Parse(data)
}

// Obj is JSON for object responses.
func (c *Client) Obj(path, method string, body any) (*jsonv.Object, error) {
	v, err := c.JSON(path, method, body)
	if err != nil {
		return nil, err
	}
	o, ok := v.(*jsonv.Object)
	if !ok {
		return nil, errors.New("unexpected response")
	}
	return o, nil
}

func esc(s string) string { return url.PathEscape(s) }

func (c *Client) Health() (*jsonv.Object, error) { return c.Obj("/v1/runtime", "GET", nil) }
func (c *Client) Models() (any, error)           { return c.JSON("/v1/models", "GET", nil) }
func (c *Client) Model(modelID string, verify bool) (any, error) {
	suffix := ""
	if verify {
		suffix = "?verify=1"
	}
	return c.JSON("/v1/models/"+esc(modelID)+suffix, "GET", nil)
}
func (c *Client) Recipe(recipeID string) (*jsonv.Object, error) {
	return c.Obj("/v1/recipes/"+esc(recipeID), "GET", nil)
}
func (c *Client) Catalog(handle string) (any, error) {
	path := "/v1/catalog"
	if handle != "" {
		path += "?handle=" + url.QueryEscape(handle)
	}
	return c.JSON(path, "GET", nil)
}
func (c *Client) Validate(request any) (any, error) {
	return c.JSON("/v1/jobs/validate", "POST", request)
}

// Submit posts a job; key is sent as Idempotency-Key when non-empty.
func (c *Client) Submit(request any, key string) (*jsonv.Object, error) {
	headers := map[string]string{}
	if key != "" {
		headers["Idempotency-Key"] = key
	}
	v, err := c.decode(c.Response("/v1/jobs", "POST", request, headers))
	if err != nil {
		return nil, err
	}
	o, _ := v.(*jsonv.Object)
	return o, nil
}

func (c *Client) Jobs(limit, after int, newest bool) (*jsonv.Object, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if after > 0 {
		q.Set("after", strconv.Itoa(after))
	}
	if newest {
		q.Set("order", "newest")
	}
	path := "/v1/jobs"
	if text := q.Encode(); text != "" {
		path += "?" + text
	}
	return c.Obj(path, "GET", nil)
}
func (c *Client) Status(job string) (*jsonv.Object, error) {
	return c.Obj("/v1/jobs/"+esc(job), "GET", nil)
}
func (c *Client) Cancel(job string) (*jsonv.Object, error) {
	return c.Obj("/v1/jobs/"+esc(job)+"/cancel", "POST", jsonv.NewObject())
}
func (c *Client) Manifest(job string) (any, error) {
	return c.JSON("/v1/jobs/"+esc(job)+"/manifest", "GET", nil)
}
func (c *Client) Output(job, output string) (*http.Response, error) {
	return c.Response("/v1/jobs/"+esc(job)+"/outputs/"+esc(output), "GET", nil, nil)
}

// UploadInput stores PNG bytes as a durable, namespace-owned input.
func (c *Client) UploadInput(png []byte) (*jsonv.Object, error) {
	v, err := c.decode(c.Response("/v1/inputs", "POST", png, map[string]string{"Content-Type": "image/png"}))
	if err != nil {
		return nil, err
	}
	o, _ := v.(*jsonv.Object)
	return o, nil
}
func (c *Client) Inputs() (any, error) { return c.JSON("/v1/inputs", "GET", nil) }
func (c *Client) Input(inputID string) (*jsonv.Object, error) {
	return c.Obj("/v1/inputs/"+esc(inputID), "GET", nil)
}
func (c *Client) DeleteInput(inputID string) (*jsonv.Object, error) {
	return c.Obj("/v1/inputs/"+esc(inputID), "DELETE", nil)
}
