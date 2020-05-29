package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/kinecosystem/agora-common/retry"
	"github.com/kinecosystem/agora-common/retry/backoff"
	"github.com/pkg/errors"

	"github.com/kinecosystem/agora/pkg/app"
	"github.com/kinecosystem/agora/pkg/webhook/signtransaction"
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
func (c *Client) SignTransaction(ctx context.Context, appConfig *app.Config, req *signtransaction.RequestBody) (envelopeXDR string, err error) {
	if appConfig.SignTransactionURL == nil {
		return string(req.TransactionXDR), nil
	}

	signTxJSON, err := json.Marshal(req)
	if err != nil {
		return "", errors.Wrap(err, "failed to marshal sign transaction request body")
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, appConfig.SignTransactionURL.String(), bytes.NewBuffer(signTxJSON))
	if err != nil {
		return "", errors.Wrap(err, "failed to create sign transaction http request")
	}
	httpReq.Header.Set("Content-Type", "application/json")

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
		return "", errors.Wrap(err, "failed to call sign transaction webhook")
	}

	if resp.StatusCode == 200 {
		decodedResp := &signtransaction.SuccessResponse{}
		err = json.NewDecoder(resp.Body).Decode(decodedResp)
		if err != nil {
			return "", errors.Wrap(err, "failed to decode 200 response")
		}
		if err = decodedResp.Validate(); err != nil {
			return "", errors.Wrap(err, "received invalid response")
		}

		return string(decodedResp.TransactionXDR), nil
	}

	if resp.StatusCode == 400 {
		decodedResp := &signtransaction.BadRequestResponse{}
		err := json.NewDecoder(resp.Body).Decode(decodedResp)
		if err != nil {
			return "", errors.Wrap(err, "failed to decode 400 response")
		}

		return "", &SignTransactionError{Message: decodedResp.Message, StatusCode: 400}
	}

	if resp.StatusCode == 403 {
		decodedResp := &signtransaction.ForbiddenResponse{}
		err := json.NewDecoder(resp.Body).Decode(decodedResp)
		if err != nil {
			return "", errors.Wrap(err, "failed to decode 403 response")
		}

		return "", &SignTransactionError{Message: decodedResp.Message, StatusCode: 403, OperationErrors: decodedResp.InvoiceErrors}
	}

	if resp.StatusCode == 404 {
		decodedResp := &signtransaction.NotFoundResponse{}
		err := json.NewDecoder(resp.Body).Decode(decodedResp)
		if err != nil {
			return "", errors.Wrap(err, "failed to decode 404 response")
		}

		return "", &SignTransactionError{Message: decodedResp.Message, StatusCode: 404}
	}

	return "", &SignTransactionError{Message: "failed to sign transaction", StatusCode: resp.StatusCode}
}
