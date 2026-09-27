package service

import (
	"bytes"
	"context"
	"io"
	"net/http"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/tidwall/gjson"
)

// RelayErrorHandlerWithAudit is RelayErrorHandler for relay paths: before the
// non-2xx upstream response is turned into an error, its body excerpt,
// headers and any model/id it declares are observed, so the error log gets a
// ursp block and an evidence row. The flip trackers are never fed from error
// bodies (appendUpstreamResponseAudit skips them for failed attempts).
func RelayErrorHandlerWithAudit(ctx context.Context, info *relaycommon.RelayInfo, resp *http.Response, showBodyWhenFail bool) *types.NewAPIError {
	if info != nil && resp != nil && resp.Body != nil {
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			// hand RelayErrorHandler the same partial read and the same error
			resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), failingReader{err: err}))
			return RelayErrorHandler(ctx, resp, showBodyWhenFail)
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		info.MarkUpstreamObserver("error_response")
		info.SetUpstreamResponseForensicsBytes(body, resp.Header)
		info.ObserveUpstreamResponse(gjson.GetBytes(body, "model").String(), gjson.GetBytes(body, "id").String(), 0, "")
	}
	return RelayErrorHandler(ctx, resp, showBodyWhenFail)
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }
