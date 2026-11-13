package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/cosy"
)

// QueueWaitCallback allows the streaming handler to emit SSE ": ping\n\n" heartbeats while waiting in 10605 queue.
type QueueWaitCallback func(wait time.Duration, round int, queueCount int)

// applyInferenceHeaders populates Qoder COSY authentication, device identity, and signing headers
// matching official qodercli 1.1.75 / qoder_sign.py.
func (c *Client) applyInferenceHeaders(req *http.Request, sess *cosy.SessionContext, encodedBody, pathWithoutAlgo, modelKey string, isSSE bool, a *auth.Auth) {
	payloadB64 := cosy.BuildPayloadB64(sess.Info)
	cosyDate := fmt.Sprintf("%d", cosy.UnixSec())
	sig := cosy.SignRequest(payloadB64, sess.CosyKey, cosyDate, encodedBody, pathWithoutAlgo)

	req.Header.Set("authorization", cosy.ComposeBearer(payloadB64, sig))
	req.Header.Set("cosy-date", cosyDate)
	req.Header.Set("cosy-user", sess.Identity.Uid)
	req.Header.Set("cosy-key", sess.CosyKey)
	req.Header.Set("cosy-data-policy", "agree")
	req.Header.Set("login-version", "v2")
	req.Header.Set("cosy-clientip", "169.254.198.161")
	req.Header.Set("accept-encoding", "identity")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", cosy.CosyUserAgent())
	if isSSE {
		req.Header.Set("accept", "text/event-stream")
		req.Header.Set("cache-control", "no-cache")
	} else {
		req.Header.Set("accept", "application/json")
	}

	// Switch client profile if account carries real captured desktop fingerprint.
	if a != nil {
		a.Lock()
		fp := a.CheckinFingerprint
		a.Unlock()
		if fp != nil && fp.MachineToken != "" {
			ver := fp.Version
			if ver == "" {
				ver = "0.4.8"
			}
			req.Header.Set("cosy-version", ver)
			req.Header.Set("cosy-clienttype", "10")
			req.Header.Set("cosy-business-product", "app")
			req.Header.Set("cosy-business-type", "agent")
			req.Header.Set("cosy-scene", "app")
			req.Header.Set("cosy-machineos", cosy.MachineOS())
			req.Header.Set("cosy-machineid", fp.MachineID)
			req.Header.Set("cosy-machinetoken", fp.MachineToken)
			if fp.MachineType != "" {
				req.Header.Set("cosy-machinetype", fp.MachineType)
			}
			if fp.MachineCode != "" {
				req.Header.Set("cosy-machinecode", fp.MachineCode)
			}
			if isSSE && fp.Hostname != "" {
				req.Header.Set("cosy-machinehostname", fp.Hostname)
			}
			goto modelHeaders
		}
	}

	req.Header.Set("cosy-version", cosy.CosyVersion())
	req.Header.Set("cosy-clienttype", cosy.CosyClientType())
	req.Header.Set("cosy-business-product", "cli")
	req.Header.Set("cosy-business-type", "agent")
	req.Header.Set("cosy-scene", "assistant")
	req.Header.Set("cosy-machineos", cosy.MachineOS())
	req.Header.Set("cosy-machineid", sess.MachineId)
	req.Header.Set("cosy-machinetoken", sess.MachineToken)
	req.Header.Set("cosy-machinetype", sess.MachineType)
	if isSSE {
		req.Header.Set("cosy-machinehostname", sess.Hostname)
	}

modelHeaders:
	if modelKey != "" {
		req.Header.Set("x-model-key", modelKey)
		req.Header.Set("x-model-source", "system")
	}
}

