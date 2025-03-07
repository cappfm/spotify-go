// Package spotify provides utilities for interfacing
// with Spotify's Web API.
package spotify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"golang.org/x/oauth2"
)

// Version is the version of this library.
const Version = "1.0.0"

const (
	// DateLayout can be used with time.Parse to create time.Time values
	// from Spotify date strings.  For example, PrivateUser.Birthdate
	// uses this format.
	DateLayout = "2006-01-02"
	// TimestampLayout can be used with time.Parse to create time.Time
	// values from SpotifyTimestamp strings.  It is an ISO 8601 UTC timestamp
	// with a zero offset.  For example, PlaylistTrack's AddedAt field uses
	// this format.
	TimestampLayout = "2006-01-02T15:04:05Z"

	// defaultRetryDurationS helps us fix an apparent server bug whereby we will
	// be told to retry but not be given a wait-interval.
	defaultRetryDuration = time.Second * 5

	// rateLimitExceededStatusCode is the code that the server returns when our
	// request frequency is too high.
	rateLimitExceededStatusCode = 429
)

// Client is a client for working with the Spotify Web API.
// It is best to create this using spotify.New()
type Client struct {
	http    *http.Client
	baseURL string

	autoRetry      bool
	acceptLanguage string
}

type ClientOption func(client *Client)

// WithRetry configures the Spotify API client to automatically retry requests that fail due to rate limiting.
func WithRetry(shouldRetry bool) ClientOption {
	return func(client *Client) {
		client.autoRetry = shouldRetry
	}
}

// WithBaseURL provides an alternative base url to use for requests to the Spotify API. This can be used to connect to a
// staging or other alternative environment.
func WithBaseURL(url string) ClientOption {
	return func(client *Client) {
		client.baseURL = url
	}
}

// WithAcceptLanguage configures the client to provide the accept language header on all requests.
func WithAcceptLanguage(lang string) ClientOption {
	return func(client *Client) {
		client.acceptLanguage = lang
	}
}

