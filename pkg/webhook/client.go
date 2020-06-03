package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/kinecosystem/agora-common/headers"
	"github.com/kinecosystem/agora-common/retry"
	"github.com/kinecosystem/agora-common/retry/backoff"
	"github.com/kinecosystem/go/xdr"
	"github.com/pkg/errors"

	"github.com/kinecosystem/agora/pkg/app"
	"github.com/kinecosystem/agora/pkg/webhook/signtransaction"
)

const (
	AppUserIDHeader      = "app-user-id"
	AppUserPasskeyHeader = "app-user-passkey"
)

type Client struct {
	httpClient *http.Client
}

type SignTransactionError struct {
	Message         string
	StatusCode      int
	OperationErrors []signtransaction.InvoiceError
}

func (e *SignTransactionError) Error() string {
	return fmt.Sprintf("%s (status code: %d)", e.Message, e.StatusCode)
}

// NewClient returns a client which can be used to submit requests to app webhooks.
func NewClient(httpClient *http.Client) *Client {
	return &Client{
		httpClient: httpClient,
	}
}

// SignTransaction submits a sign transaction request to an app webhook
func (c *Client) SignTransaction(ctx context.Context, signURL url.URL, req *signtransaction.RequestBody) (encodedXDR string, envelopeXDR *xdr.TransactionEnvelope, err error) {
	signTxJSON, err := json.Marshal(req)
	if err != nil {
		return "", nil, errors.Wrap(err, "failed to marshal sign transaction request body")
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, signURL.String(), bytes.NewBuffer(signTxJSON))
	if err != nil {
		return "", nil, errors.Wrap(err, "failed to create sign transaction http request")
	}
	httpReq.Header.Set("Content-Type", "application/json")

	userID, err := headers.GetASCIIHeaderByName(ctx, AppUserIDHeader)
	if err != nil {
		return "", nil, errors.Wrap(err, "failed to get app user ID header")
	}
	userPasskey, err := headers.GetASCIIHeaderByName(ctx, AppUserPasskeyHeader)
	if err != nil {
		return "", nil, errors.Wrap(err, "failed to get app user passkey header")
	}

	if (userID != "") != (userPasskey != "") {
		return "", nil, errors.New("if app user auth headers are used, both must be present")
	} else if userID != "" {
		httpReq.Header.Set("X-App-User-ID", userID)
		httpReq.Header.Set("X-App-User-Passkey", userPasskey)
	}

	var resp *http.Response
	_, err = retry.Retry(
		func() error {
			resp, err = c.httpClient.Do(httpReq)
			return err
		},
		retry.Limit(3),
		retry.BackoffWithJitter(backoff.BinaryExponential(100*time.Millisecond), 440*time.Millisecond, 0.1),
		retry.NonRetriableErrors(app.ErrURLNotSet),
	)
	defer resp.Body.Close()
	if err != nil {
		return "", nil, errors.Wrap(err, "failed to call sign transaction webhook")
	}

	if resp.StatusCode == 200 {
		decodedResp := &signtransaction.SuccessResponse{}
		err = json.NewDecoder(resp.Body).Decode(decodedResp)
		if err != nil {
			return "", nil, errors.Wrap(err, "failed to decode 200 response")
		}
		e, err := decodedResp.GetEnvelopeXDR()
		if err != nil {
			return "", nil, errors.Wrap(err, "received invalid response")
		}

		return decodedResp.EnvelopeXDR, e, nil
	}

	if resp.StatusCode == 400 {
		decodedResp := &signtransaction.BadRequestResponse{}
		err := json.NewDecoder(resp.Body).Decode(decodedResp)
		if err != nil {
			return "", nil, errors.Wrap(err, "failed to decode 400 response")
		}

		return "", nil, &SignTransactionError{Message: decodedResp.Message, StatusCode: 400}
	}

	if resp.StatusCode == 403 {
		decodedResp := &signtransaction.ForbiddenResponse{}
		err := json.NewDecoder(resp.Body).Decode(decodedResp)
		if err != nil {
			return "", nil, errors.Wrap(err, "failed to decode 403 response")
		}

		return "", nil, &SignTransactionError{Message: decodedResp.Message, StatusCode: 403, OperationErrors: decodedResp.InvoiceErrors}
	}

	return "", nil, &SignTransactionError{Message: "failed to sign transaction", StatusCode: resp.StatusCode}
}