// ChatStreamContext executes one Qoder streaming request with:
//   - COSY payload encoding + signature
//   - multi-gateway host failover (api3 -> api2 -> api1) on transient transport errors
//   - bounded transient HTTP retry (418/5xx)
//   - optional 10605 queue wait & SSE keepalive callback
//   - Qoder SSE envelope unwrapping (detecting wrapped HTTP 200+statusCodeValue!=200 failures)
func (c *Client) ChatStreamContext(
	ctx context.Context,
	a *auth.Auth,
	openAIReq map[string]any,
	modelKey string,
	meta ChatMeta,
	clientIP string,
	onQueueWait QueueWaitCallback,
) (io.ReadCloser, int, []byte, error) {
	if a == nil {
		return nil, 503, nil, fmt.Errorf("missing account")
	}
	realm := NormalizeRealm(a.Realm())
	if modelKey == "" {
		modelKey = ResolveUpstreamKey(anyStr(openAIReq["model"]), realm)
	}
	bodyMap, err := BuildQoderPayload(openAIReq, a, modelKey, realm, meta)
	if err != nil {
		var ue *Error
		if errors.As(err, &ue) {
			return nil, ue.Status, []byte(ue.Msg), ue
		}
		return nil, 400, []byte(err.Error()), err
	}
	plainJSON, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, 400, []byte(err.Error()), err
	}
	encodedBody, err := cosy.Encode(plainJSON)
	if err != nil {
		return nil, 500, []byte(err.Error()), err
	}

	pathWithoutAlgo := "/api/v2/service/pro/sse/agent_chat_generation"
	hosts := c.OrderedGateways(a)
	if len(hosts) == 0 {
		return nil, 500, []byte("no gateway hosts configured"), fmt.Errorf("no gateway hosts for realm %s", realm)
	}

	waitMode, maxQueueRetries, queueCap, queueDef, waitFirstToken, queueBudget := CurrentQueueSettings()
	queueStart := time.Now()
	queueRounds := 0
	transientTries := 0
	hostIdx := 0

	for {
		sess, err := c.sessionCache().Get(authToCosyIdentity(a))
		if err != nil {
			return nil, 401, []byte(err.Error()), &Error{Kind: ErrSessionDead, Status: 401, Msg: err.Error()}
		}

		gw := hosts[hostIdx]
		fullURL := strings.TrimRight(gw, "/") + PathChatStream
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewBufferString(encodedBody))
		if err != nil {
			return nil, 500, []byte(err.Error()), err
		}
		c.applyInferenceHeaders(req, sess, encodedBody, pathWithoutAlgo, modelKey, true, a)
		if c.PassthroughIP && clientIP != "" {
			req.Header.Set("X-Forwarded-For", clientIP)
			req.Header.Set("X-Real-IP", clientIP)
		}

		resp, derr := c.httpClient().Do(req)
		if derr != nil {
			if ctx.Err() != nil {
				return nil, 499, nil, ctx.Err()
			}
			c.Endpoints.noteFailure("chat-"+realm, gw, derr.Error())
			if IsTransientTransport(derr) && transientTries < TransientMaxRetries {
				transientTries++
				if hostIdx+1 < len(hosts) {
					hostIdx++
				}
				select {
				case <-ctx.Done():
					return nil, 499, nil, ctx.Err()
				case <-time.After(time.Duration(transientTries) * time.Second):
				}
				continue
			}
			return nil, 502, []byte(derr.Error()), &Error{Kind: ErrServer, Status: 502, Msg: derr.Error()}
		}

		if resp.StatusCode == http.StatusOK {
			unwrapped, uerr := unwrapQoderReader(resp.Body)
			if uerr == nil {
				c.Endpoints.noteSuccess("chat-"+realm, gw)
				return unwrapped, http.StatusOK, nil, nil
			}
			// Wrapped error inside SSE envelope (e.g. statusCodeValue=418 or 10605 queued)
			var ue *Error
			if errors.As(uerr, &ue) {
				if handled, retryErr := c.handleRetryableError(ctx, ue, &waitMode, &queueRounds, maxQueueRetries, queueCap, queueDef, waitFirstToken, queueBudget, queueStart, &transientTries, onQueueWait); handled {
					if retryErr != nil {
						return nil, ue.Status, []byte(ue.Msg), retryErr
					}
					continue
				}
				return nil, ue.Status, []byte(ue.Msg), ue
			}
			if errors.Is(uerr, errEmptyStream) {
				return nil, 502, []byte(uerr.Error()), &Error{Kind: ErrServer, Status: 502, Msg: uerr.Error()}
			}
			return nil, 502, []byte(uerr.Error()), &Error{Kind: ErrServer, Status: 502, Msg: uerr.Error()}
		}

		rawErr, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		ue := ClassifyError(resp.StatusCode, string(rawErr))

		if ue.Kind != ErrQueueWait && ue.Kind != ErrContentBlocked && ue.Kind != ErrPromptTooLong && ue.Kind != ErrBadParams {
			c.Endpoints.noteFailure("chat-"+realm, gw, ue.Msg)
		}

		handled, retryErr := c.handleRetryableError(ctx, ue, &waitMode, &queueRounds, maxQueueRetries, queueCap, queueDef, waitFirstToken, queueBudget, queueStart, &transientTries, onQueueWait)
		if handled {
			if retryErr != nil {
				return nil, resp.StatusCode, rawErr, retryErr
			}
			continue
		}
		return nil, resp.StatusCode, rawErr, ue
	}
}

func (c *Client) handleRetryableError(
	ctx context.Context,
	ue *Error,
	waitMode *bool,
	queueRounds *int,
	maxQueueRetries int,
	queueCap time.Duration,
	queueDef time.Duration,
	waitFirstToken bool,
	queueBudget time.Duration,
	queueStart time.Time,
	transientTries *int,
	onQueueWait QueueWaitCallback,
) (shouldHandle bool, fatalErr error) {
	if ue == nil {
		return false, nil
	}
	if ue.Kind == ErrQueueWait {
		if !*waitMode {
			return true, ue // fast 503 mode
		}
		wait := ue.RetryAfter
		if wait <= 0 {
			wait = queueDef
		}
		if wait > queueCap {
			wait = queueCap
		}
		canRetryRound := waitFirstToken || *queueRounds < maxQueueRetries
		if canRetryRound && time.Since(queueStart)+wait <= queueBudget {
			*queueRounds++
			if onQueueWait != nil {
				onQueueWait(wait, *queueRounds, ue.QueueCount)
			}
			select {
			case <-ctx.Done():
				return true, ctx.Err()
			case <-time.After(wait + 500*time.Millisecond):
			}
			return true, nil
		}
		return true, ue
	}

	if IsTransientUpstream(ue.Status, ue.Msg) && *transientTries < TransientMaxRetries {
		*transientTries++
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(time.Duration(*transientTries) * time.Second):
		}
		return true, nil
	}
	return false, nil
}
