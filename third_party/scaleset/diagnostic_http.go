package scaleset

import (
	"context"
	"github.com/hashicorp/go-retryablehttp"
	"net/http"
)

// Observe the existing retry client's hooks/policy, without substituting its
// transport, timeout, retry count, policy result or error handler.
func instrumentDiagnosticHTTP(c *retryablehttp.Client, d *Diagnostics) {
	if d == nil {
		return
	}
	before := c.RequestLogHook
	c.RequestLogHook = func(l retryablehttp.Logger, r *http.Request, n int) {
		diagnosticHTTPBegin(r.Context())
		if before != nil {
			before(l, r, n)
		}
	}
	policy := c.CheckRetry
	if policy == nil {
		policy = retryablehttp.DefaultRetryPolicy
	}
	c.CheckRetry = func(ctx context.Context, r *http.Response, e error) (bool, error) {
		retry, err := policy(ctx, r, e)
		status := 0
		if r != nil {
			status = r.StatusCode
		}
		diagnosticHTTPEnd(ctx, status, e, retry)
		return retry, err
	}
}
