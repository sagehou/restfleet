package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
)

const backupRetryHeader = "Restfleet-Backup-Retry"

const (
	retryUnavailable uint32 = 1 << iota
	retryRateLimited
	retryUnsafe
)

// Fixed observations survive request joins, never raw backend errors. Any
// unknown/permanent failure or ambiguous mutation overrides transient hints.
type backupRetryObservation struct{ flags atomic.Uint32 }

func (o *backupRetryObservation) code() string {
	flags := o.flags.Load()
	if flags&retryUnsafe != 0 {
		return ""
	}
	if flags&retryRateLimited != 0 {
		return "GATEWAY_RATE_LIMITED"
	}
	if flags&retryUnavailable != 0 {
		return "GATEWAY_UNAVAILABLE"
	}
	return ""
}

func (o *backupRetryObservation) observe(ctx context.Context, method string, response *http.Response, err error) {
	read := method == http.MethodGet || method == http.MethodHead
	if err != nil {
		flag := retryUnsafe
		var network *net.OpError
		if read && ctx.Err() == nil && (errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
			flag = retryUnavailable // Only the trusted local Unix backend transport.
		}
		o.flags.Or(flag)
		return
	}
	status := response.StatusCode
	if status == http.StatusOK || (read && (status == http.StatusPartialContent || status == http.StatusNotFound || status == http.StatusRequestedRangeNotSatisfiable)) {
		return
	}
	flag := retryUnsafe
	if read {
		switch status {
		case http.StatusTooManyRequests:
			flag = retryRateLimited
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			flag = retryUnavailable
		}
	}
	o.flags.Or(flag)
}
