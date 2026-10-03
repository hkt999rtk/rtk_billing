package rawretention

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Consumer struct {
	ID      string
	BaseURL string
	Token   string
}

type EvidenceClient struct {
	Consumers     map[string]Consumer
	LoggerBaseURL string
	LoggerToken   string
	HTTP          *http.Client
}

func validToken(token string) bool {
	return len(token) >= 32 && strings.TrimSpace(token) == token && !strings.ContainsAny(token, " \t\r\n")
}

func endpoint(base, path string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" ||
		(u.Path != "" && u.Path != "/") {
		return "", ErrInvalid
	}
	if u.Scheme != "https" {
		host := strings.ToLower(u.Hostname())
		ip := net.ParseIP(host)
		if u.Scheme != "http" || !(host == "localhost" || strings.HasSuffix(host, ".svc.cluster.local") || ip != nil && ip.IsLoopback()) {
			return "", ErrInvalid
		}
	}
	u.Path = path
	return u.String(), nil
}

func (c *EvidenceClient) Validate() error {
	if c == nil || len(c.Consumers) == 0 || !validToken(c.LoggerToken) {
		return ErrInvalid
	}
	if _, err := endpoint(c.LoggerBaseURL, "/"); err != nil {
		return err
	}
	for id, consumer := range c.Consumers {
		if id != consumer.ID || !consumerPattern.MatchString(id) || !validToken(consumer.Token) {
			return ErrInvalid
		}
		if _, err := endpoint(consumer.BaseURL, "/"); err != nil {
			return err
		}
	}
	return nil
}

func (c *EvidenceClient) request(ctx context.Context, method, endpointURL, token string, body, result any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return ErrInvalid
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpointURL, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{Timeout: 25 * time.Second}
	if c.HTTP != nil {
		client = *c.HTTP
		if client.Timeout == 0 {
			client.Timeout = 25 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(strings.ToLower(response.Header.Get("Cache-Control")), "no-store") {
		return fmt.Errorf("%w: authoritative evidence endpoint returned %d", ErrBlocked, response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (128<<10)+1))
	if err != nil || len(raw) > 128<<10 {
		return ErrBlocked
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(result) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrBlocked
	}
	return nil
}

func (c *EvidenceClient) Proofs(ctx context.Context, ids []string, request ReconcileRequest) ([]ConsumerProof, error) {
	proofs := make([]ConsumerProof, 0, len(ids))
	for _, id := range ids {
		consumer, ok := c.Consumers[id]
		if !ok {
			return nil, ErrBlocked
		}
		u, err := endpoint(consumer.BaseURL, "/v1/internal/billing-raw-lifecycle/reconcile")
		if err != nil {
			return nil, err
		}
		var proof ConsumerProof
		if err = c.request(ctx, http.MethodPost, u, consumer.Token, request, &proof); err != nil {
			return nil, err
		}
		if !validProof(proof, id, request, time.Now().UTC()) {
			return nil, ErrBlocked
		}
		proofs = append(proofs, proof)
	}
	return proofs, nil
}

func (c *EvidenceClient) Terminal(ctx context.Context, op Operation) (TerminalReceipt, error) {
	u, err := endpoint(c.LoggerBaseURL, "/v1/internal/billing-lifecycle/retire/"+op.OperationID)
	if err != nil {
		return TerminalReceipt{}, err
	}
	var receipt TerminalReceipt
	if err = c.request(ctx, http.MethodGet, u, c.LoggerToken, nil, &receipt); err != nil {
		return receipt, err
	}
	if receipt.OperationID != op.OperationID || receipt.Scope != op.Scope || receipt.FromSequence != op.FromSequence ||
		receipt.ThroughSequence != op.ThroughSequence || receipt.PlanSHA256 != op.PlanSHA256 || receipt.SetID != op.SetID ||
		(receipt.Status != "completed" && receipt.Status != "aborted") || !digestPattern.MatchString(receipt.ReceiptSHA256) ||
		receipt.Status == "completed" && receipt.RetiredThrough < receipt.ThroughSequence ||
		receipt.CompletedAt.IsZero() || receipt.CompletedAt.After(time.Now().Add(30*time.Second)) {
		return receipt, ErrBlocked
	}
	claimed := receipt.ReceiptSHA256
	receipt.ReceiptSHA256 = ""
	actual := digest(receipt)
	receipt.ReceiptSHA256 = claimed
	if actual != claimed {
		return receipt, ErrBlocked
	}
	return receipt, nil
}