// New returns a client for working with the Spotify Web API.
// The provided httpClient must provide Authentication with the requests.
// The auth package may be used to generate a suitable client.
func New(httpClient *http.Client, opts ...ClientOption) *Client {
	c := &Client{
		http:    httpClient,
		baseURL: "https://api.spotify.com/v1/",
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// URI identifies an artist, album, track, or category.  For example,
// spotify:track:6rqhFgbbKwnb9MLmUQDhG6
type URI string

// ID is a base-62 identifier for an artist, track, album, etc.
// It can be found at the end of a spotify.URI.
type ID string

func (id *ID) String() string {
	return string(*id)
}

// Followers contains information about the number of people following a
// particular artist or playlist.
type Followers struct {
	// The total number of followers.
	Count float64 `json:"total"`
	// A link to the Web API endpoint providing full details of the followers,
	// or the empty string if this data is not available.
	Endpoint string `json:"href"`
}

// Image identifies an image associated with an item.
type Image struct {
	// The image height, in pixels.
	Height float64 `json:"height"`
	// The image width, in pixels.
	Width float64 `json:"width"`
	// The source URL of the image.
	URL string `json:"url"`
}

// Download downloads the image and writes its data to the specified io.Writer.
func (i Image) Download(dst io.Writer) error {
	resp, err := http.Get(i.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// TODO: get Content-Type from header?
	if resp.StatusCode != http.StatusOK {
		return errors.New("Couldn't download image - HTTP" + strconv.Itoa(resp.StatusCode))
	}
	_, err = io.Copy(dst, resp.Body)
	return err
}

// Error represents an error returned by the Spotify Web API.
type Error struct {
	// A short description of the error.
	Message string `json:"message"`
	// The HTTP status code.
	Status int `json:"status"`
}

func (e Error) Error() string {
	return e.Message
}

// decodeError decodes an Error from an io.Reader.
func (c *Client) decodeError(resp *http.Response) error {
	responseBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if len(responseBody) == 0 {
		return fmt.Errorf("spotify: HTTP %d: %s (body empty)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	buf := bytes.NewBuffer(responseBody)

	var e struct {
		E Error `json:"error"`
	}
	err = json.NewDecoder(buf).Decode(&e)
	if err != nil {
		return fmt.Errorf("spotify: couldn't decode error: (%d) [%s]", len(responseBody), responseBody)
	}

	if e.E.Message == "" {
		// Some errors will result in there being a useful status-code but an
		// empty message, which will confuse the user (who only has access to
		// the message and not the code). An example of this is when we send
		// some of the arguments directly in the HTTP query and the URL ends-up
		// being too long.

		e.E.Message = fmt.Sprintf("spotify: unexpected HTTP %d: %s (empty error)",
			resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	return e.E
}

// shouldRetry determines whether the status code indicates that the
// previous operation should be retried at a later time
func shouldRetry(status int) bool {
	return status == http.StatusAccepted || status == http.StatusTooManyRequests
}

// isFailure determines whether the code indicates failure
func isFailure(code int, validCodes []int) bool {
	for _, item := range validCodes {
		if item == code {
			return false
		}
	}
	return true
}

// `execute` executes a non-GET request. `needsStatus` describes other HTTP
// status codes that will be treated as success. Note that we allow all 200s
// even if there are additional success codes that represent success.
func (c *Client) execute(req *http.Request, result interface{}, needsStatus ...int) error {
	logger := slog.With(":spotify", true, "url", req.URL.String())

	if c.acceptLanguage != "" {
		req.Header.Set("Accept-Language", c.acceptLanguage)
	}
	for {
		beforeReq := time.Now().UTC()
		logger.DebugContext(req.Context(), "request spotify")
		resp, err := c.http.Do(req)

		var statusCode int
		if resp != nil {
			statusCode = resp.StatusCode
		}
		ellapsed := time.Since(beforeReq)

		// observability: metrics
		// observability: logs
		metricLatencyHist.Record(req.Context(), int64(ellapsed/time.Millisecond),
			metric.WithAttributes(
				semconv.HTTPStatusCode(statusCode),
				semconv.HTTPRoute(req.URL.Path),
			),
		)

		switch statusCode {
		case rateLimitExceededStatusCode:
			retryAfter := resp.Header.Get("retry-after")
			slog.WarnContext(req.Context(), "will retry...",
				":spotify-resp", true, "err", err, "ellapsed", ellapsed,
				"status", statusCode, "retryAfter", retryAfter)
		default:
			slog.DebugContext(req.Context(), "spotify response",
				":spotify-resp", true, "err", err, "ellapsed", ellapsed,
				"status", statusCode)
		}

		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if shouldRetry(resp.StatusCode) {
			if c.autoRetry {
				logger.WarnContext(req.Context(), "rate limit exceeded", "retry", retryDuration(resp))
				if err := sleep(req.Context(), retryDuration(resp)); err != nil {
					return err
				}
				continue
			} else {
				return &TooManyRequestsError{retryDuration(resp)}
			}
		}
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		if (resp.StatusCode >= 300 ||
			resp.StatusCode < 200) &&
			isFailure(resp.StatusCode, needsStatus) {
			return c.decodeError(resp)
		}

		if result != nil {
			if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
				return err
			}
		}
		break
	}
	return nil
}

func retryDuration(resp *http.Response) time.Duration {
	raw := resp.Header.Get("Retry-After")
	if raw == "" {
		return defaultRetryDuration
	}
	seconds, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return defaultRetryDuration
	}
	return time.Duration(seconds) * time.Second
}

func (c *Client) get(ctx context.Context, url string, result interface{}) error {
	logger := slog.With(":spotify", true, "url", url)

	for {
		beforeReq := time.Now().UTC()
		logger.DebugContext(ctx, "request spotify", ":spotify-req", true)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if c.acceptLanguage != "" {
			req.Header.Set("Accept-Language", c.acceptLanguage)
		}
		if err != nil {
			logger.ErrorContext(ctx, "unable to request spotify", "err", err)
			return err
		}
		resp, err := c.http.Do(req)
		ellapsed := time.Since(beforeReq)

		var statusCode int
		if resp != nil {
			statusCode = resp.StatusCode
		}
		metricLatencyHist.Record(req.Context(), int64(ellapsed/time.Millisecond),
			metric.WithAttributes(
				semconv.HTTPStatusCode(statusCode),
				semconv.HTTPRoute(req.URL.Path),
			),
		)

		switch statusCode {
		case rateLimitExceededStatusCode:
			retryAfter := resp.Header.Get("retry-after")
			slog.WarnContext(req.Context(), "will retry...",
				":spotify-resp", true, "err", err, "ellapsed", ellapsed,
				"status", statusCode, "retryAfter", retryAfter)
		}

		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode == rateLimitExceededStatusCode {
			if c.autoRetry {
				logger.WarnContext(ctx, "rate limit exceeded", "retry", retryDuration(resp))
				if err := sleep(ctx, retryDuration(resp)); err != nil {
					return err
				}
				continue
			} else {
				return &TooManyRequestsError{retryDuration(resp)}
			}
		}
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		if resp.StatusCode != http.StatusOK {
			return c.decodeError(resp)
		}

		err = json.NewDecoder(resp.Body).Decode(result)
		if err != nil {
			return err
		}

		break
	}

	return nil
}

func (c *Client) Get(ctx context.Context, path string, result interface{}) error {
	return c.get(ctx, c.baseURL+path, result)
}

// NewReleases gets a list of new album releases featured in Spotify.
// Supported options: Country, Limit, Offset
func (c *Client) NewReleases(ctx context.Context, opts ...RequestOption) (albums *SimpleAlbumPage, err error) {
	spotifyURL := c.baseURL + "browse/new-releases"
	if params := processOptions(opts...).urlParams.Encode(); params != "" {
		spotifyURL += "?" + params
	}

	var objmap map[string]*json.RawMessage
	err = c.get(ctx, spotifyURL, &objmap)
	if err != nil {
		return nil, err
	}

	var result SimpleAlbumPage
	err = json.Unmarshal(*objmap["albums"], &result)
	if err != nil {
		return nil, err
	}

	return &result, nil
}

// Token gets the client's current token.
func (c *Client) Token() (*oauth2.Token, error) {
	transport, ok := c.http.Transport.(*oauth2.Transport)
	if !ok {
		return nil, errors.New("spotify: client not backed by oauth2 transport")
	}
	t, err := transport.Source.Token()
	if err != nil {
		return nil, err
	}
	return t, nil
}

func sleep(ctx context.Context, dur time.Duration) error {
	select {
	case <-ctx.Done():
		break
	case <-time.After(dur):
		break
	}

	return ctx.Err()
}
